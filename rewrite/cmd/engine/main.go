package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"reflect"
	"strconv"
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
	Backlog        tracker.Backlog  `json:"backlog,omitzero"`
	GitHub         *tracker.GitHub  `json:"github,omitempty"`
	ModelSelection *selectionConfig `json:"model_selection,omitempty"`
	Intake         *intakeConfig    `json:"intake,omitempty"`
	AssignedIssue  string           `json:"assigned_issue,omitempty"`
	Workflow       *chain.Workflow  `json:"workflow,omitempty"`
	// runtimeUser is the tracker account the credential belongs to, read at
	// start when issues are handed over.
	runtimeUser tracker.Account
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

// A key the engine does not know is refused, not skipped. A setting spelled
// wrong would otherwise be no setting at all: a misspelled intake filter takes
// up every issue of the project, a misspelled stop list leaves the operator
// unable to stop a request. The same goes for a key written twice in one
// object, where the later one would win without a word. The engine says which
// key it is and where, and starts nothing.
func readConfig(data []byte) (config, error) {
	var cfg config
	keys := json.NewDecoder(bytes.NewReader(data))
	if err := checkKeys(keys, reflect.TypeOf(cfg), &[]string{}); err != nil {
		// A text that is not JSON at all is described by the decoder below.
		var refused keyRefusal
		if errors.As(err, &refused) {
			return config{}, fmt.Errorf("reading the configuration: %w", err)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("reading the configuration: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return config{}, errors.New("reading the configuration: text follows the configuration object")
	}
	if err := validateGitHubConfig(cfg, data); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// keyRefusal is a key of the configuration text that the engine does not
// take: one it does not know, or one written twice.
type keyRefusal string

func (r keyRefusal) Error() string { return string(r) }

// checkKeys reads one JSON value beside the Go type it will be decoded into.
// In an object decoded into a struct, every key must be one of the struct's
// names, spelled exactly: the decoder itself matches without regard to letter
// case, so `Category_IDs` beside `category_ids` would be the same setting
// twice. In any object a key may be written once. A value whose type is not
// known here (a map's values, a list's items) is still walked for the keys
// below it. The place is kept as its parts and written out only for a
// refusal; a text nested deeper than the decoder reads is left for the
// decoder to refuse.
func checkKeys(decoder *json.Decoder, expected reflect.Type, place *[]string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	opening, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if len(*place) > 10000 {
		return errors.New("nested deeper than a configuration is read")
	}
	for expected != nil && expected.Kind() == reflect.Pointer {
		expected = expected.Elem()
	}
	var fields map[string]reflect.Type
	var item reflect.Type
	if expected != nil {
		switch expected.Kind() {
		case reflect.Struct:
			// A list where an object belongs is for the decoder to describe.
			if opening == '{' {
				fields = map[string]reflect.Type{}
				for i := 0; i < expected.NumField(); i++ {
					field := expected.Field(i)
					name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
					if !field.IsExported() || name == "-" {
						continue
					}
					if name == "" {
						name = field.Name
					}
					fields[name] = field.Type
				}
			}
		case reflect.Map:
			if opening == '{' {
				item = expected.Elem()
			}
		case reflect.Slice, reflect.Array:
			if opening == '[' {
				item = expected.Elem()
			}
		}
	}
	// Said of a key: the object it is in, by the path that leads there.
	in := func() string {
		if len(*place) == 0 {
			return "at the top level"
		}
		return "in " + strings.Join(*place, "")
	}
	seen := map[string]bool{}
	for index := 0; decoder.More(); index++ {
		part, next := "["+strconv.Itoa(index)+"]", item
		if opening == '{' {
			name, err := decoder.Token()
			if err != nil {
				return err
			}
			key, _ := name.(string)
			if seen[key] {
				return keyRefusal(fmt.Sprintf("the key %q is written twice %s; the later one would win without a word", key, in()))
			}
			seen[key] = true
			if fields != nil {
				known := false
				if next, known = fields[key]; !known {
					return keyRefusal(fmt.Sprintf("unknown key %q %s", key, in()))
				}
			}
			if part = key; len(*place) > 0 {
				part = "." + key
			}
		}
		*place = append(*place, part)
		if err := checkKeys(decoder, next, place); err != nil {
			return err
		}
		*place = (*place)[:len(*place)-1]
	}
	_, err = decoder.Token()
	return err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, log io.Writer) (failure error) {
	flags := flag.NewFlagSet("engine", flag.ContinueOnError)
	flags.SetOutput(log)
	configPath := flags.String("config", "", "configured roles and router")
	requestPath := flags.String("request", "", "original request as text")
	issue := flags.String("issue", "", "read the original issue from the configured tracker")
	showModels := flags.Bool("list-models", false, "fetch the current OpenRouter catalog; no request is run")
	watch := flags.Bool("watch", false, "poll the explicitly configured intake into separate request directories")
	directory := flags.String("run-dir", "", "private directory for this request's history")
	logFile := flags.String("log-file", "", "also append the runtime's own observations to this file (the status page reads it)")
	check := flags.Bool("check", false, "read the configuration and run the checks of a --watch start, say which issues the watch would take up, and start nothing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *check && (*configPath == "" || *requestPath != "" || *issue != "" || *directory != "" || *logFile != "" || *watch || *showModels || flags.NArg() != 0) {
		return errors.New("--check reads --config and starts nothing; do not combine it with anything else")
	}
	if *logFile != "" {
		file, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return fmt.Errorf("opening --log-file: %w", err)
		}
		defer file.Close()
		// The status page shows this file, not the process's standard error.
		// A runtime that could not start, or stopped, says why here too.
		defer func() {
			if failure != nil && !errors.Is(failure, context.Canceled) {
				fmt.Fprintln(file, "the runtime stopped: "+failure.Error())
			}
		}()
		log = io.MultiWriter(log, file)
	}
	if *showModels {
		if *configPath != "" || *requestPath != "" || *issue != "" || *directory != "" || *watch || flags.NArg() != 0 {
			return errors.New("--list-models is a separate catalog query; do not combine it with a request")
		}
		return writeModelList(ctx, output)
	}
	if !*check && (*configPath == "" || *directory == "" || flags.NArg() != 0 ||
		(*watch && (*requestPath != "" || *issue != "")) ||
		(!*watch && (*requestPath == "") == (*issue == ""))) {
		return errors.New("provide --config, --run-dir and either --watch or exactly one of --request/--issue")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	cfg, err := readConfig(data)
	if err != nil {
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
		for _, process := range role.Processes {
			if process.TimeoutMinutes < 0 {
				return errors.New("a process's timeout_minutes must not be negative")
			}
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
	services := []chain.Jev{cfg.Router.Decision, cfg.Router.LLM}
	if cfg.ModelSelection != nil {
		services = append(services, cfg.ModelSelection.Judge)
		if cfg.ModelSelection.Fallback != nil {
			services = append(services, *cfg.ModelSelection.Fallback)
		}
	}
	for _, service := range services {
		if service.TimeoutMinutes < 0 {
			return errors.New("a model service's timeout_minutes must not be negative")
		}
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
	if *check {
		// The check is of a watch's start: everything above and the watch's
		// own checks ran as they would there, so what passes here is what
		// `--watch` starts on. Nothing was created, read from a service or
		// written.
		if cfg.AssignedIssue != "" {
			return errors.New("watch assigns each issue; do not configure assigned_issue for the entire queue")
		}
		_, since, _, _, err := watchSettings(&cfg, "queue")
		if err != nil {
			return err
		}
		fmt.Fprintln(output, intakeScope(cfg, since))
		fmt.Fprintln(output, "the configuration is accepted; nothing was started")
		return nil
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
			return cfg.source().Request(ctx, *issue)
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
		// A stage is announced when it begins, which is before this launch has
		// returned anything to the history. Write the choice down as it is made
		// so the announcement can say which model took the work on. A runtime
		// that declares its models keeps every launch's choice, so a stage
		// launched again is declared when its models change.
		declaring := cfg.Intake != nil && cfg.Intake.DeclareModels
		executor.Chosen = func(role, _, model string, launch int) {
			record := func() error { return recordChosen(*directory, role, model) }
			if declaring {
				record = func() error { return recordLaunch(*directory, role, model, launch) }
			}
			if err := record(); err != nil {
				observe("the model chosen for " + role + " was not written down for its announcement: " + err.Error())
			}
		}
	}
	questionAware, err := questionExecutor(cfg, assigned, *directory, executor)
	if err != nil {
		return err
	}
	engine := chain.Chain{
		Router:   router,
		Executor: questionAware, Store: store,
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
