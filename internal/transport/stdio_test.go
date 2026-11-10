package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startProxy runs Proxy against endpoint and returns a client connected to its downstream side.
func startProxy(t *testing.T, endpoint, secret string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	cliT, srvT := mcp.NewInMemoryTransports()
	go Proxy(ctx, endpoint, secret, srvT)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestProxyListsAndForwards(t *testing.T) {
	e := newEnv(t, func() (json.RawMessage, error) { return json.RawMessage(`{"results":[]}`), nil })
	owner := httptest.NewServer(Handler(e.g, e.ks, slog.New(slog.DiscardHandler)))
	t.Cleanup(owner.Close)
	cs := startProxy(t, owner.URL+"/mcp", e.rw)

	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 5 {
		t.Fatalf("got %d tools, want 5", len(list.Tools))
	}
	res, err := callTool(t, cs, "memory_recall", map[string]any{"query": "q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || resultText(t, res) != `{"results":[]}` {
		t.Fatalf("recall IsError=%v text=%q", res.IsError, resultText(t, res))
	}
	if e.fb.count() != 1 {
		t.Fatalf("backend called %d times, want 1", e.fb.count())
	}
}

func TestProxyIndeterminateWhenOwnerGone(t *testing.T) {
	e := newEnv(t, nil)
	owner := httptest.NewServer(Handler(e.g, e.ks, slog.New(slog.DiscardHandler)))
	cs := startProxy(t, owner.URL+"/mcp", e.rw)
	if _, err := callTool(t, cs, "memory_stats", nil); err != nil {
		t.Fatal(err)
	}

	owner.Close()
	before := e.fb.count()
	res, err := callTool(t, cs, "memory_remember", map[string]any{"content": "x"})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	want := "indeterminate: lost contact with the memory service; " + indeterminate
	if !res.IsError || resultText(t, res) != want {
		t.Fatalf("IsError=%v text=%q", res.IsError, resultText(t, res))
	}
	if e.fb.count() != before {
		t.Fatal("backend saw a call after the owner was gone")
	}
}

// At login the service and the agents start together; the adapter must wait for
// the owner rather than fail the agent's whole session.
func TestProxyWaitsForOwnerToStart(t *testing.T) {
	e := newEnv(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here until the owner starts below

	started := make(chan *httptest.Server, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			started <- nil
			return
		}
		owner := httptest.NewUnstartedServer(Handler(e.g, e.ks, slog.New(slog.DiscardHandler)))
		owner.Listener = ln
		owner.Start()
		started <- owner
	}()
	cs := startProxy(t, "http://"+addr+"/mcp", e.rw)
	if owner := <-started; owner != nil {
		t.Cleanup(owner.Close)
	}
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 5 {
		t.Fatalf("got %d tools, want 5", len(list.Tools))
	}
}

func TestProxyFailsOnBadKey(t *testing.T) {
	e := newEnv(t, nil)
	owner := httptest.NewServer(Handler(e.g, e.ks, slog.New(slog.DiscardHandler)))
	t.Cleanup(owner.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, srvT := mcp.NewInMemoryTransports()
	bad := "msm_0000000000000000_" + strings.Repeat("A", 43)
	if err := Proxy(ctx, owner.URL+"/mcp", bad, srvT); err == nil {
		t.Fatal("bad key: no error")
	}
}

func TestProxyRefusesUnsafeEndpoints(t *testing.T) {
	for endpoint, want := range map[string]string{
		"http://example.com/mcp":            "want https",
		"https://u:msm_secret@example.com/": "must not contain credentials",
		"https://example.com/mcp?key=x":     "must not contain credentials",
	} {
		_, srvT := mcp.NewInMemoryTransports()
		if err := Proxy(t.Context(), endpoint, "msm_x", srvT); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", endpoint, err, want)
		}
	}
}

func TestProxyRejectsOversizedArgsAndStaysUsable(t *testing.T) {
	e := newEnv(t, func() (json.RawMessage, error) { return json.RawMessage(`{}`), nil })
	owner := httptest.NewServer(Handler(e.g, e.ks, slog.New(slog.DiscardHandler)))
	t.Cleanup(owner.Close)
	cs := startProxy(t, owner.URL+"/mcp", e.rw)

	res, err := callTool(t, cs, "memory_recall", map[string]any{"query": strings.Repeat("x", 300<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(resultText(t, res), "validation:") {
		t.Fatalf("oversized call: IsError=%v text=%q, want a validation error", res.IsError, resultText(t, res))
	}
	if res, err := callTool(t, cs, "memory_stats", nil); err != nil || res.IsError {
		t.Fatalf("adapter unusable after oversized call: %v %+v", err, res)
	}
	if e.fb.count() != 1 {
		t.Fatalf("backend called %d times, want 1 (stats only)", e.fb.count())
	}
}
