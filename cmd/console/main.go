// Command console serves the read-only operations view: every ticket's
// current state at a glance, and per ticket the generation history with its
// evidence. It assembles the view from the canonical sources - the state
// table, the tracker and the workflow API - at request time, holds no state
// of its own, and writes nothing anywhere.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

//go:embed dist
var distFS embed.FS

type consoleConfig struct {
	StateTable    string
	TrackerDomain string
	TrackerKey    string
	ProjectID     string
	InstanceRepo  string
	GitHubToken   string
	Listen        string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("console", flag.ContinueOnError)
	stateTable := flags.String("state-table", os.Getenv("LASSDAS_CONSOLE_STATE_TABLE"), "DynamoDB state table name")
	trackerDomain := flags.String("tracker-domain", os.Getenv("LASSDAS_CONSOLE_TRACKER_DOMAIN"), "Backlog domain (example.backlog.com)")
	projectID := flags.String("project-id", os.Getenv("LASSDAS_CONSOLE_PROJECT_ID"), "tracker numeric project id")
	instanceRepo := flags.String("instance-repo", os.Getenv("LASSDAS_CONSOLE_INSTANCE_REPO"), "instance repository (owner/name)")
	listen := flags.String("listen", "127.0.0.1:8542", "listen address")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	cfg := consoleConfig{
		StateTable:    *stateTable,
		TrackerDomain: *trackerDomain,
		TrackerKey:    os.Getenv("LASSDAS_CONSOLE_TRACKER_KEY"),
		ProjectID:     *projectID,
		InstanceRepo:  *instanceRepo,
		Listen:        *listen,
		GitHubToken:   githubToken(),
	}
	var missing []string
	for name, value := range map[string]string{
		"--state-table / LASSDAS_CONSOLE_STATE_TABLE":       cfg.StateTable,
		"--tracker-domain / LASSDAS_CONSOLE_TRACKER_DOMAIN": cfg.TrackerDomain,
		"LASSDAS_CONSOLE_TRACKER_KEY":                       cfg.TrackerKey,
		"--instance-repo / LASSDAS_CONSOLE_INSTANCE_REPO":   cfg.InstanceRepo,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return errors.New("missing configuration: " + strings.Join(missing, ", "))
	}

	if !strings.HasPrefix(cfg.Listen, "127.0.0.1:") && !strings.HasPrefix(cfg.Listen, "localhost:") {
		// A read-only view of live operations still names tickets, states
		// and comments; this tool binds to the loopback and nothing else.
		return errors.New("--listen must stay on 127.0.0.1 (got " + cfg.Listen + ")")
	}

	awsConfig, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return fmt.Errorf("AWS configuration: %w", err)
	}
	server := &consoleServer{
		config: cfg,
		dynamo: dynamodb.NewFromConfig(awsConfig),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// The tracker key travels in the query - the only form the
			// tracker accepts - so a followed redirect would hand the full
			// URL to the next host in the Referer. Redirects are not
			// followed, ever.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	static, err := fs.Sub(distFS, "dist")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", server.handleOverview)
	mux.HandleFunc("GET /api/tickets/{key}", server.handleTicket)
	mux.Handle("GET /", http.FileServerFS(static))

	// A local tool is still reachable from any page the operator's browser
	// visits: DNS rebinding serves a hostile page whose hostname resolves
	// to 127.0.0.1, and the browser will happily send it here. Only the
	// addresses this server answers as are accepted.
	guarded := hostGuard(cfg.Listen, mux)

	fmt.Printf("console: http://%s (read-only)\n", cfg.Listen)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           guarded,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpServer.ListenAndServe()
}

// hostGuard rejects requests whose Host header is not this server's own
// address, closing the DNS-rebinding read.
func hostGuard(listen string, next http.Handler) http.Handler {
	allowed := map[string]struct{}{listen: {}}
	if strings.HasPrefix(listen, "127.0.0.1:") {
		allowed["localhost:"+strings.TrimPrefix(listen, "127.0.0.1:")] = struct{}{}
	}
	if strings.HasPrefix(listen, "localhost:") {
		allowed["127.0.0.1:"+strings.TrimPrefix(listen, "localhost:")] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := allowed[r.Host]; !ok {
			http.Error(w, "unexpected host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// githubToken reads the workflow API credential the way an operator already
// has it: the environment first, the gh CLI's stored login second.
func githubToken() string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
