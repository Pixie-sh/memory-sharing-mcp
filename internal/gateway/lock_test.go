package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireLock(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	db := filepath.Join(dir, "mnemosyne.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}

	release, err := AcquireLock(state, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{db, link} {
		if _, err := AcquireLock(state, p); err == nil || !strings.Contains(err.Error(), "already owned") {
			t.Fatalf("second lock via %s: err = %v, want already owned", p, err)
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release, err = AcquireLock(state, link)
	if err != nil {
		t.Fatalf("after release, via symlink: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireLockMissingDB(t *testing.T) {
	dir := t.TempDir()
	if _, err := AcquireLock(filepath.Join(dir, "state"), filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("missing database: no error")
	}
}

func TestAcquireLockIsPerDatabase(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ra, err := AcquireLock(state, a)
	if err != nil {
		t.Fatal(err)
	}
	defer ra()
	rb, err := AcquireLock(state, b)
	if err != nil {
		t.Fatalf("second database blocked by the first: %v", err)
	}
	rb()
}

func TestAcquireLockRefusesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	db := filepath.Join(dir, "mnemosyne.db")
	if err := os.WriteFile(db, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(db)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canon))
	if err := os.MkdirAll(filepath.Join(state, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(db, filepath.Join(state, "locks", hex.EncodeToString(sum[:])+".lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(state, db); err == nil {
		t.Fatal("lock followed a planted symlink")
	}
	if data, _ := os.ReadFile(db); string(data) != "data" {
		t.Fatalf("database modified: %q", data)
	}
}
