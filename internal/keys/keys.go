// Package keys manages API keys. Secrets are random and never stored; the
// registry keeps only their SHA-256 hashes. A Set serves authentication from
// a snapshot that Reload swaps atomically.
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Role limits what an authenticated key may do.
type Role string

const (
	ReadOnly    Role = "read-only"
	ReadWrite   Role = "read-write"
	Maintenance Role = "maintenance"
)

// ParseRole validates a role name.
func ParseRole(s string) (Role, error) {
	switch r := Role(s); r {
	case ReadOnly, ReadWrite, Maintenance:
		return r, nil
	}
	return "", fmt.Errorf("unknown role %q", s)
}

// Key is one registry entry. Only the hash of its secret is stored.
type Key struct {
	ID      string     `json:"id"`
	Name    string     `json:"name,omitempty"`
	Role    Role       `json:"role"`
	Hash    string     `json:"hash"`
	Created time.Time  `json:"created"`
	Revoked *time.Time `json:"revoked,omitempty"`
}

type registry struct {
	Keys []Key `json:"keys"`
}

const secretPrefix = "msm_"

// Generate registers a new key and returns its secret. The secret is not
// stored and cannot be returned again.
func Generate(path, name string, role Role) (string, Key, error) {
	if _, err := ParseRole(string(role)); err != nil {
		return "", Key{}, err
	}
	var secret string
	var k Key
	err := mutate(path, func(keys []Key) ([]Key, error) {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		if secret, err = newSecret(id); err != nil {
			return nil, err
		}
		k = Key{ID: id, Name: name, Role: role, Hash: hashSecret(secret), Created: now()}
		return append(keys, k), nil
	})
	if err != nil {
		if k.ID != "" { // the key may have been committed before the failure; never leave it usable
			// ErrUnknownKey: the key was never committed, so there is nothing to revoke.
			if rerr := Revoke(path, k.ID); rerr != nil && !errors.Is(rerr, ErrUnknownKey) {
				err = errors.Join(err, fmt.Errorf("revoke %s: %w", k.ID, rerr))
			}
		}
		return "", Key{}, err
	}
	return secret, k, nil
}

// ErrUnknownKey reports a key ID that is not in the registry.
var ErrUnknownKey = errors.New("unknown key")

// Revoke marks a key revoked. Revoking an already-revoked key is a no-op.
func Revoke(path, id string) error {
	return mutate(path, func(keys []Key) ([]Key, error) {
		for i := range keys {
			if keys[i].ID != id {
				continue
			}
			if keys[i].Revoked == nil {
				t := now()
				keys[i].Revoked = &t
			}
			return keys, nil
		}
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, id)
	})
}

// List returns every registry entry, including revoked ones.
func List(path string) ([]Key, error) {
	return load(path)
}

// WriteCredential saves a client secret to a new owner-only file. It never
// overwrites an existing file.
func WriteCredential(path, secret string) error {
	if err := OwnerDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(secret + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path) // do not leave a partial credential behind
		return err
	}
	return nil
}

// Set is the server's view of the registry. Lookups are lock-free.
type Set struct {
	path   string
	snap   atomic.Pointer[map[string]Key]
	reload sync.Mutex // serializes Reload so an older file can never replace a newer one
}

// Open loads the registry at path. A missing file is an empty registry.
func Open(path string) (*Set, error) {
	s := &Set{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Authenticate returns the active key that secret belongs to. Malformed,
// unknown, mismatched, and revoked secrets all return false.
func (s *Set) Authenticate(secret string) (Key, bool) {
	id, ok := parseSecret(secret)
	if !ok {
		return Key{}, false
	}
	k, ok := s.Active(id)
	if !ok || subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(k.Hash)) != 1 {
		return Key{}, false
	}
	return k, true
}

// Active returns the key with this ID if it exists and is not revoked.
func (s *Set) Active(id string) (Key, bool) {
	k, ok := (*s.snap.Load())[id]
	if !ok || k.Revoked != nil {
		return Key{}, false
	}
	return k, true
}

// Reload replaces the snapshot with the current file. On error the previous
// snapshot stays in effect.
func (s *Set) Reload() error {
	s.reload.Lock()
	defer s.reload.Unlock()
	keys, err := load(s.path)
	if err != nil {
		return err
	}
	m := make(map[string]Key, len(keys))
	for _, k := range keys {
		m[k.ID] = k
	}
	s.snap.Store(&m)
	return nil
}

// Watch reloads whenever the registry file changes (mtime, size, mode, or inode), checking every
// interval until ctx is done. Reload failures are passed to onErr.
//
// ponytail: polls os.Stat instead of using fsnotify. Known ceiling: a revocation
// takes up to one interval to apply. Upgrade path: fsnotify, if that latency matters.
func (s *Set) Watch(ctx context.Context, interval time.Duration, onErr func(error)) {
	if onErr == nil {
		onErr = func(error) {}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	last := stamp{size: -1} // never matches, so the first tick reloads and closes any gap since Open
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur, err := statFile(s.path)
		if err != nil {
			onErr(err)
			continue
		}
		if cur == last {
			continue
		}
		if err := s.Reload(); err != nil {
			onErr(err) // retried next tick, so a later chmod-only fix is picked up
			continue
		}
		last = cur
	}
}

type stamp struct {
	mtime int64
	size  int64
	mode  fs.FileMode
	ino   uint64
}

// statFile returns the zero stamp for a missing file, which loads as empty.
func statFile(path string) (stamp, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return stamp{}, nil
	}
	if err != nil {
		return stamp{}, err
	}
	st := stamp{mtime: fi.ModTime().UnixNano(), size: fi.Size(), mode: fi.Mode()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st, nil
}

// mutate applies fn to the registry under an exclusive lock and replaces the
// file atomically.
func mutate(path string, fn func([]Key) ([]Key, error)) error {
	if err := OwnerDir(filepath.Dir(path)); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing the descriptor releases the flock
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	keys, err := load(path)
	if err != nil {
		return err
	}
	keys, err = fn(keys)
	if err != nil {
		return err
	}
	return writeRegistry(path, keys)
}

// OwnerDir creates dir 0700 if missing and refuses an existing directory that is
// accessible to group or other, or owned by another user: anyone who can write
// to it could replace or delete the registry or credentials.
func OwnerDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	return OwnerOnly(fi, "directory "+dir, 0o700)
}

// OwnerOnly refuses fi if it is accessible to group or other, or owned by another
// user. what names it in the error; want is the mode the error suggests.
func OwnerOnly(fi fs.FileInfo, what string, want fs.FileMode) error {
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s: mode %04o, want %04o", what, perm, want)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s: not owned by current user", what)
	}
	return nil
}

// load reads the registry. A missing file is empty. It refuses files that are
// accessible to group or other, or owned by another user.
func load(path string) ([]Key, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := OwnerOnly(fi, "registry "+path, 0o600); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var r registry
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("registry %s: %w", path, err)
	}
	seen := make(map[string]bool, len(r.Keys))
	for _, k := range r.Keys {
		if _, err := ParseRole(string(k.Role)); err != nil {
			return nil, fmt.Errorf("registry %s: key %s: %w", path, k.ID, err)
		}
		if seen[k.ID] {
			return nil, fmt.Errorf("registry %s: duplicate key %s", path, k.ID)
		}
		seen[k.ID] = true
	}
	return r.Keys, nil
}

// writeRegistry writes a temp file in the same directory, fsyncs it, renames it
// over path, and fsyncs the directory.
func writeRegistry(path string, keys []Key) error {
	data, err := json.MarshalIndent(registry{Keys: keys}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// newID returns "key_" plus 16 lowercase hex characters.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "key_" + hex.EncodeToString(b), nil
}

// newSecret returns "msm_" + the ID without "key_" + "_" + 32 random bytes in base64url.
func newSecret(id string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + strings.TrimPrefix(id, "key_") + "_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// parseSecret returns the key ID embedded in a secret without checking the
// registry. The ID is 16 hex characters, so the first "_" after the prefix ends it.
func parseSecret(secret string) (string, bool) {
	rest, ok := strings.CutPrefix(secret, secretPrefix)
	if !ok {
		return "", false
	}
	hexID, body, ok := strings.Cut(rest, "_")
	if !ok || len(hexID) != 16 || len(body) != 43 {
		return "", false
	}
	if _, err := hex.DecodeString(hexID); err != nil {
		return "", false
	}
	return "key_" + hexID, true
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}
