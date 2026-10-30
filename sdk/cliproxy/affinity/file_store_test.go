package affinity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, retention Retention) (*FileStore, string, time.Time) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := OpenFileStore(dir, retention)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 10, 3, 17, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, dir, now
}
func requireReason(t *testing.T, err error, reason UnavailableReason) {
	t.Helper()
	var local *Error
	if !errors.As(err, &local) || local.Reason() != reason {
		t.Fatalf("got %v, want reason %d", err, reason)
	}
}
func TestFileStoreConcurrentWinnerRestartAndCallerIsolation(t *testing.T) {
	ctx := context.Background()
	retention := DefaultRetention()
	s, dir, now := openTestStore(t, retention)
	key := testKey(t, "deployment", "caller", "chat")
	const count = 64
	candidates := make([]Binding, count)
	for i := range candidates {
		candidates[i] = testBinding(t, key, "auth-"+strconv.Itoa(i), now, retention)
	}
	winners := make(chan Binding, count)
	errs := make(chan error, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, candidate := range candidates {
		workers.Add(1)
		go func(b Binding) {
			defer workers.Done()
			<-start
			winner, _, err := s.CreateIfAbsent(ctx, b)
			winners <- winner
			errs <- err
		}(candidate)
	}
	close(start)
	workers.Wait()
	close(winners)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var saved Binding
	for winner := range winners {
		if saved == (Binding{}) {
			saved = winner
		}
		if saved != winner {
			t.Fatal("multiple durable winners")
		}
	}
	secondKey := testKey(t, "deployment", "another caller", "chat")
	if _, created, err := s.CreateIfAbsent(ctx, testBinding(t, secondKey, "other", now, retention)); err != nil || !created {
		t.Fatal("caller isolation", err)
	}
	if _, err := OpenFileStore(dir, retention); err == nil {
		t.Fatal("second process lock accepted")
	}
	before, err := s.APIKeyLineage(ctx, "secret", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(dir, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, found, err := reopened.Lookup(ctx, key)
	if err != nil || !found || got != saved {
		t.Fatal("restart changed binding", err)
	}
	after, err := reopened.APIKeyLineage(ctx, "secret", "origin")
	if err != nil || before != after {
		t.Fatal("restart changed deployment key")
	}
	changed, _ := reopened.APIKeyLineage(ctx, "secret2", "origin")
	if changed == after {
		t.Fatal("key replacement has same lineage")
	}
	changed, _ = reopened.APIKeyLineage(ctx, "secret", "origin2")
	if changed == after {
		t.Fatal("origin replacement has same lineage")
	}
	for _, path := range []string{dir, filepath.Join(dir, ".lock"), filepath.Join(dir, ".deployment-key"), filepath.Join(dir, recordName(key))} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatal("state not private", path, err)
		}
	}
}
func TestFileStoreCapacityTouchHardRetentionAndPrune(t *testing.T) {
	ctx := context.Background()
	retention, err := NewRetention(time.Hour, 2*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, _, now := openTestStore(t, retention)
	key := testKey(t, "n", "caller", "first")
	b := testBinding(t, key, "first-auth", now, retention)
	if _, _, err := s.CreateIfAbsent(ctx, b); err != nil {
		t.Fatal(err)
	}
	other := testBinding(t, testKey(t, "n", "caller", "second"), "second-auth", now, retention)
	_, _, err = s.CreateIfAbsent(ctx, other)
	requireReason(t, err, StoreFull)
	if _, found, err := s.Lookup(ctx, key); err != nil || !found {
		t.Fatal("capacity evicted live binding")
	}
	wrong, err := NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Touch(ctx, key, wrong, now.Add(time.Minute))
	requireReason(t, err, BindingConflict)
	touched, err := s.Touch(ctx, key, b.handle, now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if touched.ref != b.ref || touched.handle != b.handle || !touched.created.Equal(b.created) || !touched.expires.Equal(now.Add(90*time.Minute)) {
		t.Fatal("touch changed assignment")
	}
	touched, err = s.Touch(ctx, key, b.handle, now.Add(89*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !touched.expires.Equal(now.Add(2 * time.Hour)) {
		t.Fatal("hard retention not enforced")
	}
	_, err = s.Touch(ctx, key, b.handle, now.Add(2*time.Hour))
	requireReason(t, err, BindingExpired)
	if err := s.Prune(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Touch(ctx, key, b.handle, now.Add(2*time.Hour))
	requireReason(t, err, BindingMissing)
}
func TestFileStoreFailurePoisonsInstanceAndPreventsAdmission(t *testing.T) {
	ctx := context.Background()
	s, _, now := openTestStore(t, DefaultRetention())
	b := testBinding(t, testKey(t, "n", "c", "id"), "auth", now, DefaultRetention())
	s.ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("injected write failure") }
	_, created, err := s.CreateIfAbsent(ctx, b)
	requireReason(t, err, StoreUnavailable)
	if created {
		t.Fatal("failed commit admitted binding")
	}
	_, _, err = s.Lookup(ctx, b.key)
	requireReason(t, err, StoreUnavailable)
}
func TestFileStoreUncertainCommitFailsClosedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	s, dir, now := openTestStore(t, DefaultRetention())
	b := testBinding(t, testKey(t, "n", "c", "id"), "auth", now, DefaultRetention())
	s.ops.syncState = func(path string) error {
		if err := syncLocalDirectory(path); err != nil {
			return err
		}
		return errors.New("injected error after state sync")
	}
	_, created, err := s.CreateIfAbsent(ctx, b)
	requireReason(t, err, StoreUnavailable)
	if created {
		t.Fatal("uncertain commit admitted dispatch")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(dir, DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, found, err := reopened.Lookup(ctx, b.key)
	if err != nil || !found || saved != b {
		t.Fatal("restart lost uncertain durable winner", err)
	}
}
func TestFileStoreCorruptAndFutureSchemaFailClosed(t *testing.T) {
	for _, kind := range []string{"future", "truncated", "unknown field", "oversize", "wrong key", "permissions", "missing deployment key"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s, dir, now := openTestStore(t, DefaultRetention())
			b := testBinding(t, testKey(t, "n", "c", "id"), "auth", now, DefaultRetention())
			if _, _, err := s.CreateIfAbsent(ctx, b); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, recordName(b.key))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "future":
				var r diskRecord
				if err := json.Unmarshal(data, &r); err != nil {
					t.Fatal(err)
				}
				r.SchemaVersion++
				data, _ = json.Marshal(r)
			case "truncated":
				data = data[:len(data)/2]
			case "unknown field":
				data = append(data[:len(data)-1], []byte(`,"unexpected":true}`)...)
			case "oversize":
				data = make([]byte, maxRecordBytes+1)
			case "wrong key":
				var r diskRecord
				_ = json.Unmarshal(data, &r)
				r.KeyDigest[0]++
				data, _ = json.Marshal(r)
			case "permissions":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "missing deployment key":
				if err := os.Remove(filepath.Join(dir, ".deployment-key")); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "permissions" && kind != "missing deployment key" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := OpenFileStore(dir, DefaultRetention())
			if err == nil {
				_ = reopened.Close()
				t.Fatal("invalid state accepted")
			}
			requireReason(t, err, StoreUnavailable)
		})
	}
}
func TestFileStoreCancelledAndClosed(t *testing.T) {
	s, _, _ := openTestStore(t, DefaultRetention())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.Lookup(ctx, testKey(t, "n", "c", "id"))
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Lookup(context.Background(), testKey(t, "n", "c", "id"))
	requireReason(t, err, StoreUnavailable)
}

func TestFileStoreDetectsDirectoryLockAndKeyReplacement(t *testing.T) {
	for _, kind := range []string{"directory", "lock", "key"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, _ := openTestStore(t, DefaultRetention())
			switch kind {
			case "directory":
				if err := os.Rename(dir, dir+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "lock":
				path := filepath.Join(dir, ".lock")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "key":
				if err := os.WriteFile(filepath.Join(dir, ".deployment-key"), make([]byte, 32), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := s.Lookup(context.Background(), testKey(t, "n", "c", "id"))
			requireReason(t, err, StoreUnavailable)
		})
	}
}

func TestFileStoreRejectsUnsafePathsAndRuntimeCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenFileStore(dir, DefaultRetention()); err == nil {
		s.Close()
		t.Fatal("public state directory accepted")
	}
	target := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenFileStore(link, DefaultRetention()); err == nil {
		s.Close()
		t.Fatal("symlink state directory accepted")
	}
	s, dir, at := openTestStore(t, DefaultRetention())
	b := testBinding(t, testKey(t, "n", "c", "id"), "auth", at, DefaultRetention())
	if _, _, err := s.CreateIfAbsent(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recordName(b.key)), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Lookup(context.Background(), b.key)
	requireReason(t, err, StoreUnavailable)
	_, _, err = s.Lookup(context.Background(), testKey(t, "n", "c", "other"))
	requireReason(t, err, StoreUnavailable)
}

func TestFileStoreTouchFailureRetainsDurableAssignment(t *testing.T) {
	ctx := context.Background()
	s, dir, at := openTestStore(t, DefaultRetention())
	b := testBinding(t, testKey(t, "n", "c", "id"), "auth", at, DefaultRetention())
	if _, _, err := s.CreateIfAbsent(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.ops.write = func(*os.File, []byte) (int, error) { return 0, errors.New("touch write failed") }
	_, err := s.Touch(ctx, b.key, b.handle, at.Add(time.Minute))
	requireReason(t, err, StoreUnavailable)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(dir, DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, found, err := reopened.Lookup(ctx, b.key)
	if err != nil || !found || saved != b {
		t.Fatal("failed touch changed durable credential", err)
	}
}

func TestFileStoreProcessLock(t *testing.T) {
	if dir := os.Getenv("CLIPROXY_AFFINITY_LOCK_TEST_DIR"); dir != "" {
		store, err := OpenFileStore(dir, DefaultRetention())
		if os.Getenv("CLIPROXY_AFFINITY_LOCK_EXPECT") == "locked" {
			if err == nil {
				_ = store.Close()
				t.Fatal("child process acquired parent lock")
			}
			requireReason(t, err, StoreUnavailable)
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	store, dir, _ := openTestStore(t, DefaultRetention())
	child := func(expect string) {
		t.Helper()
		command := exec.Command(os.Args[0], "-test.run=^TestFileStoreProcessLock$")
		command.Env = append(os.Environ(), "CLIPROXY_AFFINITY_LOCK_TEST_DIR="+dir, "CLIPROXY_AFFINITY_LOCK_EXPECT="+expect)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("child process: %v\n%s", err, output)
		}
	}
	child("locked")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	child("available")
}
