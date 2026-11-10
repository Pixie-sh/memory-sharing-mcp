package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pixie-sh/memory-sharing-mcp/internal/gateway"
	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

// fakeBackend records every operation and answers with reply (or {}).
type fakeBackend struct {
	mu    sync.Mutex
	ops   []string
	reply func() (json.RawMessage, error)
}

func (f *fakeBackend) Call(_ context.Context, op string, _ any) (json.RawMessage, error) {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
	if f.reply == nil {
		return json.RawMessage(`{}`), nil
	}
	return f.reply()
}

func (f *fakeBackend) Close(context.Context) {}

func (f *fakeBackend) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ops)
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a running server over a real registry, a gateway, and a fake backend.
type env struct {
	t      *testing.T
	ks     *keys.Set
	path   string
	fb     *fakeBackend
	g      *gateway.Gateway
	url    string
	logs   *syncBuf
	rw, ro string // secrets of a read-write and a read-only key
	rwID   string
	roID   string
}

func newEnv(t *testing.T, reply func() (json.RawMessage, error)) *env {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keys.json")
	rw, rwKey, err := keys.Generate(path, "writer", keys.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	ro, roKey, err := keys.Generate(path, "reader", keys.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := keys.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBackend{reply: reply}
	g, err := gateway.New(context.Background(), gateway.Config{}, func(context.Context) (gateway.Backend, error) { return fb, nil }, ks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.Shutdown(ctx)
	})
	logs := &syncBuf{}
	srv := httptest.NewServer(Handler(g, ks, slog.New(slog.NewTextHandler(logs, nil))))
	t.Cleanup(srv.Close)
	return &env{t: t, ks: ks, path: path, fb: fb, g: g, url: srv.URL, logs: logs,
		rw: rw, ro: ro, rwID: rwKey.ID, roID: roKey.ID}
}

// post sends a JSON POST and returns the status and body. An empty token sends no Authorization header.
func (e *env) post(path, token, body string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodPost, e.url+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(e.t, req)
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func expectError(t *testing.T, status int, body string, wantStatus int, wantClass string) {
	t.Helper()
	var got struct {
		Error string `json:"error"`
	}
	if status != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", status, wantStatus, body)
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.Error != wantClass {
		t.Fatalf("body = %s, want error class %q", body, wantClass)
	}
}

func TestOutcome(t *testing.T) {
	cases := []struct {
		err    error
		class  string
		status int
	}{
		{fmt.Errorf("x: %w", gateway.ErrValidation), "validation", 400},
		{gateway.ErrForbidden, "forbidden", 403},
		{gateway.ErrBusy, "busy", 429},
		{gateway.ErrUnavailable, "unavailable", 503},
		{gateway.ErrShuttingDown, "unavailable", 503},
		{fmt.Errorf("x: %w", provider.ErrIndeterminate), "indeterminate", 502},
		{&provider.OpError{Msg: "boom"}, "provider", 422},
		{context.Canceled, "indeterminate", 502},
		{context.DeadlineExceeded, "indeterminate", 502},
		{errors.New("other"), "internal", 500},
	}
	for _, c := range cases {
		class, status, _ := outcome(c.err)
		if class != c.class || status != c.status {
			t.Errorf("outcome(%v) = %s %d, want %s %d", c.err, class, status, c.class, c.status)
		}
	}
}

func TestRESTAuth(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]string{
		"missing":      "",
		"malformed":    "msm_bogus",
		"unknown key":  "msm_0000000000000000_" + strings.Repeat("A", 43),
		"wrong secret": e.rw[:21] + strings.Repeat("A", 43), // real ID, wrong random part
	}
	for name, token := range cases {
		status, body := e.post("/v1/recall", token, `{"query":"q"}`)
		expectError(t, status, body, http.StatusUnauthorized, "unauthorized")
		if strings.Contains(body, e.rw) {
			t.Errorf("%s: body leaks secret", name)
		}
	}
	if n := e.fb.count(); n != 0 {
		t.Errorf("backend called %d times for rejected requests", n)
	}
}

func TestRESTRevokedKey(t *testing.T) {
	e := newEnv(t, nil)
	if err := keys.Revoke(e.path, e.rwID); err != nil {
		t.Fatal(err)
	}
	if err := e.ks.Reload(); err != nil {
		t.Fatal(err)
	}
	status, body := e.post("/v1/recall", e.rw, `{"query":"q"}`)
	expectError(t, status, body, http.StatusUnauthorized, "unauthorized")
	if n := e.fb.count(); n != 0 {
		t.Errorf("backend called %d times for a revoked key", n)
	}
}

func TestRESTErrors(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		body     string
		readOnly bool
		reply    func() (json.RawMessage, error)
		status   int
		class    string
	}{
		{"validation", "/v1/recall", `{"query":"   "}`, false, nil, 400, "validation"},
		{"forbidden", "/v1/remember", `{"content":"x"}`, true, nil, 403, "forbidden"},
		{"provider", "/v1/recall", `{"query":"q"}`, false,
			func() (json.RawMessage, error) { return nil, &provider.OpError{Msg: "rejected"} }, 422, "provider"},
		{"indeterminate", "/v1/remember", `{"content":"x"}`, false,
			func() (json.RawMessage, error) { return nil, fmt.Errorf("lost: %w", provider.ErrIndeterminate) }, 502, "indeterminate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, c.reply)
			token := e.rw
			if c.readOnly {
				token = e.ro
			}
			status, body := e.post(c.path, token, c.body)
			expectError(t, status, body, c.status, c.class)
		})
	}
}

func TestRESTAcceptsMaxEscapedContent(t *testing.T) {
	e := newEnv(t, func() (json.RawMessage, error) { return json.RawMessage(`{"id":"m1"}`), nil })
	content, _ := json.Marshal(map[string]string{"content": strings.Repeat("\x01", 32768)})
	if status, body := e.post("/v1/remember", e.rw, string(content)); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
}

func TestRESTBodyRejections(t *testing.T) {
	e := newEnv(t, nil)
	big := `{"content":"` + strings.Repeat("a", maxBody) + `"}`
	status, body := e.post("/v1/remember", e.rw, big)
	expectError(t, status, body, http.StatusRequestEntityTooLarge, "too_large")

	status, body = e.post("/v1/recall", e.rw, `{"query":"q","bogus":1}`)
	expectError(t, status, body, http.StatusBadRequest, "validation")

	if n := e.fb.count(); n != 0 {
		t.Errorf("backend called %d times for rejected bodies", n)
	}
}

func TestRESTSuccessReturnsContractResult(t *testing.T) {
	e := newEnv(t, func() (json.RawMessage, error) {
		return json.RawMessage(`{"results":[{"id":"m1","memory":"hello"}]}`), nil
	})
	status, body := e.post("/v1/recall", e.rw, `{"query":"hello"}`)
	const want = `{"results":[{"id":"memory:m1","memory":"hello","provider":"memory"}]}`
	if status != http.StatusOK || body != want {
		t.Fatalf("got %d %s, want 200 %s", status, body, want)
	}
}

// mcpRecorder authenticates every request with token and records each response status.
type mcpRecorder struct {
	token    string
	mu       sync.Mutex
	statuses []int
}

func (r *mcpRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		r.mu.Lock()
		r.statuses = append(r.statuses, resp.StatusCode)
		r.mu.Unlock()
	}
	return resp, err
}

func (r *mcpRecorder) lastStatus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statuses[len(r.statuses)-1]
}

func (r *mcpRecorder) maxStatus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := 0
	for _, s := range r.statuses {
		m = max(m, s)
	}
	return m
}

// connect opens an MCP session authenticated with token. Standalone SSE and retries
// are off so that a revoked key fails on the next call instead of being retried.
func (e *env) connect(token string) (*mcp.ClientSession, *mcpRecorder) {
	e.t.Helper()
	rec := &mcpRecorder{token: token}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	tr := &mcp.StreamableClientTransport{
		Endpoint:             e.url + "/mcp",
		HTTPClient:           &http.Client{Transport: rec},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}
	cs, err := client.Connect(e.t.Context(), tr, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs, rec
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	return cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("got %d content blocks, want 1", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want *mcp.TextContent", res.Content[0])
	}
	return tc.Text
}

func TestMCPToolsListAndRecall(t *testing.T) {
	long := `{"results":[{"id":"m1","memory":"` + strings.Repeat("y", 5000) + `"}]}`
	e := newEnv(t, func() (json.RawMessage, error) { return json.RawMessage(long), nil })
	cs, _ := e.connect(e.rw)

	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
		if tool.Name == "memory_recall" {
			schema, _ := json.Marshal(tool.InputSchema)
			var s struct {
				Required []string `json:"required"`
			}
			json.Unmarshal(schema, &s)
			if len(s.Required) != 1 || s.Required[0] != "query" {
				t.Errorf("recall required = %v, want [query]", s.Required)
			}
		}
	}
	for _, want := range []string{"memory_recall", "memory_remember", "memory_stats", "memory_forget", "memory_sleep"} {
		if !names[want] {
			t.Errorf("tool %s missing; have %v", want, names)
		}
	}
	if len(list.Tools) != 5 {
		t.Errorf("got %d tools, want 5", len(list.Tools))
	}

	res, err := callTool(t, cs, "memory_recall", map[string]any{"query": "y"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(t, res), strings.Repeat("y", 5000)) {
		t.Fatalf("recall IsError=%v, text not the complete backend result", res.IsError)
	}
}

func TestMCPFailuresAreToolErrorsOn200(t *testing.T) {
	t.Run("indeterminate", func(t *testing.T) {
		e := newEnv(t, func() (json.RawMessage, error) { return nil, fmt.Errorf("lost: %w", provider.ErrIndeterminate) })
		cs, rec := e.connect(e.rw)
		res, err := callTool(t, cs, "memory_remember", map[string]any{"content": "x"})
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		if !res.IsError || !strings.HasPrefix(resultText(t, res), "indeterminate: ") {
			t.Fatalf("got IsError=%v text=%q", res.IsError, resultText(t, res))
		}
		if m := rec.maxStatus(); m >= 400 {
			t.Errorf("HTTP status %d, want all < 400", m)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		e := newEnv(t, nil)
		cs, rec := e.connect(e.ro)
		res, err := callTool(t, cs, "memory_remember", map[string]any{"content": "x"})
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		if !res.IsError || !strings.HasPrefix(resultText(t, res), "forbidden: ") {
			t.Fatalf("got IsError=%v text=%q", res.IsError, resultText(t, res))
		}
		if m := rec.maxStatus(); m >= 400 {
			t.Errorf("HTTP status %d, want all < 400", m)
		}
		if n := e.fb.count(); n != 0 {
			t.Errorf("backend called %d times for a forbidden call", n)
		}
	})
}

func TestMCPRevocationMidSession(t *testing.T) {
	e := newEnv(t, nil)
	cs, rec := e.connect(e.rw)
	if _, err := callTool(t, cs, "memory_stats", nil); err != nil {
		t.Fatal(err)
	}
	before := e.fb.count()

	if err := keys.Revoke(e.path, e.rwID); err != nil {
		t.Fatal(err)
	}
	if err := e.ks.Reload(); err != nil {
		t.Fatal(err)
	}
	res, err := callTool(t, cs, "memory_stats", nil)
	if err == nil && !res.IsError {
		t.Fatal("call on a revoked key succeeded")
	}
	if rec.lastStatus() != http.StatusUnauthorized {
		t.Errorf("last HTTP status %d, want 401", rec.lastStatus())
	}
	if n := e.fb.count(); n != before {
		t.Errorf("backend called after revocation: %d -> %d", before, n)
	}
}

func TestMCPIsStateless(t *testing.T) {
	e := newEnv(t, nil)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+"/mcp", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.rw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("status %d, session id %q; want 200 and no session", resp.StatusCode, resp.Header.Get("Mcp-Session-Id"))
	}
}

func TestForeignOriginRejected(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/v1/stats", "/mcp"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+e.rw)
		req.Header.Set("Origin", "https://evil.example")
		if status, _ := do(t, req); status != http.StatusForbidden {
			t.Errorf("%s with foreign Origin: status %d, want 403", path, status)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	e := newEnv(t, nil)
	get := func(path string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return do(t, req)
	}
	if s, b := get("/healthz"); s != http.StatusOK || b != "ok" {
		t.Errorf("/healthz = %d %q", s, b)
	}
	if s, b := get("/readyz"); s != http.StatusOK || b != "ready" {
		t.Errorf("/readyz before shutdown = %d %q", s, b)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.g.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s, b := get("/readyz"); s != http.StatusServiceUnavailable || b != "not ready" {
		t.Errorf("/readyz after shutdown = %d %q", s, b)
	}
}

func TestLogsOmitSecretsAndContent(t *testing.T) {
	e := newEnv(t, nil)
	const restContent = "distinctive-rest-content-7f3a"
	const mcpContent = "distinctive-mcp-content-9d2c"

	if status, body := e.post("/v1/remember", e.rw, `{"content":"`+restContent+`"}`); status != http.StatusOK {
		t.Fatalf("rest remember: %d %s", status, body)
	}
	cs, _ := e.connect(e.rw)
	if _, err := callTool(t, cs, "memory_remember", map[string]any{"content": mcpContent}); err != nil {
		t.Fatal(err)
	}

	logs := e.logs.String()
	for _, secret := range []string{restContent, mcpContent, e.rw, "Bearer", "msm_"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log output contains %q", secret)
		}
	}
	if !strings.Contains(logs, "op=remember") || !strings.Contains(logs, "outcome=ok") {
		t.Errorf("expected per-call log lines, got:\n%s", logs)
	}
}

func TestOpErrorMessageRedacted(t *testing.T) {
	if _, _, msg := outcome(&provider.OpError{Msg: "no colon, just 'secret content'"}); msg != "provider error: unknown" {
		t.Fatalf("colon-less message leaked: %q", msg)
	}
	_, _, msg := outcome(&provider.OpError{Msg: "ValueError: memory 'secret content' is bad"})
	if msg != "provider error: ValueError" {
		t.Fatalf("msg = %q", msg)
	}
}

func TestMCPRequiresBearer(t *testing.T) {
	e := newEnv(t, nil)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	for name, auth := range map[string]string{"missing": "", "invalid": "Bearer msm_bogus", "valid": "Bearer " + e.rw} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+"/mcp", strings.NewReader(init))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		status, _ := do(t, req)
		if want := http.StatusUnauthorized; name != "valid" && status != want {
			t.Errorf("%s token: status %d, want %d", name, status, want)
		}
		if name == "valid" && status != http.StatusOK {
			t.Errorf("valid token: status %d, want 200", status)
		}
	}
}

func TestRebindingHostRejected(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/v1/stats", "/healthz", "/mcp"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.url+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "evil.example"
		req.Header.Set("Authorization", "Bearer "+e.rw)
		if status, _ := do(t, req); status != http.StatusForbidden {
			t.Errorf("%s with Host evil.example: status %d, want 403", path, status)
		}
	}
	if e.fb.count() != 0 {
		t.Errorf("backend called %d times", e.fb.count())
	}
}
