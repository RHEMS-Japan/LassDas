package initwizard

import (
	"strings"
	"testing"
)

// The answer file's host decides whether apply starts the engine at all.
// The consequence is said while the answer can still be changed - not only
// after a whole apply has run, which is where it used to appear.
func TestTheRecordedHostSaysWhetherTheEngineWillStart(t *testing.T) {
	root := t.TempDir()
	writeAnswers(t, root, `{"answers":{}}`)
	answers, _ := LoadAnswers(root)
	if notice := HostNotice(answers); notice != "" {
		t.Fatalf("このマシンで動かす回答に注意書きが出ました: %q", notice)
	}
	writeAnswers(t, root, `{"answers":{"host":"K8s の rin-base / lassdas namespace"}}`)
	answers, _ = LoadAnswers(root)
	notice := HostNotice(answers)
	if !strings.Contains(notice, "lassdas namespace") {
		t.Fatalf("書かれた場所が出ていません: %q", notice)
	}
	if !strings.Contains(notice, "起動しません") || !strings.Contains(notice, "空に") {
		t.Fatalf("起動しないことと、その戻し方が出ていません: %q", notice)
	}
	// "Run it here" is an empty answer, and an answer of spaces is the
	// same thing: the notice and placedElsewhere must read it that way.
	writeAnswers(t, root, `{"answers":{"host":"   "}}`)
	answers, _ = LoadAnswers(root)
	if notice := HostNotice(answers); notice != "" {
		t.Fatalf("空白が場所として扱われました: %q", notice)
	}
}
