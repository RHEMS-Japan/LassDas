package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

type config struct {
	Router struct {
		Mode     string    `json:"mode"`
		Decision chain.Jev `json:"decision"`
		LLM      chain.Jev `json:"llm"`
	} `json:"router"`
	Instructions   string           `json:"instructions"`
	Roles          []chain.Role     `json:"roles"`
	Backlog        tracker.Backlog  `json:"backlog"`
	ModelSelection *selectionConfig `json:"model_selection,omitempty"`
	Intake         *intakeConfig    `json:"intake,omitempty"`
	AssignedIssue  string           `json:"assigned_issue,omitempty"`
	Workflow       *chain.Workflow  `json:"workflow,omitempty"`
	// runtimeUser is the tracker account the credential belongs to, read at
	// start when issues are handed over.
	runtimeUser int64
}

// Give the dispatcher the same configured work instructions as its workers.
// A role title alone does not describe which tools and boundaries it has.
// These are operator facts, not a verdict about a worker's answer. Do not
// serialize Process: commands, environment and credentials are not router input.
func routingRoleDescription(role chain.Role) string {
	var description strings.Builder
	description.WriteString(role.Purpose)
	for _, process := range role.Processes {
		access := process.TrackerAccess
		if access == "" {
			access = "none"
		}
		fmt.Fprintf(&description, "\nConfigured process %s; engine-issued tracker access: %s.\n%s\n",
			process.Name, access, process.Instructions)
	}
	return description.String()
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, log io.Writer) error {
	flags := flag.NewFlagSet("engine", flag.ContinueOnError)
	flags.SetOutput(log)
	configPath := flags.String("config", "", "configured roles and router")
	requestPath := flags.String("request", "", "original request as text")
	issue := flags.String("issue", "", "read the original issue from the configured tracker")
	showModels := flags.Bool("list-models", false, "fetch the current OpenRouter catalog; no request is run")
	watch := flags.Bool("watch", false, "poll the explicitly configured intake into separate request directories")
	directory := flags.String("run-dir", "", "private directory for this request's history")
	logFile := flags.String("log-file", "", "also append the runtime's own observations to this file (the status page reads it)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *logFile != "" {
		file, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("opening --log-file: %w", err)
		}
		defer file.Close()
		log = io.MultiWriter(log, file)
	}
	if *showModels {
		if *configPath != "" || *requestPath != "" || *issue != "" || *directory != "" || *watch || flags.NArg() != 0 {
			return errors.New("--list-models is a separate catalog query; do not combine it with a request")
		}
		return writeModelList(ctx, output)
	}
	if *configPath == "" || *directory == "" || flags.NArg() != 0 ||
		(*watch && (*requestPath != "" || *issue != "")) ||
		(!*watch && (*requestPath == "") == (*issue == "")) {
		return errors.New("provide --config, --run-dir and either --watch or exactly one of --request/--issue")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if err := cfg.ModelSelection.validate(); err != nil {
		return err
	}
	roles, purposes := map[string]chain.Role{}, map[string]string{}
	for _, role := range cfg.Roles {
		if role.Name == "" || role.Name == "done" || len(role.Processes) == 0 {
			return errors.New("a configured role needs a name and processes")
		}
		if _, exists := roles[role.Name]; exists {
			return errors.New("two roles have the same name")
		}
		purposes[role.Name] = routingRoleDescription(role)
		// Every role needs the same operator workflow context as the router.
		// Keep the configuration untouched: watch serializes it for each job,
		// so mutating its process slice here would duplicate these instructions.
		if cfg.Instructions != "" {
			role.Processes = append([]chain.Process(nil), role.Processes...)
			for i := range role.Processes {
				role.Processes[i].Instructions = "Shared operator workflow instructions:\n" + cfg.Instructions +
					"\n\nProcess-specific instructions:\n" + role.Processes[i].Instructions
			}
		}
		roles[role.Name] = role
	}
	if len(roles) == 0 {
		return errors.New("no roles configured")
	}
	if err := prepareStages(&cfg); err != nil {
		return err
	}
	if err := cfg.Workflow.Validate(purposes); err != nil {
		return err
	}
	if err := validateQuestionRole(cfg); err != nil {
		return err
	}
	observe := func(message string) { fmt.Fprintln(log, message) }
	var router chain.Router
	chatService := chain.ChatRouter{Service: cfg.Router.LLM, Roles: purposes, Instructions: cfg.Instructions}
	var chat chain.Router = chatService
	if cfg.ModelSelection != nil {
		selection := *cfg.ModelSelection
		selection.observe = observe
		chat = selectedChatRouter{chat: chatService, selection: selection}
	}
	decision := func() chain.Router {
		var next chain.Router = chain.DecisionRouter{Judge: cfg.Router.Decision, Roles: purposes, Instructions: cfg.Instructions}
		if cfg.Router.LLM.Model != "" || (cfg.ModelSelection != nil && cfg.Router.LLM.URL != "") {
			next = chain.Alternate{Primary: next, Secondary: chat, Observe: observe}
		}
		return next
	}
	if strings.TrimSpace(cfg.Router.Decision.URL) == "" && (strings.TrimSpace(cfg.Router.Decision.Model) != "" || strings.TrimSpace(cfg.Router.Decision.KeyEnv) != "") {
		return errors.New("router.decision needs url when model or key_env is given; omit it entirely to decide with the chat service")
	}
	switch cfg.Router.Mode {
	case "llm":
		router = chat
	case "jev":
		router = decision()
	case "stages":
		// The ordered run needs no router of its own. The configured decision
		// service is handed to it only for the entrance question, and which
		// service that is follows router.decision/router.llm as in the other
		// two modes: the decision API when one is named, the chat API when not.
		entrance := chat
		if cfg.Router.Decision.Model != "" {
			entrance = decision()
		}
		router = chain.StageRouter{Entrance: entrance}
	case "single":
		// One role, run until it returns: the report about a stopped request
		// runs this way, so no decision service reads a stopped record.
		if len(cfg.Roles) != 1 {
			return errors.New("router.mode single runs exactly one configured role")
		}
		router = chain.OneRoleRouter{Role: cfg.Roles[0].Name}
	default:
		return errors.New("choose router.mode jev, llm or stages")
	}
	if *watch {
		if cfg.AssignedIssue != "" {
			return errors.New("watch assigns each issue; do not configure assigned_issue for the entire queue")
		}
		return watchRequests(ctx, cfg, *directory, log)
	}
	assigned := cfg.AssignedIssue
	if *issue != "" {
		if assigned != "" && assigned != *issue {
			return errors.New("configured tracker assignment differs from --issue")
		}
		assigned = *issue
	}
	prepareAccess, err := roleAccess(cfg, assigned)
	if err != nil {
		return err
	}
	readRequest := func(ctx context.Context) (string, error) {
		if *issue != "" {
			return cfg.Backlog.Request(ctx, *issue)
		}
		data, err := os.ReadFile(*requestPath)
		return string(data), err
	}
	store, err := acquireRequest(ctx, *directory, readRequest, 10*time.Second, observe)
	if err != nil {
		return err
	}
	defer store.Close()
	executor := chain.Processes{Roles: roles, Prepare: prepareAccess}
	if cfg.ModelSelection != nil {
		selection := *cfg.ModelSelection
		selection.observe = observe
		executor.SelectModel = selection.choose
		executor.ModelPrefix = selection.invocationPrefix()
	}
	engine := chain.Chain{
		Router:   router,
		Executor: executor, Store: store,
		Workflow: cfg.Workflow,
		Observe:  observe,
	}
	if cfg.Intake != nil {
		engine.WaitAfter = cfg.Intake.QuestionRole
	}
	// A question put to the requester is not a failure and not a completion.
	// A single run has nobody watching the issue for the answer, so say which
	// mode carries the request on instead of reporting it as finished.
	if err := engine.Run(ctx); err != nil {
		if errors.Is(err, chain.ErrWaiting) {
			return fmt.Errorf("%w; --watch resumes the request when the answer arrives", err)
		}
		return err
	}
	return nil
}
