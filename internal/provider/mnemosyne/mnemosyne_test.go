package mnemosyne

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/pyproc"
)

func TestProcessArgs(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want []string
	}{
		{Config{DataDir: "/d"}, []string{TestedVersion, "default", "open"}},
		{Config{DataDir: "/d", Version: "9.9.9", Session: "team", Create: true}, []string{"9.9.9", "team", "create"}},
	} {
		got := process(tc.cfg, "src")
		if !slices.Equal(got.Args, tc.want) || !slices.Equal(got.Env, []string{"MNEMOSYNE_DATA_DIR=/d"}) || got.Script != "src" {
			t.Errorf("process(%+v) = %+v; want args %v", tc.cfg, got, tc.want)
		}
	}
}

// realBackend starts the real script against a copy of MSM_TEST_DB (a
// mnemosyne.db file, never the live store) using MSM_TEST_PYTHON.
func realBackend(t *testing.T, db string) *pyproc.Backend {
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
