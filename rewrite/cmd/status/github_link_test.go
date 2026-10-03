package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredGitHubOnlyLinksToItsOwnWebHostAndPort(t *testing.T) {
	for _, test := range []struct {
		name, settings, page string
		want                 bool
	}{
		{"public default", `{}`, "https://github.com/example/project/issues/12", true},
		{"public empty", `{"api_url":""}`, "https://github.com/example/project/issues/12", true},
		{"public explicit", `{"api_url":"https://api.github.com/"}`, "https://github.com/example/project/issues/12", true},
		{"host case", `{"api_url":"https://API.GITHUB.COM"}`, "https://GITHUB.COM/example/project/issues/12", true},
		{"other host", `{}`, "https://other.example/example/project/issues/12", false},
		{"suffix host", `{}`, "https://github.com.evil.example/example/project/issues/12", false},
		{"API not web", `{}`, "https://api.github.com/example/project/issues/12", false},
		{"unexpected port", `{}`, "https://github.com:8443/example/project/issues/12", false},
		{"enterprise", `{"api_url":"https://git.example/api/v3"}`, "https://git.example/example/project/issues/12", true},
		{"enterprise port", `{"api_url":"https://git.example:8443/api/v3/"}`, "https://git.example:8443/example/project/issues/12", true},
		{"enterprise missing port", `{"api_url":"https://git.example:8443/api/v3"}`, "https://git.example/example/project/issues/12", false},
		{"enterprise wrong port", `{"api_url":"https://git.example:8443/api/v3"}`, "https://git.example:9443/example/project/issues/12", false},
		{"enterprise wrong host", `{"api_url":"https://git.example/api/v3"}`, "https://other.example/example/project/issues/12", false},
		{"userinfo", `{}`, "https://github.com@evil.example/example/project/issues/12", false},
		{"credentials", `{}`, "https://fixture:synthetic@github.com/example/project/issues/12", false},
		{"http page", `{}`, "http://github.com/example/project/issues/12", false},
		{"wrong number", `{}`, "https://github.com/example/project/issues/13", false},
		{"invalid config", `null`, "https://github.com/example/project/issues/12", false},
		{"invalid API type", `{"api_url":13}`, "https://github.com/example/project/issues/12", false},
		{"API userinfo", `{"api_url":"https://fixture@github.com"}`, "https://github.com/example/project/issues/12", false},
		{"API query", `{"api_url":"https://github.com?query=value"}`, "https://github.com/example/project/issues/12", false},
		{"API fragment", `{"api_url":"https://github.com#part"}`, "https://github.com/example/project/issues/12", false},
		{"API http", `{"api_url":"http://github.com"}`, "https://github.com/example/project/issues/12", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "jobs", "12")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			record, err := json.Marshal(map[string]any{"number": 12, "title": "Visible request", "html_url": test.page,
				"repository_url": "https://api.github.com/repos/example/project"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "issue.json"), record, 0600); err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(root, "operator.json")
			if err := os.WriteFile(config, []byte(`{"github":`+test.settings+`}`), 0600); err != nil {
				t.Fatal(err)
			}
			ts := serve(t, root, config, "", "")
			_, body := get(t, ts, "/jobs/12")
			expectAll(t, body, "<title>#12 status</title>", "Visible request")
			linked := strings.Contains(body, ">open the issue on GitHub</a>")
			if linked != test.want {
				t.Fatalf("linked=%t want=%t for %s", linked, test.want, test.page)
			}
		})
	}
}
