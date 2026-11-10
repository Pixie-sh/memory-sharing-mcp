package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
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
	default:
		return nil, "", fmt.Errorf("unknown provider %q (supported: mnemosyne)", cfg.Provider)
	}
}
