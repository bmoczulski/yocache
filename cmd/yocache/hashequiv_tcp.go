package main

// Raw-TCP transport for the OEHASHEQUIV protocol (see hashequiv.go for the
// WebSocket variant and the shared protocol/business logic).
//
// Pre-Scarthgap bitbake (Dunfell and earlier) has no ws:// client for hash
// equivalence — its only client speaks bb.asyncrpc's StreamConnection
// directly over a plain socket: newline-delimited lines, with large messages
// split into a "chunk-stream" marker plus 32 KiB fragment lines (see
// bb/asyncrpc/connection.py). heqTCPConn reproduces that framing so the
// existing serve/dispatch/handle* logic in hashequiv.go can run over it
// unchanged via the heqTransport interface.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// heqTCPConn wraps a net.Conn with the same recv/send/recvMessage/sendMessage
// contract as heConn (heqTransport), implementing bb.asyncrpc's
// StreamConnection framing: newline-delimited lines, with receive-side
// chunk-stream reassembly for large client->server messages.
type heqTCPConn struct {
	r *bufio.Reader
	w *bufio.Writer
}

func newHeqTCPConn(c net.Conn) *heqTCPConn {
	return &heqTCPConn{r: bufio.NewReader(c), w: bufio.NewWriter(c)}
}

func (c *heqTCPConn) recv() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		if line != "" && err == io.EOF {
			// A partial line before the peer closed isn't a valid frame;
			// surface it as an unexpected EOF so callers' error
			// classification (io.EOF = clean disconnect) doesn't misclassify
			// a mid-message drop as a normal close.
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimRight(line, "\n"), nil
}

func (c *heqTCPConn) send(msg string) error {
	if _, err := c.w.WriteString(msg); err != nil {
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		return err
	}
	return c.w.Flush()
}

// recvMessage mirrors bb.asyncrpc's StreamConnection.recv_message: a message
// is either one JSON line, or (for messages too large to fit in one 32 KiB
// line) a `{"chunk-stream": null}` marker line followed by raw fragment
// lines concatenated up to a terminating empty line.
func (c *heqTCPConn) recvMessage() (map[string]json.RawMessage, error) {
	line, err := c.recv()
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil, fmt.Errorf("bad message %q: %w", line, err)
	}
	if _, chunked := m["chunk-stream"]; !chunked {
		return m, nil
	}

	var sb strings.Builder
	for {
		l, err := c.recv()
		if err != nil {
			return nil, err
		}
		if l == "" {
			break
		}
		sb.WriteString(l)
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sb.String()), &full); err != nil {
		return nil, fmt.Errorf("bad chunked message: %w", err)
	}
	return full, nil
}

// sendMessage always sends a single line — no send-side chunking. Every
// server->client reply in this protocol (a hash, a bool, or a short echo
// object) is well under 32 KiB; the only field big enough to plausibly need
// chunking (outhash_siginfo) only ever flows client->server on `report`
// (see bb/siggen.py's report_unihash, gated by
// SSTATE_HASHEQUIV_REPORT_TASKDATA). Don't add chunkify-equivalent logic here
// without a real oversized server response to justify it.
func (c *heqTCPConn) sendMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.send(string(b))
}

// heqTCPServer runs the raw-TCP OEHASHEQUIV accept loop and tracks in-flight
// connections so shutdown can drain them — net.Conn has no built-in
// equivalent of http.Server.Shutdown.
type heqTCPServer struct {
	ln  net.Listener
	h   *hashEquiv
	log *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newHeqTCPServer(ln net.Listener, h *hashEquiv, log *slog.Logger) *heqTCPServer {
	return &heqTCPServer{ln: ln, h: h, log: log, conns: make(map[net.Conn]struct{})}
}

func (s *heqTCPServer) addConn(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (s *heqTCPServer) removeConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *heqTCPServer) closeAllConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		c.Close()
	}
}

// serve accepts connections until the listener is closed (by shutdown, from
// another goroutine), dispatching each to h.serve over a heqTCPConn. It
// returns nil on a shutdown-triggered close, or the first unexpected Accept
// error otherwise.
func (s *heqTCPServer) serve() error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.addConn(nc)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.removeConn(nc)
			defer nc.Close()
			c := newHeqTCPConn(nc)
			switch err := s.h.serve(c, nc.RemoteAddr().String()); {
			case err == nil:
			case errors.Is(err, io.EOF),
				errors.Is(err, io.ErrUnexpectedEOF),
				errors.Is(err, net.ErrClosed):
				s.log.Debug("hashequiv-tcp: client disconnected", "remote", nc.RemoteAddr())
			default:
				s.log.Warn("hashequiv-tcp: client error", "err", err, "remote", nc.RemoteAddr())
			}
		}()
	}
}

// shutdown closes the listener (stopping new accepts), waits up to timeout
// for in-flight connections to finish naturally, then force-closes any that
// remain — these are long-lived, mostly-idle protocol connections, so a
// build-side hash lookup blocks the build regardless of whether we let it
// finish; forcing closed after the grace period is the pragmatic choice.
func (s *heqTCPServer) shutdown(timeout time.Duration) {
	s.ln.Close()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		s.closeAllConns()
		<-done
	}
}
