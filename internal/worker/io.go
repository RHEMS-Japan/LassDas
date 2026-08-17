package worker

import (
	"automation.internal/ticket-ingress/internal/hook"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	MaxConfigJSONBytes     int64 = 128 * 1024
	MaxEnvelopeJSONBytes   int64 = hook.MaxDeliveredEnvelopeBytes
	MaxTicketJSONBytes     int64 = 64 * 1024
	MaxArtifactJSONBytes   int64 = 8 * 1024 * 1024
	MaxReviewJSONBytes     int64 = 256 * 1024
	MaxDecisionJSONBytes   int64 = 256 * 1024
	MaxValidationJSONBytes int64 = 256 * 1024
	MaxUsageJSONBytes      int64 = 16 * 1024
	MaxReadinessJSONBytes  int64 = 256 * 1024
)

// ReadJSONFile reads one regular, non-symlink file and decodes exactly one JSON
// value. Unknown object fields are rejected so artifacts cannot silently gain
// authority that an older worker does not understand.
func ReadJSONFile(filename string, maxBytes int64, destination any) error {
	if maxBytes <= 0 || destination == nil {
		return errors.New("JSON input contract is invalid")
	}
	encoded, err := readBoundedRegularFile(filename, maxBytes)
	if err != nil || len(encoded) == 0 {
		return errors.New("JSON input is unavailable")
	}
	if err := rejectDuplicateJSONKeys(encoded); err != nil {
		return errors.New("JSON input is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("JSON input is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON input is invalid")
	}
	return nil
}

// rejectDuplicateJSONKeys walks every object before typed decoding. The
// standard encoding/json decoder otherwise silently keeps the last duplicate,
// which can make different downstream parsers disagree about artifact identity.
func rejectDuplicateJSONKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("JSON object contains a duplicate key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("JSON object is incomplete")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("JSON array is incomplete")
		}
	default:
		return errors.New("JSON delimiter is invalid")
	}
	return nil
}

func readBoundedRegularFile(filename string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("file input contract is invalid")
	}
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("file input is unavailable")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, errors.New("file input is unavailable")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, errors.New("file input changed while opening")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(encoded)) > maxBytes {
		return nil, errors.New("file input size is invalid")
	}
	openedAfter, err := file.Stat()
	if err != nil || !os.SameFile(openedInfo, openedAfter) || openedInfo.Size() != openedAfter.Size() || openedInfo.ModTime() != openedAfter.ModTime() {
		return nil, errors.New("file input changed while reading")
	}
	after, err := os.Lstat(filename)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedAfter, after) {
		return nil, errors.New("file input changed while reading")
	}
	return encoded, nil
}

// WriteJSONFileExclusive publishes an artifact without ever replacing an
// existing path. The temporary inode and final hard link are both mode 0600;
// linking makes publication atomic and preserves no-replace semantics.
func WriteJSONFileExclusive(filename string, value any, maxBytes int64) error {
	if maxBytes <= 0 || value == nil {
		return errors.New("JSON output contract is invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errors.New("JSON output could not be encoded")
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > maxBytes {
		return errors.New("JSON output size is invalid")
	}

	abs, err := filepath.Abs(filename)
	if err != nil || filepath.Base(abs) == "." || filepath.Base(abs) == string(filepath.Separator) {
		return errors.New("JSON output path is invalid")
	}
	parent := filepath.Dir(abs)
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("JSON output directory is invalid")
	}
	if _, err := os.Lstat(abs); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("JSON output already exists")
	}

	temporary, file, err := createExclusiveTemporary(parent, filepath.Base(abs), 0o600)
	if err != nil {
		return errors.New("JSON output could not be prepared")
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if err := writeAndSeal(file, encoded, 0o600); err != nil {
		return errors.New("JSON output could not be written")
	}
	if err := os.Link(temporary, abs); err != nil {
		return errors.New("JSON output could not be published")
	}
	if err := os.Remove(temporary); err != nil {
		_ = os.Remove(abs)
		return errors.New("JSON output could not be finalized")
	}
	keepTemporary = false
	if err := syncDirectory(parent); err != nil {
		return errors.New("JSON output could not be finalized")
	}
	return nil
}

func createExclusiveTemporary(directory, label string, mode os.FileMode) (string, *os.File, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(directory, "."+label+".tmp-"+hex.EncodeToString(random[:]))
		file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err == nil {
			return name, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, errors.New("temporary name collision")
}

func writeAndSeal(file *os.File, content []byte, mode os.FileMode) error {
	if file == nil {
		return errors.New("temporary file is unavailable")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
