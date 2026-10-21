// Package fsutil provides filesystem operations shared by persistent stores.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// OpenPrivateRoot persists newly created parent entries before accepting files.
// The confined handle also keeps publication on the same directory if its path
// is renamed concurrently. Shared filesystems must support these OS operations.
func OpenPrivateRoot(directory string) (*os.Root, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}

	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private storage directory must be a real directory")
	}
	// Another initializer may have created existing ancestors without syncing them yet.
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		if err := SyncDir(parent); err != nil {
			return nil, err
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	if err := root.Chmod(".", 0700); err != nil {
		_ = root.Close()
		return nil, err
	}

	return root, nil
}

// SyncDir persists directory entries where the OS supports directory fsync.
// Windows does not support flushing a directory opened by os.Open. Callers
// still sync file contents before publishing them; directory metadata durability
// on Windows is provided by the filesystem rather than an explicit flush.
func SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(SyncOpenDir(dir), dir.Close())
}

// SyncOpenDir keeps a caller's confined directory handle instead of resolving
// its path again. Windows has the same filesystem durability limitation as SyncDir.
func SyncOpenDir(dir *os.File) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return dir.Sync()
}
