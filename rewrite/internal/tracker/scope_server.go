package tracker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"
)

// IssueAccess is a loopback, per-launch API. The public certificate may be
// passed to a worker; the TLS private key and upstream key stay in controller
// memory. A real launcher must also isolate the controller and other workers.
type IssueAccess struct {
	URL, Certificate, Key string
	close                 func()
	settle                func()
}

// Close ends the launch's access. A scope that keeps only its latest post
// removes the earlier ones now, after the endpoint is closed, so nothing a
// role posted waits on a removal. A post still in flight when the endpoint
// closes may be stored after the count; it then stays as one comment more,
// and nothing stored at or after the kept one is ever removed.
func (a *IssueAccess) Close() {
	a.close()
	if a.settle != nil {
		a.settle()
	}
}

func ServeIssue(ctx context.Context, source Backlog, issue string, mayPost bool, options ...func(*IssueScope)) (*IssueAccess, error) {
	scope, err := NewIssueScope(source, issue, mayPost, options...)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	certificate := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "task tracker access"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	// No worker URL (which carries a capability) goes to the HTTP error logger.
	server := &http.Server{Handler: scope, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 35 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    log.New(io.Discard, "", 0),
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
	}
	done := make(chan struct{})
	var once sync.Once
	closeServer := func() { once.Do(func() { server.Close(); close(done) }) }
	go func() {
		// Serving errors become unavailable access on the role's next request,
		// not a false success. No request is automatically replayed here.
		if err := server.Serve(tls.NewListener(listener, server.TLSConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			closeServer()
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			closeServer()
		case <-done:
		}
	}()
	var settled sync.Once
	settle := func() {
		settled.Do(func() {
			// The launch's context may already be cancelled; the removal has
			// its own short bound instead of inheriting that.
			bounded, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			scope.RemoveEarlierPosts(bounded)
		})
	}
	return &IssueAccess{URL: "https://" + listener.Addr().String(), Key: scope.Key(),
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), close: closeServer, settle: settle}, nil
}

// CertificateClient trusts this explicit local endpoint certificate instead of
// disabling verification or modifying process-wide/system trust.
func CertificateClient(certificate string) (*http.Client, error) {
	pool := x509.NewCertPool()
	if certificate == "" || !pool.AppendCertsFromPEM([]byte(certificate)) {
		return nil, errors.New("tracker certificate is unavailable or unreadable")
	}
	// Loopback access is direct; do not expose the worker token to an ambient
	// proxy selected by the controller's or worker's environment.
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: time.Minute}
	return &http.Client{Transport: transport}, nil
}
