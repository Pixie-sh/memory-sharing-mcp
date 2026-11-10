// Package gateway is the single owner of a memory provider. It validates and
// authorizes each operation, runs the accepted ones on the provider one at a time,
// and restarts the provider after a failure. It never replays a call.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

// Backend and Starter are the provider contract, named for the gateway's use.
type (
	Backend = provider.Provider
	Starter = provider.Starter
)

var (
	ErrValidation   = errors.New("invalid request")
	ErrForbidden    = errors.New("forbidden")
	ErrBusy         = errors.New("gateway busy")
	ErrUnavailable  = provider.ErrUnavailable
	ErrShuttingDown = errors.New("gateway shutting down")
)

// Op is one memory operation. Only the types below implement it.
type Op interface {
	name() string
}

// Recall searches memories. TopK 0 means 5.
type Recall struct {
	Query string
	TopK  int
}

// Remember stores a memory. Source defaults to the key name; Importance to 0.5.
type Remember struct {
	Content    string
	Source     string
	Importance *float64
}

type Stats struct{}

// Forget deletes one memory by ID.
type Forget struct{ ID string }

type Sleep struct{}

func (Recall) name() string {
	return "recall"
}

func (Remember) name() string {
	return "remember"
}

func (Stats) name() string {
	return "stats"
}

func (Forget) name() string {
	return "forget"
}

func (Sleep) name() string {
	return "sleep"
}

// Config tunes the gateway. Provider names the provider in result IDs
// ("<provider>:<id>"); it defaults to "memory". Zero values select the defaults: QueueLen 32, Timeout
// 30s, SleepTimeout 10m, StartTimeout 2m, MaxRestarts 5 (so 0 cannot mean zero),
// restart backoff 1s. AllowForget and AllowSleep enable those operations for
// maintenance keys only.
type Config struct {
	Provider     string
	QueueLen     int
	Timeout      time.Duration
	SleepTimeout time.Duration
	StartTimeout time.Duration
	MaxRestarts  int
	AllowForget  bool
	AllowSleep   bool

	restartBase time.Duration
}

const maxRestartDelay = 30 * time.Second

func (c Config) withDefaults() Config {
	if c.Provider == "" {
		c.Provider = "memory"
	}
	if c.QueueLen <= 0 {
		c.QueueLen = 32
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.SleepTimeout <= 0 {
		c.SleepTimeout = 10 * time.Minute
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 2 * time.Minute
	}
	if c.MaxRestarts <= 0 {
		c.MaxRestarts = 5
	}
	if c.restartBase <= 0 {
		c.restartBase = time.Second
	}
	return c
}

// restartDelay doubles restartBase per consecutive failure, capped at maxRestartDelay.
func (c Config) restartDelay(failures int) time.Duration {
	d := c.restartBase
	for i := 0; i < failures && d < maxRestartDelay; i++ {
		d *= 2
	}
	return min(d, maxRestartDelay)
}

// activeKeys is the registry view the gateway needs; *keys.Set satisfies it.
type activeKeys interface {
	Active(id string) (keys.Key, bool)
}

// Gateway runs operations on one backend. The owner goroutine drains jobs in order;
// closing jobs (via Shutdown) lets it finish. halt is cancelled when shutdown gives up
// waiting, and done receives the worker's final backend (nil if none) on exit.
type Gateway struct {
	cfg      Config
	start    Starter
	keys     activeKeys
	jobs     chan *job
	ready    atomic.Bool
	stopping atomic.Bool

	mu     sync.RWMutex
	closed bool

	halt   context.Context
	haltFn context.CancelFunc
	done   chan Backend
	once   sync.Once
	err    error

	dead     chan struct{}
	deadOnce sync.Once
}

type job struct {
	ctx     context.Context
	key     keys.Key
	name    string
	args    any
	timeout time.Duration
	done    chan result
}

type result struct {
	raw json.RawMessage
	err error
}

// New starts the backend and the owner goroutine.
func New(ctx context.Context, cfg Config, start Starter, ks activeKeys) (*Gateway, error) {
	cfg = cfg.withDefaults()
	sctx, cancel := context.WithTimeout(ctx, cfg.StartTimeout)
	b, err := start(sctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("start backend: %w", err)
	}
	halt, haltFn := context.WithCancel(context.Background())
	// ponytail: one worker and one backend call at a time, so throughput is the
	// slowest provider call, and a full queue returns ErrBusy at once. Upgrade path:
	// parallel readers, for providers whose reads are side-effect free
	// (Mnemosyne recall updates recall_count).
	g := &Gateway{
		cfg:    cfg,
		start:  start,
		keys:   ks,
		jobs:   make(chan *job, cfg.QueueLen),
		halt:   halt,
		haltFn: haltFn,
		done:   make(chan Backend, 1),
		dead:   make(chan struct{}),
	}
	g.ready.Store(true)
	go g.run(b)
	return g, nil
}

// Ready reports whether a backend is running and accepting work.
func (g *Gateway) Ready() bool { return g.ready.Load() && !g.stopping.Load() }

// Dead is closed when restarts are exhausted: no backend will ever run again and
// every operation fails with ErrUnavailable. The owner should exit so its service
// manager can start a fresh process. It is not closed by Shutdown.
func (g *Gateway) Dead() <-chan struct{} { return g.dead }

func (g *Gateway) giveUp() {
	if g.halt.Err() == nil {
		g.deadOnce.Do(func() { close(g.dead) })
	}
}

// Do validates and authorizes op, then waits for its result. If ctx ends first,
// Do returns ctx.Err(); a job already queued is still run or skipped, never requeued.
func (g *Gateway) Do(ctx context.Context, key keys.Key, op Op) (json.RawMessage, error) {
	args, err := prepare(key, op, g.cfg.Provider)
	if err != nil {
		return nil, err
	}
	if !permitted(g.cfg, key.Role, op.name()) {
		return nil, fmt.Errorf("%w: role %s may not %s", ErrForbidden, key.Role, op.name())
	}
	timeout := g.cfg.Timeout
	if _, ok := op.(Sleep); ok {
		timeout = g.cfg.SleepTimeout
	}
	j := &job{ctx: ctx, key: key, name: op.name(), args: args, timeout: timeout, done: make(chan result, 1)}
	if err := g.enqueue(j); err != nil {
		return nil, err
	}
	select {
	case r := <-j.done:
		if r.err != nil {
			return nil, r.err
		}
		return contract(g.cfg.Provider, op, r.raw)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// contract turns a provider result into the client contract, which follows mem0's
// shapes: recall results and remember IDs get the "<provider>:" prefix, remember
// reports {results: [{id, memory, event: "ADD"}]}, and stats and sleep are keyed
// by provider name. A result that does not match the provider contract is an
// *OpError; the operation itself did complete.
func contract(name string, op Op, raw json.RawMessage) (json.RawMessage, error) {
	bad := func(err error) (json.RawMessage, error) {
		return nil, &provider.OpError{Msg: "InvalidResult: " + err.Error()}
	}
	switch o := op.(type) {
	case Recall:
		var r struct{ Results []provider.Memory }
		if err := json.Unmarshal(raw, &r); err != nil {
			return bad(err)
		}
		if r.Results == nil {
			r.Results = []provider.Memory{}
		}
		for i := range r.Results {
			r.Results[i].ID = name + ":" + r.Results[i].ID
			r.Results[i].Provider = name
		}
		return json.Marshal(map[string]any{"results": r.Results})
	case Remember:
		var r struct{ ID string }
		if err := json.Unmarshal(raw, &r); err != nil {
			return bad(err)
		}
		added := map[string]string{"id": name + ":" + r.ID, "memory": o.Content, "event": "ADD", "provider": name}
		return json.Marshal(map[string]any{"results": []map[string]string{added}})
	case Stats, Sleep:
		return json.Marshal(map[string]any{"providers": map[string]json.RawMessage{name: raw}})
	}
	return raw, nil
}

func (g *Gateway) enqueue(j *job) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	switch {
	case g.closed:
		return ErrShuttingDown
	case !g.Ready():
		return ErrUnavailable
	}
	select {
	case g.jobs <- j:
		return nil
	default:
		return ErrBusy
	}
}

// Shutdown stops accepting work and waits for the in-flight call and queued jobs
// until ctx ends. Then it cancels the in-flight call, fails the queued jobs with
// ErrShuttingDown, and closes the backend. Later calls return the first result.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.once.Do(func() { g.err = g.shutdown(ctx) })
	return g.err
}

func (g *Gateway) shutdown(ctx context.Context) error {
	g.stopping.Store(true)
	g.mu.Lock()
	g.closed = true
	close(g.jobs)
	g.mu.Unlock()

	var err error
	var b Backend
	select {
	case b = <-g.done:
	case <-ctx.Done():
		err = ctx.Err()
		g.haltFn()
		b = <-g.done
	}
	g.haltFn()
	if b != nil {
		cctx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			cctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		b.Close(cctx)
	}
	return err
}

// run is the owner goroutine. It is the only code that touches the backend.
func (g *Gateway) run(b Backend) {
	// deaths counts consecutive failed calls, so a backend that starts but dies on
	// every call is given up after MaxRestarts instead of restarting forever.
	deaths := 0
	for j := range g.jobs {
		// A job whose ctx ended before dispatch never starts.
		if err := j.ctx.Err(); err != nil {
			j.done <- result{err: err}
			continue
		}
		if err := g.recheck(j); err != nil {
			j.done <- result{err: err}
			continue
		}
		// b is nil once restarts are exhausted.
		if b == nil {
			j.done <- result{err: ErrUnavailable}
			continue
		}
		raw, err := g.call(b, j)
		var opErr *provider.OpError
		dead := err != nil && !errors.As(err, &opErr)
		// Call has already killed and reaped the process.
		if dead {
			g.ready.Store(false)
		}
		j.done <- result{raw: raw, err: err}
		if !dead {
			deaths = 0
			continue
		}
		if deaths++; deaths > g.cfg.MaxRestarts {
			b = nil
		} else {
			b = g.restartDraining()
		}
		if b == nil {
			g.giveUp()
		}
	}
	g.done <- b
}

// recheck repeats the authorization done in Do: a key revoked or re-roled while
// queued must not reach the backend.
func (g *Gateway) recheck(j *job) error {
	if g.halt.Err() != nil {
		return ErrShuttingDown
	}
	k, ok := g.keys.Active(j.key.ID)
	if !ok || !permitted(g.cfg, k.Role, j.name) {
		return fmt.Errorf("%w: key %s no longer permits %s", ErrForbidden, j.key.ID, j.name)
	}
	return nil
}

func (g *Gateway) call(b Backend, j *job) (json.RawMessage, error) {
	// Once dispatched, a call is not tied to its caller: a client disconnect must
	// not kill the backend mid-write. Only the timeout and shutdown can stop it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(j.ctx), j.timeout)
	defer cancel()
	stop := context.AfterFunc(g.halt, cancel)
	defer stop()
	return b.Call(ctx, j.name, j.args)
}

// restartDraining runs restart while failing queued jobs with ErrUnavailable, so
// callers queued behind a dead backend do not wait out the whole restart sequence.
func (g *Gateway) restartDraining() Backend {
	nb := make(chan Backend, 1)
	go func() { nb <- g.restart() }()
	for {
		select {
		case b := <-nb:
			if b != nil {
				g.ready.Store(true)
			}
			return b
		case j, ok := <-g.jobs:
			if !ok { // shutdown: run's range loop ends once restart returns
				return <-nb
			}
			err := ErrUnavailable
			if g.halt.Err() != nil {
				err = ErrShuttingDown
			}
			j.done <- result{err: err}
		}
	}
}

// restart starts a replacement with exponential backoff. It returns nil after
// MaxRestarts consecutive failed starts or when shutdown begins; the caller
// tells the two apart with g.halt.
func (g *Gateway) restart() Backend {
	for failures := 0; failures < g.cfg.MaxRestarts; failures++ {
		t := time.NewTimer(g.cfg.restartDelay(failures))
		select {
		case <-t.C:
		case <-g.halt.Done():
			t.Stop()
			return nil
		}
		ctx, cancel := context.WithTimeout(g.halt, g.cfg.StartTimeout)
		b, err := g.start(ctx)
		cancel()
		if err == nil {
			return b
		}
	}
	return nil
}

// prepare validates op and returns the provider arguments for it. name is the
// provider's ID prefix.
func prepare(key keys.Key, op Op, name string) (any, error) {
	switch o := op.(type) {
	case Recall:
		if strings.TrimSpace(o.Query) == "" {
			return nil, invalid("query is empty")
		}
		if len(o.Query) > 4096 {
			return nil, invalid("query exceeds 4096 bytes")
		}
		if !utf8.ValidString(o.Query) {
			return nil, invalid("query is not valid UTF-8")
		}
		topK := o.TopK
		if topK == 0 {
			topK = 5
		}
		if topK < 1 || topK > 50 {
			return nil, invalid("top_k %d outside 1-50", o.TopK)
		}
		return map[string]any{"query": o.Query, "top_k": topK}, nil
	case Remember:
		if strings.TrimSpace(o.Content) == "" {
			return nil, invalid("content is empty")
		}
		if len(o.Content) > 32768 {
			return nil, invalid("content exceeds 32768 bytes")
		}
		if !utf8.ValidString(o.Content) {
			return nil, invalid("content is not valid UTF-8")
		}
		if len(o.Source) > 64 {
			return nil, invalid("source exceeds 64 bytes")
		}
		if !utf8.ValidString(o.Source) {
			return nil, invalid("source is not valid UTF-8")
		}
		source := o.Source
		if strings.TrimSpace(source) == "" {
			source = key.Name
			if source == "" {
				source = "gateway"
			}
		}
		importance := 0.5
		if o.Importance != nil {
			importance = *o.Importance
			// Written so NaN fails too: it compares false with everything.
			if !(importance >= 0 && importance <= 1) {
				return nil, invalid("importance %v outside [0,1]", importance)
			}
		}
		return map[string]any{"content": o.Content, "source": source, "importance": importance, "author_id": key.ID}, nil
	case Forget:
		id, ok := strings.CutPrefix(o.ID, name+":")
		if !ok {
			return nil, invalid("id must start with %q", name+":")
		}
		if id == "" {
			return nil, invalid("id is empty")
		}
		if len(id) > 128 {
			return nil, invalid("id exceeds 128 bytes")
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return nil, invalid("id has characters outside [A-Za-z0-9_-]")
			}
		}
		return map[string]any{"id": id}, nil
	case Stats, Sleep:
		return map[string]any{}, nil
	default:
		return nil, invalid("unknown operation")
	}
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, a...))
}

// permitted implements the role table in README.md (API keys).
func permitted(cfg Config, role keys.Role, op string) bool {
	switch role {
	case keys.ReadOnly:
		return op == "recall" || op == "stats"
	case keys.ReadWrite:
		return op == "recall" || op == "stats" || op == "remember"
	case keys.Maintenance:
		switch op {
		case "forget":
			return cfg.AllowForget
		case "sleep":
			return cfg.AllowSleep
		}
		return op == "recall" || op == "stats" || op == "remember"
	}
	return false
}
