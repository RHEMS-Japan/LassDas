package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A writable scope the repository itself ignores used to deliver nothing and
// call it success: git collapses the whole ignored directory into one record,
// the tolerated-byproduct path skipped it, and the candidate came out empty
// with no error anywhere (#18).
func TestAWritableScopeTheRepositoryIgnoresIsRefused(t *testing.T) {
	root, _ := buildAgentRepository(t)
	ignore(t, root, "client/src/generated/\n")
	generated := filepath.Join(root, "client", "src", "generated")
	if err := os.MkdirAll(generated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generated, "api.ts"), []byte("export const x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ChangedFilesUnder(root, []string{"client/src/generated/"}, nil)
	if err == nil {
		t.Fatal("無視されている書き込み先が、何も納品しないまま成功しました")
	}
	if !strings.Contains(err.Error(), "client/src/generated/") {
		t.Fatalf("どの場所が無視されているか言っていません: %v", err)
	}

	// The same directory, when it is merely inside a writable scope, is an
	// ordinary byproduct and stays tolerated: a dependency install must not
	// fail a finished run.
	ignore(t, root, "client/src/node_modules/\n")
	modules := filepath.Join(root, "client", "src", "node_modules")
	if err := os.MkdirAll(modules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modules, "pkg.js"), []byte("module.exports = {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ChangedFilesUnder(root, []string{"client/src/"}, nil); err != nil {
		t.Fatalf("依存の導入物で実行が落ちました: %v", err)
	}
}

// An ignored file inside the scope still stops the run - a deliverable that
// vanishes silently is worse - but the message now names both ways out,
// instead of leaving the reader to find ignored_byproducts on their own.
func TestAnIgnoredFileInsideTheScopeNamesBothWaysOut(t *testing.T) {
	root, _ := buildAgentRepository(t)
	ignore(t, root, "*.map\n")
	if err := os.WriteFile(filepath.Join(root, "client", "src", "app.map"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ChangedFilesUnder(root, []string{"client/src/"}, nil)
	if err == nil {
		t.Fatal("無視される納品物が黙って消えました")
	}
	for _, want := range []string{"app.map", "ignored_byproducts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q が出ていません: %v", want, err)
		}
	}
	// Declared as a byproduct, the same file no longer stops the run.
	if _, err := ChangedFilesUnder(root, []string{"client/src/"}, []string{"app.map"}); err != nil {
		t.Fatalf("宣言済みの副産物で落ちました: %v", err)
	}
}

// ignore writes the repository's own ignore rules and commits them, so the
// rules are part of the tree rather than an uncommitted change of their own.
func ignore(t *testing.T, root, rules string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, root, "add", "-f", ".gitignore")
	agentGit(t, root, "commit", "-m", "ignore rules")
}
