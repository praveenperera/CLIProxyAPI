package affinity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileStoreDirectoryCreationSyscallFaults(t *testing.T) {
	for _, stage := range []string{"mkdir", "parent-sync"} {
		t.Run(stage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "first", "second", "state")
			ops := localFileOperations()
			injected := errors.New("injected syscall failure")
			mkdir := ops.mkdir
			var created bool
			ops.mkdir = func(path string, mode os.FileMode) error {
				if stage == "mkdir" {
					return injected
				}
				err := mkdir(path, mode)
				created = err == nil
				return err
			}
			parent := ops.syncParent
			ops.syncParent = func(path string) error {
				if created && stage == "parent-sync" {
					return injected
				}
				return parent(path)
			}
			store, err := openFileStore(dir, DefaultRetention(), time.Now, ops)
			requireReason(t, err, StoreUnavailable)
			if store != nil {
				t.Fatal("uncertain directory admitted store")
			}
			// reopening must commit edges left behind by a failed parent sync
			retry := localFileOperations()
			var synced bool
			retry.syncParent = func(path string) error {
				if path == filepath.Dir(filepath.Join(dir, "..", "..")) {
					synced = true
				}
				return syncLocalDirectory(path)
			}
			store, err = openFileStore(dir, DefaultRetention(), time.Now, retry)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if !synced {
				t.Fatal("retry did not sync the previously created parent entry")
			}
		})
	}
}

func TestFileStoreAtomicSyscallFaultsFailClosed(t *testing.T) {
	for _, stage := range []string{"write", "short-write", "file-sync", "close", "rename", "state-sync", "prune-sync"} {
		t.Run(stage, func(t *testing.T) {
			s, dir, now := openTestStore(t, DefaultRetention())
			b := testBinding(t, testKey(t, "n", "c", "chat"), "auth", now, DefaultRetention())
			injected := errors.New("injected syscall failure")
			if stage == "prune-sync" {
				if _, _, err := s.CreateIfAbsent(context.Background(), b); err != nil {
					t.Fatal(err)
				}
				s.ops.syncPrune = func(string) error { return injected }
				requireReason(t, s.Prune(context.Background(), b.ExpiresAt()), StoreUnavailable)
			} else {
				switch stage {
				case "write":
					s.ops.write = func(*os.File, []byte) (int, error) { return 0, injected }
				case "short-write":
					s.ops.write = func(*os.File, []byte) (int, error) { return 1, nil }
				case "file-sync":
					s.ops.syncFile = func(*os.File) error { return injected }
				case "close":
					s.ops.closeFile = func(f *os.File) error { return errors.Join(f.Close(), injected) }
				case "rename":
					s.ops.rename = func(string, string) error { return injected }
				case "state-sync":
					s.ops.syncState = func(string) error { return injected }
				}
				_, admitted, err := s.CreateIfAbsent(context.Background(), b)
				requireReason(t, err, StoreUnavailable)
				if admitted {
					t.Fatal("uncertain write admitted dispatch")
				}
			}
			_, _, err := s.Lookup(context.Background(), b.key)
			requireReason(t, err, StoreUnavailable)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenFileStore(dir, DefaultRetention())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			saved, found, err := reopened.Lookup(context.Background(), b.key)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "state-sync" && (!found || saved != b) {
				t.Fatal("renamed winner lost on reopen")
			}
			if stage != "state-sync" && found {
				t.Fatal("failed pre-rename write admitted binding")
			}
		})
	}
}
