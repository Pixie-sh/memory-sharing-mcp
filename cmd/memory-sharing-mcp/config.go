package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// config is the JSON configuration file. Unknown fields are rejected. Paths must be
// absolute after a leading "~/" is expanded.
type config struct {
	Listen         string          `json:"listen"`
	Provider       string          `json:"provider"`
	Mnemosyne      mnemosyneConfig `json:"mnemosyne"`
	StateDir       string          `json:"state_dir"`
	Registry       string          `json:"registry"`
	CredentialsDir string          `json:"credentials_dir"`
	AllowForget    bool            `json:"allow_forget"`
	AllowSleep     bool            `json:"allow_sleep"`
	ProviderLog    bool            `json:"provider_log"`
	Endpoint       string          `json:"endpoint"`
	CredentialFile string          `json:"credential_file"`
}

// mnemosyneConfig configures the "mnemosyne" provider.
type mnemosyneConfig struct {
	DataDir string `json:"data_dir"` // contains mnemosyne.db; required
	Python  string `json:"python"`   // interpreter of the mnemosyne-memory environment; required
	Session string `json:"session"`  // default "default", as Pi uses
	Version string `json:"version"`  // exact mnemosyne-memory version; default the tested one
}

// configPath resolves the config file: the --config flag, then $MSM_CONFIG, then the
// default location. explicit reports whether the user named it, in which case it
// must exist: a mistyped path must not silently fall back to defaults.
func configPath(flagValue string) (path string, explicit bool, err error) {
	p := flagValue
	if p == "" {
		p = os.Getenv("MSM_CONFIG")
	}
	explicit = p != ""
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, err
		}
		p = filepath.Join(home, ".config", "memory-sharing-mcp", "config.json")
	}
	path, err = absPath(p)
	return path, explicit, err
}

// loadConfig reads path. A missing file yields the defaults; commands that need a
// field the defaults do not cover report it themselves.
func loadConfig(path string, explicit bool) (config, error) {
	var c config
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && explicit:
		return c, fmt.Errorf("config file %s does not exist", path)
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return c, err
	default:
		defer f.Close()
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := dec.Token(); err != io.EOF {
			return c, fmt.Errorf("%s: trailing data after the config object", path)
		}
	}
	return c, c.finish()
}

// finish applies defaults, expands and checks paths, and checks the listen address.
func (c *config) finish() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&c.Listen, "127.0.0.1:8790")
	def(&c.Provider, "mnemosyne")
	def(&c.Endpoint, "http://"+c.Listen+"/mcp") // the same file serves both roles on one machine
	def(&c.StateDir, filepath.Join(home, ".local", "state", "memory-sharing-mcp"))
	def(&c.Registry, filepath.Join(c.StateDir, "keys.json"))
	def(&c.CredentialsDir, filepath.Join(home, ".config", "memory-sharing-mcp", "credentials"))
	for _, p := range []*string{&c.Mnemosyne.DataDir, &c.Mnemosyne.Python, &c.StateDir, &c.Registry, &c.CredentialsDir, &c.CredentialFile} {
		if *p == "" {
			continue
		}
		if *p, err = absPath(*p); err != nil {
			return err
		}
	}
	return checkLoopback(c.Listen)
}

// checkLoopback refuses a listen address whose host is not a loopback address.
func checkLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q: want host:port", listen)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen %q: host must be loopback (127.0.0.1, ::1, or localhost)", listen)
	}
	return nil
}

// absPath expands a leading "~/" and requires the result to be absolute.
func absPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute (or start with ~/)", p)
	}
	return filepath.Clean(p), nil
}
