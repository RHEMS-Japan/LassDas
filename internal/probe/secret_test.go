package probe

import "testing"

func TestSecretShapedOutputIsRefused(t *testing.T) {
	shaped := map[string]string{
		"aws access key id":               "credentials: AKIAABCDEFGHIJKLMNOP present",
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
