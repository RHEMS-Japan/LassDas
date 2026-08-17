package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// WorkflowDispatcher wakes the instance's receive workflow through the GitHub
// workflow-dispatch API, authenticating as a dedicated GitHub App whose only
// grant is starting workflows in the instance repository. It exists so a
// queued ticket starts within seconds instead of waiting for the schedule,
// and it is a separate credential precisely so the internet-facing ingress
// never holds the delivery App's key.
type WorkflowDispatcher struct {
	repository     string
	workflow       string
	ref            string
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	apiOrigin      string
	client         *http.Client
	now            func() time.Time
}

type DispatchConfig struct {
	Repository     string // owner/name of the repository whose workflow is woken
	Workflow       string // workflow file name, e.g. receive-backlog-ticket.yml
	Ref            string // branch the workflow runs on
	AppID          int64  // the dispatch App's identifier
	InstallationID int64  // the App's installation on the instance repository
	PrivateKeyB64  string // base64 of the App's PEM private key
}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)
	workflowPattern   = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(yml|yaml)$`)
)

func (c DispatchConfig) validate() error {
	if !repositoryPattern.MatchString(c.Repository) {
		return errors.New("dispatch repository is invalid")
	}
	if name := c.Repository[strings.IndexByte(c.Repository, '/')+1:]; name == "." || name == ".." {
		return errors.New("dispatch repository is invalid")
	}
	if !workflowPattern.MatchString(c.Workflow) {
		return errors.New("dispatch workflow is invalid")
	}
	if c.Ref == "" || strings.TrimSpace(c.Ref) != c.Ref || strings.ContainsAny(c.Ref, " :\r\n") {
		return errors.New("dispatch ref is invalid")
	}
	if c.AppID <= 0 || c.InstallationID <= 0 {
		return errors.New("dispatch app identity is invalid")
	}
	if _, err := decodeDispatchKey(c.PrivateKeyB64); err != nil {
		return err
	}
	return nil
}

func decodeDispatchKey(encoded string) (*rsa.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("dispatch key is invalid")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("dispatch key is invalid")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("dispatch key is invalid")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("dispatch key is invalid")
	}
	return key, nil
}

func NewWorkflowDispatcher(config DispatchConfig, client *http.Client) (*WorkflowDispatcher, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	key, err := decodeDispatchKey(config.PrivateKeyB64)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &WorkflowDispatcher{
		repository: config.Repository, workflow: config.Workflow, ref: config.Ref,
		appID: config.AppID, installationID: config.InstallationID, key: key,
		apiOrigin: "https://api.github.com", client: client, now: time.Now,
	}, nil
}

func (d *WorkflowDispatcher) DispatchWork(ctx context.Context) error {
	token, err := d.installationToken(ctx)
	if err != nil {
		return err
	}
	endpoint := d.apiOrigin + "/repos/" + d.repository + "/actions/workflows/" + d.workflow + "/dispatches"
	body, err := json.Marshal(map[string]any{"ref": d.ref, "inputs": map[string]string{"operation": "pull"}})
	if err != nil {
		return errors.New("dispatch body could not be built")
	}
	response, err := d.send(ctx, endpoint, token, body)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("workflow dispatch returned status %d", response.StatusCode)
	}
	return nil
}

// installationToken trades a short-lived App JWT for an installation token.
// The token is minted per dispatch: ticket volume is a handful a day, and a
// fresh token removes every caching and expiry concern from this path.
func (d *WorkflowDispatcher) installationToken(ctx context.Context) (string, error) {
	jwt, err := d.signedJWT()
	if err != nil {
		return "", err
	}
	endpoint := d.apiOrigin + "/app/installations/" + strconv.FormatInt(d.installationID, 10) + "/access_tokens"
	response, err := d.send(ctx, endpoint, jwt, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("installation token request returned status %d", response.StatusCode)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&payload); err != nil || payload.Token == "" {
		return "", errors.New("installation token response is invalid")
	}
	return payload.Token, nil
}

func (d *WorkflowDispatcher) send(ctx context.Context, endpoint, bearer string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(request)
}

func (d *WorkflowDispatcher) signedJWT() (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	issued := d.now().UTC().Unix()
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`, issued-30, issued+540, d.appID)
	payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
	digest := sha256.Sum256([]byte(header + "." + payload))
	signature, err := rsa.SignPKCS1v15(rand.Reader, d.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("dispatch jwt could not be signed")
	}
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
