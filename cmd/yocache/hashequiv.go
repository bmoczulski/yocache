package main

// Hash-equivalence server, spoken over WebSocket.
//
// bitbake's hash-equivalence service (which lets independent builders agree that
// two different task input hashes produce the same output, so sstate is reused
// across machines) talks a bespoke line-delimited JSON protocol — "OEHASHEQUIV"
// over bb.asyncrpc. That protocol is also offered natively over ws://, and
// BB_HASHSERVE accepts a ws/wss address directly, so yocache can BE the
// authoritative hash-equiv server over an HTTP-friendly transport (no custom TCP
// port between machines, TLS via wss, shares port 6768). Point a build at it with
//
//     BB_HASHSERVE = "ws://yocache.local:6768/hashequiv"
//
// (in local.conf/site.conf — cooker reads BB_HASHSERVE, a per-recipe class can't
// set it).
//
// Over WebSocket each message is already framed, so none of the newline-framing
// or 32 KiB chunking in bb.asyncrpc's StreamConnection applies — one frame is one
// message. We mirror the subset of the protocol a build actually exercises
// (verified against bitbake's siggen + hashserv.server): the handshake, ping,
// get/get-outhash/report/report-equiv, and the get-stream/exists-stream pipelined
// modes. Admin/gc/user RPCs and backfill-wait are never issued by a build and are
// not implemented.
//
// This is the thin slice: a SQLite-backed store (hashequiv_store.go) with first-
// write-wins unihashes and NO cross-output equivalence dedup yet (a reported
// outhash never unifies two different taskhashes). That already shares unihashes
// for identical taskhashes across machines, and now survives a restart; output-
// based equivalence is the next follow-up.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

const hashEquivProto = "OEHASHEQUIV"

// outhashRecord is one row of the outhashes table: an output hash and the
// taskhash/unihash that produced it. It backs get-outhash, and gives the later
// equivalence dedup data to query; the optional report fields
// (owner/PN/PV/PR/task/outhash_siginfo) are ignored for now.
type outhashRecord struct {
	Method   string
	Taskhash string
	Outhash  string
	Unihash  string
}

// hashEquiv is the WebSocket handler that serves the OEHASHEQUIV protocol over
// the SQLite-backed store (hashequiv_store.go).
type hashEquiv struct {
	store  *hashEquivStore
	log    *slog.Logger
	ledger *Ledger
}

func newHashEquiv(store *hashEquivStore, log *slog.Logger, ledger *Ledger) *hashEquiv {
	return &hashEquiv{store: store, log: log, ledger: ledger}
}

// heqTransport is the send/recv contract serve/dispatch/handle* need from a
// connection, satisfied by both heConn (WebSocket) and heTCPConn (raw TCP,
// hashequiv_tcp.go).
type heqTransport interface {
	recv() (string, error)
	send(msg string) error
	recvMessage() (map[string]json.RawMessage, error)
	sendMessage(v any) error
}

// heConn wraps a WebSocket with the send/recv helpers that mirror bb.asyncrpc's
// StreamConnection: recv/send move one raw frame (a "line"), recvMessage/
// sendMessage move one JSON frame. Over WebSocket framing is intrinsic, so there
// is no newline or chunk handling.
type heConn struct {
	ws  *websocket.Conn
	ctx context.Context
}

func (c *heConn) recv() (string, error) {
	_, data, err := c.ws.Read(c.ctx)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *heConn) send(msg string) error {
	return c.ws.Write(c.ctx, websocket.MessageText, []byte(msg))
}

func (c *heConn) recvMessage() (map[string]json.RawMessage, error) {
	line, err := c.recv()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil, fmt.Errorf("bad message %q: %w", line, err)
	}
	return m, nil
}

func (c *heConn) sendMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.send(string(b))
}

func (h *hashEquiv) handle(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Internal build-farm service; the Origin check is browser-CSRF
		// protection and bitbake's client is not a browser.
		InsecureSkipVerify: true,
	})
	if err != nil {
		h.log.Warn("hashequiv: accept failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	// report payloads can carry outhash_siginfo (large) when
	// SSTATE_HASHEQUIV_REPORT_TASKDATA=1; don't truncate them.
	ws.SetReadLimit(32 << 20)
	defer ws.CloseNow()

	conn := &heConn{ws: ws, ctx: r.Context()}
	switch err := h.serve(conn, r.RemoteAddr); {
	case err == nil:
	case errors.Is(err, io.EOF),
		errors.Is(err, context.Canceled),
		websocket.CloseStatus(err) == websocket.StatusNormalClosure,
		websocket.CloseStatus(err) == websocket.StatusGoingAway,
		websocket.CloseStatus(err) == websocket.StatusAbnormalClosure:
		h.log.Debug("hashequiv: client disconnected", "remote", r.RemoteAddr)
	default:
		h.log.Warn("hashequiv: client error", "err", err, "remote", r.RemoteAddr)
	}
}

// serve runs the per-connection handshake then the message loop. It returns the
// first transport error (including a normal close), which handle() classifies.
func (h *hashEquiv) serve(c heqTransport, remote string) error {
	// Handshake line: "OEHASHEQUIV <version>".
	hello, err := c.recv()
	if err != nil {
		return err
	}
	parts := strings.Fields(hello)
	if len(parts) != 2 || parts[0] != hashEquivProto {
		return fmt.Errorf("bad protocol header %q", hello)
	}
	if !validProtoVersion(parts[1]) {
		return fmt.Errorf("unsupported protocol version %q", parts[1])
	}

	// Headers until an empty frame. bitbake sends "needs-headers: false" by
	// default; we expose no server headers, so we only have to terminate the
	// (empty) reply block if the client asked for one.
	needsHeaders := false
	for {
		hdr, err := c.recv()
		if err != nil {
			return err
		}
		if hdr == "" {
			break
		}
		if tag, val, ok := strings.Cut(hdr, ":"); ok &&
			strings.EqualFold(strings.TrimSpace(tag), "needs-headers") {
			needsHeaders = strings.TrimSpace(val) == "true"
		}
	}
	if needsHeaders {
		if err := c.send(""); err != nil {
			return err
		}
	}

	h.log.Info("hashequiv: client connected", "remote", remote, "version", parts[1])

	for {
		msg, err := c.recvMessage()
		if err != nil {
			return err
		}
		if err := h.dispatch(c, msg); err != nil {
			return err
		}
	}
}

// dispatch handles one message. Each message is an object with a single command
// key, mirroring bb.asyncrpc's dispatch_message.
func (h *hashEquiv) dispatch(c heqTransport, msg map[string]json.RawMessage) error {
	if _, ok := msg["get-stream"]; ok {
		return h.handleStream(c, h.lookupStream)
	}
	if _, ok := msg["exists-stream"]; ok {
		return h.handleStream(c, h.existsStream)
	}
	if raw, ok := msg["get"]; ok {
		return h.handleGet(c, raw)
	}
	if raw, ok := msg["get-outhash"]; ok {
		return h.handleGetOuthash(c, raw)
	}
	if raw, ok := msg["report"]; ok {
		return h.handleReport(c, raw)
	}
	if raw, ok := msg["report-equiv"]; ok {
		return h.handleReportEquiv(c, raw)
	}
	if _, ok := msg["ping"]; ok {
		return c.sendMessage(map[string]any{"alive": true})
	}

	var cmd string
	for k := range msg {
		cmd = k
		break
	}
	h.log.Warn("hashequiv: unrecognized command", "cmd", cmd)
	return c.sendMessage(invokeErr(fmt.Errorf("Unrecognized command %q", cmd)))
}

// handleStream mirrors bb.asyncrpc._stream_handler: ack with a JSON-quoted "ok"
// (the client reads it as a message), then for each raw line send fn(line) as a
// raw frame, until "END" (or an empty line / close), then a raw "ok" ack.
func (h *hashEquiv) handleStream(c heqTransport, fn func(string) string) error {
	if err := c.sendMessage("ok"); err != nil {
		return err
	}
	for {
		line, err := c.recv()
		if err != nil {
			return err
		}
		if line == "" || line == "END" {
			break
		}
		if err := c.send(fn(line)); err != nil {
			return err
		}
	}
	return c.send("ok")
}

// lookupStream backs get-stream: "<method> <taskhash>" -> unihash, or "" on miss
// (which bitbake reads as "no equivalence, use the taskhash"). A store error is
// logged and degraded to a miss — a flaky DB costs a cache hit, it doesn't stall
// the build.
func (h *hashEquiv) lookupStream(line string) string {
	f := strings.Fields(line)
	if len(f) != 2 {
		return ""
	}
	u, ok, err := h.store.getEquivalent(f[0], f[1])
	if err != nil {
		h.log.Warn("hashequiv: get-stream lookup failed", "err", err, "method", f[0])
		return ""
	}
	if !ok {
		return ""
	}
	return u
}

// existsStream backs exists-stream: a unihash -> "true"/"false". A store error is
// logged and degraded to "false" (treated as absent) for the same reason.
func (h *hashEquiv) existsStream(line string) string {
	ok, err := h.store.unihashExists(line)
	if err != nil {
		h.log.Warn("hashequiv: exists-stream lookup failed", "err", err)
		return "false"
	}
	if ok {
		return "true"
	}
	return "false"
}

func (h *hashEquiv) handleGet(c heqTransport, raw json.RawMessage) error {
	var req struct {
		Taskhash string `json:"taskhash"`
		Method   string `json:"method"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return c.sendMessage(invokeErr(err))
	}
	u, ok, err := h.store.getEquivalent(req.Method, req.Taskhash)
	if err != nil {
		h.log.Warn("hashequiv: get lookup failed", "err", err, "method", req.Method)
		return c.sendMessage(nil) // degrade to a miss; the build recomputes
	}
	if !ok {
		return c.sendMessage(nil) // JSON null — bitbake's "no result"
	}
	return c.sendMessage(map[string]any{
		"method":   req.Method,
		"taskhash": req.Taskhash,
		"unihash":  u,
	})
}

func (h *hashEquiv) handleGetOuthash(c heqTransport, raw json.RawMessage) error {
	var req struct {
		Method   string `json:"method"`
		Outhash  string `json:"outhash"`
		Taskhash string `json:"taskhash"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return c.sendMessage(invokeErr(err))
	}
	rec, ok, err := h.store.getOuthash(req.Method, req.Outhash)
	if err != nil {
		h.log.Warn("hashequiv: get-outhash lookup failed", "err", err, "method", req.Method)
		return c.sendMessage(nil)
	}
	if !ok {
		return c.sendMessage(nil)
	}
	return c.sendMessage(map[string]any{
		"method":   rec.Method,
		"outhash":  rec.Outhash,
		"taskhash": rec.Taskhash,
		"unihash":  rec.Unihash,
	})
}

func (h *hashEquiv) handleReport(c heqTransport, raw json.RawMessage) error {
	var req struct {
		Method   string `json:"method"`
		Taskhash string `json:"taskhash"`
		Outhash  string `json:"outhash"`
		Unihash  string `json:"unihash"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return c.sendMessage(invokeErr(err))
	}
	if err := h.store.insertOuthash(outhashRecord{
		Method:   req.Method,
		Taskhash: req.Taskhash,
		Outhash:  req.Outhash,
		Unihash:  req.Unihash,
	}); err != nil {
		h.log.Warn("hashequiv: outhash persist failed", "err", err, "method", req.Method)
	}
	// Thin slice: no cross-output dedup, so the in-effect unihash is simply the
	// first one reported for this (method, taskhash). On a write failure, fall
	// back to the reported unihash so the build still gets an answer (it just
	// won't be shared with other machines).
	unihash, err := h.store.insertUnihash(req.Method, req.Taskhash, req.Unihash)
	if err != nil {
		h.log.Warn("hashequiv: unihash persist failed", "err", err, "method", req.Method)
		unihash = req.Unihash
	}
	h.log.Info("hashequiv report",
		"method", req.Method,
		"taskhash", short(req.Taskhash),
		"unihash", short(unihash))
	h.ledger.RecordHashEquivSet(req.Method, req.Taskhash, unihash, "")
	return c.sendMessage(map[string]any{
		"taskhash": req.Taskhash,
		"method":   req.Method,
		"unihash":  unihash,
	})
}

func (h *hashEquiv) handleReportEquiv(c heqTransport, raw json.RawMessage) error {
	var req struct {
		Method   string `json:"method"`
		Taskhash string `json:"taskhash"`
		Unihash  string `json:"unihash"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return c.sendMessage(invokeErr(err))
	}
	unihash, err := h.store.insertUnihash(req.Method, req.Taskhash, req.Unihash)
	if err != nil {
		h.log.Warn("hashequiv: unihash persist failed", "err", err, "method", req.Method)
		unihash = req.Unihash
	}
	h.ledger.RecordHashEquivSet(req.Method, req.Taskhash, unihash, "")
	return c.sendMessage(map[string]any{
		"taskhash": req.Taskhash,
		"method":   req.Method,
		"unihash":  unihash,
	})
}

// validProtoVersion mirrors hashserv ServerClient.validate_proto_version:
// accept (1,0) < ver <= (1,1). Today only "1.1" qualifies.
func validProtoVersion(v string) bool {
	var maj, min int
	if _, err := fmt.Sscanf(v, "%d.%d", &maj, &min); err != nil {
		return false
	}
	gtLow := maj > 1 || (maj == 1 && min > 0)
	leHigh := maj < 1 || (maj == 1 && min <= 1)
	return gtLow && leHigh
}

// invokeErr builds the {"invoke-error": {"message": ...}} reply bitbake's client
// recognises and raises (see bb.asyncrpc check_invoke_error).
func invokeErr(err error) map[string]any {
	return map[string]any{"invoke-error": map[string]any{"message": err.Error()}}
}

// short trims a 64-hex hash for readable logs.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
