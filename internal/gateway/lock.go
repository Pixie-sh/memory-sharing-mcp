package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
)

// AcquireLock takes the exclusive owner lock for the database at dbPath. The lock
// is keyed by the resolved path, so a symlink names the same store. The lock file
// lives under stateDir; the database itself is never written. release unlocks and
// closes the file.
func AcquireLock(stateDir, dbPath string) (release func() error, err error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(canon))
	dir := filepath.Join(stateDir, "locks")
	for _, d := range []string{stateDir, dir} {
		if err := keys.OwnerDir(d); err != nil {
			return nil, err
		}
	}
	// O_NOFOLLOW: a planted symlink must not make us lock and truncate another file.
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("lock file %s is not a regular file", f.Name())
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("database %s already owned by another gateway", canon)
		}
		return nil, err
	}
	if err := writePID(f); err != nil {
		f.Close() // closing releases the flock
		return nil, err
	}
	return func() error {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return errors.Join(err, f.Close())
	}, nil
}

// writePID records the owner's PID for diagnostics.
func writePID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := fmt.Fprintf(f, "%d\n", os.Getpid())
	return err
}
