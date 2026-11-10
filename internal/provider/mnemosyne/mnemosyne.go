// Package mnemosyne is the Mnemosyne memory provider. It runs an embedded
// Python script with the interpreter of a mnemosyne-memory installation and
// exchanges one JSON line per request with it.
package mnemosyne

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

//go:embed mnemosyne.py
var script string

// TestedVersion is the mnemosyne-memory release the script is written and tested
// against, and the default for Config.Version.
const TestedVersion = "3.15.1"

// DefaultSession is the session Pi's Mnemosyne integration stores memories in.
const DefaultSession = "default"

// maxLine bounds a single response; larger output is treated as a backend failure.
const maxLine = 16 << 20

// Config locates the Mnemosyne installation and the store.
type Config struct {
	Python  string    // interpreter of the mnemosyne-memory environment
	DataDir string    // Mnemosyne data dir containing mnemosyne.db
	Session string    // session memories are read from and written to; DefaultSession when empty
	Version string    // exact mnemosyne-memory version required; TestedVersion when empty
	Create  bool      // create an empty store when mnemosyne.db is missing, instead of refusing
	Stderr  io.Writer // backend diagnostics; os.Stderr when nil
}

// DBPath is the store file inside dataDir.
func DBPath(dataDir string) string { return filepath.Join(dataDir, "mnemosyne.db") }

type response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Backend is one running backend process; it implements provider.Provider.
// Calls are serialized.
//
// ponytail: one process, one request at a time. Throughput is bounded by the
// slowest Mnemosyne call, which is fine for agent memory traffic. Upgrade path:
// parallel readers, only if Mnemosyne reads become side-effect free (recall
// currently updates recall_count).
type Backend struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	exited chan struct{}
	nextID uint64
	dead   bool
}

// Start launches the backend and waits until it reports ready or fails.
func Start(ctx context.Context, cfg Config) (*Backend, error) {
	return start(ctx, cfg, script)
}

// Starter returns a provider.Starter that starts a backend with cfg.
func Starter(cfg Config) provider.Starter {
	return func(ctx context.Context) (provider.Provider, error) {
		b, err := Start(ctx, cfg)
		if err != nil {
			return nil, err // not a typed nil inside the interface
		}
		return b, nil
	}
}

func start(ctx context.Context, cfg Config, src string) (*Backend, error) {
	if cfg.Session == "" {
		cfg.Session = DefaultSession
	}
	if cfg.Version == "" {
		cfg.Version = TestedVersion
	}
	mode := "open"
	if cfg.Create {
		mode = "create"
	}
	// -X utf8: the protocol is UTF-8 on both pipes whatever the (absent) locale says.
	cmd := exec.Command(cfg.Python, "-I", "-X", "utf8", "-c", src, cfg.Version, cfg.Session, mode)
	// Minimal environment, as under launchd: nothing inherited from the shell.
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"MNEMOSYNE_DATA_DIR=" + cfg.DataDir,
	}
	// Own process group, killed as a whole: a forked helper (e.g. multiprocessing)
	// inherits the pipes and would otherwise keep Wait, and so kill, from returning.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = cfg.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start backend: %w", err)
	}

	b := &Backend{cmd: cmd, stdin: stdin, lines: make(chan []byte), exited: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			b.lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(b.lines)
		if sc.Err() != nil { // oversized line: the process may still be running
			killGroup(cmd)
		}
		cmd.Wait() // stdout is fully read, as os/exec requires
		close(b.exited)
	}()

	ready, err := b.read(ctx, 0)
	if err == nil && !ready.OK {
		err = errors.New(ready.Error)
	}
	if err != nil {
		b.kill()
		return nil, fmt.Errorf("backend startup: %w", err)
	}
	return b, nil
}

// Call runs one operation. A *provider.OpError leaves the backend usable. Any other
// error kills the process: provider.ErrIndeterminate when the request may have been
// applied, provider.ErrUnavailable when the backend was already gone.
func (b *Backend) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dead {
		return nil, provider.ErrUnavailable
	}
	b.nextID++
	req, err := json.Marshal(map[string]any{"id": b.nextID, "op": op, "args": args})
	if err != nil { // nothing was sent, so the process is still usable
		return nil, &provider.OpError{Msg: err.Error()}
	}
	if _, err := b.stdin.Write(append(req, '\n')); err != nil {
		b.kill()
		return nil, fmt.Errorf("%w: %v", provider.ErrUnavailable, err)
	}
	resp, err := b.read(ctx, b.nextID)
	if err != nil {
		b.kill()
		return nil, fmt.Errorf("%w: %v", provider.ErrIndeterminate, err)
	}
	if !resp.OK {
		return nil, &provider.OpError{Msg: resp.Error}
	}
	return resp.Result, nil
}

func (b *Backend) read(ctx context.Context, id uint64) (response, error) {
	var resp response
	select {
	case <-ctx.Done():
		return resp, ctx.Err()
	case line, ok := <-b.lines:
		if !ok {
			return resp, errors.New("backend exited")
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return resp, fmt.Errorf("malformed backend response: %w", err)
		}
		if resp.ID != id {
			return resp, fmt.Errorf("backend response id %d, want %d", resp.ID, id)
		}
		return resp, nil
	}
}

// kill stops the process and waits until it has been reaped, so no replacement
// can overlap with it. Callers hold b.mu or own b exclusively.
func (b *Backend) kill() {
	b.dead = true
	killGroup(b.cmd)
	for range b.lines { // unblock the reader so it can reap the process
	}
	<-b.exited
}

func killGroup(cmd *exec.Cmd) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// Close asks the backend to exit by closing its stdin, killing it if it has
// not exited when ctx ends.
func (b *Backend) Close(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dead {
		return
	}
	b.dead = true
	b.stdin.Close()
	select {
	case <-b.exited:
	case <-ctx.Done():
		b.kill()
	}
}

// Exited is closed once the backend process has been reaped.
func (b *Backend) Exited() <-chan struct{} { return b.exited }
