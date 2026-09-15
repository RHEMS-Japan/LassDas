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
	want := strings.Replace(script, "postgres://user:password@", "[masked:connection-string-with-password]", 1)
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

// maskedRead records one repo.read of content and returns what the model
// was told, the stored record, and the session.
func maskedRead(t *testing.T, name, content string, limits Limits) (Outcome, Measurement) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
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
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: limits}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": name}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := ReadPrefix(path, 1)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	return outcome, stored[0]
}

var maskedLimits = Limits{MaxProbes: 5, MaxTotalBytes: 1 << 20, ExcerptBytes: 4096, MaxReads: 5}

// A value found by a shape is removed from the whole output, in whatever
// other context it appears: a bearer token echoed in a response body, a
// password exported on its own line, a key id inside a file name. Before
// masking existed the whole output was refused, so none of these may be
// stored now.
func TestMaskedValuesAreRemovedEverywhere(t *testing.T) {
	token := "abcdef0123456789abcdef0123456789"
	for name, c := range map[string]struct{ content, secret, kind string }{
		"token in body":     {"> Authorization: Bearer " + token + "\n< {\"access_token\":\"" + token + "\",\"token_type\":\"bearer\"}\n", token, "bearer token"},
		"password exported": {"export PGPASSWORD=s3cretpass\nexport DATABASE_URL=postgres://reader:s3cretpass@db.example.invalid:5432/app\n", "s3cretpass", "connection string with password"},
		"key id in a name":  {"AWS_ACCESS_KEY_ID=" + fakeAWSKeyID + "\ncredentials cache: /tmp/" + fakeAWSKeyID + "_creds.json\n", fakeAWSKeyID, "aws access key id"},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, stored := maskedRead(t, "out.log", c.content, maskedLimits)
			if outcome.Measurement.Refused {
				t.Fatalf("refused: %s", outcome.Measurement.Reason)
			}
			if strings.Contains(stored.Output, c.secret) || strings.Contains(outcome.Excerpt, c.secret) {
				t.Fatalf("the value is stored: %q", stored.Output)
			}
			if strings.Count(stored.Output, maskMarker(c.kind)) != 2 || strings.Join(stored.Masked, ",") != c.kind {
				t.Fatalf("stored = %q masked %v, want both occurrences marked", stored.Output, stored.Masked)
			}
		})
	}
	masked, _, refusal := MaskSecrets("export PGPASSWORD=s3cretpass\nurl postgres://reader:s3cretpass@db.example.invalid:5432/app\n", nil)
	if refusal != "" || strings.Contains(masked, "s3cretpass") || masked != "export PGPASSWORD=[masked:connection-string-with-password]\nurl [masked:connection-string-with-password]db.example.invalid:5432/app\n" {
		t.Fatalf("masked = %q refusal %q", masked, refusal)
	}
}

// A value whose alphabet runs past the old pattern's charset leaves no tail:
// a password with an '@' in it and a bearer token with a ':' in it are
// taken whole.
func TestMaskedValueTailsDoNotLeak(t *testing.T) {
	for name, c := range map[string]struct{ content, tail string }{
		"password with @":   {"url postgres://app:s3cr@t-pass@db.example.invalid:5432/app used\n", "t-pass"},
		"bearer with colon": {"Authorization: Bearer abcdefghijklmnop:qrstuvwxyz0123456789\n", "qrstuvwxyz0123456789"},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, stored := maskedRead(t, "cfg.txt", c.content, maskedLimits)
			if outcome.Measurement.Refused {
				t.Fatalf("refused: %s", outcome.Measurement.Reason)
			}
			if strings.Contains(stored.Output, c.tail) || strings.Contains(stored.Output, "s3cr") {
				t.Fatalf("a tail of the value is stored: %q", stored.Output)
			}
		})
	}
}

// A record refused for the output budget after masking names no masked
// kinds: nothing of it is stored.
func TestBudgetRefusalKeepsNoMaskedKinds(t *testing.T) {
	limits := maskedLimits
	limits.MaxTotalBytes = 10
	outcome, stored := maskedRead(t, "aws.log", "id "+fakeAWSKeyID+"\n", limits)
	if !outcome.Measurement.Refused || !strings.Contains(outcome.Measurement.Reason, "budget") {
		t.Fatalf("not refused for the budget: %+v", outcome.Measurement)
	}
	if outcome.Measurement.Masked != nil || stored.Masked != nil || stored.Output != "" {
		t.Fatalf("a refused record kept masked kinds or output: %+v", stored)
	}
}

// The value a shape captured may carry a closing quote, comma or semicolon,
// or run past a later '@'; the same value standing bare elsewhere is still
// removed, because the forms the value can take are all candidates.
func TestMaskedValueCandidatesCoverBareOccurrences(t *testing.T) {
	token := "abcdef0123456789abcdef0123456789"
	for name, c := range map[string]struct{ content, secret string }{
		"pretty json":      {"{\n  \"authorization\": \"Bearer " + token + "\",\n  \"token\": \"" + token + "\"\n}\n", token},
		"shell then curl":  {"TOKEN=" + token + "\ncurl -H \"Authorization: Bearer " + token + "\" https://api.example.invalid/\n", token},
		"dsn with later @": {"PGPASSWORD=s3cretpass\nDATABASE_URL=postgres://reader:s3cretpass@db.example.invalid/app?application_name=svc@prod\n", "s3cretpass"},
		"semicolon":        {"Authorization: Bearer " + token + "\ntoken=" + token + ";\n", token},
	} {
		t.Run(name, func(t *testing.T) {
			outcome, stored := maskedRead(t, "out.txt", c.content, maskedLimits)
			if outcome.Measurement.Refused {
				t.Fatalf("refused: %s", outcome.Measurement.Reason)
			}
			if strings.Contains(stored.Output, c.secret) {
				t.Fatalf("the value is stored: %q", stored.Output)
			}
		})
	}
}

// A letters-only value shorter than sixteen bytes is removed from the rest
// of the output only where it stands as a word of its own in a value
// position (after '=', ':' or a quote), so a four-letter password does not
// take "data" out of "data_dir" or out of a path, while PGPASSWORD=data and
// "password": "data" lose it; a value with a digit is removed anywhere.
func TestShortValuesAreRemovedInValuePositionsOnly(t *testing.T) {
	_, stored := maskedRead(t, "cfg.txt", "url postgres://app:data@db.example.invalid/x\ndata_dir=/var/lib/data\nPGPASSWORD=data\nsecret: data\n{\"password\": \"data\"}\n", maskedLimits)
	want := "url [masked:connection-string-with-password]db.example.invalid/x\ndata_dir=/var/lib/data\nPGPASSWORD=[masked:connection-string-with-password]\nsecret: [masked:connection-string-with-password]\n{\"password\": \"[masked:connection-string-with-password]\"}\n"
	if stored.Output != want {
		t.Fatalf("stored = %q, want %q", stored.Output, want)
	}
	_, stored = maskedRead(t, "cfg2.txt", "url postgres://app:l0ngpassw0rd@db.example.invalid/x\nl0ngpassw0rd_dir=/x\n", maskedLimits)
	if strings.Contains(stored.Output, "l0ngpassw0rd") {
		t.Fatalf("a value with digits survived inside a word: %q", stored.Output)
	}
	// A letters-only sample value, even eight bytes long, leaves prose and
	// keys alone: the line that motivated masking stays readable.
	_, stored = maskedRead(t, "cfg3.txt", "# set the password in the environment before running\npassword: ${DB_PASSWORD}\nurl postgres://user:password@host:5432/db\n", maskedLimits)
	if !strings.HasPrefix(stored.Output, "# set the password in the environment before running\npassword: ${DB_PASSWORD}\nurl [masked:connection-string-with-password]host") {
		t.Fatalf("prose or key lost: %q", stored.Output)
	}
}

// Minified JSON keeps its host and its other fields: the password runs to
// the last '@' before a closing quote, not to the last '@' in the line.
func TestMinifiedJSONKeepsTheHost(t *testing.T) {
	_, stored := maskedRead(t, "cfg.json", `{"db":"postgres://app:s3cretpass@db.example.invalid/app","contact":"ops@example.invalid","replicas":3}`, maskedLimits)
	want := `{"db":"[masked:connection-string-with-password]db.example.invalid/app","contact":"ops@example.invalid","replicas":3}`
	if stored.Output != want {
		t.Fatalf("stored = %q, want %q", stored.Output, want)
	}
}

// The delimiter after a token may be anything a document puts there - a
// backtick, an HTML tag, Markdown emphasis - and is masked with it; the
// same token standing bare elsewhere is still found through the token runs
// inside the captured value.
func TestDelimitedTokensStillRemoveBareOccurrences(t *testing.T) {
	token := "abcdef0123456789abcdef0123456789"
	for name, content := range map[string]string{
		"markdown code": "Send `Authorization: Bearer " + token + "` on every call.\nexport API_TOKEN=" + token + "\n",
		"html code":     "<code>Authorization: Bearer " + token + "</code>\nAPI_TOKEN=" + token + "\n",
		"markdown bold": "header: **Bearer " + token + "**\ntoken=" + token + "\n",
		"sentence end":  "The header is Bearer " + token + ".\ntoken=" + token + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			outcome, stored := maskedRead(t, "doc.md", content, maskedLimits)
			if outcome.Measurement.Refused {
				t.Fatalf("refused: %s", outcome.Measurement.Reason)
			}
			if strings.Contains(stored.Output, token) {
				t.Fatalf("the token is stored: %q", stored.Output)
			}
		})
	}
}

// A password with punctuation in it is still a password: detection is as
// wide as it was when a hit refused the whole output, and the value is not
// stored.
func TestPunctuatedPasswordsAreDetected(t *testing.T) {
	for _, password := range []string{"Xy7,kQ2p", "Xy7)kQ2p", "Xy7'kQ2p", "Xy7\"kQ2p", "Xy7<kQ2p", "Xy7}kQ2p", "Xy7]kQ2p", "Xy7`kQ2p"} {
		content := "PGPASSWORD=" + password + "\nurl postgres://app:" + password + "@db.example.invalid/app\n"
		outcome, stored := maskedRead(t, "env.sh", content, maskedLimits)
		if outcome.Measurement.Refused || strings.Contains(stored.Output, password) || strings.Join(stored.Masked, ",") != "connection string with password" {
			t.Errorf("%q: refused=%v masked=%v stored=%q", password, outcome.Measurement.Refused, stored.Masked, stored.Output)
		}
	}
}

// A token that goes on after a line break cannot be bounded: the output is
// refused rather than stored with the rest of the value in it. A header
// line after the token is not a continuation.
func TestWrappedTokensAreRefused(t *testing.T) {
	for name, content := range map[string]string{
		"bearer": "Authorization: Bearer abcdefghijklmnopqrst\nuvwxyz0123456789abcd\n",
		"jwt":    "id_token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3\njVmNHl0w5N_XgL0n3I9PVJ3n5RQ0\n",
	} {
		outcome, stored := maskedRead(t, "wrap.txt", content, maskedLimits)
		if !outcome.Measurement.Refused || stored.Output != "" {
			t.Errorf("%s: not refused: %+v", name, outcome.Measurement)
		}
	}
	outcome, stored := maskedRead(t, "hdr.txt", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789\nContent-Type: application/json\n", maskedLimits)
	if outcome.Measurement.Refused || stored.Output != "Authorization: [masked:bearer-token]\nContent-Type: application/json\n" {
		t.Errorf("header after a token: %+v %q", outcome.Measurement, stored.Output)
	}
}

// A letters-only value of eight bytes or more that is not a sample word is
// a real password: it is removed from every place a command line, a config
// format or a runbook may put it, not only from value positions.
func TestRealLettersOnlyPasswordsAreRemovedEverywhere(t *testing.T) {
	for _, line := range []string{
		"redis-cli -a secretpass ping", "mongosh --password secretpass", "mysql -u app -psecretpass -e 'select 1'",
		"password secretpass;", "password => secretpass", "password -> secretpass",
		"パスワード：secretpass", "secretpass", "- secretpass", "| password | secretpass |",
	} {
		content := "url postgres://app:secretpass@db.example.invalid/app\n" + line + "\n"
		outcome, stored := maskedRead(t, "runbook.md", content, maskedLimits)
		if outcome.Measurement.Refused || strings.Contains(stored.Output, "secretpass") {
			t.Errorf("%q: refused=%v stored=%q", line, outcome.Measurement.Refused, stored.Output)
		}
	}
	// A sample word stays a word of the prose, and a JSON key is a key.
	_, stored := maskedRead(t, "cfg.json", "url postgres://user:password@host:5432/db\n{\"password\": \"other\", \"user\": \"x\"}\npassword secretword;\n", maskedLimits)
	if !strings.Contains(stored.Output, "{\"password\": \"other\", \"user\": \"x\"}") || !strings.Contains(stored.Output, "password secretword;") {
		t.Fatalf("a sample word was removed outside a value position: %q", stored.Output)
	}
}

// A wrapped token is refused in the forms mail and logs fold it: an
// indented continuation, a space before the break, and a short tail.
func TestWrappedTokenFormsAreRefused(t *testing.T) {
	for name, content := range map[string]string{
		"indented":   "Authorization: Bearer abcdefghijklmnopqrst\n uvwxyz0123456789abcd\n",
		"tabbed":     "Authorization: Bearer abcdefghijklmnopqrst\n\tuvwxyz0123456789abcd\n",
		"flowed":     "Authorization: Bearer abcdefghijklmnopqrst \nuvwxyz0123456789abcd\n",
		"short tail": "Authorization: Bearer abcdefghijklmnopqrst\nuvwxyz1\n",
	} {
		outcome, stored := maskedRead(t, "mail.txt", content, maskedLimits)
		if !outcome.Measurement.Refused || stored.Output != "" {
			t.Errorf("%s: not refused: %+v", name, outcome.Measurement)
		}
	}
}

// A later '@' in the same run extends what is masked, not what is removed
// elsewhere: the host and path of the connection string stay readable on
// another line.
func TestOuterCredentialPartDoesNotRemoveTheHost(t *testing.T) {
	_, stored := maskedRead(t, "env.sh", "DATABASE_URL=postgres://reader:s3cretpass@db.example.invalid/app?application_name=svc@prod\nHOST=db.example.invalid/app\n", maskedLimits)
	if !strings.HasSuffix(stored.Output, "\nHOST=db.example.invalid/app\n") || strings.Contains(stored.Output, "s3cretpass") {
		t.Fatalf("stored = %q", stored.Output)
	}
}

// A default credential - the connection string's own scheme, user or
// database name, or a word development setups use - is masked in its shape
// and nowhere else: image names, variables, links and comments that carry
// the same word stay readable. A letters-only real password is removed as
// a word of its own, never from inside another word.
func TestDefaultCredentialsAreMaskedInTheirShapeOnly(t *testing.T) {
	compose := "services:\n  db:\n    image: postgres:15\n    environment:\n      POSTGRES_USER: postgres\n      POSTGRES_PASSWORD: postgres\n  app:\n    environment:\n      DATABASE_URL: postgres://postgres:postgres@db:5432/app\n      READ_URL: postgres://db.example.invalid/app\n# see https://www.postgresql.org/docs/\n"
	_, stored := maskedRead(t, "compose.yml", compose, maskedLimits)
	want := strings.Replace(compose, "postgres://postgres:postgres@", "[masked:connection-string-with-password]", 1)
	if stored.Output != want {
		t.Fatalf("stored = %q, want %q", stored.Output, want)
	}
	for name, c := range map[string]struct{ content, keep string }{
		"minio":       {"MINIO_ROOT_USER=minioadmin\nMINIO_ROOT_PASSWORD=minioadmin\nurl s3://minioadmin:minioadmin@minio:9000/\n", "MINIO_ROOT_USER=minioadmin\n"},
		"wordpress":   {"image: wordpress:6\nurl mysql://wordpress:wordpress@db/wordpress\n", "image: wordpress:6\n"},
		"development": {"NODE_ENV=development\n# development settings\nurl postgres://app:development@db/app\n", "NODE_ENV=development\n# development settings\n"},
		"dbname":      {"url postgres://app:shopdb@db/shopdb\nSCHEMA=shopdb\n", "SCHEMA=shopdb\n"},
	} {
		outcome, stored := maskedRead(t, name+".env", c.content, maskedLimits)
		if !strings.Contains(stored.Output, c.keep) {
			t.Errorf("%s: %q lost from %q (refused=%v %s)", name, c.keep, stored.Output, outcome.Measurement.Refused, outcome.Measurement.Reason)
		}
	}
	// A real letters-only password goes from a command line, glued to a
	// flag too.
	_, stored = maskedRead(t, "run.sh", "url postgres://app:secretpass@db/app\nredis-cli -a secretpass ping\nmysql -psecretpass\n", maskedLimits)
	if strings.Contains(stored.Output, "secretpass") {
		t.Fatalf("stored = %q", stored.Output)
	}
}

// Lines a script or a document puts after a token are not the rest of it:
// keywords, numbers, versions, dates and rules do not refuse the output.
func TestScriptWordsAfterATokenAreNotContinuations(t *testing.T) {
	for _, next := range []string{"done", "  done", "else", "then", "esac", "true", "null", "1.2.3", "2026-09-15", "----", "42"} {
		content := "curl -H \"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789\"\n" + next + "\n"
		outcome, stored := maskedRead(t, "loop.sh", content, maskedLimits)
		if outcome.Measurement.Refused || !strings.HasSuffix(stored.Output, "\n"+next+"\n") {
			t.Errorf("%q: refused=%v stored=%q", next, outcome.Measurement.Refused, stored.Output)
		}
	}
	outcome, _ := maskedRead(t, "wrap.txt", "Authorization: Bearer abcdefghijklmnopqrst\nuvwxyz0123456789abcd\n", maskedLimits)
	if !outcome.Measurement.Refused {
		t.Fatal("a real continuation was not refused")
	}
}

// A PHP or Ruby array key is a key: with the short password "data",
// ['data' => 'other'] keeps its key while 'value' => 'data' loses the value.
func TestArrowKeysAreKeys(t *testing.T) {
	_, stored := maskedRead(t, "config.php", "url postgres://user:data@host/db\n$db = ['data' => 'other', 'value' => 'data'];\n", maskedLimits)
	if !strings.Contains(stored.Output, "['data' => 'other', 'value' => '[masked:connection-string-with-password]']") {
		t.Fatalf("stored = %q", stored.Output)
	}
}
