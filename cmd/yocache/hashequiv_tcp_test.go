package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestHashEquiv bundles a throwaway store, a discard logger, and a
// throwaway ledger into a ready-to-use *hashEquiv, since no existing helper
// builds all three together.
func newTestHashEquiv(t *testing.T) *hashEquiv {
	t.Helper()
	store := newTestStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ledger, err := openLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), log)
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	t.Cleanup(func() { ledger.Close() })
	return newHashEquiv(store, log, ledger)
}

// startTestHeqTCPServer starts a heqTCPServer on a loopback port, serving
// until the test ends, and returns its address.
func startTestHeqTCPServer(t *testing.T, h *hashEquiv) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	s := newHeqTCPServer(ln, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	errCh := make(chan error, 1)
	go func() { errCh <- s.serve() }()
	t.Cleanup(func() {
		s.shutdown(2 * time.Second)
		if err := <-errCh; err != nil {
			t.Errorf("server.serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// dialHeqTCP dials addr and performs the OEHASHEQUIV handshake (no headers
// requested), returning a ready-to-use connection.
func dialHeqTCP(t *testing.T, addr string) *heqTCPConn {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	t.Cleanup(func() { nc.Close() })
	c := newHeqTCPConn(nc)
	if err := c.send("OEHASHEQUIV 1.1"); err != nil {
		t.Fatalf("send handshake: %v", err)
	}
	if err := c.send(""); err != nil { // terminates the (empty) header block
		t.Fatalf("send header terminator: %v", err)
	}
	return c
}

// sendChunked writes msg using bb.asyncrpc's chunkify wire shape: a
// "chunk-stream" marker line, then msg split into maxChunk-sized fragment
// lines, then a terminating empty line. Mirrors connection.py's chunkify()
// so tests can drive the server the same way a real chunking client would.
func sendChunked(c *heqTCPConn, msg string, maxChunk int) error {
	if err := c.send(`{"chunk-stream": null}`); err != nil {
		return err
	}
	for len(msg) > 0 {
		n := maxChunk
		if n > len(msg) {
			n = len(msg)
		}
		if err := c.send(msg[:n]); err != nil {
			return err
		}
		msg = msg[n:]
	}
	return c.send("")
}

func rawJSONString(t *testing.T, m map[string]json.RawMessage, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		t.Fatalf("field %q not a JSON string (%s): %v", key, m[key], err)
	}
	return s
}

// TestHeqTCPConnChunkStreamReassembly exercises heqTCPConn.recvMessage's
// chunk-stream reassembly directly over a net.Pipe(), independent of the
// listener — this is the one genuinely new piece of protocol logic in the
// raw-TCP transport (everything else is wiring already exercised by
// hashequiv.go's WebSocket path).
func TestHeqTCPConnChunkStreamReassembly(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	want := map[string]any{
		"method":          "m",
		"taskhash":        "t",
		"outhash":         "o",
		"unihash":         "u",
		"outhash_siginfo": strings.Repeat("x", 5000),
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	go func() {
		w := newHeqTCPConn(server)
		if err := sendChunked(w, string(wantJSON), 64); err != nil {
			t.Errorf("sendChunked: %v", err)
		}
	}()

	r := newHeqTCPConn(client)
	got, err := r.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage: %v", err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(got): %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("reassembled message = %s, want %s", gotJSON, wantJSON)
	}
}

// TestHeqTCPRoundTrip drives a handshake, a report, and a get back through a
// real listener, confirming hashequiv.go's shared serve/dispatch logic works
// correctly over the raw-TCP transport, not just WebSocket.
func TestHeqTCPRoundTrip(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)
	c := dialHeqTCP(t, addr)

	if err := c.sendMessage(map[string]any{"report": map[string]any{
		"method": "m", "taskhash": "t1", "outhash": "o1", "unihash": "u1",
	}}); err != nil {
		t.Fatalf("sendMessage(report): %v", err)
	}
	resp, err := c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(report ack): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("report ack unihash = %q, want u1", got)
	}

	if err := c.sendMessage(map[string]any{"get": map[string]any{
		"method": "m", "taskhash": "t1",
	}}); err != nil {
		t.Fatalf("sendMessage(get): %v", err)
	}
	resp, err = c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(get): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("get unihash = %q, want u1", got)
	}
}

// TestHeqTCPReportCrossOutputEquivalence drives two reports for different
// taskhashes that happen to produce the same outhash (e.g. a metadata-only
// recipe change that doesn't affect the task's actual output) and confirms the
// later taskhash adopts the earlier one's unihash instead of keeping its own —
// mirroring bitbake's own hashserv get_equivalent_for_outhash.
func TestHeqTCPReportCrossOutputEquivalence(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)
	c := dialHeqTCP(t, addr)

	if err := c.sendMessage(map[string]any{"report": map[string]any{
		"method": "m", "taskhash": "t1", "outhash": "same-out", "unihash": "u1",
	}}); err != nil {
		t.Fatalf("sendMessage(report t1): %v", err)
	}
	resp, err := c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(report t1 ack): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("report t1 ack unihash = %q, want u1 (first report, nothing to unify with)", got)
	}

	// t2 has a different taskhash (different input) but the same outhash
	// (identical task output) — it should be unified onto t1's unihash, not
	// keep its own reported "u2".
	if err := c.sendMessage(map[string]any{"report": map[string]any{
		"method": "m", "taskhash": "t2", "outhash": "same-out", "unihash": "u2",
	}}); err != nil {
		t.Fatalf("sendMessage(report t2): %v", err)
	}
	resp, err = c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(report t2 ack): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("report t2 ack unihash = %q, want u1 (unified with t1's earlier outhash)", got)
	}

	// A subsequent get for t2 must also return the unified unihash.
	if err := c.sendMessage(map[string]any{"get": map[string]any{
		"method": "m", "taskhash": "t2",
	}}); err != nil {
		t.Fatalf("sendMessage(get t2): %v", err)
	}
	resp, err = c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(get t2): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("get t2 unihash = %q, want u1", got)
	}
}

func TestHeqTCPReportEquivAndPing(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)
	c := dialHeqTCP(t, addr)

	if err := c.sendMessage(map[string]any{"ping": nil}); err != nil {
		t.Fatalf("sendMessage(ping): %v", err)
	}
	resp, err := c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(ping): %v", err)
	}
	if got := string(resp["alive"]); got != "true" {
		t.Errorf("ping alive = %s, want true", got)
	}

	if err := c.sendMessage(map[string]any{"report-equiv": map[string]any{
		"method": "m", "taskhash": "t2", "unihash": "u2",
	}}); err != nil {
		t.Fatalf("sendMessage(report-equiv): %v", err)
	}
	resp, err = c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(report-equiv ack): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u2" {
		t.Errorf("report-equiv ack unihash = %q, want u2", got)
	}
}

// TestHeqTCPLargeReportChunked sends a report large enough that a real
// bb.asyncrpc client would chunk it (mirrors bitbake's own
// lib/hashserv/tests.py test_huge_message), through the real listener, and
// confirms the server reassembles and completes it correctly.
func TestHeqTCPLargeReportChunked(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)
	c := dialHeqTCP(t, addr)

	msg := map[string]any{"report": map[string]any{
		"method": "m", "taskhash": "big1", "outhash": "o1", "unihash": "u1",
		"outhash_siginfo": strings.Repeat("0", 40000),
	}}
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := sendChunked(c, string(b), 4096); err != nil {
		t.Fatalf("sendChunked: %v", err)
	}
	resp, err := c.recvMessage()
	if err != nil {
		t.Fatalf("recvMessage(report ack): %v", err)
	}
	if got := rawJSONString(t, resp, "unihash"); got != "u1" {
		t.Errorf("chunked report ack unihash = %q, want u1", got)
	}
}

func TestHeqTCPBadHandshake(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer nc.Close()
	c := newHeqTCPConn(nc)
	if err := c.send("GARBAGE"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := c.recv(); err == nil {
		t.Fatal("recv after bad handshake: want error, got nil")
	}
}

func TestHeqTCPUnsupportedProtoVersion(t *testing.T) {
	h := newTestHashEquiv(t)
	addr := startTestHeqTCPServer(t, h)

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer nc.Close()
	c := newHeqTCPConn(nc)
	if err := c.send("OEHASHEQUIV 0.5"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := c.recv(); err == nil {
		t.Fatal("recv after unsupported version: want error, got nil")
	}
}
