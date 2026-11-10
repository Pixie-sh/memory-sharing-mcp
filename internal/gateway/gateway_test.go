package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

// fake stands in for one backend process; call, when set, decides each reply.
type fake struct {
	call    func(ctx context.Context, op string, args any) (json.RawMessage, error)
	active  atomic.Int32
	overlap atomic.Bool
	closed  atomic.Int32
	mu      sync.Mutex
	ops     []string
	args    []any
}

func (f *fake) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	if f.active.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.active.Add(-1)
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.args = append(f.args, args)
	f.mu.Unlock()
	if f.call == nil {
		return json.RawMessage(`{}`), nil
	}
	return f.call(ctx, op, args)
}

func (f *fake) Close(context.Context) { f.closed.Add(1) }

func (f *fake) opsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *fake) argsSeen() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]any(nil), f.args...)
}

// blocking returns a fake whose every call reports start and then waits for release.
func blocking(started chan<- struct{}, release <-chan struct{}) *fake {
	return &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
		started <- struct{}{}
		<-release
		return json.RawMessage(`{}`), nil
	}}
}

// seq returns a Starter that hands out bs in order; a nil entry fails to start.
func seq(bs ...Backend) (Starter, *atomic.Int32) {
	var n atomic.Int32
	var mu sync.Mutex
	return func(context.Context) (Backend, error) {
		mu.Lock()
		defer mu.Unlock()
		n.Add(1)
		if len(bs) == 0 {
			return nil, errors.New("no backend left")
		}
		b := bs[0]
		bs = bs[1:]
		if b == nil {
			return nil, errors.New("start failed")
		}
		return b, nil
	}, &n
}

type keyset struct {
	mu sync.Mutex
	m  map[string]keys.Key
}

func newKeys(ks ...keys.Key) *keyset {
	s := &keyset{m: map[string]keys.Key{}}
	for _, k := range ks {
		s.m[k.ID] = k
	}
	return s
}

func (s *keyset) Active(id string) (keys.Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.m[id]
	return k, ok
}

func (s *keyset) setRole(id string, r keys.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.m[id]
	k.Role = r
	s.m[id] = k
}

func (s *keyset) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

var (
	reader  = keys.Key{ID: "key_ro", Name: "reader", Role: keys.ReadOnly}
	writer  = keys.Key{ID: "key_rw", Name: "writer", Role: keys.ReadWrite}
	admin   = keys.Key{ID: "key_mt", Name: "admin", Role: keys.Maintenance}
	unnamed = keys.Key{ID: "key_an", Role: keys.ReadWrite}
)

func allKeys() *keyset { return newKeys(reader, writer, admin, unnamed) }

// open starts a gateway over bs and shuts it down when the test ends. It also
// returns the number of backend starts so far.
func open(t *testing.T, cfg Config, ks *keyset, bs ...Backend) (*Gateway, *atomic.Int32) {
	t.Helper()
	start, n := seq(bs...)
	g, err := New(context.Background(), cfg, start, ks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.Shutdown(ctx)
	})
	return g, n
}

// asyncDo runs Do in a goroutine and reports its result on the returned channel.
func asyncDo(ctx context.Context, g *Gateway, key keys.Key, op Op) chan result {
	ch := make(chan result, 1)
	go func() {
		raw, err := g.Do(ctx, key, op)
		ch <- result{raw, err}
	}()
	return ch
}

// wait receives from ch or fails the test after a bounded time.
func wait[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

// waitFor polls cond until it holds or the test fails after a bounded time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPermissions(t *testing.T) {
	off := Config{}
	on := Config{AllowForget: true, AllowSleep: true}
	tests := []struct {
		key  keys.Key
		op   Op
		cfg  Config
		want bool
	}{
		{reader, Recall{Query: "q"}, off, true},
		{reader, Stats{}, off, true},
		{reader, Remember{Content: "c"}, on, false},
		{reader, Forget{ID: "memory:m1"}, on, false},
		{reader, Sleep{}, on, false},
		{writer, Remember{Content: "c"}, off, true},
		{writer, Forget{ID: "memory:m1"}, on, false},
		{writer, Sleep{}, on, false},
		{admin, Remember{Content: "c"}, off, true},
		{admin, Forget{ID: "memory:m1"}, off, false},
		{admin, Sleep{}, off, false},
		{admin, Forget{ID: "memory:m1"}, on, true},
		{admin, Sleep{}, on, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s_%v", tt.key.Role, tt.op.name(), tt.cfg.AllowForget), func(t *testing.T) {
			f := &fake{}
			g, _ := open(t, tt.cfg, allKeys(), f)
			_, err := g.Do(context.Background(), tt.key, tt.op)
			if tt.want && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !tt.want && (!errors.Is(err, ErrForbidden) || len(f.opsSeen()) != 0) {
				t.Fatalf("err = %v, backend ops = %v; want ErrForbidden and no backend call", err, f.opsSeen())
			}
		})
	}
}

func TestValidation(t *testing.T) {
	nan, inf, neg, over := math.NaN(), math.Inf(1), -0.1, 1.1
	zero, one := 0.0, 1.0
	f := &fake{}
	g, _ := open(t, Config{AllowForget: true}, allKeys(), f)
	ctx := context.Background()

	bad := []Op{
		nil,
		Recall{Query: " \t"},
		Recall{Query: strings.Repeat("q", 4097)},
		Recall{Query: "q", TopK: -1},
		Recall{Query: "q", TopK: 51},
		Remember{Content: ""},
		Remember{Content: "  "},
		Remember{Content: strings.Repeat("x", 32769)},
		Remember{Content: "\xff"},
		Remember{Content: "x", Importance: &nan},
		Remember{Content: "x", Importance: &inf},
		Remember{Content: "x", Importance: &neg},
		Remember{Content: "x", Importance: &over},
		Remember{Content: "x", Source: strings.Repeat("s", 65)},
		Forget{},
		Forget{ID: "memory:"},
		Forget{ID: "m1"},       // no provider prefix
		Forget{ID: "other:m1"}, // another provider's ID
		Forget{ID: "memory:a b"},
		Forget{ID: "memory:a/b"},
		Forget{ID: "memory:" + strings.Repeat("a", 129)},
	}
	for _, op := range bad {
		if _, err := g.Do(ctx, admin, op); !errors.Is(err, ErrValidation) {
			t.Errorf("%#v: err = %v, want ErrValidation", op, err)
		}
	}
	if n := len(f.opsSeen()); n != 0 {
		t.Fatalf("backend saw %d calls for invalid input", n)
	}

	good := []Op{
		Recall{Query: "q", TopK: 50},
		Remember{Content: "x", Importance: &zero},
		Remember{Content: "x", Importance: &one},
		Forget{ID: "memory:a-B_9"},
	}
	for _, op := range good {
		if _, err := g.Do(ctx, admin, op); err != nil {
			t.Errorf("%#v: err = %v, want nil", op, err)
		}
	}
}

func TestBackendArgs(t *testing.T) {
	f := &fake{}
	g, _ := open(t, Config{}, allKeys(), f)
	ctx := context.Background()
	if _, err := g.Do(ctx, writer, Remember{Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Do(ctx, unnamed, Remember{Content: "bye"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Do(ctx, reader, Recall{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"content": "hello", "source": "writer", "importance": 0.5, "author_id": "key_rw"},
		map[string]any{"content": "bye", "source": "gateway", "importance": 0.5, "author_id": "key_an"},
		map[string]any{"query": "q", "top_k": 5},
	}
	if got := f.argsSeen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestCallsNeverOverlap(t *testing.T) {
	f := &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
		runtime.Gosched()
		return json.RawMessage(`{}`), nil
	}}
	g, _ := open(t, Config{QueueLen: 64}, allKeys(), f)
	results := make(chan result, 50)
	for i := 0; i < 50; i++ {
		go func() {
			raw, err := g.Do(context.Background(), reader, Stats{})
			results <- result{raw, err}
		}()
	}
	for i := 0; i < 50; i++ {
		if r := wait(t, results); r.err != nil {
			t.Fatalf("call %d: %v", i, r.err)
		}
	}
	if f.overlap.Load() {
		t.Fatal("backend calls overlapped")
	}
}

func TestQueueFullIsBusy(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	f := blocking(started, release)
	g, _ := open(t, Config{QueueLen: 1}, allKeys(), f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started) // the worker holds the first job
	second := asyncDo(context.Background(), g, reader, Stats{})
	waitFor(t, "second job queued", func() bool { return len(g.jobs) == 1 })

	if _, err := g.Do(context.Background(), reader, Stats{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	close(release)
	for _, ch := range []chan result{first, second} {
		if r := wait(t, ch); r.err != nil {
			t.Fatal(r.err)
		}
	}
}

func TestRevokedWhileQueued(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	f := blocking(started, release)
	ks := allKeys()
	g, _ := open(t, Config{}, ks, f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started)
	second := asyncDo(context.Background(), g, writer, Remember{Content: "queued"})
	waitFor(t, "remember queued", func() bool { return len(g.jobs) == 1 })

	ks.revoke(writer.ID)
	close(release)
	if r := wait(t, second); !errors.Is(r.err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", r.err)
	}
	if r := wait(t, first); r.err != nil {
		t.Fatal(r.err)
	}
	if ops := f.opsSeen(); !reflect.DeepEqual(ops, []string{"stats"}) {
		t.Fatalf("backend ops = %v, want only stats", ops)
	}
}

func TestCancelledBeforeDispatchIsSkipped(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	f := blocking(started, release)
	g, _ := open(t, Config{}, allKeys(), f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	second := asyncDo(ctx, g, writer, Remember{Content: "never"})
	waitFor(t, "remember queued", func() bool { return len(g.jobs) == 1 })
	cancel()
	if r := wait(t, second); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", r.err)
	}
	close(release)
	wait(t, first)

	// Jobs run in order, so once a later job completes the cancelled one was skipped.
	if _, err := g.Do(context.Background(), reader, Stats{}); err != nil {
		t.Fatal(err)
	}
	if ops := f.opsSeen(); !reflect.DeepEqual(ops, []string{"stats", "stats"}) {
		t.Fatalf("backend ops = %v, want two stats and no remember", ops)
	}
}

func TestTimeoutThenRestartWithoutReplay(t *testing.T) {
	hang := &fake{call: func(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("%w: %w", provider.ErrIndeterminate, ctx.Err())
	}}
	good := &fake{}
	g, starts := open(t, Config{Timeout: 10 * time.Millisecond, restartBase: time.Millisecond}, allKeys(), hang, good)

	_, err := g.Do(context.Background(), writer, Remember{Content: "x"})
	if !errors.Is(err, provider.ErrIndeterminate) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want indeterminate timeout", err)
	}
	if g.Ready() {
		t.Fatal("ready right after a dead backend")
	}
	waitFor(t, "restart", g.Ready)
	if _, err := g.Do(context.Background(), reader, Stats{}); err != nil {
		t.Fatal(err)
	}
	if ops := hang.opsSeen(); !reflect.DeepEqual(ops, []string{"remember"}) {
		t.Fatalf("failed backend saw %v, want one remember and no replay", ops)
	}
	if ops := good.opsSeen(); !reflect.DeepEqual(ops, []string{"stats"}) {
		t.Fatalf("replacement saw %v, want only stats", ops)
	}
	if n := starts.Load(); n != 2 {
		t.Fatalf("starts = %d, want 2", n)
	}
}

func TestQueuedJobsFailFastDuringRestart(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	dying := &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
		started <- struct{}{}
		<-release
		return nil, fmt.Errorf("%w: lost", provider.ErrIndeterminate)
	}}
	good := &fake{}
	unblock := make(chan struct{})
	var starts atomic.Int32
	start := func(context.Context) (Backend, error) {
		if starts.Add(1) == 1 {
			return dying, nil
		}
		<-unblock // a slow restart
		return good, nil
	}
	g, err := New(context.Background(), Config{restartBase: time.Millisecond}, start, allKeys())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g.Shutdown(ctx)
	}()

	first := asyncDo(context.Background(), g, writer, Remember{Content: "x"})
	<-started
	queued := asyncDo(context.Background(), g, reader, Stats{})
	waitFor(t, "job queued", func() bool { return len(g.jobs) == 1 })
	close(release)
	if r := wait(t, first); !errors.Is(r.err, provider.ErrIndeterminate) {
		t.Fatalf("first err = %v, want indeterminate", r.err)
	}
	if r := wait(t, queued); !errors.Is(r.err, ErrUnavailable) {
		t.Fatalf("queued err = %v, want ErrUnavailable before restart finishes", r.err)
	}
	close(unblock)
	waitFor(t, "restart", g.Ready)
	if _, err := g.Do(context.Background(), reader, Stats{}); err != nil {
		t.Fatal(err)
	}
	if ops := good.opsSeen(); !reflect.DeepEqual(ops, []string{"stats"}) {
		t.Fatalf("replacement saw %v, want only the new stats", ops)
	}
}

func TestRestartsExhaustedIsPermanent(t *testing.T) {
	bad := &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
		return nil, fmt.Errorf("%w: lost", provider.ErrIndeterminate)
	}}
	g, starts := open(t, Config{MaxRestarts: 2, restartBase: time.Millisecond}, allKeys(), bad, nil, nil)

	if _, err := g.Do(context.Background(), writer, Remember{Content: "x"}); !errors.Is(err, provider.ErrIndeterminate) {
		t.Fatalf("err = %v, want indeterminate", err)
	}
	waitFor(t, "restart attempts", func() bool { return starts.Load() == 3 })
	for i := 0; i < 2; i++ {
		if _, err := g.Do(context.Background(), reader, Stats{}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	}
	if g.Ready() {
		t.Fatal("ready after giving up")
	}
	select {
	case <-g.Dead():
	default:
		t.Fatal("Dead not closed after giving up")
	}
	if n := starts.Load(); n != 3 {
		t.Fatalf("starts = %d, want 3", n)
	}
	if n := len(bad.opsSeen()); n != 1 {
		t.Fatalf("failed backend saw %d calls, want 1", n)
	}
}

func TestOpErrorKeepsBackend(t *testing.T) {
	f := &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
		return nil, &provider.OpError{Msg: "ValueError: bad"}
	}}
	g, starts := open(t, Config{}, allKeys(), f)
	for i := 0; i < 2; i++ {
		_, err := g.Do(context.Background(), writer, Remember{Content: "x"})
		var opErr *provider.OpError
		if !errors.As(err, &opErr) || opErr.Msg != "ValueError: bad" {
			t.Fatalf("err = %v, want the OpError", err)
		}
	}
	if !g.Ready() || starts.Load() != 1 {
		t.Fatalf("ready = %v, starts = %d; want ready with no restart", g.Ready(), starts.Load())
	}
}

func TestShutdownWaitsForInFlightAndClosesBackend(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	f := blocking(started, release)
	g, _ := open(t, Config{}, allKeys(), f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started)
	done := make(chan error, 1)
	go func() { done <- g.Shutdown(context.Background()) }()
	waitFor(t, "shutdown to start", func() bool {
		g.mu.RLock()
		defer g.mu.RUnlock()
		return g.closed
	})
	if _, err := g.Do(context.Background(), reader, Stats{}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Do after Shutdown: err = %v, want ErrShuttingDown", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned %v before the in-flight call finished", err)
	default:
	}

	close(release)
	if r := wait(t, first); r.err != nil {
		t.Fatalf("in-flight call: %v", r.err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if n := f.closed.Load(); n != 1 {
		t.Fatalf("backend closed %d times, want 1", n)
	}
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestShutdownDeadlineAbortsInFlightAndQueue(t *testing.T) {
	started := make(chan struct{}, 8)
	f := &fake{call: func(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, fmt.Errorf("%w: %w", provider.ErrIndeterminate, ctx.Err())
	}}
	g, _ := open(t, Config{}, allKeys(), f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started)
	queued := asyncDo(context.Background(), g, reader, Stats{})
	waitFor(t, "stats queued", func() bool { return len(g.jobs) == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown: err = %v, want context.Canceled", err)
	}
	if r := wait(t, first); !errors.Is(r.err, provider.ErrIndeterminate) {
		t.Fatalf("in-flight: err = %v, want indeterminate", r.err)
	}
	if r := wait(t, queued); !errors.Is(r.err, ErrShuttingDown) {
		t.Fatalf("queued: err = %v, want ErrShuttingDown", r.err)
	}
	if g.Ready() {
		t.Fatal("ready after shutdown")
	}
	// The aborted call killed the backend and the restart was cut short by
	// shutdown: that is not exhaustion.
	select {
	case <-g.Dead():
		t.Fatal("Dead closed by shutdown, not exhaustion")
	default:
	}
}

func TestNewReturnsStartError(t *testing.T) {
	start, _ := seq(nil)
	if _, err := New(context.Background(), Config{}, start, allKeys()); err == nil {
		t.Fatal("New with a failing backend: no error")
	}
}

func TestCallerCancelDoesNotStopDispatchedCall(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	callErr := make(chan error, 1)
	f := &fake{call: func(ctx context.Context, op string, _ any) (json.RawMessage, error) {
		if op == "remember" {
			started <- struct{}{}
			<-release
			callErr <- ctx.Err()
		}
		return json.RawMessage(`{}`), nil
	}}
	g, starts := open(t, Config{}, allKeys(), f)
	ctx, cancel := context.WithCancel(context.Background())
	res := asyncDo(ctx, g, writer, Remember{Content: "x"})
	wait(t, started)
	cancel()
	if r := wait(t, res); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("caller err = %v, want context.Canceled", r.err)
	}
	close(release)
	if _, err := g.Do(context.Background(), reader, Stats{}); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, callErr); err != nil || starts.Load() != 1 || !g.Ready() {
		t.Fatalf("call ctx err = %v, starts = %d, ready = %v; want uncancelled call and no restart", err, starts.Load(), g.Ready())
	}
}

func TestRoleChangedWhileQueued(t *testing.T) {
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	f := blocking(started, release)
	ks := allKeys()
	g, _ := open(t, Config{}, ks, f)

	first := asyncDo(context.Background(), g, reader, Stats{})
	wait(t, started)
	second := asyncDo(context.Background(), g, writer, Remember{Content: "queued"})
	waitFor(t, "remember queued", func() bool { return len(g.jobs) == 1 })

	ks.setRole(writer.ID, keys.ReadOnly)
	close(release)
	if r := wait(t, second); !errors.Is(r.err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", r.err)
	}
	wait(t, first)
	if ops := f.opsSeen(); !reflect.DeepEqual(ops, []string{"stats"}) {
		t.Fatalf("backend ops = %v, want only stats", ops)
	}
}

func TestBackendDyingOnEveryCallIsGivenUp(t *testing.T) {
	dies := func() Backend {
		return &fake{call: func(context.Context, string, any) (json.RawMessage, error) {
			return nil, provider.ErrIndeterminate
		}}
	}
	cfg := Config{MaxRestarts: 2, restartBase: time.Millisecond}
	g, starts := open(t, cfg, allKeys(), dies(), dies(), dies(), dies(), dies())
	for i := range cfg.MaxRestarts + 1 {
		if _, err := g.Do(context.Background(), reader, Stats{}); !errors.Is(err, provider.ErrIndeterminate) {
			t.Fatalf("call %d: err = %v, want ErrIndeterminate", i, err)
		}
		if i < cfg.MaxRestarts {
			waitFor(t, "restarted", g.Ready)
		}
	}
	time.Sleep(20 * time.Millisecond) // a further restart would happen within a few ms
	if _, err := g.Do(context.Background(), reader, Stats{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable after giving up", err)
	}
	select {
	case <-g.Dead():
	default:
		t.Fatal("Dead not closed after giving up")
	}
	if n := starts.Load(); n != int32(cfg.MaxRestarts+1) {
		t.Fatalf("starts = %d, want %d", n, cfg.MaxRestarts+1)
	}
}

func TestNotReadyAfterShutdown(t *testing.T) {
	g, _ := open(t, Config{}, allKeys(), &fake{})
	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.Ready() {
		t.Fatal("Ready() true after Shutdown")
	}
}

func TestContractShapesResults(t *testing.T) {
	imp := 0.9
	for _, tc := range []struct {
		op        Op
		raw, want string
	}{
		{Recall{Query: "q"}, `{"results":[{"id":"m1","memory":"c","metadata":{"importance":0.9},"tier":"working"}]}`,
			`{"results":[{"id":"mnemosyne:m1","memory":"c","provider":"mnemosyne","metadata":{"importance":0.9}}]}`},
		{Recall{Query: "q"}, `{"results":null}`, `{"results":[]}`},
		{Remember{Content: "x", Importance: &imp}, `{"id":"m2"}`,
			`{"results":[{"event":"ADD","id":"mnemosyne:m2","memory":"x","provider":"mnemosyne"}]}`},
		{Stats{}, `{"total":1}`, `{"providers":{"mnemosyne":{"total":1}}}`},
		{Forget{ID: "mnemosyne:m1"}, `{"deleted":true}`, `{"deleted":true}`},
	} {
		got, err := contract("mnemosyne", tc.op, json.RawMessage(tc.raw))
		if err != nil || string(got) != tc.want {
			t.Errorf("%T %s: got %s, %v; want %s", tc.op, tc.raw, got, err, tc.want)
		}
	}
	var opErr *provider.OpError
	if _, err := contract("mnemosyne", Recall{}, json.RawMessage(`{"results":"nope"}`)); !errors.As(err, &opErr) {
		t.Errorf("malformed recall: err = %v, want *OpError", err)
	}
}

func TestForgetStripsPrefix(t *testing.T) {
	args, err := prepare(keys.Key{}, Forget{ID: "mnemosyne:m1"}, "mnemosyne")
	if err != nil || args.(map[string]any)["id"] != "m1" {
		t.Fatalf("args = %v, err = %v", args, err)
	}
}
