package mem0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/pyproc"
)

func TestProcess(t *testing.T) {
	got := process(Config{DataDir: "/d", Settings: json.RawMessage(`{"a":1}`)}, "src")
	wantEnv := []string{"MEM0_DIR=/d", "MEM0_TELEMETRY=False", `MSM_MEM0_CONFIG={"a":1}`}
	if !slices.Equal(got.Args, []string{TestedVersion, "default", "open"}) || !slices.Equal(got.Env, wantEnv) || got.Script != "src" {
		t.Errorf("defaults: %+v", got)
	}
	// Extra variables come first, so they cannot replace the gateway's own.
	got = process(Config{Version: "9.9.9", UserID: "team", Create: true, Env: []string{"MEM0_DIR=/evil", "OLLAMA_HOST=h"}}, "src")
	if !slices.Equal(got.Args, []string{"9.9.9", "team", "create"}) || !slices.Equal(got.Env[:3], []string{"MEM0_DIR=/evil", "OLLAMA_HOST=h", "MEM0_DIR="}) {
		t.Errorf("explicit: %+v", got)
	}
}

const dims = 64

// embeddings is an OpenAI-compatible /embeddings endpoint that hashes words
// into a vector, so texts sharing words are similar and no model is needed.
func embeddings(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		type datum struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		var data []datum
		for i, text := range req.Input {
			v := make([]float64, dims)
			for word := range strings.FieldsSeq(strings.ToLower(text)) {
				h := fnv.New32a()
				h.Write([]byte(word))
				v[h.Sum32()%dims]++
			}
			data = append(data, datum{"embedding", i, v})
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": "fake", "data": data,
			"usage": map[string]int{"prompt_tokens": 0, "total_tokens": 0}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// realConfig is a mem0 setup that needs no network beyond the embeddings stub.
// The key is a placeholder: mem0 builds its LLM client at startup but
// verbatim storage never calls it.
func realConfig(t *testing.T, dir string, create bool) Config {
	t.Helper()
	py := os.Getenv("MSM_TEST_MEM0_PYTHON")
	if py == "" {
		t.Skip("set MSM_TEST_MEM0_PYTHON to the interpreter of a mem0ai install to run against real mem0")
	}
	settings := fmt.Sprintf(`{
		"llm": {"provider": "openai", "config": {"api_key": "unused"}},
		"embedder": {"provider": "openai", "config": {"api_key": "unused", "openai_base_url": %q, "embedding_dims": %d}},
		"vector_store": {"provider": "qdrant", "config": {"path": %q, "embedding_model_dims": %d}}
	}`, embeddings(t), dims, filepath.Join(dir, "qdrant"), dims)
	return Config{Python: py, DataDir: dir, Settings: json.RawMessage(settings), Create: create, Stderr: io.Discard}
}

func startReal(t *testing.T, cfg Config) *pyproc.Backend {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := pyproc.Start(ctx, process(cfg, script))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	return b
}

func TestRealMissingStore(t *testing.T) {
	dir := t.TempDir()
	_, err := pyproc.Start(context.Background(), process(realConfig(t, dir, false), script))
	if err == nil || !strings.Contains(err.Error(), "store not found") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("backend wrote %v for a missing store", entries)
	}
}

// Where memories are stored must be configured; mem0's /tmp default is refused.
func TestRealNeedsDestination(t *testing.T) {
	for _, settings := range []string{
		``,
		`null`,
		`[]`,
		`{"vector_store": "qdrant"}`,
		`{"vector_store": {"provider": "faiss", "config": {}}}`,
		`{"vector_store": {"provider": "faiss", "config": {"path": "relative/faiss"}}}`,
		`{"vector_store": {"provider": "qdrant", "config": {"embedding_model_dims": 64}}}`,
		`{"vector_store": {"provider": "qdrant", "config": {"url": "http://localhost:6333"}}}`,
		`{"vector_store": {"provider": "qdrant", "config": {"path": "relative/qdrant"}}}`,
	} {
		dir := t.TempDir()
		cfg := realConfig(t, dir, true)
		cfg.Settings = json.RawMessage(settings)
		_, err := pyproc.Start(context.Background(), process(cfg, script))
		if err == nil || !strings.Contains(err.Error(), "mem0.config") {
			t.Errorf("settings %q: err = %v", settings, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("settings %q: backend wrote %v before refusing", settings, entries)
		}
	}
}

func TestRealRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	call := func(b *pyproc.Backend, op string, args any, into any) {
		t.Helper()
		raw, err := b.Call(ctx, op, args)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v in %s", op, err, raw)
		}
	}

	// store init: create, then one stats call.
	b := startReal(t, realConfig(t, dir, true))
	var stats struct{ Memories *int }
	call(b, "stats", nil, &stats)
	if _, err := os.Stat(StorePath(dir)); err != nil || stats.Memories == nil || *stats.Memories != 0 {
		t.Fatalf("new store: stat err %v, stats %+v", err, stats)
	}
	content := "the deploy runbook lives in the zqxjv wiki\nsecond line"
	var stored struct{ ID string }
	call(b, "remember", map[string]any{"content": content, "source": "smoke", "importance": 0.7, "author_id": "key_test"}, &stored)
	call(b, "remember", map[string]any{"content": "unrelated note about lunch", "source": "smoke", "importance": 0.5, "author_id": "key_other"}, new(struct{}))
	b.Close(ctx)

	// A second process, opened without create, sees what the first one stored.
	b = startReal(t, realConfig(t, dir, false))
	var recall struct{ Results []provider.Memory }
	call(b, "recall", map[string]any{"query": "deploy runbook zqxjv wiki", "top_k": 5}, &recall)
	if len(recall.Results) == 0 {
		t.Fatal("recall returned nothing")
	}
	got := recall.Results[0]
	if got.ID != stored.ID || got.Memory != content || got.AgentID != "key_test" || got.CreatedAt == "" ||
		got.Metadata["source"] != "smoke" || got.Metadata["importance"] != 0.7 {
		t.Fatalf("best match = %+v, want the stored memory intact", got)
	}

	// A query matching nothing still ranks what there is, up to top_k.
	call(b, "recall", map[string]any{"query": "qqqq", "top_k": 5}, &recall)
	if len(recall.Results) != 2 {
		t.Fatalf("recall of an unrelated query = %d results, want both memories", len(recall.Results))
	}

	var forgot struct{ Deleted bool }
	call(b, "forget", map[string]any{"id": "not-a-uuid"}, &forgot)
	if forgot.Deleted {
		t.Fatal("forget of a foreign id reported a deletion")
	}
	call(b, "forget", map[string]any{"id": stored.ID}, &forgot)
	if !forgot.Deleted {
		t.Fatal("forget reported nothing deleted")
	}
	call(b, "forget", map[string]any{"id": stored.ID}, &forgot)
	if forgot.Deleted {
		t.Fatal("forget of a deleted memory reported a deletion")
	}
	call(b, "stats", nil, &stats)
	if stats.Memories == nil || *stats.Memories != 1 {
		t.Fatalf("stats after forget = %+v, want 1 memory", stats)
	}

	// Unsupported, but the backend stays usable.
	var opErr *provider.OpError
	if _, err := b.Call(ctx, "sleep", nil); !errors.As(err, &opErr) || !strings.HasPrefix(opErr.Msg, "NotImplementedError") {
		t.Fatalf("sleep err = %v, want NotImplementedError OpError", err)
	}
	call(b, "stats", nil, &stats)
	b.Close(ctx)

	// history.db is there, but the vector store location is wrong: refuse
	// rather than start on a new, empty collection.
	moved := realConfig(t, dir, false)
	moved.Settings = json.RawMessage(strings.Replace(string(moved.Settings), filepath.Join(dir, "qdrant"), filepath.Join(dir, "qdrant-typo"), 1))
	if _, err := pyproc.Start(ctx, process(moved, script)); err == nil || !strings.Contains(err.Error(), "no collection") {
		t.Fatalf("wrong vector store location: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "qdrant-typo")); err == nil {
		t.Fatal("backend created a store at the wrong location")
	}

	// store init with history.db gone but the collection still there: refuse
	// rather than report the old collection as a new store.
	if err := os.Remove(StorePath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := pyproc.Start(ctx, process(realConfig(t, dir, true), script)); err == nil || !strings.Contains(err.Error(), "already has a collection") {
		t.Fatalf("create over an existing collection: err = %v", err)
	}
}

// A memory of another user_id in the same collection is neither recalled nor forgotten.
func TestRealUserScope(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	cfg := realConfig(t, dir, true)
	cfg.UserID = "alice"
	b := startReal(t, cfg)
	raw, err := b.Call(ctx, "remember", map[string]any{"content": "alice likes zqxjv tea", "source": "s", "importance": 0.5, "author_id": "k"})
	if err != nil {
		t.Fatal(err)
	}
	b.Close(ctx)

	cfg.UserID, cfg.Create = "bob", false
	b = startReal(t, cfg)
	if got, err := b.Call(ctx, "recall", map[string]any{"query": "zqxjv tea", "top_k": 5}); err != nil || string(got) != `{"results": []}` {
		t.Fatalf("recall as bob = %s, %v", got, err)
	}
	var stored struct{ ID string }
	json.Unmarshal(raw, &stored)
	if got, err := b.Call(ctx, "forget", map[string]any{"id": stored.ID}); err != nil || string(got) != `{"deleted": false, "hidden": false}` {
		t.Fatalf("forget as bob = %s, %v", got, err)
	}
}
