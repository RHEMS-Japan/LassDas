package app

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testDispatchKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func testDispatchKeyB64() string {
	der := x509.MarshalPKCS1PrivateKey(testDispatchKey())
	block := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	return base64.StdEncoding.EncodeToString(block)
}

func validDispatchConfig() DispatchConfig {
	return DispatchConfig{
		Repository: "example/instance", Workflow: "receive.yml", Ref: "main",
		AppID: 4001, InstallationID: 9001, PrivateKeyB64: testDispatchKeyB64(),
	}
}

func TestDispatchConfigValidation(t *testing.T) {
	if err := validDispatchConfig().validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	mutations := map[string]func(*DispatchConfig){
		"repository without owner":   func(c *DispatchConfig) { c.Repository = "instance" },
		"repository extra slash":     func(c *DispatchConfig) { c.Repository = "a/../b" },
		"repository dot-dot segment": func(c *DispatchConfig) { c.Repository = "a/.." },
		"repository dot segment":     func(c *DispatchConfig) { c.Repository = "a/." },
		"workflow not a file":        func(c *DispatchConfig) { c.Workflow = "receive" },
		"workflow with slash":        func(c *DispatchConfig) { c.Workflow = "dir/receive.yml" },
		"empty ref":                  func(c *DispatchConfig) { c.Ref = "" },
		"ref with space":             func(c *DispatchConfig) { c.Ref = "ma in" },
		"zero app id":                func(c *DispatchConfig) { c.AppID = 0 },
		"zero installation id":       func(c *DispatchConfig) { c.InstallationID = 0 },
		"empty key":                  func(c *DispatchConfig) { c.PrivateKeyB64 = "" },
		"key that is not base64":     func(c *DispatchConfig) { c.PrivateKeyB64 = "not-base64!" },
		"base64 that is not a key":   func(c *DispatchConfig) { c.PrivateKeyB64 = base64.StdEncoding.EncodeToString([]byte("plain")) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			config := validDispatchConfig()
			mutate(&config)
			if config.validate() == nil {
				t.Fatal("invalid dispatch configuration was accepted")
			}
		})
	}
}

func TestNewWorkflowDispatcherDefaultsToABoundedClient(t *testing.T) {
	dispatcher, err := NewWorkflowDispatcher(validDispatchConfig(), nil)
	if err != nil {
		t.Fatalf("NewWorkflowDispatcher() error = %v", err)
	}
	if dispatcher.client.Timeout != 5*time.Second {
		t.Fatalf("default client timeout = %v; the webhook response depends on this bound", dispatcher.client.Timeout)
	}
}

// The dispatcher must make exactly two calls: mint an installation token with
// a JWT the App key verifiably signed, then start the workflow with that
// token. The fake GitHub below checks each request as it arrives.
func TestWorkflowDispatcherMintsATokenAndStartsTheWorkflow(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/app/installations/9001/access_tokens":
			verifyAppJWT(t, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_short_lived"}`))
		case "/repos/example/instance/actions/workflows/receive.yml/dispatches":
			if got := r.Header.Get("Authorization"); got != "Bearer ghs_short_lived" {
				t.Errorf("dispatch authorization = %q", got)
			}
			var payload struct {
				Ref    string            `json:"ref"`
				Inputs map[string]string `json:"inputs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Ref != "main" || payload.Inputs["operation"] != "pull" {
				t.Errorf("dispatch payload invalid: %+v (error %v)", payload, err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	dispatcher, err := NewWorkflowDispatcher(validDispatchConfig(), server.Client())
	if err != nil {
		t.Fatalf("NewWorkflowDispatcher() error = %v", err)
	}
	dispatcher.apiOrigin = server.URL

	if err := dispatcher.DispatchWork(context.Background()); err != nil {
		t.Fatalf("DispatchWork() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want exactly a token mint then a dispatch", requests)
	}
}

func verifyAppJWT(t *testing.T, authorization string) {
	t.Helper()
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		t.Errorf("token request authorization = %q", authorization)
		return
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Errorf("jwt has %d parts", len(parts))
		return
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || rsa.VerifyPKCS1v15(&testDispatchKey().PublicKey, crypto.SHA256, digest[:], signature) != nil {
		t.Error("jwt signature does not verify against the App key")
	}
	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err != nil || json.Unmarshal(claimBytes, &claims) != nil || claims.Issuer != "4001" {
		t.Errorf("jwt claims = %s", claimBytes)
	}
}

func TestWorkflowDispatcherStopsWhenTheTokenMintFails(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	dispatcher, err := NewWorkflowDispatcher(validDispatchConfig(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.apiOrigin = server.URL
	if dispatcher.DispatchWork(context.Background()) == nil {
		t.Fatal("a refused token mint was treated as success")
	}
	if requests != 1 {
		t.Fatalf("requests = %d; the dispatch must not run without a token", requests)
	}
}

func TestWorkflowDispatcherRejectsNon204(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_short_lived"}`))
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	dispatcher, err := NewWorkflowDispatcher(validDispatchConfig(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.apiOrigin = server.URL
	if dispatcher.DispatchWork(context.Background()) == nil {
		t.Fatal("a 422 response was treated as success")
	}
}
