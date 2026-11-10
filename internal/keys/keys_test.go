package keys

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// registryPath returns a path in a fresh owner-only directory (t.TempDir is 0755).
func registryPath(t *testing.T) string {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "keys.json")
}

func TestAuthenticate(t *testing.T) {
	path := registryPath(t)
	secret, k, err := Generate(path, "laptop", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	got, ok := s.Authenticate(secret)
	if !ok || got.ID != k.ID || got.Name != "laptop" || got.Role != ReadOnly {
		t.Fatalf("Authenticate(valid) = %+v, %v", got, ok)
	}

	rejected := map[string]string{
		"wrong secret": secret[:21] + other[21:], // real ID, other key's random part
		"empty":        "",
		"no body":      "msm_" + k.ID[4:] + "_",
		"truncated":    secret[:len(secret)-1],
		"wrong prefix": "key_" + secret[4:],
		"unknown ID":   "msm_0000000000000000_" + strings.Repeat("A", 43),
	}
	for name, bad := range rejected {
		if _, ok := s.Authenticate(bad); ok {
			t.Errorf("%s: authenticated", name)
		}
	}
}

func TestRevokeRejectsAfterReload(t *testing.T) {
	path := registryPath(t)
	secret, k, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Revoke(path, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Authenticate(secret); ok {
		t.Error("revoked key authenticated")
	}
	if _, ok := s.Active(k.ID); ok {
		t.Error("revoked key is active")
	}
	if err := Revoke(path, k.ID); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	if err := Revoke(path, "key_0000000000000000"); err == nil {
		t.Error("revoke of unknown ID succeeded")
	}
}

func TestReloadKeepsSnapshotOnCorruptFile(t *testing.T) {
	path := registryPath(t)
	secret, _, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("Reload accepted corrupt JSON")
	}
	if _, ok := s.Authenticate(secret); !ok {
		t.Error("previous snapshot lost after failed reload")
	}
}

func TestOpenRejectsLoosePermissions(t *testing.T) {
	path := registryPath(t)
	if _, _, err := Generate(path, "", ReadWrite); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open accepted a 0644 registry")
	}
}

func TestConcurrentGenerate(t *testing.T) {
	path := registryPath(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := Generate(path, fmt.Sprint(i), ReadWrite)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	list, err := List(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != n {
		t.Fatalf("List = %d keys, want %d", len(list), n)
	}
}

func TestWriteCredentialNeverOverwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "creds", "client.key")
	if err := WriteCredential(p, "msm_one"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credential mode %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("credential dir mode %o, want 0700", di.Mode().Perm())
	}
	if err := WriteCredential(p, "msm_two"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("overwrite err = %v, want fs.ErrExist", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "msm_one\n" {
		t.Errorf("credential content = %q", b)
	}
}

func TestWatchPicksUpRevocation(t *testing.T) {
	path := registryPath(t)
	secret, k, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Watch(ctx, 10*time.Millisecond, func(err error) {
			t.Errorf("Watch reload error: %v", err)
		})
	}()

	if err := Revoke(path, k.ID); err != nil {
		t.Fatal(err)
	}
	revoked := false
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if _, ok := s.Active(k.ID); !ok {
			revoked = true
			break
		}
	}
	cancel()
	<-done
	if !revoked {
		t.Fatal("Watch did not apply the revocation within 2s")
	}
	if _, ok := s.Authenticate(secret); ok {
		t.Error("revoked key authenticated after Watch")
	}
}

// watch runs s.Watch until the test ends, ignoring reload errors.
func watch(t *testing.T, s *Set) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Watch(ctx, 10*time.Millisecond, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("not within 2s: %s", what)
}

func TestWatchDeletedBeforeFirstTick(t *testing.T) {
	path := registryPath(t)
	_, k, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	watch(t, s)
	eventually(t, "deleted registry clears keys", func() bool { _, ok := s.Active(k.ID); return !ok })
}

func TestWatchRetriesAfterChmodFix(t *testing.T) {
	path := registryPath(t)
	_, k, err := Generate(path, "", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	watch(t, s)
	// Revoke, then loosen the mode so the reload fails; fixing only the mode must still apply the revocation.
	if err := Revoke(path, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "revocation applied after chmod fix", func() bool { _, ok := s.Active(k.ID); return !ok })
}

func TestRejectsLooseDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Generate(filepath.Join(dir, "keys.json"), "", ReadWrite); err == nil {
		t.Error("Generate accepted a group/other-accessible directory")
	}
	if err := WriteCredential(filepath.Join(dir, "c.key"), "x"); err == nil {
		t.Error("WriteCredential accepted a group/other-accessible directory")
	}
}
