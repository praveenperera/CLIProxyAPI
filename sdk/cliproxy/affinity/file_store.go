package affinity

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const maxRecordBytes = 4096

type diskRecord struct {
	SchemaVersion uint32
	KeyDigest     [32]byte
	Handle        [32]byte
	Provider      string
	AuthID        string
	LineageDigest [32]byte
	CreatedAt     time.Time
	TouchedAt     time.Time
	ExpiresAt     time.Time
}

// FileStore holds a lifetime exclusive lock on a private local durable directory
// The host must keep this directory outside token watchers and on a local durable volume
// An uncertain write poisons the instance until it is closed and reopened
type FileStore struct {
	mu               sync.Mutex
	dir              string
	lock             *flock.Flock
	directoryInfo    os.FileInfo
	lockInfo         os.FileInfo
	retention        Retention
	key              [32]byte
	now              func() time.Time
	closed, poisoned bool
	ops              fileOperations
}

// OpenFileStore locks and validates a private absolute state directory
// It rejects all unsupported or corrupt records before returning an instance
func OpenFileStore(dir string, retention Retention) (*FileStore, error) {
	return OpenFileStoreWithClock(dir, retention, time.Now)
}

// OpenFileStoreWithClock shares the host retention clock with atomic store admission
// The clock must be non-nil and safe for concurrent calls
func OpenFileStoreWithClock(dir string, retention Retention, now func() time.Time) (*FileStore, error) {
	return openFileStore(dir, retention, now, localFileOperations())
}

func openFileStore(dir string, retention Retention, now func() time.Time, ops fileOperations) (*FileStore, error) {
	if now == nil {
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || retention.idle <= 0 || retention.hard < retention.idle || retention.capacity < 1 || retention.capacity > 65536 {
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	if err := ops.createDirectory(dir); err != nil {
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	if err := privatePath(dir, true); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".lock")
	if info, err := os.Lstat(lockPath); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, NewError(StoreUnavailable, time.Time{})
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	acquired, err := lock.TryLock()
	if err != nil || !acquired {
		_ = lock.Unlock()
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	s := &FileStore{dir: dir, lock: lock, retention: retention, now: now, ops: ops}
	if err := s.initialize(); err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	var errStat error
	s.directoryInfo, errStat = os.Lstat(dir)
	if errStat == nil {
		s.lockInfo, errStat = os.Lstat(lockPath)
	}
	if errStat != nil || privatePath(lockPath, false) != nil {
		_ = lock.Unlock()
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	return s, nil
}

func privatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return NewError(StoreUnavailable, time.Time{})
	}
	return nil
}

func (s *FileStore) initialize() error {
	entries, err := readEntries(s.dir)
	if err != nil {
		return NewError(StoreUnavailable, time.Time{})
	}
	if len(entries) > 65539 {
		return NewError(StoreUnavailable, time.Time{})
	}
	bindings := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" || name == ".deployment-key" {
			continue
		}
		if strings.HasPrefix(name, ".pending-") {
			if err := privatePath(filepath.Join(s.dir, name), false); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
				return NewError(StoreUnavailable, time.Time{})
			}
			continue
		}
		if !strings.HasSuffix(name, ".binding") {
			return NewError(StoreUnavailable, time.Time{})
		}
		if _, err := s.read(name); err != nil {
			return err
		}
		bindings++
	}
	keyPath := filepath.Join(s.dir, ".deployment-key")
	data, err := readKeyFile(keyPath)
	if errors.Is(err, os.ErrNotExist) && bindings == 0 {
		if _, err := rand.Read(s.key[:]); err != nil {
			return NewError(StoreUnavailable, time.Time{})
		}
		return s.atomicWrite(".deployment-key", s.key[:])
	}
	if err != nil || len(data) != 32 {
		return NewError(StoreUnavailable, time.Time{})
	}
	if err := privatePath(keyPath, false); err != nil {
		return err
	}
	copy(s.key[:], data)
	if s.key == ([32]byte{}) {
		return NewError(StoreUnavailable, time.Time{})
	}
	return s.syncDirectory()
}

func recordName(key ChatKey) string { return hex.EncodeToString(key.digest[:]) + ".binding" }
func (s *FileStore) read(name string) (Binding, error) {
	path := filepath.Join(s.dir, name)
	if err := privatePath(path, false); err != nil {
		return Binding{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxRecordBytes+1))
	errClose := file.Close()
	if errRead != nil || errClose != nil || len(data) > maxRecordBytes {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	var record diskRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	if err := decoder.Decode(new(any)); err != io.EOF || record.SchemaVersion != SchemaVersion {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	ref, err := NewCredentialRef(record.Provider, record.AuthID, record.LineageDigest)
	if err != nil {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	binding, err := NewBinding(ChatKey{record.KeyDigest}, Handle{record.Handle}, ref, record.CreatedAt, record.TouchedAt, record.ExpiresAt)
	if err != nil || recordName(binding.key) != name || !binding.expires.Equal(s.retention.Expiration(binding.created, binding.touched)) {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	return binding, nil
}

func (s *FileStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed || s.poisoned {
		return NewError(StoreUnavailable, time.Time{})
	}
	if err := privatePath(s.dir, true); err != nil {
		s.poisoned = true
		return err
	}
	info, errDir := os.Lstat(s.dir)
	lockInfo, errLock := os.Lstat(filepath.Join(s.dir, ".lock"))
	data, errKey := readKeyFile(filepath.Join(s.dir, ".deployment-key"))
	if errDir != nil || errLock != nil || errKey != nil || !os.SameFile(info, s.directoryInfo) || !os.SameFile(lockInfo, s.lockInfo) || privatePath(filepath.Join(s.dir, ".lock"), false) != nil || !hmac.Equal(data, s.key[:]) {
		// an unlinked lock or replaced directory no longer protects this store's writes
		s.poisoned = true
		return NewError(StoreUnavailable, time.Time{})
	}
	return nil
}

// Lookup validates a durable record; expired records remain distinguishable until prune
func (s *FileStore) Lookup(ctx context.Context, key ChatKey) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return Binding{}, false, err
	}
	return s.lookup(key)
}
func (s *FileStore) lookup(key ChatKey) (Binding, bool, error) {
	if key.digest == ([32]byte{}) {
		return Binding{}, false, NewError(IdentityInvalid, time.Time{})
	}
	if _, err := os.Lstat(filepath.Join(s.dir, recordName(key))); errors.Is(err, os.ErrNotExist) {
		return Binding{}, false, nil
	}
	b, err := s.read(recordName(key))
	if err != nil {
		s.poisoned = true
		return Binding{}, false, err
	}
	return b, true, nil
}

// CreateIfAbsent returns the saved winner without changing its handle or credential
// It prunes only expired records and commits the new record before it returns
func (s *FileStore) CreateIfAbsent(ctx context.Context, b Binding) (Binding, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return Binding{}, false, err
	}
	now := s.now().UTC()
	if _, err := NewBinding(b.key, b.handle, b.ref, b.created, b.touched, b.expires); err != nil || !b.expires.Equal(s.retention.Expiration(b.created, b.touched)) || b.created.After(now) || !b.expires.After(now) {
		return Binding{}, false, NewError(IdentityInvalid, time.Time{})
	}
	winner, found, err := s.lookup(b.key)
	if err != nil {
		return Binding{}, false, err
	}
	if found && winner.expires.After(now) {
		return winner, false, nil
	}
	count, err := s.prune(now)
	if err != nil {
		return Binding{}, false, err
	}
	if count >= s.retention.capacity {
		return Binding{}, false, NewError(StoreFull, time.Time{})
	}
	if err := s.write(b); err != nil {
		return Binding{}, false, err
	}
	return b, true, nil
}

// Touch verifies the handle and preserves the exact credential and hard-retention origin
func (s *FileStore) Touch(ctx context.Context, key ChatKey, handle Handle, now time.Time) (Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return Binding{}, err
	}
	b, found, err := s.lookup(key)
	if err != nil {
		return Binding{}, err
	}
	if !found {
		return Binding{}, NewError(BindingMissing, time.Time{})
	}
	if !hmac.Equal(b.handle.value[:], handle.value[:]) {
		return Binding{}, NewError(BindingConflict, time.Time{})
	}
	if now.IsZero() || now.Before(b.touched) {
		return Binding{}, NewError(IdentityInvalid, time.Time{})
	}
	if !b.expires.After(now) {
		return Binding{}, NewError(BindingExpired, time.Time{})
	}
	next, err := NewBinding(key, handle, b.ref, b.created, now, s.retention.Expiration(b.created, now))
	if err != nil {
		return Binding{}, NewError(StoreUnavailable, time.Time{})
	}
	if err := s.write(next); err != nil {
		return Binding{}, err
	}
	return next, nil
}

func (s *FileStore) write(b Binding) error {
	record := diskRecord{SchemaVersion, b.key.digest, b.handle.value, b.ref.provider, b.ref.authID, b.ref.lineage, b.created, b.touched, b.expires}
	data, err := json.Marshal(record)
	if err == nil {
		err = s.atomicWrite(recordName(b.key), data)
	}
	if err != nil {
		s.poisoned = true
		return NewError(StoreUnavailable, time.Time{})
	}
	return nil
}

func (s *FileStore) atomicWrite(name string, data []byte) (err error) {
	file, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		if file != nil {
			if errClose := file.Close(); err == nil {
				err = errClose
			}
		}
		if errRemove := os.Remove(path); err == nil && !errors.Is(errRemove, os.ErrNotExist) {
			err = errRemove
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	n, errWrite := s.ops.write(file, data)
	if errWrite != nil {
		return errWrite
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err = s.ops.syncFile(file); err != nil {
		return err
	}
	if err = s.ops.closeFile(file); err != nil {
		file = nil
		return err
	}
	file = nil
	if err = s.ops.rename(path, filepath.Join(s.dir, name)); err != nil {
		return err
	}
	return s.syncDirectory()
}
func (s *FileStore) syncDirectory() error { return s.ops.syncState(s.dir) }

// Prune deletes expired records only and synchronizes the directory before return
func (s *FileStore) Prune(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	if now.IsZero() {
		return NewError(IdentityInvalid, time.Time{})
	}
	_, err := s.prune(now)
	return err
}
func (s *FileStore) prune(now time.Time) (int, error) {
	entries, err := readEntries(s.dir)
	if err != nil {
		s.poisoned = true
		return 0, NewError(StoreUnavailable, time.Time{})
	}
	live := 0
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" || name == ".deployment-key" {
			continue
		}
		b, err := s.read(name)
		if err != nil {
			s.poisoned = true
			return 0, err
		}
		if b.expires.After(now) {
			live++
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			s.poisoned = true
			return 0, NewError(StoreUnavailable, time.Time{})
		}
		changed = true
	}
	if changed {
		if err := s.ops.syncPrune(s.dir); err != nil {
			s.poisoned = true
			return 0, NewError(StoreUnavailable, time.Time{})
		}
	}
	return live, nil
}

// APIKeyLineage computes a deployment-key HMAC of exact key and origin bytes
// It neither changes identity nor claims a verified provider organization
func (s *FileStore) APIKeyLineage(ctx context.Context, key, origin string) ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return [32]byte{}, err
	}
	if key == "" || origin == "" || len(key) > 16384 || len(origin) > 2048 {
		return [32]byte{}, NewError(IdentityInvalid, time.Time{})
	}
	// length-delimited parts prevent alternate key/origin splits from sharing a digest
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write([]byte("cliproxy/api-key-lineage/v1"))
	for _, part := range []string{key, origin} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		mac.Write(size[:])
		mac.Write([]byte(part))
	}
	var digest [32]byte
	copy(digest[:], mac.Sum(nil))
	return digest, nil
}

// Close releases the process lock; all later operations fail closed
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Unlock()
}

func readKeyFile(path string) ([]byte, error) {
	if err := privatePath(path, false); err != nil {
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, errRead := io.ReadAll(io.LimitReader(file, 33))
	errClose := file.Close()
	return data, errors.Join(errRead, errClose)
}

func readEntries(path string) ([]os.DirEntry, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	entries, errRead := dir.ReadDir(65540)
	errClose := dir.Close()
	if errors.Is(errRead, io.EOF) {
		errRead = nil
	}
	if len(entries) > 65539 {
		return nil, NewError(StoreUnavailable, time.Time{})
	}
	return entries, errors.Join(errRead, errClose)
}
