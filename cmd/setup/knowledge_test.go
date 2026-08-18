package main

import (
	"os"
	"path/filepath"
	"testing"
)

// copyKnowledge mirrors Markdown notes with structure, skips what it must,
// and refuses the two states that would poison an instance: the engine name
// inside a note, and a source too large to be notes at all.
func TestCopyKnowledgeMirrorsMarkdownAndEnforcesTheGates(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	write := func(path, content string) {
		full := filepath.Join(source, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("policy.md", "# 方針\n決定 A\n")
	write("answers/T-1.md", "# 回答\n選択 b\n")
	write("notes.txt", "md ではないので写さない\n")
	write(".hidden/secret.md", "隠しフォルダは写さない\n")

	copied, err := copyKnowledge(source, destination)
	if err != nil {
		t.Fatalf("copyKnowledge() error = %v", err)
	}
	if copied != 2 {
		t.Fatalf("copied = %d, want 2", copied)
	}
	if _, err := os.Stat(filepath.Join(destination, "answers", "T-1.md")); err != nil {
		t.Fatal("nested note was not mirrored")
	}
	if _, err := os.Stat(filepath.Join(destination, "notes.txt")); !os.IsNotExist(err) {
		t.Fatal("a non-Markdown file was mirrored")
	}
	if _, err := os.Stat(filepath.Join(destination, ".hidden", "secret.md")); !os.IsNotExist(err) {
		t.Fatal("a hidden directory was mirrored")
	}

	write("bad.md", "この製品は LassDas で動く\n")
	if _, err := copyKnowledge(source, t.TempDir()); err == nil {
		t.Fatal("the engine name inside a note must refuse the copy")
	}
}
