package pyproc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

// fake replies to each request according to its op, standing in for a provider script.
const fake = `
import json, os, sys
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
        print(json.dumps({"id": req["id"], "ok": True, "result": sys.argv[1:] + sorted(os.environ)}), flush=True)
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

func startFake(t *testing.T, cfg Config) *Backend {
	t.Helper()
	cfg.Python, cfg.Script, cfg.Stderr = python(t), fake, io.Discard
	b, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b
}

func TestCallAndOpError(t *testing.T) {
	b := startFake(t, Config{})
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

// The script sees its arguments and only HOME plus the configured environment.
func TestArgsAndEnv(t *testing.T) {
	t.Setenv("MSM_TEST_LEAK", "1")
	b := startFake(t, Config{Args: []string{"a", "b"}, Env: []string{"STORE_DIR=/x"}})
	got, err := b.Call(context.Background(), "argv", nil)
	// Python may add its own variables (such as LC_CTYPE); the shell's must not leak.
	if s := string(got); err != nil || !strings.HasPrefix(s, `["a", "b", `) || !strings.Contains(s, `"STORE_DIR"`) ||
		!strings.Contains(s, `"HOME"`) || strings.Contains(s, "MSM_TEST_LEAK") {
		t.Fatalf("argv = %s, %v", got, err)
	}
}

func TestFailuresKillBackend(t *testing.T) {
	for _, op := range []string{"hang", "wrongid", "garbage", "exit"} {
		t.Run(op, func(t *testing.T) {
			b := startFake(t, Config{})
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
	_, err := Start(context.Background(), Config{Python: python(t), Script: src, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartupTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Start(ctx, Config{Python: python(t), Script: `import time; time.sleep(30)`, Stderr: io.Discard})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// A forked child holding the pipes must not stop kill from reaping the backend.
func TestStartupTimeoutWithForkedChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := Start(ctx, Config{Python: python(t), Script: `import os, time; os.fork(); time.sleep(30)`, Stderr: io.Discard})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 10*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(t0))
	}
}
