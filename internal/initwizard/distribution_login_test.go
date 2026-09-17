package initwizard

import "testing"

func TestLoginCarriesPassword(t *testing.T) {
	rejected := []string{
		"docker login -u me --password hunter2 ghcr.io",
		"docker login -u me --password=hunter2 ghcr.io",
		"docker login -u me -p hunter2 ghcr.io",
		"docker login -u me -phunter2 ghcr.io",
		"docker login -u me -P hunter2 ghcr.io",
		"docker login --Password hunter2 ghcr.io",
	}
	for _, login := range rejected {
		if !loginCarriesPassword(login) {
			t.Errorf("accepted a password form: %q", login)
		}
	}
	accepted := []string{
		"",
		"aws ecr get-login-password --region ap-northeast-1 | docker login --username AWS --password-stdin 123.dkr.ecr.ap-northeast-1.amazonaws.com",
		"gh auth token | docker login ghcr.io -u me --password-stdin",
	}
	for _, login := range accepted {
		if loginCarriesPassword(login) {
			t.Errorf("rejected a stdin form: %q", login)
		}
	}
}
