package chain

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const receiptBytes = 1 << 20

func readReceipt(workspace, directory, name string) ([]byte, error) {
	if name == "" || filepath.IsAbs(name) {
		return nil, errors.New("receipt must be a relative path")
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if workspace == "" {
		workspace = directory
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(workspace, directory)
	if err != nil {
		return nil, err
	}
	if !filepath.IsLocal(rel) {
		return nil, errors.New("process directory must be inside the workspace")
	}
	rel = filepath.Clean(filepath.Join(rel, name))
	if !filepath.IsLocal(rel) {
		return nil, errors.New("receipt must be inside the workspace")
	}
	// The operator chooses the workspace location; links above this boundary
	// are allowed. Inspect every directory below it without following links.
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, err := receiptDirectory(root, part)
		if err != nil {
			return nil, err
		}
		defer next.Close()
		root = next
	}
	name = parts[len(parts)-1]
	// os.Root can re-resolve a final symlink despite O_NOFOLLOW.
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("receipt is not a regular file")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, info) {
		return nil, errors.New("receipt changed during inspection")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 || info.Size() > receiptBytes {
		return nil, errors.New("receipt is not an ordinary bounded file")
	}
	data, err := io.ReadAll(io.LimitReader(file, receiptBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > receiptBytes {
		return nil, errors.New("receipt exceeds the size limit")
	}
	return data, nil
}

// Match the opened directory to the inspected entry before descending further.
func receiptDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errors.New("receipt will not follow a directory link")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	file, err := child.Open(".")
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && !os.SameFile(before, info) {
			err = errors.New("receipt directory changed during inspection")
		}
		file.Close()
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}
