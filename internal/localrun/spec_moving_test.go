package localrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The board the wizard sets up answers only a loopback Host: put that same
// environment behind a Service and every request is refused, with a Pod that
// starts and looks healthy. The spec says so where a person reads what to
// place, not in a document they may not open.
func TestTheSpecSaysWhatHasToChangeWhenItLeavesThisMachine(t *testing.T) {
	i := fixture(t)

	// As set up by the wizard: the local board, no user and no password.
	path := filepath.Join(i.Dir, "runtime.env")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "LASSDAS_BOARD_USER=") || strings.HasPrefix(line, "LASSDAS_BOARD_PASS=") {
			continue
		}
		kept = append(kept, line)
	}
	writeFile(t, path, []byte(strings.Join(append(kept, "LASSDAS_BOARD_AUTH=local"), "\n")+"\n"), 0o600)

	spec, err := Describe(i)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.BeforeMoving, "\n")
	for _, want := range []string{"LASSDAS_BOARD_AUTH=basic", "403", "SQLite", "二重"} {
		if !strings.Contains(joined, want) {
			t.Errorf("移す前に変えるものに %q がありません:\n%s", want, joined)
		}
	}

	// A board already on basic auth does not need that change, and saying
	// it anyway teaches people to skim the list.
	writeFile(t, path, raw, 0o600)
	spec, err = Describe(i)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(spec.BeforeMoving, "\n")
	if strings.Contains(joined, "LASSDAS_BOARD_AUTH=basic") {
		t.Errorf("既に basic なのに板の認証を変えろと言っています:\n%s", joined)
	}
	if len(spec.BeforeMoving) == 0 {
		t.Error("台帳の移し方と二重起動は、板の設定に関わらず言う")
	}
}

// The generated name carries a hex digest, and hex spells host names: a
// digest containing "ec2" used to fail the spec's "names no host" check at
// random (CI, 2026-09-19). The check reads what the spec says, not the
// identifiers it generates.
func TestAGeneratedIdentifierIsNotReadAsAHostName(t *testing.T) {
	i := fixture(t)
	spec, err := Describe(i)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name == "" || spec.DataName == "" {
		t.Fatal("識別子がありません")
	}
	for _, line := range spec.BeforeMoving {
		for _, host := range []string{"docker", "kubernetes", "kubectl", "ec2"} {
			if strings.Contains(strings.ToLower(line), host) {
				t.Errorf("移設前の注意が置き場所を名指ししています (%s): %s", host, line)
			}
		}
	}
}
