// Package mem0 is the mem0 memory provider. It runs an embedded Python script
// with the interpreter of a mem0ai installation, as a pyproc backend.
package mem0

import (
	_ "embed"
	"encoding/json"
	"io"
	"path/filepath"
	"slices"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/pyproc"
)

//go:embed mem0.py
var script string

// TestedVersion is the mem0ai release the script is written and tested
// against, and the default for Config.Version.
const TestedVersion = "2.2.1"

// DefaultUserID is the mem0 user_id memories are scoped to when none is set.
const DefaultUserID = "default"

// Config locates the mem0 installation and the store.
type Config struct {
	Python   string          // interpreter of the mem0ai environment
	DataDir  string          // holds history.db; the vector store is wherever Settings says
	UserID   string          // mem0 user_id memories are read from and written to; DefaultUserID when empty
	Version  string          // exact mem0ai version required; TestedVersion when empty
	Settings json.RawMessage // mem0's own configuration object (llm, embedder, vector_store); may hold API keys
	Env      []string        // extra KEY=value variables for the script, such as OLLAMA_HOST or a model cache path
	Create   bool            // create an empty store when history.db is missing, instead of refusing
	Stderr   io.Writer       // backend diagnostics; os.Stderr when nil
}

// StorePath is the file inside dataDir whose presence marks an existing store.
func StorePath(dataDir string) string { return filepath.Join(dataDir, "history.db") }

// Starter returns a provider.Starter that starts a backend with cfg.
func Starter(cfg Config) provider.Starter {
	return pyproc.Starter(process(cfg, script))
}

// process is the script invocation for cfg: mem0.py's argv and environment.
func process(cfg Config, src string) pyproc.Config {
	if cfg.UserID == "" {
		cfg.UserID = DefaultUserID
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
		Args:   []string{cfg.Version, cfg.UserID, mode},
		// The gateway's own variables come last, so cfg.Env cannot replace them.
		Env: append(slices.Clone(cfg.Env),
			"MEM0_DIR="+cfg.DataDir, // also where mem0 keeps its own config.json
			"MEM0_TELEMETRY=False",  // mem0 otherwise reports usage to its PostHog
			// Not an argument: argv is visible to every user on the machine.
			"MSM_MEM0_CONFIG="+string(cfg.Settings),
		),
		Stderr: cfg.Stderr,
	}
}
