package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const chatHandoff = `{"choices":[{"message":{"tool_calls":[{"function":{"name":"handoff","arguments":"{\"role\":\"implement\"}"}}]}}]}`

func TestModelTransportAllowsReasoningLatencyWithBoundedWait(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	for _, api := range []string{"decision", "chat-router", "chat-selection"} {
		t.Run(api, func(t *testing.T) {
			service := Jev{URL: "https://model.example/inference", KeyEnv: "ROUTER_TEST_TOKEN"}
			service.Client = &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
				deadline, bounded := r.Context().Deadline()
				if remaining := time.Until(deadline); !bounded || remaining < 4*time.Minute || remaining > 5*time.Minute {
					t.Errorf("inference wait must allow slow completions and remain bounded: bounded=%v remaining=%s", bounded, remaining)
				}
				body := chatHandoff
				if api == "decision" {
					body = `{"answers":{"next":{"choice":"implement"}}}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			choices := map[string]string{"implement": "continue the work"}
			var choice string
			var err error
			switch api {
			case "decision":
				choice, err = service.Choose(context.Background(), State{}, "choose", choices)
			case "chat-router":
				var next Assignment
				next, err = (ChatRouter{Service: service, Roles: choices}).Next(context.Background(), State{})
				choice = next.Role
			case "chat-selection":
				choice, err = (ChatJudge{Service: service}).Choose(context.Background(), State{}, "choose", choices)
			}
			if err != nil || choice != "implement" {
				t.Fatalf("choice=%q error=%v", choice, err)
			}
		})
	}
}

func TestModelTransportPreservesEarlierParentDeadline(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	client := &http.Client{Transport: modelTransport(func(r *http.Request) (*http.Response, error) {
		actual, ok := r.Context().Deadline()
		if !ok || !actual.Equal(deadline) {
			t.Errorf("parent deadline changed: %s != %s", actual, deadline)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}
	_, err := (Jev{URL: "https://model.example/inference", KeyEnv: "ROUTER_TEST_TOKEN", Client: client}).request(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestModelTransportStopCancelsHeadersAndBodyWait(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	for _, startedBody := range []bool{false, true} {
		t.Run(fmt.Sprint(startedBody), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if startedBody {
					fmt.Fprint(w, `{"choices":`)
					w.(http.Flusher).Flush()
				}
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan error, 1)
			go func() {
				_, err := (Jev{URL: server.URL, KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}).request(ctx, nil)
				returned <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request never reached local TLS service")
			}
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("stop lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request ignored stop while waiting for response")
			}
		})
	}
}
