package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces a private file only after its contents have reached the disk.
// The temporary file is created in the same directory so rename stays atomic.
func Write(filename string, data []byte) error {
	_, err := WriteCommitted(filename, data)
	return err
}

// WriteCommitted distinguishes a failure before publication from a failure to
// confirm directory durability after rename. Once published is true, callers
// must keep the same identity even when Sync returns an error.
func WriteCommitted(filename string, data []byte) (published bool, err error) {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	// Existing configuration/spool directories can predate private defaults.
	// Tighten the actual parent before publishing credentials or log bodies.
	if err := os.Chmod(dir, 0700); err != nil {
		return false, err
	}
	file, err := os.CreateTemp(dir, ".edge-write-*")
	if err != nil {
		return false, err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	if _, err = file.Write(data); err != nil {
		return false, err
	}
	if err = file.Sync(); err != nil {
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(file.Name(), filename); err != nil {
		return false, err
	}
	return true, SyncDir(dir)
}

// SyncDir persists a rename/unlink after the data file itself is durable.
func SyncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
