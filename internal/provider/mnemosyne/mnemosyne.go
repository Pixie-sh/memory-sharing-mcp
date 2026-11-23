// Package mnemosyne is the Mnemosyne memory provider. It runs an embedded
// Python script with the interpreter of a mnemosyne-memory installation, as a
// pyproc backend.
package mnemosyne

import (
	"context"
	_ "embed"
	"io"
	"path/filepath"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/pyproc"
)

//go:embed mnemosyne.py
var script string

// TestedVersion is the mnemosyne-memory release the script is written and tested
// against, and the default for Config.Version.
const TestedVersion = "3.15.1"

// DefaultSession is the session Pi's Mnemosyne integration stores memories in.
const DefaultSession = "default"

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

// Start launches the backend and waits until it reports ready or fails.
func Start(ctx context.Context, cfg Config) (*pyproc.Backend, error) {
	return pyproc.Start(ctx, process(cfg, script))
}

// Starter returns a provider.Starter that starts a backend with cfg.
func Starter(cfg Config) provider.Starter {
	return pyproc.Starter(process(cfg, script))
}

// process is the script invocation for cfg: mnemosyne.py's argv and environment.
func process(cfg Config, src string) pyproc.Config {
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
	return pyproc.Config{
		Python: cfg.Python,
		Script: src,
		Args:   []string{cfg.Version, cfg.Session, mode},
		Env:    []string{"MNEMOSYNE_DATA_DIR=" + cfg.DataDir},
		Stderr: cfg.Stderr,
	}
}
