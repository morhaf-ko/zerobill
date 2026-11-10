// Package atomicfile replaces files so readers never see a partial write.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with raw, readable only by its owner, through a temporary
// file in the same directory, syncing the file and directory before returning.
func Write(path string, raw []byte) error {
	return write(path, raw, true)
}

// Create publishes a complete file only if path does not already exist.
func Create(path string, raw []byte) error {
	return write(path, raw, false)
}

func write(path string, raw []byte, replace bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(tmp.Name(), path)
	} else {
		err = os.Link(tmp.Name(), path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
