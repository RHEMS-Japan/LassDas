package main

import (
	"errors"
	"os"
	"path/filepath"
)

// trimFinished removes what a finished request no longer needs and nobody
// reads: the caches its roles' agents built in their home directories, which
// are most of a request's footprint. Everything a person reads back stays,
// the record, the request, the notices, the workspace with its changes, and
// each home's logs and files. It runs once; a marker says it has.
func trimFinished(directory string) error {
	homes := filepath.Join(directory, "homes")
	marker := filepath.Join(homes, ".trimmed")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	entries, err := os.ReadDir(homes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, home := range entries {
		if !home.IsDir() || home.Type()&os.ModeSymlink != 0 {
			continue
		}
		inside, err := os.ReadDir(filepath.Join(homes, home.Name()))
		if err != nil {
			return err
		}
		for _, item := range inside {
			if !item.IsDir() || item.Name() == "logs" || item.Type()&os.ModeSymlink != 0 {
				continue
			}
			if err := removeTree(filepath.Join(homes, home.Name(), item.Name())); err != nil {
				return err
			}
		}
	}
	return os.WriteFile(marker, []byte("caches removed after the request finished\n"), 0600)
}

// removeTree removes a directory a build tool may have left read-only, as a
// Go module cache is: every directory is made writable first so its entries
// can be unlinked. Symbolic links are removed as links, never followed.
func removeTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0700); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}
