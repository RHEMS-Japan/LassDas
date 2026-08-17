package worker

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// ApplyCandidate replaces only the files bound by the ticket, source snapshot,
// and candidate. All current files are re-read and compared with the immutable
// source hashes before any replacement is prepared.
func ApplyCandidate(repoRoot string, candidate Candidate, source SourceSnapshot, request TicketRequest, config Config) error {
	root, err := validatedApplyRoot(repoRoot, candidate, source, request, config)
	if err != nil {
		return err
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("apply artifact bindings are invalid")
	}
	type replacement struct {
		final     string
		temporary string
		mode      os.FileMode
		content   []byte
	}
	replacements := make([]replacement, 0, len(candidate.Files))
	cleanup := func() {
		for _, item := range replacements {
			if item.temporary != "" {
				_ = os.Remove(item.temporary)
			}
		}
	}
	defer cleanup()

	// Validate the complete set first. A stale or unsafe file must fail before
	// any target is changed.
	for index, file := range candidate.Files {
		filename, info, current, err := currentBoundFile(root, file.Path, consumer.Mode.MaxFileBytes)
		if err != nil || digestBytes(current) != source.Files[index].SHA256 || file.BeforeSHA256 != source.Files[index].SHA256 {
			return errors.New("apply source no longer matches the snapshot")
		}
		replacements = append(replacements, replacement{
			final: filename, mode: info.Mode().Perm(), content: []byte(file.Content),
		})
	}

	// Prepare every replacement in its target directory before publishing any
	// of them. O_EXCL prevents an existing file from being reused as a temp file.
	for index := range replacements {
		item := &replacements[index]
		temporary, file, err := createExclusiveTemporary(filepath.Dir(item.final), filepath.Base(item.final), 0o600)
		if err != nil {
			return errors.New("apply replacement could not be prepared")
		}
		item.temporary = temporary
		if err := writeAndSeal(file, item.content, item.mode); err != nil {
			return errors.New("apply replacement could not be prepared")
		}
	}

	// Narrow the validation-to-rename window. os.Rename replaces a symlink
	// itself rather than following it, but a changed source is still rejected.
	for index := range replacements {
		_, _, current, err := currentBoundFile(root, candidate.Files[index].Path, consumer.Mode.MaxFileBytes)
		if err != nil || digestBytes(current) != source.Files[index].SHA256 {
			return errors.New("apply source changed during preparation")
		}
	}
	for index, item := range replacements {
		if err := os.Rename(item.temporary, item.final); err != nil {
			return errors.New("apply replacement could not be published")
		}
		replacements[index].temporary = ""
		if err := syncDirectory(filepath.Dir(item.final)); err != nil {
			return errors.New("apply replacement could not be finalized")
		}
	}
	return nil
}

// VerifyApplied proves that every bound target is a regular non-symlink file
// whose complete bytes exactly match the candidate. It does not execute code.
func VerifyApplied(repoRoot string, candidate Candidate, source SourceSnapshot, request TicketRequest, config Config) error {
	root, err := validatedApplyRoot(repoRoot, candidate, source, request, config)
	if err != nil {
		return err
	}
	consumer, err := request.Consumer(config)
	if err != nil {
		return errors.New("apply artifact bindings are invalid")
	}
	for _, file := range candidate.Files {
		_, _, current, err := currentBoundFile(root, file.Path, consumer.Mode.MaxFileBytes)
		if err != nil || string(current) != file.Content {
			return errors.New("applied candidate does not match")
		}
	}
	return nil
}

func validatedApplyRoot(repoRoot string, candidate Candidate, source SourceSnapshot, request TicketRequest, config Config) (string, error) {
	if err := candidate.Validate(source, request, config); err != nil {
		return "", errors.New("apply artifact bindings are invalid")
	}
	if len(request.TargetFiles) != len(source.Files) || len(source.Files) != len(candidate.Files) {
		return "", errors.New("apply file set is invalid")
	}
	for index, target := range request.TargetFiles {
		if source.Files[index].Path != target || candidate.Files[index].Path != target {
			return "", errors.New("apply file set is invalid")
		}
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil || filepath.Clean(root) != root {
		return "", errors.New("apply root is invalid")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("apply root is invalid")
	}
	return root, nil
}

func currentBoundFile(root, relative string, limit int) (string, os.FileInfo, []byte, error) {
	filename, err := regularFileWithin(root, relative)
	if err != nil {
		return "", nil, nil, errors.New("bound file is invalid")
	}
	before, err := os.Lstat(filename)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", nil, nil, errors.New("bound file is invalid")
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", nil, nil, errors.New("bound file is invalid")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", nil, nil, errors.New("bound file changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil || len(content) > limit || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return "", nil, nil, errors.New("bound file content is invalid")
	}
	openedAfter, err := file.Stat()
	if err != nil || !os.SameFile(opened, openedAfter) || opened.Size() != openedAfter.Size() || opened.ModTime() != openedAfter.ModTime() {
		return "", nil, nil, errors.New("bound file changed while reading")
	}
	after, err := os.Lstat(filename)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedAfter, after) {
		return "", nil, nil, errors.New("bound file changed while reading")
	}
	return filename, after, content, nil
}
