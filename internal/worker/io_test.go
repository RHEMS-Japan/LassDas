package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ioFixture struct {
	Name string `json:"name"`
}

func TestReadJSONFileIsStrictAndBounded(t *testing.T) {
	directory := t.TempDir()
	tests := map[string]string{
		"unknown field":    `{"name":"ok","secret_payload":"do-not-echo"}`,
		"trailing value":   `{"name":"ok"} {"name":"again"}`,
		"malformed":        `{"name":"do-not-echo"`,
		"duplicate key":    `{"name":"first","name":"do-not-echo"}`,
		"nested duplicate": `{"name":"ok","nested":{"key":1,"key":2}}`,
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(filename, []byte(encoded), 0o600); err != nil {
				t.Fatal(err)
			}
			var value ioFixture
			err := ReadJSONFile(filename, 1024, &value)
			if err == nil || strings.Contains(err.Error(), "do-not-echo") {
				t.Fatalf("ReadJSONFile() error = %v", err)
			}
		})
	}

	oversized := filepath.Join(directory, "oversized.json")
	if err := os.WriteFile(oversized, []byte(`{"name":"too-large"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var value ioFixture
	if err := ReadJSONFile(oversized, 4, &value); err == nil {
		t.Fatal("ReadJSONFile() accepted oversized input")
	}
}

func TestReadJSONFileRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	link := filepath.Join(directory, "link.json")
	if err := os.WriteFile(target, []byte(`{"name":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var value ioFixture
	if err := ReadJSONFile(link, 1024, &value); err == nil {
		t.Fatal("ReadJSONFile() accepted a symlink")
	}
}

func TestWriteJSONFileExclusiveIsPrivateAndNoReplace(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "artifact.json")
	if err := WriteJSONFileExclusive(filename, ioFixture{Name: "first"}, 1024); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o", got)
	}
	first, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONFileExclusive(filename, ioFixture{Name: "second"}, 1024); err == nil {
		t.Fatal("WriteJSONFileExclusive() replaced an existing artifact")
	}
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(first) {
		t.Fatal("existing artifact changed")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "artifact.json" {
		t.Fatalf("unexpected output entries = %v", entries)
	}
}

func TestWriteJSONFileExclusiveRejectsSymlinkDestination(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	link := filepath.Join(directory, "artifact.json")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONFileExclusive(link, ioFixture{Name: "unsafe"}, 1024); err == nil {
		t.Fatal("WriteJSONFileExclusive() accepted a symlink destination")
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "unchanged" {
		t.Fatalf("target content = %q, error = %v", content, err)
	}
}
