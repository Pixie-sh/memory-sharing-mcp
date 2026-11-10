// Command memory-sharing-mcp runs the memory gateway that owns one provider's
// store (http serve), the stdio MCP adapter (mcp serve), and API key management
// (mcp keygen, mcp keys).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pixie-sh/memory-sharing-mcp/internal/gateway"
	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/mnemosyne"
	"github.com/pixie-sh/memory-sharing-mcp/internal/transport"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usageText = `usage: memory-sharing-mcp [--config PATH] <command>

commands:
  version
  install [--bin-dir DIR]      copy to DIR (default ~/.local/bin) and run http serve as a macOS LaunchAgent or Linux systemd user unit
  uninstall [--bin-dir DIR]
  store init
  http serve
  mcp serve
  mcp keygen [--name N] [--role read-only|read-write|maintenance] [--print]
  mcp keys list
  mcp keys revoke <id>
`

func main() {
	transport.Version = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes one command and returns the exit code: 0 ok, 1 failure, 2 usage.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("memory-sharing-mcp", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() { fmt.Fprint(stderr, usageText) }
	cfgFlag := fset.String("config", "", "config file (default $MSM_CONFIG, then ~/.config/memory-sharing-mcp/config.json)")
	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fset.Args()
	if len(rest) == 1 && rest[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	// For scripts/setup.sh, so the install it offers matches the version the backend requires.
	if len(rest) == 2 && rest[0] == "version" && rest[1] == "mnemosyne" {
		fmt.Fprintln(stdout, mnemosyne.TestedVersion)
		return 0
	}
	var cmd string
	var cargs []string
	switch {
	case len(rest) > 0 && (rest[0] == "install" || rest[0] == "uninstall"):
		cmd, cargs = rest[0], rest[1:]
	case len(rest) >= 2:
		cmd, cargs = rest[0]+" "+rest[1], rest[2:]
	default:
		fmt.Fprint(stderr, usageText)
		return 2
	}
	switch cmd {
	case "uninstall":
		return uninstall(cargs, stdout, stderr)
	case "install", "store init", "http serve", "mcp serve", "mcp keygen", "mcp keys":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", cmd)
		fmt.Fprint(stderr, usageText)
		return 2
	}
	if (cmd == "store init" || cmd == "http serve" || cmd == "mcp serve") && len(cargs) != 0 {
		fmt.Fprintf(stderr, "%s takes no arguments\n", cmd)
		return 2
	}

	path, explicit, err := configPath(*cfgFlag)
	if err != nil {
		return report(stderr, err)
	}
	cfg, err := loadConfig(path, explicit)
	if err != nil {
		return report(stderr, err)
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	switch cmd {
	case "install":
		return install(cargs, path, cfg, stdout, stderr)
	case "store init":
		return report(stderr, storeInit(ctx, cfg, stdout, stderr))
	case "http serve":
		return report(stderr, httpServe(ctx, cfg, log, stderr))
	case "mcp serve":
		return report(stderr, mcpServe(ctx, cfg, log))
	case "mcp keygen":
		return keygen(cargs, cfg, stdout, stderr)
	default:
		return keysCmd(cargs, cfg, stdout, stderr)
	}
}

func report(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, "error:", err)
	return 1
}

// httpServe owns the canonical store for the life of the process: it holds the
// lock, runs the one provider, and serves HTTP until a signal arrives.
func httpServe(ctx context.Context, cfg config, log *slog.Logger, providerOut io.Writer) error {
	providerStderr := io.Discard
	if cfg.ProviderLog {
		providerStderr = providerOut
	}
	start, db, err := openProvider(cfg, providerStderr, false)
	if err != nil {
		return err
	}
	if err := checkStore(db); err != nil {
		return err
	}
	canon, err := filepath.EvalSymlinks(db)
	if err != nil {
		return err
	}
	release, err := gateway.AcquireLock(cfg.StateDir, db)
	if err != nil {
		return err
	}
	defer func() {
		if err := release(); err != nil {
			log.Error("release lock", "err", err)
		}
	}()

	ks, err := keys.Open(cfg.Registry)
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// Watch retries every tick, so a broken registry file would otherwise log once
	// a second; the same error is reported once. Only Watch's goroutine calls onErr.
	var lastWatchErr string
	go ks.Watch(ctx, time.Second, func(err error) {
		if s := err.Error(); s != lastWatchErr {
			lastWatchErr = s
			log.Error("registry watch", "err", err)
		}
	})

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := ks.Reload(); err != nil {
					log.Error("registry reload", "err", err)
				} else {
					log.Info("registry reloaded")
				}
			}
		}
	}()

	// Probe the port so a conflict fails fast, but only listen once the provider is
	// up: connections then get ECONNREFUSED, which adapters retry, instead of
	// hanging unserved in the backlog through a slow provider start.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	ln.Close()
	g, err := gateway.New(ctx, gateway.Config{Provider: cfg.Provider, AllowForget: cfg.AllowForget, AllowSleep: cfg.AllowSleep}, start, ks)
	if err != nil {
		return err
	}
	if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
		gctx, cancelG := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelG()
		return errors.Join(err, g.Shutdown(gctx))
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: transport.Handler(g, ks, log),
		// No WriteTimeout: SSE streams and sleep calls legitimately outlive any fixed deadline.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("http serve started", "listen", ln.Addr().String(), "provider", cfg.Provider, "store", canon, "version", version)

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	case <-g.Dead():
		// Exit rather than serve 503 forever: the service manager restarts us.
		serveErr = errors.New("provider restarts exhausted")
		log.Error("provider restarts exhausted; exiting for the service manager to restart")
	}
	log.Info("http serve stopping")
	sctx, cancelS := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelS()
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("http shutdown", "err", err)
		serveErr = errors.Join(serveErr, err)
	}
	gctx, cancelG := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelG()
	if err := g.Shutdown(gctx); err != nil {
		log.Error("gateway shutdown", "err", err)
		serveErr = errors.Join(serveErr, err)
	}
	log.Info("http serve stopped")
	return serveErr
}

// checkStore names the fix when the store is missing, instead of a raw stat error.
func checkStore(db string) error {
	if _, err := os.Stat(db); err != nil {
		return fmt.Errorf("store %s: %w (create one with store init)", db, err)
	}
	return nil
}

// storeInit creates an empty store for the configured provider and prints its
// path. It refuses an existing store, so it cannot touch one in use.
func storeInit(ctx context.Context, cfg config, stdout, stderr io.Writer) error {
	start, db, err := openProvider(cfg, stderr, true)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(db); err == nil {
		return fmt.Errorf("store %s already exists", db)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		return err
	}
	p, err := start(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p.Close(cctx)
	}()
	if _, err := p.Call(ctx, "stats", nil); err != nil {
		return err
	}
	fmt.Fprintln(stdout, db)
	return nil
}

// mcpServe forwards stdio MCP to the owner. stdout carries only MCP traffic.
func mcpServe(ctx context.Context, cfg config, log *slog.Logger) error {
	secret, err := credential(cfg)
	if err != nil {
		return err
	}
	// The endpoint is not logged: Proxy has not yet rejected one carrying credentials.
	log.Info("mcp serve started")
	return transport.Proxy(ctx, cfg.Endpoint, secret, &mcp.StdioTransport{})
}

// credential returns MSM_API_KEY if set, else the trimmed credential file, which must
// not be readable by group or other.
func credential(cfg config) (string, error) {
	if s := strings.TrimSpace(os.Getenv("MSM_API_KEY")); s != "" {
		return s, nil
	}
	if cfg.CredentialFile == "" {
		return "", errors.New("no API key: set MSM_API_KEY or credential_file in the config file")
	}
	f, err := os.Open(cfg.CredentialFile)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if err := keys.OwnerOnly(fi, "credential file "+cfg.CredentialFile, 0o600); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("credential file %s is empty", cfg.CredentialFile)
	}
	return s, nil
}

// keygen registers a new key. With --print the secret is the only stdout output;
// otherwise it is saved to a new owner-only credential file and that path is printed.
func keygen(args []string, cfg config, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("mcp keygen", flag.ContinueOnError)
	fset.SetOutput(stderr)
	name := fset.String("name", "", "optional label, at most 64 bytes, printable")
	role := fset.String("role", string(keys.ReadWrite), "read-only, read-write, or maintenance")
	printOnly := fset.Bool("print", false, "print the secret to stdout instead of saving a credential file")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintln(stderr, "mcp keygen takes no positional arguments")
		return 2
	}
	r, err := keys.ParseRole(*role)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if err := checkName(*name); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	secret, k, err := keys.Generate(cfg.Registry, *name, r)
	if err != nil {
		return report(stderr, err)
	}
	fmt.Fprintf(stderr, "key %s role=%s name=%q\n", k.ID, k.Role, k.Name)
	if *printOnly {
		// A closed pipe must surface as a write error we can roll back, not kill us.
		signal.Ignore(syscall.SIGPIPE)
		if _, err := fmt.Fprintln(stdout, secret); err != nil {
			if rerr := keys.Revoke(cfg.Registry, k.ID); rerr != nil {
				fmt.Fprintf(stderr, "error: revoke %s after failed output: %v\n", k.ID, rerr)
			}
			return report(stderr, err)
		}
		return 0
	}
	path := filepath.Join(cfg.CredentialsDir, k.ID+".key")
	if err := keys.WriteCredential(path, secret); err != nil {
		// Do not leave a usable key that nobody holds.
		if rerr := keys.Revoke(cfg.Registry, k.ID); rerr != nil {
			fmt.Fprintf(stderr, "error: revoke %s after failed credential write: %v\n", k.ID, rerr)
		}
		return report(stderr, err)
	}
	fmt.Fprintln(stdout, path)
	return 0
}

// checkName enforces the --name rules: at most 64 bytes, valid UTF-8, all printable.
func checkName(s string) error {
	if len(s) > 64 {
		return errors.New("--name exceeds 64 bytes")
	}
	if !utf8.ValidString(s) {
		return errors.New("--name is not valid UTF-8")
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return errors.New("--name must be printable")
		}
	}
	return nil
}

// keysCmd lists or revokes registry entries. Hashes are never printed.
func keysCmd(args []string, cfg config, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "list":
		ks, err := keys.List(cfg.Registry)
		if err != nil {
			return report(stderr, err)
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tROLE\tSTATUS\tCREATED")
		for _, k := range ks {
			status, name := "active", k.Name
			if k.Revoked != nil {
				status = "revoked"
			}
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, name, k.Role, status, k.Created.Format(time.RFC3339))
		}
		tw.Flush()
		return 0
	case len(args) == 2 && args[0] == "revoke":
		if err := keys.Revoke(cfg.Registry, args[1]); err != nil {
			return report(stderr, err)
		}
		fmt.Fprintln(stdout, "revoked", args[1])
		return 0
	}
	fmt.Fprintln(stderr, "usage: memory-sharing-mcp mcp keys list | mcp keys revoke <id>")
	return 2
}
