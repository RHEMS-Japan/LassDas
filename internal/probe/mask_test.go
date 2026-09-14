package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repo.read of a file whose only secret-shaped text is a comment showing
// a connection string's format is stored and readable: the credentials are
// masked, the rest of the file is the excerpt and the stored output, the
// record says what was masked, the chain still verifies, and the attachment
// re-scan finds no shape. A file carrying a private key block is still
// refused whole.
func TestMaskedOutputStaysReadable(t *testing.T) {
	root := t.TempDir()
	script := "#!/bin/sh\n#   PROD_DATABASE_URL  - 本番 Aurora への接続文字列（postgres://user:password@host:5432/db）\nset -euo pipefail\necho migrate\n"
	if err := os.WriteFile(filepath.Join(root, "migrate.sh"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "key.pem"), []byte("-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: Limits{MaxProbes: 5, MaxTotalBytes: 1 << 20, ExcerptBytes: 4096, MaxReads: 5}}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "migrate.sh"}})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(script, "postgres://user:password@", "[masked:connection string with password]", 1)
	if outcome.Measurement.Refused || outcome.Excerpt != want {
		t.Fatalf("repo.read = %+v excerpt %q, want the masked file", outcome.Measurement, outcome.Excerpt)
	}
	if strings.Join(outcome.Measurement.Masked, ",") != "connection string with password" || outcome.Measurement.Output != "" {
		t.Fatalf("the model's copy = %+v, want the masked kind named and no full output", outcome.Measurement)
	}
	window, err := session.Read(outcome.Measurement.ID, 0)
	if err != nil || window.Text != want {
		t.Fatalf("stored output read back = %q, %v, want the masked file", window.Text, err)
	}
	stored, err := ReadPrefix(path, 1)
	if err != nil || len(stored) != 1 || stored[0].Output != want || strings.Join(stored[0].Masked, ",") != "connection string with password" {
		t.Fatalf("stored record = %+v, %v", stored, err)
	}
	if kind, found := SecretShaped(stored[0].Output, nil); found {
		t.Fatalf("the stored output still carries a %s", kind)
	}
	if _, err := OpenRecorder(path); err != nil {
		t.Fatalf("the chain does not verify after a masked record: %v", err)
	}
	outcome, err = session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "key.pem"}})
	if err != nil || !outcome.Measurement.Refused || !strings.Contains(outcome.Measurement.Reason, "private key") || outcome.Excerpt != "" || outcome.Measurement.Masked != nil {
		t.Fatalf("private key file = %+v excerpt %q, %v, want the whole output refused", outcome.Measurement, outcome.Excerpt, err)
	}
}
