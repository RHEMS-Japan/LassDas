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

func readReceipt(directory, name string) ([]byte, error) {
	if !filepath.IsLocal(name) {
		return nil, errors.New("receipt must be inside the process directory")
	}
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		if part == ".." {
			return nil, errors.New("receipt must not contain a parent component")
		}
	}
	if !filepath.IsAbs(directory) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		directory = cwd + string(filepath.Separator) + directory
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// Do not clean the process path: even a link followed by /.. must be
	// rejected. Keep opened parents so ordinary /.. still resolves safely.
	parents := []*os.Root{root}
	for _, part := range strings.Split(directory, string(filepath.Separator)) {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parents) > 1 {
				parents = parents[:len(parents)-1]
			}
			root = parents[len(parents)-1]
			continue
		}
		next, err := receiptDirectory(root, part)
		if err != nil {
			return nil, err
		}
		defer next.Close()
		parents = append(parents, next)
		root = next
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, err := receiptDirectory(root, part)
		if err != nil {
			return nil, err
		}
		defer next.Close()
		root = next
	}
	file, err := root.OpenFile(parts[len(parts)-1], os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
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
