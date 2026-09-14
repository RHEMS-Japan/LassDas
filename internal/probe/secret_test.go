package probe

import (
	"strings"
	"testing"
)

// fakeAWSKeyID and fakeAWSKeyID2 are key-shaped test values assembled at
// run time so the source never carries a contiguous key-shaped token
// (the repository's push protection scans for those).
var (
	fakeAWSKeyID  = "AKIA" + "ABCDEFGHIJKLMNOP"
	fakeAWSKeyID2 = "AKIA" + "ABCDEFGHIJKLMNOQ"
)

func TestSecretShapesAreDetected(t *testing.T) {
	shaped := map[string]string{
		"aws access key id":               "credentials: " + fakeAWSKeyID + " present",
		"gateway key":                     "Authorization: csk-abcdefgh12345678",
		"bearer token":                    "authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789",
		"private key":                     "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		"json web token":                  "session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"github token":                    "token ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"provider key":                    "key sk-abcdefghijklmnopqrstuvwxyz0123",
		"connection string with password": "postgres://reader:s3cretpass@db.example.invalid:5432/app",
	}
	for want, output := range shaped {
		kind, found := SecretShaped(output, nil)
		if !found || kind != want {
			t.Errorf("%q: kind %q found %v, want %q", output, kind, found, want)
		}
	}
	kind, found := SecretShaped("set-cookie seen; value 0123456789abcdef", []string{"0123456789abcdef"})
	if !found || kind != "known secret value" {
		t.Errorf("forbidden literal: kind %q found %v", kind, found)
	}
	for _, benign := range []string{
		"NAME READY STATUS RESTARTS AGE\nweb-1 1/1 Running 0 3d",
		"count\n42",
		"status=200 time_total=0.412 bytes=18422",
		"postgres://db.example.invalid:5432/app",
		"short literal abc",
	} {
		if kind, found := SecretShaped(benign, []string{"abc"}); found {
			t.Errorf("%q flagged as %q", benign, kind)
		}
	}
}

// Store-time scan: a value whose shape bounds it is replaced by a marker
// naming the kind, the rest of the output stays readable, and the kinds are
// reported. The masked text carries no shape any more. A private key block
// and a value the kernel itself holds refuse the whole output as before.
func TestSecretShapedOutputIsMaskedNotDropped(t *testing.T) {
	for kind, sample := range map[string]string{
		"aws access key id":               "credentials: " + fakeAWSKeyID + " present",
		"gateway key":                     "Authorization: csk-abcdefgh12345678 sent",
		"bearer token":                    "authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789 sent",
		"json web token":                  "session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PVJ3n5RQ0 set",
		"github token":                    "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 used",
		"chat token":                      "slack xoxb-1234567890-abcdefghij used",
		"provider key":                    "key sk-abcdefghijklmnopqrstuvwxyz0123 used",
		"connection string with password": "url postgres://reader:s3cretpass@db.example.invalid:5432/app used",
	} {
		masked, kinds, refusal := MaskSecrets(sample, nil)
		if refusal != "" || len(kinds) != 1 || kinds[0] != kind {
			t.Errorf("%s: kinds %v refusal %q, want the one kind masked", kind, kinds, refusal)
			continue
		}
		if !strings.Contains(masked, maskMarker(kind)) || !strings.HasSuffix(masked, sample[strings.LastIndex(sample, " "):]) {
			t.Errorf("%s: masked = %q, want the marker with the surrounding text kept", kind, masked)
		}
		if k, found := SecretShaped(masked, nil); found {
			t.Errorf("%s: masked text still carries a %s: %q", kind, k, masked)
		}
	}
	// The line that made a target file unreadable: a comment showing the
	// format of a connection string. Everything but the credentials stays.
	line := "#   PROD_DATABASE_URL  - 本番 Aurora への接続文字列（postgres://user:password@host:5432/db）\nset -euo pipefail"
	masked, kinds, refusal := MaskSecrets(line, nil)
	want := "#   PROD_DATABASE_URL  - 本番 Aurora への接続文字列（[masked:connection-string-with-password]host:5432/db）\nset -euo pipefail"
	if masked != want || refusal != "" || len(kinds) != 1 {
		t.Errorf("doc comment: masked = %q kinds %v refusal %q, want %q", masked, kinds, refusal, want)
	}
	// Two shapes in one output: both masked, kinds in shape order, no repeats.
	masked, kinds, refusal = MaskSecrets("a "+fakeAWSKeyID+" b sk-abcdefghijklmnopqrstuvwxyz0123 c "+fakeAWSKeyID2, nil)
	if refusal != "" || strings.Join(kinds, ",") != "aws access key id,provider key" || strings.Contains(masked, "AKIA") || strings.Contains(masked, "sk-") {
		t.Errorf("two shapes: masked = %q kinds %v refusal %q", masked, kinds, refusal)
	}
	// Refused whole: a private key block (its body cannot be bounded) and a
	// value the kernel holds.
	if masked, kinds, refusal := MaskSecrets("-----BEGIN RSA PRIVATE KEY-----\nMIIE...", nil); refusal != "private key" || masked != "" || kinds != nil {
		t.Errorf("private key: masked %q kinds %v refusal %q, want the whole output refused", masked, kinds, refusal)
	}
	if masked, kinds, refusal := MaskSecrets("set-cookie seen; value 0123456789abcdef", []string{"0123456789abcdef"}); refusal != "known secret value" || masked != "" || kinds != nil {
		t.Errorf("known value: masked %q kinds %v refusal %q, want the whole output refused", masked, kinds, refusal)
	}
	// Markers are looked through by the scan: a marker right after
	// "Bearer " or "user:" is not itself a shape, so a gateway key sent as
	// a bearer token and a key id used as a password stay readable, and a
	// marker cannot join the text after it into a shape either.
	for _, glued := range []string{
		"Bearer [masked:connection-string-with-password] abcdefghijklmnopqrstuvwxyz",
		"Bearer [masked:connection-string-with-password].eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0",
		"postgres://u:[masked:aws-access-key-id]@host/db",
	} {
		if kind, found := SecretShaped(glued, nil); found {
			t.Errorf("a marker joined into a %s: %q", kind, glued)
		}
	}
	for sample, want := range map[string]string{
		"Authorization: Bearer csk-abcdefgh12345678 sent\n": "Authorization: Bearer [masked:gateway-key] sent\n",
		"dsn postgres://u:" + fakeAWSKeyID + "@host/db\n":   "dsn postgres://u:[masked:aws-access-key-id]@host/db\n",
	} {
		masked, _, refusal := MaskSecrets(sample, nil)
		if refusal != "" || masked != want {
			t.Errorf("marker after a shape prefix: masked %q refusal %q, want %q", masked, refusal, want)
		}
	}
	// The private-use runes the placeholders are built from cannot come
	// from the output: they are replaced before anything is masked.
	if masked, kinds, refusal := MaskSecrets("note \uE0000\uE001 here\nAuthorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789\n", nil); refusal != "" || masked != "note \uFFFD0\uFFFD here\nAuthorization: [masked:bearer-token]\n" || strings.Join(kinds, ",") != "bearer token" {
		t.Errorf("private-use runes: masked %q kinds %v refusal %q", masked, kinds, refusal)
	}
	// Benign output comes back untouched.
	for _, benign := range []string{"NAME READY STATUS\nweb-1 1/1 Running", "postgres://db.example.invalid:5432/app", ""} {
		if masked, kinds, refusal := MaskSecrets(benign, []string{"abc"}); masked != benign || kinds != nil || refusal != "" {
			t.Errorf("benign %q: masked %q kinds %v refusal %q", benign, masked, kinds, refusal)
		}
	}
}
