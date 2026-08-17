package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// validateOutputDestination rejects an obvious orphan-artifact failure before
// a command reaches a GitHub mutation. Final publication still uses the
// worker's atomic exclusive writer, which closes the remaining race.
func validateOutputDestination(filename string) error {
	if filename == "" {
		return errors.New("output artifact path is invalid")
	}
	absolute, err := filepath.Abs(filename)
	if err != nil || filepath.Base(absolute) == "." || filepath.Base(absolute) == string(filepath.Separator) {
		return errors.New("output artifact path is invalid")
	}
	parent, err := os.Lstat(filepath.Dir(absolute))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return errors.New("output artifact directory is invalid")
	}
	if _, err := os.Lstat(absolute); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("output artifact already exists")
	}
	return nil
}

func distinctOutputDestinations(left, right string) bool {
	leftResolved, leftErr := resolvedOutputDestination(left)
	rightResolved, rightErr := resolvedOutputDestination(right)
	return leftErr == nil && rightErr == nil && leftResolved != rightResolved
}

func resolvedOutputDestination(filename string) (string, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// readBoundedRegularArtifact reads one binary artifact without following a
// symlink and rejects replacement or mutation during the read.
func readBoundedRegularArtifact(filename string, maximum int64) ([]byte, error) {
	if filename == "" || maximum <= 0 {
		return nil, errors.New("binary artifact contract is invalid")
	}
	before, err := os.Lstat(filename)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("binary artifact is unavailable")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("binary artifact is unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("binary artifact changed while opening")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(encoded) == 0 || int64(len(encoded)) > maximum {
		return nil, errors.New("binary artifact size is invalid")
	}
	openedAfter, err := file.Stat()
	if err != nil || !os.SameFile(opened, openedAfter) || opened.Size() != openedAfter.Size() ||
		opened.ModTime() != openedAfter.ModTime() {
		return nil, errors.New("binary artifact changed while reading")
	}
	after, err := os.Lstat(filename)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedAfter, after) {
		return nil, errors.New("binary artifact changed while reading")
	}
	return encoded, nil
}
