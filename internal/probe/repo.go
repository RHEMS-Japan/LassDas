package probe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	repoMaxFileBytes = 1024 * 1024
	repoMaxGrepHits  = 500
)

// repoPath resolves a slot path inside the working copy and refuses
// anything that leaves it: parent references, absolute paths, and
// symbolic links that point outside (checked after resolution).
func repoPath(root, relative string) (string, error) {
	cleaned := filepath.Clean("/" + relative)
	full := filepath.Join(root, cleaned)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("repository root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", errors.New("path leaves the repository")
	}
	return resolved, nil
}

// runRepo serves the built-in probes over the baseline working copy.
func runRepo(plan Plan, root string) execResult {
	if root == "" {
		return execResult{exitCode: -1, failure: "no repository is available"}
	}
	writer := &cappedWriter{cap: plan.Spec.MaxOutput()}
	var err error
	switch plan.Spec.ID {
	case "repo.list":
		err = repoList(writer, root, plan.Args["path"])
	case "repo.read":
		err = repoRead(writer, root, plan.Args["path"])
	case "repo.grep":
		err = repoGrep(writer, root, plan.Args["path"], plan.Args["pattern"])
	default:
		err = fmt.Errorf("repo probe %q is unknown", plan.Spec.ID)
	}
	result := execResult{output: writer.buf.String(), total: writer.total, truncated: writer.truncated()}
	if err != nil {
		result.exitCode = 1
		result.failure = err.Error()
	}
	return result
}

func repoList(writer *cappedWriter, root, relative string) error {
	start, err := repoPath(root, relative)
	if err != nil {
		return err
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	var paths []string
	err = filepath.WalkDir(start, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && path != start {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(resolvedRoot, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, err := writer.Write([]byte(path + "\n")); err != nil {
			return err
		}
	}
	return nil
}

func repoRead(writer *cappedWriter, root, relative string) error {
	full, err := repoPath(root, relative)
	if err != nil {
		return err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Size() > repoMaxFileBytes {
		return errors.New("file is larger than the read limit")
	}
	content, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	_, err = writer.Write(content)
	return err
}

func repoGrep(writer *cappedWriter, root, relative, pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("pattern: %w", err)
	}
	start, err := repoPath(root, relative)
	if err != nil {
		return err
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	hits := 0
	return filepath.WalkDir(start, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && path != start {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > repoMaxFileBytes {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(content, 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(resolvedRoot, path)
		scanner := bufio.NewScanner(bytes.NewReader(content))
		scanner.Buffer(make([]byte, 0, 64*1024), repoMaxFileBytes)
		line := 0
		for scanner.Scan() {
			line++
			if re.Match(scanner.Bytes()) {
				hits++
				if _, err := fmt.Fprintf(writer, "%s:%d: %s\n", filepath.ToSlash(rel), line, scanner.Text()); err != nil {
					return err
				}
				if hits >= repoMaxGrepHits {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
}
