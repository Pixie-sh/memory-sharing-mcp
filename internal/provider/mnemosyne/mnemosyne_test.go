package mnemosyne

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

// fake replies to each request according to its op, standing in for Mnemosyne.
const fake = `
import json, sys
print(json.dumps({"id": 0, "ok": True, "result": {}}), flush=True)
for line in sys.stdin:
    req = json.loads(line)
    op = req["op"]
    if op == "echo":
        print(json.dumps({"id": req["id"], "ok": True, "result": req["args"]}), flush=True)
    elif op == "oops":
        print(json.dumps({"id": req["id"], "ok": False, "error": "ValueError: bad"}), flush=True)
    elif op == "hang":
        sys.stdin.readline()
    elif op == "wrongid":
        print(json.dumps({"id": 999, "ok": True, "result": {}}), flush=True)
    elif op == "garbage":
        print("not json", flush=True)
    elif op == "argv":
        print(json.dumps({"id": req["id"], "ok": True, "result": sys.argv[1:]}), flush=True)
    elif op == "exit":
        sys.exit(3)
`

func python(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	return p
}

func startFake(t *testing.T, src string) *Backend {
	t.Helper()
	b, err := start(context.Background(), Config{Python: python(t), Stderr: io.Discard}, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b
}

func TestCallAndOpError(t *testing.T) {
	b := startFake(t, fake)
	got, err := b.Call(context.Background(), "echo", map[string]string{"q": "x"})
	if err != nil || string(got) != `{"q": "x"}` {
		t.Fatalf("echo = %s, %v", got, err)
	}
	_, err = b.Call(context.Background(), "oops", nil)
	var opErr *provider.OpError
	if !errors.As(err, &opErr) || opErr.Msg != "ValueError: bad" {
		t.Fatalf("oops err = %v", err)
	}
	if _, err := b.Call(context.Background(), "echo", 1); err != nil {
		t.Fatalf("backend unusable after OpError: %v", err)
	}
}

func TestVersionAndSessionArgs(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, `["` + TestedVersion + `", "default", "open"]`},
		{Config{Version: "9.9.9", Session: "team", Create: true}, `["9.9.9", "team", "create"]`},
	} {
		tc.cfg.Python, tc.cfg.Stderr = python(t), io.Discard
		b, err := start(context.Background(), tc.cfg, fake)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Call(context.Background(), "argv", nil)
		b.Close(context.Background())
		if err != nil || string(got) != tc.want {
			t.Errorf("argv = %s, %v; want %s", got, err, tc.want)
		}
	}
}

func TestFailuresKillBackend(t *testing.T) {
	for _, op := range []string{"hang", "wrongid", "garbage", "exit"} {
		t.Run(op, func(t *testing.T) {
			b := startFake(t, fake)
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			if _, err := b.Call(ctx, op, nil); !errors.Is(err, provider.ErrIndeterminate) {
				t.Fatalf("err = %v, want ErrIndeterminate", err)
			}
			select {
			case <-b.Exited():
			default:
				t.Fatal("process not reaped after failure")
			}
			if _, err := b.Call(context.Background(), "echo", 1); !errors.Is(err, provider.ErrUnavailable) {
				t.Fatalf("after failure err = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestStartupFailure(t *testing.T) {
	src := `import json; print(json.dumps({"id": 0, "ok": False, "error": "database not found: x"}))`
	_, err := start(context.Background(), Config{Python: python(t), Stderr: io.Discard}, src)
	if err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartupTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := start(ctx, Config{Python: python(t), Stderr: io.Discard}, `import time; time.sleep(30)`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// A forked child holding the pipes must not stop kill from reaping the backend.
func TestStartupTimeoutWithForkedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := start(ctx, Config{Python: python(t), Stderr: io.Discard}, `import os, time; os.fork(); time.sleep(30)`)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 10*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(t0))
	}
}

// realBackend starts the real script against a copy of MSM_TEST_DB (a
// mnemosyne.db file, never the live store) using MSM_TEST_PYTHON.
func realBackend(t *testing.T, db string) *Backend {
	t.Helper()
	py := os.Getenv("MSM_TEST_PYTHON")
	if py == "" || db == "" {
		t.Skip("set MSM_TEST_PYTHON and MSM_TEST_DB to run against real Mnemosyne")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := Start(ctx, Config{Python: py, DataDir: db, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b
}

func copyDB(t *testing.T) string {
	t.Helper()
	src := os.Getenv("MSM_TEST_DB")
	if src == "" {
		return ""
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mnemosyne.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRealMissingDatabase(t *testing.T) {
	py := os.Getenv("MSM_TEST_PYTHON")
	if py == "" {
		t.Skip("set MSM_TEST_PYTHON")
	}
	dir := t.TempDir()
	_, err := Start(context.Background(), Config{Python: py, DataDir: dir, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "mnemosyne.db")); !os.IsNotExist(statErr) {
		t.Fatal("backend created a database for a missing store")
	}
}

func TestRealCreate(t *testing.T) {
	py := os.Getenv("MSM_TEST_PYTHON")
	if py == "" {
		t.Skip("set MSM_TEST_PYTHON")
	}
	dir := t.TempDir()
	b, err := Start(context.Background(), Config{Python: py, DataDir: dir, Create: true, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	if _, err := b.Call(context.Background(), "stats", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mnemosyne.db")); err != nil {
		t.Fatalf("no database created: %v", err)
	}
}

// An operation failure reaches the caller as an OpError with only the type
// visible to clients; the traceback goes to stderr, the only place it is kept.
func TestRealOpErrorTraceback(t *testing.T) {
	py, dir := os.Getenv("MSM_TEST_PYTHON"), copyDB(t)
	if py == "" || dir == "" {
		t.Skip("set MSM_TEST_PYTHON and MSM_TEST_DB to run against real Mnemosyne")
	}
	var stderr bytes.Buffer
	b, err := Start(context.Background(), Config{Python: py, DataDir: dir, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Call(context.Background(), "no_such_op", nil)
	var opErr *provider.OpError
	if !errors.As(err, &opErr) || !strings.HasPrefix(opErr.Msg, "KeyError:") {
		t.Fatalf("err = %v, want KeyError OpError", err)
	}
	b.Close(context.Background()) // waits for exit, so stderr is complete
	if !strings.Contains(stderr.String(), "Traceback (most recent call last)") {
		t.Fatalf("no traceback on stderr:\n%s", stderr.String())
	}
}

func TestRealRoundTrip(t *testing.T) {
	b := realBackend(t, copyDB(t))
	ctx := context.Background()
	call := func(op string, args any, into any) {
		t.Helper()
		raw, err := b.Call(ctx, op, args)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v in %s", op, err, raw)
		}
	}
	type result struct {
		ID       string `json:"id"`
		Content  string `json:"memory"`
		AuthorID string `json:"agent_id"`
	}
	var recall struct{ Results []result }

	// Existing memories (session "default") must be visible.
	call("recall", map[string]any{"query": "user", "top_k": 5}, &recall)
	if len(recall.Results) == 0 {
		t.Fatal("recall returned no existing memories")
	}

	// Longer than the 500 characters Mnemosyne's recall keeps.
	content := "memory-sharing-mcp smoke test zqxjv marker " + strings.Repeat("long tail ", 80) + "end"
	var stored struct{ ID string }
	call("remember", map[string]any{"content": content, "source": "smoke", "importance": 0.5, "author_id": "key_test"}, &stored)
	call("recall", map[string]any{"query": "zqxjv marker", "top_k": 5}, &recall)
	var found *result
	for i := range recall.Results {
		if recall.Results[i].ID == stored.ID {
			found = &recall.Results[i]
		}
	}
	if found == nil || found.Content != content || found.AuthorID != "key_test" {
		t.Fatalf("stored memory not recalled intact: %+v", recall.Results)
	}

	var stats map[string]any
	call("stats", nil, &stats)
	var forgot struct{ Deleted bool }
	call("forget", map[string]any{"id": stored.ID}, &forgot)
	if !forgot.Deleted {
		t.Fatal("forget reported nothing deleted")
	}
}
