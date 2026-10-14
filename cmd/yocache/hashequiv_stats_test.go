package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHashEquivStats(t *testing.T) {
	var stats hashEquivRequestStats
	if got := stats.snapshot(); got != (hashEquivStatsSnapshot{}) {
		t.Fatalf("empty stats = %+v", got)
	}
	stats.add(1)
	if got := stats.snapshot(); got != (hashEquivStatsSnapshot{Num: 1, TotalTime: 1, MaxTime: 1, Average: 1}) {
		t.Fatalf("single sample = %+v", got)
	}
	stats.add(2)
	stats.add(3)
	if got := stats.snapshot(); got != (hashEquivStatsSnapshot{Num: 3, TotalTime: 6, MaxTime: 3, Average: 2, Stdev: 1}) {
		t.Fatalf("three samples = %+v", got)
	}
}

func TestHashEquivStatsConcurrent(t *testing.T) {
	var stats hashEquivRequestStats
	var workers sync.WaitGroup
	for worker := 0; worker < 10; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for sample := 0; sample < 100; sample++ {
				stats.add(1)
				stats.snapshot()
			}
		}()
	}
	workers.Wait()
	if got := stats.snapshot(); got != (hashEquivStatsSnapshot{Num: 1000, TotalTime: 1000, MaxTime: 1, Average: 1}) {
		t.Fatalf("concurrent samples = %+v", got)
	}
}

func TestHashEquivGetStats(t *testing.T) {
	for _, transport := range []string{"tcp", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h := newTestHashEquiv(t)
			var conn heqTransport
			if transport == "tcp" {
				conn = dialHeqTCP(t, startTestHeqTCPServer(t, h))
			} else {
				server := httptest.NewServer(http.HandlerFunc(h.handle))
				t.Cleanup(server.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				t.Cleanup(cancel)
				ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { ws.CloseNow() })
				conn = &heConn{ws: ws, ctx: ctx}
				if err := conn.send("OEHASHEQUIV 1.1"); err != nil {
					t.Fatal(err)
				}
				if err := conn.send(""); err != nil {
					t.Fatal(err)
				}
			}
			readStats := func(wantNum uint64) {
				t.Helper()
				if err := conn.sendMessage(map[string]any{"get-stats": nil}); err != nil {
					t.Fatal(err)
				}
				response, err := conn.recvMessage()
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(response["requests"], &fields); err != nil {
					t.Fatalf("requests missing from %v: %v", response, err)
				}
				if len(fields) != 5 {
					t.Fatalf("stats fields = %v", fields)
				}
				for _, key := range []string{"num", "total_time", "max_time", "average", "stdev"} {
					if _, ok := fields[key]; !ok {
						t.Fatalf("missing stats field %q", key)
					}
				}
				var got hashEquivStatsSnapshot
				if err := json.Unmarshal(response["requests"], &got); err != nil {
					t.Fatal(err)
				}
				if got.Num != wantNum || got.TotalTime < 0 || got.MaxTime < 0 || got.Stdev < 0 {
					t.Fatalf("stats = %+v, want num %d", got, wantNum)
				}
				if wantNum == 0 && got != (hashEquivStatsSnapshot{}) {
					t.Fatalf("initial stats = %+v", got)
				}
				if wantNum > 0 && (got.TotalTime <= 0 || math.Abs(got.Average-got.TotalTime/float64(got.Num)) > 1e-12) {
					t.Fatalf("invalid timing stats = %+v", got)
				}
			}
			readStats(0)
			if err := conn.sendMessage(map[string]any{"ping": nil}); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.recvMessage(); err != nil {
				t.Fatal(err)
			}
			readStats(2)
			for _, command := range []string{"get-stream", "exists-stream"} {
				if err := conn.sendMessage(map[string]any{command: nil}); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.recv(); err != nil {
					t.Fatal(err)
				}
				for _, line := range []string{"m missing", "END"} {
					if err := conn.send(line); err != nil {
						t.Fatal(err)
					}
					if _, err := conn.recv(); err != nil {
						t.Fatal(err)
					}
				}
			}
			readStats(7)
			other := dialHeqTCP(t, startTestHeqTCPServer(t, h))
			if err := other.sendMessage(map[string]any{"get-stats": nil}); err != nil {
				t.Fatal(err)
			}
			response, err := other.recvMessage()
			if err != nil {
				t.Fatal(err)
			}
			var shared hashEquivStatsSnapshot
			if err := json.Unmarshal(response["requests"], &shared); err != nil {
				t.Fatal(err)
			}
			if shared.Num != 8 {
				t.Fatalf("stats on another connection = %+v, want num 8", shared)
			}
		})
	}
}
