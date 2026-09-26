package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func useCatalogTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = fn
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func catalogReply(request *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}
}

func TestModelQueryFetchesNewCatalogEveryTimeWithoutCredentials(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "synthetic-not-for-this-query")
	bodies := []string{
		`{"data":[{"id":"maker/old","pricing":{"prompt":"0.01"}},{"id":"decider/current","architecture":{"output_modalities":["decisions"]}}]}`,
		`{"fetched_at":"not our observation time","unknown":true,"data":[{"id":"maker/new","pricing":{"prompt":"0.005"},"new_capability":{"x":true}},{"id":"decider/current","architecture":{"output_modalities":["decisions"]}}]}`,
	}
	calls := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://openrouter.ai/api/v1/models?output_modalities=all" || request.Method != "GET" {
			t.Fatalf("wrong catalog query: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cache-Control") != "no-cache" {
			t.Fatal("catalog query sent a credential or did not request refresh")
		}
		calls++
		return catalogReply(request, 200, bodies[calls-1]), nil
	})
	for _, id := range []string{"maker/old", "maker/new"} {
		var output, log bytes.Buffer
		started := time.Now()
		if err := run(context.Background(), []string{"--list-models"}, &output, &log); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), id) || !strings.Contains(output.String(), "fetched_at") || log.Len() != 0 {
			t.Fatalf("catalog was not returned on stdout: %s / %s", &output, &log)
		}
		if id == "maker/new" && (!strings.Contains(output.String(), `"new_capability":{"x":true}`) || !strings.Contains(output.String(), "0.005")) {
			t.Fatal("new catalog metadata or prices were lost")
		}
		var snapshot modelList
		if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil || snapshot.FetchedAt.Before(started) || snapshot.FetchedAt.After(time.Now()) {
			t.Fatalf("not this fetch's observation time: %s (%v)", &output, err)
		}
		var want struct{ Data []json.RawMessage }
		if err := json.Unmarshal([]byte(bodies[calls-1]), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(snapshot.Data, want.Data) {
			t.Fatalf("list entries were dropped, rewritten or merged with old models: %s", &output)
		}
	}
	if calls != 2 {
		t.Fatalf("fresh calls=%d", calls)
	}
}

func TestModelQueryOutageNeverReturnsEarlierSuccessfulList(t *testing.T) {
	calls := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return catalogReply(request, 200, `{"data":[{"id":"maker/old"}]}`), nil
		}
		return catalogReply(request, 503, `{"error":"catalog temporarily unavailable"}`), nil
	})
	if err := writeModelList(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := writeModelList(context.Background(), &output)
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "temporarily unavailable") || output.Len() != 0 {
		t.Fatalf("outage was hidden by a cached or partial list: %s (%v)", &output, err)
	}
}

func TestModelQueryInvalidIncompleteAndOversizedResponsesReturnNoList(t *testing.T) {
	for name, body := range map[string]string{
		"empty": `{"data":[]}`, "null": `{"data":null}`, "missing": `{}`,
		"missing id": `{"data":[{}]}`, "null entry": `{"data":[null]}`, "unreadable id": `{"data":[{"id":42}]}`,
		"malformed": `{"data":[`, "trailing": `{"data":[{"id":"maker/new"}]} extra`,
		"too large": `{"data":[{"id":"maker/new","description":"` + strings.Repeat("x", 16<<20) + `"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) { return catalogReply(request, 200, body), nil })
			var output bytes.Buffer
			if err := writeModelList(context.Background(), &output); err == nil || output.Len() != 0 {
				t.Fatalf("invalid/partial list was published: bytes=%d error=%v", output.Len(), err)
			}
		})
	}
}

func TestModelQueryRefusesRedirectAndHonorsCancellation(t *testing.T) {
	calls := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		calls++
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		response := catalogReply(request, 302, "moved")
		response.Header.Set("Location", "https://elsewhere.example/models")
		return response, nil
	})
	var output bytes.Buffer
	if err := writeModelList(context.Background(), &output); err == nil || !strings.Contains(err.Error(), "redirect refused") || calls != 1 {
		t.Fatalf("redirect followed: calls=%d error=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeModelList(ctx, &output); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("cancellation returned a list: %s (%v)", &output, err)
	}
}

func TestModelQueryCannotSilentlyReplaceRequestedWork(t *testing.T) {
	useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("combined request unexpectedly made a network call")
		return nil, errors.New("unexpected call")
	})
	for _, args := range [][]string{
		{"--list-models", "--request", "request.txt"}, {"--list-models", "--issue", "EXAMPLE-1"},
		{"--list-models", "--config", "operator.json"}, {"--list-models", "--run-dir", "run"},
		{"--list-models", "unexpected"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, io.Discard); err == nil || output.Len() != 0 {
			t.Fatalf("query silently displaced work: %v (%v)", args, err)
		}
	}
}
