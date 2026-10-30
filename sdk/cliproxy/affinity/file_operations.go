package affinity

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// File operations keep fault injection at the actual syscall owner, not the transaction
type fileOperations struct {
	mkdir      func(string, os.FileMode) error
	syncParent func(string) error
	write      func(*os.File, []byte) (int, error)
	syncFile   func(*os.File) error
	closeFile  func(*os.File) error
	rename     func(string, string) error
	syncState  func(string) error
	syncPrune  func(string) error
}

func localFileOperations() fileOperations {
	return fileOperations{os.Mkdir, syncLocalDirectory, (*os.File).Write, (*os.File).Sync, (*os.File).Close, os.Rename, syncLocalDirectory, syncLocalDirectory}
}

func syncLocalDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (ops fileOperations) createDirectory(dir string) error {
	// these platforms provide the inspected directory fsync path; other platforms fail closed
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return errors.New("unsupported state directory synchronization")
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return nil
	}
	if err := ops.createDirectory(parent); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := ops.mkdir(dir, 0700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("state ancestor is not a directory")
	}
	// sync existing edges too: a previous failed startup can leave an uncommitted directory
	return ops.syncParent(parent)
}
