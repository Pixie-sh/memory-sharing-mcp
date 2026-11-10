package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
)

// setup writes a config whose paths all live under a fresh owner-only temp dir,
// with extra fields merged in. It returns the config path and the registry path.
func setup(t *testing.T, extra map[string]any) (cfgPath, registry string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	registry = filepath.Join(root, "state", "keys.json")
	m := map[string]any{
		"state_dir":       filepath.Join(root, "state"),
		"registry":        registry,
		"credentials_dir": filepath.Join(root, "creds"),
	}
	maps.Copy(m, extra)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, registry
}

// cli runs the command with the given config and returns the exit code and output.
func cli(t *testing.T, cfg string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(t.Context(), append([]string{"--config", cfg}, args...), nil, &out, &errb)
	return code, out.String(), errb.String()
}

func TestConfigRejectsUnknownFieldAndRelativePath(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"bogus": 1})
	if code, _, errs := cli(t, cfg, "mcp", "keys", "list"); code != 1 || !strings.Contains(errs, "unknown field") {
		t.Errorf("unknown field: code %d, stderr %q", code, errs)
	}

	cfg, _ = setup(t, map[string]any{"state_dir": "relative/state"})
	if code, _, errs := cli(t, cfg, "mcp", "keys", "list"); code != 1 || !strings.Contains(errs, "must be absolute") {
		t.Errorf("relative path: code %d, stderr %q", code, errs)
	}
}

func TestConfigRefusesNonLoopbackListen(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"listen": "0.0.0.0:8790", "mnemosyne": map[string]any{"data_dir": "/x", "python": "/x/py"}})
	if code, _, errs := cli(t, cfg, "http", "serve"); code != 1 || !strings.Contains(errs, "loopback") {
		t.Errorf("code %d, stderr %q", code, errs)
	}
}

func TestMnemosyneConfig(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"mnemosyne": map[string]any{
		"data_dir": "~/store", "python": "/py", "session": "team", "version": "9.9.9",
	}})
	c, err := loadConfig(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := mnemosyneConfig{DataDir: filepath.Join(home, "store"), Python: "/py", Session: "team", Version: "9.9.9"}
	if c.Provider != "mnemosyne" || c.Mnemosyne != want {
		t.Fatalf("provider=%q mnemosyne=%+v, want %+v", c.Provider, c.Mnemosyne, want)
	}
	if _, store, err := openProvider(c, io.Discard, false); err != nil || store != filepath.Join(want.DataDir, "mnemosyne.db") {
		t.Fatalf("store=%q err=%v", store, err)
	}
	cfg, _ = setup(t, map[string]any{"mnemosyne": map[string]any{"data_dir": "/x", "bogus": 1}})
	if _, err := loadConfig(cfg, true); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown nested field: err = %v", err)
	}
}

func TestEndpointFollowsListen(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"listen": "127.0.0.1:9999"})
	c, err := loadConfig(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "http://127.0.0.1:9999/mcp" {
		t.Fatalf("endpoint = %q, want it derived from listen", c.Endpoint)
	}
	cfg, _ = setup(t, map[string]any{"listen": "127.0.0.1:9999", "endpoint": "https://memory.example/mcp"})
	if c, err = loadConfig(cfg, true); err != nil || c.Endpoint != "https://memory.example/mcp" {
		t.Fatalf("explicit endpoint = %q err=%v", c.Endpoint, err)
	}
}

func TestUnknownProvider(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"provider": "nope"})
	if code, _, errs := cli(t, cfg, "http", "serve"); code != 1 || !strings.Contains(errs, `unknown provider "nope"`) {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errs strings.Builder
	if code := run(t.Context(), []string{"version"}, strings.NewReader(""), &out, &errs); code != 0 || out.String() != version+"\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errs.String())
	}
}

func TestStoreInitRefusesExistingStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mnemosyne.db"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := setup(t, map[string]any{"mnemosyne": map[string]any{"data_dir": dir, "python": "/nonexistent/py"}})
	if code, _, errs := cli(t, cfg, "store", "init"); code != 1 || !strings.Contains(errs, "already exists") {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestHTTPServeNeedsProviderConfig(t *testing.T) {
	cfg, _ := setup(t, nil)
	if code, _, errs := cli(t, cfg, "http", "serve"); code != 1 || !strings.Contains(errs, "needs mnemosyne.data_dir and mnemosyne.python") {
		t.Errorf("code %d, stderr %q", code, errs)
	}
}

func TestKeygenPrintWritesOnlySecret(t *testing.T) {
	cfg, registry := setup(t, nil)
	code, out, errs := cli(t, cfg, "mcp", "keygen", "--print", "--name", "laptop", "--role", "read-only")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errs)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "msm_") {
		t.Fatalf("stdout %q, want one line holding the secret", out)
	}
	if strings.Contains(errs, strings.TrimSpace(out)) {
		t.Fatal("secret also written to stderr")
	}
	ks, err := keys.Open(registry)
	if err != nil {
		t.Fatal(err)
	}
	k, ok := ks.Authenticate(strings.TrimSpace(out))
	if !ok || k.Role != keys.ReadOnly || k.Name != "laptop" {
		t.Fatalf("secret does not authenticate as the new read-only key: ok=%v key=%+v", ok, k)
	}
}

func TestKeygenSavesCredentialFiles(t *testing.T) {
	cfg, registry := setup(t, nil)
	var paths []string
	for range 2 {
		code, out, errs := cli(t, cfg, "mcp", "keygen")
		if code != 0 {
			t.Fatalf("code %d, stderr %q", code, errs)
		}
		path := strings.TrimSpace(out)
		if strings.Contains(out, "msm_") || strings.Count(out, "\n") != 1 {
			t.Fatalf("stdout %q should be only the path", out)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("credential mode %04o, want 0600", fi.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := keys.Open(registry)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ks.Authenticate(strings.TrimSpace(string(b))); !ok {
			t.Fatal("credential file does not authenticate")
		}
		paths = append(paths, path)
	}
	if paths[0] == paths[1] {
		t.Fatalf("two keygen runs returned the same path %s", paths[0])
	}
}

func TestKeysListRevoke(t *testing.T) {
	cfg, registry := setup(t, nil)
	_, out, _ := cli(t, cfg, "mcp", "keygen", "--print", "--name", "ci")
	secret := strings.TrimSpace(out)

	all, err := keys.List(registry)
	if err != nil || len(all) != 1 {
		t.Fatalf("registry: %v, %d keys", err, len(all))
	}
	id, hash := all[0].ID, all[0].Hash

	code, out, errs := cli(t, cfg, "mcp", "keys", "list")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "active") {
		t.Fatalf("list: code %d, stdout %q, stderr %q", code, out, errs)
	}
	if strings.Contains(out, hash) {
		t.Fatal("list output contains the key hash")
	}

	if code, _, errs := cli(t, cfg, "mcp", "keys", "revoke", id); code != 0 {
		t.Fatalf("revoke: code %d, stderr %q", code, errs)
	}
	_, out, _ = cli(t, cfg, "mcp", "keys", "list")
	if !strings.Contains(out, "revoked") {
		t.Fatalf("list after revoke: %q", out)
	}
	ks, err := keys.Open(registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ks.Authenticate(secret); ok {
		t.Fatal("revoked secret still authenticates")
	}
}

func TestCredentialFileMustBePrivate(t *testing.T) {
	t.Setenv("MSM_API_KEY", "")
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("msm_x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config{CredentialFile: p}
	if _, err := credential(cfg); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("group-readable credential: err = %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := credential(cfg); err != nil || s != "msm_x" {
		t.Fatalf("0600 credential: %q, %v", s, err)
	}
}

func TestUsageErrors(t *testing.T) {
	cfg, _ := setup(t, nil)
	if code, _, _ := cli(t, cfg, "frobnicate", "now"); code != 2 {
		t.Errorf("unknown command: code %d, want 2", code)
	}
	if code, _, _ := cli(t, cfg, "install", "extra"); code != 2 {
		t.Errorf("install with argument: code %d, want 2", code)
	}
}

func TestInstallRefusesMissingStore(t *testing.T) {
	cfg, _ := setup(t, map[string]any{"mnemosyne": map[string]any{"data_dir": t.TempDir(), "python": "/py"}})
	want := "store init"
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		want = "macOS"
	}
	if code, _, errs := cli(t, cfg, "install", "--bin-dir", t.TempDir()); code != 1 || !strings.Contains(errs, want) {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
}

func TestPlistEscapesAndParses(t *testing.T) {
	x := plistXML("/a b/msm", "/cfg/<&>.json", "/log")
	if !strings.Contains(x, "<string>/cfg/&lt;&amp;&gt;.json</string>") {
		t.Fatalf("config path not escaped:\n%s", x)
	}
	d := xml.NewDecoder(strings.NewReader(x))
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
	}
	if p, err := exec.LookPath("plutil"); err == nil {
		f := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(f, []byte(x), 0o600)
		if out, err := exec.Command(p, "-lint", f).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v %s", err, out)
		}
	}
}

func TestSystemdUnitQuotes(t *testing.T) {
	u := systemdUnit("/a b/msm", `/cfg/"x"%$.json`, "/log/100%.log")
	for _, want := range []string{
		`ExecStart="/a b/msm" "--config" "/cfg/\"x\"%%$$.json" "http" "serve"`,
		"StandardOutput=append:/log/100%%.log",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if p, err := exec.LookPath("systemd-analyze"); err == nil {
		f := filepath.Join(t.TempDir(), "x.service")
		os.WriteFile(f, []byte(systemdUnit("/bin/true", "/cfg.json", filepath.Join(t.TempDir(), "log"))), 0o600)
		if out, err := exec.Command(p, "verify", f).CombinedOutput(); err != nil {
			t.Fatalf("systemd-analyze: %v %s", err, out)
		}
	}
}

func TestExplicitMissingConfigFails(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(t.Context(), []string{"--config", filepath.Join(t.TempDir(), "missing.json"), "mcp", "keys", "list"}, strings.NewReader(""), &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "does not exist") {
		t.Fatalf("code %d, stderr %q; want failure naming the missing file", code, errb.String())
	}
}
