package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const ApprovalBytes = 32 * 1024

type ApprovalBundle struct {
	Files    []ApprovalFile
	Contents map[string][]byte
	Digest   string
}

func ApprovalDigest(contents map[string][]byte) (string, error) {
	if len(contents) == 0 {
		return "", ApprovalFailure("bundle_limit")
	}
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := sha256.New()
	var length [8]byte
	for _, path := range paths {
		binary.BigEndian.PutUint32(length[:4], uint32(len(path)))
		h.Write(length[:4])
		h.Write([]byte(path))
		binary.BigEndian.PutUint64(length[:], uint64(len(contents[path])))
		h.Write(length[:])
		h.Write(contents[path])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func ApprovalSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func approvalName(name string) bool {
	if name == "" || name[0] == '.' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}
func approvalText(data []byte) error {
	if !utf8.Valid(data) {
		return ApprovalFailure("post_fence")
	}
	for _, c := range string(data) {
		if unicode.Is(unicode.Cf, c) || unicode.IsControl(c) && c != '\n' && c != '\t' {
			return ApprovalFailure("post_fence")
		}
	}
	if bytes.Contains(data, []byte("```")) {
		return ApprovalFailure("post_fence")
	}
	return nil
}
func approvalLimits(data []byte) error {
	blanks := 0
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for _, line := range lines {
		if utf8.RuneCountInString(line) > 200 {
			return ApprovalFailure("bundle_limit")
		}
		run := 0
		for _, c := range line {
			if c == ' ' || c == '\t' {
				run++
				if run > 16 {
					return ApprovalFailure("bundle_limit")
				}
			} else {
				run = 0
			}
		}
		if strings.Trim(line, " \t") == "" {
			blanks++
		} else {
			blanks = 0
		}
		if blanks > 2 {
			return ApprovalFailure("bundle_limit")
		}
	}
	return nil
}

// This is the same descriptor-by-descriptor boundary used by readReceipt.
func approvalRoot(workspace, relative string) (*os.Root, error) {
	if !filepath.IsLocal(relative) {
		return nil, ApprovalFailure("bundle_name")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if !approvalName(part) {
			root.Close()
			return nil, ApprovalFailure("bundle_name")
		}
		child, err := receiptDirectory(root, part)
		root.Close()
		if err != nil {
			return nil, ApprovalFailure("bundle_kind")
		}
		root = child
	}
	return root, nil
}
func approvalRead(root *os.Root, name string, limit int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, ApprovalFailure("bundle_kind")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ApprovalFailure("bundle_kind")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, info) || !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		return nil, ApprovalFailure("bundle_kind")
	}
	if info.Size() > limit {
		return nil, ApprovalFailure("bundle_limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		return nil, ApprovalFailure("bundle_limit")
	}
	return data, err
}
func collectApproval(root *os.Root) (ApprovalBundle, error) {
	b := ApprovalBundle{Contents: map[string][]byte{}}
	total := 0
	var walk func(*os.Root, string, int) error
	walk = func(root *os.Root, prefix string, depth int) error {
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, err := dir.ReadDir(-1)
		dir.Close()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !approvalName(entry.Name()) {
				return ApprovalFailure("bundle_name")
			}
			if depth > 4 {
				return ApprovalFailure("bundle_limit")
			}
			name := entry.Name()
			path := prefix + name
			info, err := root.Lstat(name)
			if err != nil {
				return err
			}
			if info.IsDir() {
				child, err := receiptDirectory(root, name)
				if err != nil {
					return ApprovalFailure("bundle_kind")
				}
				err = walk(child, path+"/", depth+1)
				child.Close()
				if err != nil {
					return err
				}
				continue
			}
			data, err := approvalRead(root, name, ApprovalBytes-int64(total))
			if err != nil {
				return err
			}
			total += len(data)
			if len(b.Contents) >= 16 {
				return ApprovalFailure("bundle_limit")
			}
			if err = approvalLimits(data); err != nil {
				return err
			}
			b.Contents[path] = data
			b.Files = append(b.Files, ApprovalFile{path, int64(len(data)), ApprovalSHA256(data)})
		}
		return nil
	}
	if err := walk(root, "", 1); err != nil {
		return b, err
	}
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	var err error
	b.Digest, err = ApprovalDigest(b.Contents)
	return b, err
}
func ReadApprovalBundle(path string) (ApprovalBundle, error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return ApprovalBundle{}, err
	}
	defer parent.Close()
	root, err := receiptDirectory(parent, filepath.Base(path))
	if err != nil {
		return ApprovalBundle{}, ApprovalFailure("bundle_kind")
	}
	defer root.Close()
	return collectApproval(root)
}
func hasCredential(data []byte, values []string) bool {
	for _, v := range values {
		if v != "" && bytes.Contains(data, []byte(v)) {
			return true
		}
	}
	return false
}

// ApprovalSummary validates presentation without changing the bytes that are hashed.
func ApprovalSummary(data []byte) error {
	text := string(data)
	trimmed := strings.TrimSpace(text)
	if !utf8.Valid(data) || trimmed == "" || utf8.RuneCountInString(trimmed) > 2000 || strings.Contains(text, "```") {
		return ApprovalFailure("summary_unreadable")
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			return ApprovalFailure("summary_unreadable")
		}
	}
	return nil
}

func CollectApprovalBundle(workspace string, approval *Approval, destination string, credentials []string) (ApprovalBundle, error) {
	root, err := approvalRoot(workspace, approval.Bundle)
	if err != nil {
		return ApprovalBundle{}, err
	}
	defer root.Close()
	// Inspect show first, so credentials are removed even when another bundle
	// file is invalid. Do not let malformed text hide a credential-bearing show.
	for _, show := range approval.Show {
		data, err := readReceipt(workspace, filepath.Join(workspace, approval.Bundle), show)
		if errors.Is(err, os.ErrNotExist) {
			return ApprovalBundle{}, ApprovalFailure("show_missing")
		}
		if err != nil {
			return ApprovalBundle{}, ApprovalFailure("bundle_kind")
		}
		if hasCredential(data, credentials) {
			if err = RemoveApprovalFile(workspace, filepath.Join(workspace, approval.Bundle), show); err != nil {
				return ApprovalBundle{}, err
			}
			return ApprovalBundle{}, ApprovalFailure("bundle_credential")
		}
	}
	summary, err := approval.SummaryPath()
	if err != nil {
		return ApprovalBundle{}, ApprovalFailure("summary_unreadable")
	}
	if summary != "" {
		// Check before generic bundle limits so malformed summaries have their own
		// actionable reason. The collected bytes are checked again below.
		data, err := readReceipt(workspace, filepath.Join(workspace, approval.Bundle), filepath.FromSlash(summary))
		if err != nil {
			return ApprovalBundle{}, ApprovalFailure("summary_unreadable")
		}
		if err := ApprovalSummary(data); err != nil {
			return ApprovalBundle{}, err
		}
	}
	b, err := collectApproval(root)
	if err != nil {
		return b, err
	}
	if summary != "" {
		if err := ApprovalSummary(b.Contents[summary]); err != nil {
			return b, err
		}
	}
	for _, show := range approval.Show {
		if _, ok := b.Contents[filepath.ToSlash(show)]; !ok {
			return b, ApprovalFailure("show_missing")
		}
	}
	for _, data := range b.Contents {
		if err = approvalText(data); err != nil {
			return b, err
		}
		if hasCredential(data, credentials) {
			return b, ApprovalFailure("post_masked")
		}
	}
	parent := filepath.Dir(destination)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return b, err
	}
	temp, err := os.MkdirTemp(parent, ".bundle-*")
	if err != nil {
		return b, err
	}
	defer os.RemoveAll(temp)
	for _, file := range b.Files {
		path := filepath.Join(temp, filepath.FromSlash(file.Path))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return b, err
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return b, openErr
		}
		_, err = f.Write(b.Contents[file.Path])
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return b, err
		}
		if closeErr != nil {
			return b, closeErr
		}
	}
	// Compute from the completed copy, never from the mutable working tree.
	copied, err := ReadApprovalBundle(temp)
	if err != nil {
		return b, err
	}
	if copied.Digest != b.Digest {
		return b, ApprovalFailure("bundle_kind")
	}
	if err = filepath.WalkDir(temp, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		d, err := os.Open(path)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}); err != nil {
		return b, err
	}
	if err = os.Rename(temp, destination); err != nil {
		return b, err
	}
	d, err := os.Open(parent)
	if err != nil {
		return b, err
	}
	defer d.Close()
	if err = d.Sync(); err != nil {
		return b, err
	}
	return copied, nil
}
func CheckApprovalCollision(digest string, posts []ApprovalPost) error {
	if len(digest) != 64 {
		return ApprovalFailure("digest_collision")
	}
	for _, p := range posts {
		if len(p.Digest) != 64 {
			return ApprovalFailure("digest_collision")
		}
		if p.Digest != digest && p.Digest[:8] == digest[:8] {
			return ApprovalFailure("digest_collision")
		}
	}
	return nil
}

// Missing entries need no cleanup; unsafe entries are never followed/deleted.
func RemoveApprovalFile(workspace, directory, name string) error {
	if name == "" {
		return nil
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if workspace == "" {
		workspace = directory
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(workspace, directory)
	if err != nil || !filepath.IsLocal(rel) || filepath.IsAbs(name) {
		return ApprovalFailure("bundle_kind")
	}
	rel = filepath.Clean(filepath.Join(rel, name))
	if !filepath.IsLocal(rel) {
		return ApprovalFailure("bundle_kind")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		child, err := receiptDirectory(root, part)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return ApprovalFailure("bundle_kind")
		}
		defer child.Close()
		root = child
	}
	name = parts[len(parts)-1]
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return ApprovalFailure("bundle_kind")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ApprovalFailure("bundle_kind")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, info) || !info.Mode().IsRegular() || !ok || st.Nlink != 1 {
		return ApprovalFailure("bundle_kind")
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, current) {
		return ApprovalFailure("bundle_kind")
	}
	return root.Remove(name)
}
