package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/mem0"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider/mnemosyne"
)

// openProvider returns a starter for the configured memory provider and the path
// of the store it owns; the owner lock is keyed on that path. With create, the
// starter creates an empty store if none exists (store init). New providers are
// added here, with their own section in the config file.
func openProvider(cfg config, stderr io.Writer, create bool) (provider.Starter, string, error) {
	switch cfg.Provider {
	case "mnemosyne":
		m := cfg.Mnemosyne
		if m.DataDir == "" || m.Python == "" {
			return nil, "", errors.New(`provider "mnemosyne" needs mnemosyne.data_dir and mnemosyne.python in the config file`)
		}
		start := mnemosyne.Starter(mnemosyne.Config{
			Python: m.Python, DataDir: m.DataDir, Session: m.Session, Version: m.Version, Create: create, Stderr: stderr,
		})
		return start, mnemosyne.DBPath(m.DataDir), nil
	case "mem0":
		m := cfg.Mem0
		if m.DataDir == "" || m.Python == "" {
			return nil, "", errors.New(`provider "mem0" needs mem0.data_dir and mem0.python in the config file`)
		}
		// mem0 rejects such ids on every add and search, long after startup.
		if strings.ContainsFunc(m.UserID, unicode.IsSpace) {
			return nil, "", errors.New("mem0.user_id must not contain whitespace")
		}
		var env []string
		for _, k := range slices.Sorted(maps.Keys(m.Env)) {
			if k == "" || strings.Contains(k, "=") {
				return nil, "", fmt.Errorf("mem0.env: invalid variable name %q", k)
			}
			env = append(env, k+"="+m.Env[k])
		}
		start := mem0.Starter(mem0.Config{
			Python: m.Python, DataDir: m.DataDir, UserID: m.UserID, Version: m.Version, Settings: m.Config, Env: env, Create: create, Stderr: stderr,
		})
		return start, mem0.StorePath(m.DataDir), nil
	default:
		return nil, "", fmt.Errorf("unknown provider %q (supported: mnemosyne, mem0)", cfg.Provider)
	}
}

// checkInstall reports a provider whose interpreter or package is missing or at
// another version, with the command that installs it. It does not open the store.
func checkInstall(ctx context.Context, cfg config) error {
	var py, pkg, ver, install string
	switch cfg.Provider {
	case "mnemosyne":
		py, pkg, ver = cfg.Mnemosyne.Python, "mnemosyne-memory", cmp.Or(cfg.Mnemosyne.Version, mnemosyne.TestedVersion)
		install = "uv tool install " + pkg + "==" + ver
	case "mem0":
		py, pkg, ver = cfg.Mem0.Python, "mem0ai", cmp.Or(cfg.Mem0.Version, mem0.TestedVersion)
		// mem0ai has no command of its own, so it gets a plain environment instead of a uv tool.
		install = fmt.Sprintf("uv pip install --python %s %s==%s", py, pkg, ver)
		if _, err := os.Stat(py); err != nil {
			// Only a path shaped like a virtual environment's says where to create one.
			if env, ok := strings.CutSuffix(py, "/bin/python"); ok {
				install = fmt.Sprintf("uv venv %s && %s", env, install)
			} else {
				install = "create that Python environment, then " + install
			}
		}
	default:
		return fmt.Errorf("unknown provider %q (supported: mnemosyne, mem0)", cfg.Provider)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, py, "-I", "-c",
		"import sys; from importlib.metadata import version; print(version(sys.argv[1]))", pkg).Output()
	if err != nil {
		return fmt.Errorf("%s is not installed for %s (%w); install it with: %s", pkg, py, err, install)
	}
	if got := strings.TrimSpace(string(out)); got != ver {
		return fmt.Errorf("%s %s found for %s, %s required; install it with: %s", pkg, got, py, ver, install)
	}
	return nil
}
