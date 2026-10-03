package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestNativeFailureDisplaysKeepGraphemesAfterRedacting(t *testing.T) {
	for _, cluster := range []string{"🇯🇵", "👩‍👩‍👧‍👦", "e\u0301"} {
		t.Run(cluster, func(t *testing.T) {
			cfg := watchConfiguration(t)
			padding := strings.Repeat("x", 186)
			body := padding + "synthetic-watch-key " + cluster + " remainder"
			want := padding + "[credential] …"
			if got := noticeDetail(cfg, body); got != want {
				t.Errorf("native notice split the redacted display: %q", got)
			}
			github := githubConfiguration(t)
			if got := noticeDetail(github, body); got != "` "+want+" `" {
				t.Errorf("quoted notice split the display or its delimiter: %q", got)
			}
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				return catalogReply(r, http.StatusServiceUnavailable, body), nil
			})
			_, err := modelCreditRemaining(context.Background(), cfg)
			if err == nil || err.Error() != "model budget endpoint returned HTTP 503: "+want {
				t.Errorf("model budget error split the redacted display: %v", err)
			}
		})
	}
}
