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
	"syscall"

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
	directory := flags.String("run-dir", "", "private directory for this request's history")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showModels {
		if *configPath != "" || *requestPath != "" || *issue != "" || *directory != "" || flags.NArg() != 0 {
			return errors.New("--list-models is a separate catalog query; do not combine it with a request")
		}
		return writeModelList(ctx, output)
	}
	if *configPath == "" || (*requestPath == "") == (*issue == "") || *directory == "" {
		return errors.New("provide --config, --run-dir and exactly one of --request or --issue")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
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
		roles[role.Name], purposes[role.Name] = role, role.Purpose
	}
	if len(roles) == 0 {
		return errors.New("no roles configured")
	}
	var request string
	if *issue != "" {
		request, err = cfg.Backlog.Request(ctx, *issue)
	} else {
		var data []byte
		data, err = os.ReadFile(*requestPath)
		request = string(data)
	}
	if err != nil {
		return err
	}
	store, err := chain.Open(*directory, request)
	if err != nil {
		return err
	}
	defer store.Close()
	observe := func(message string) { fmt.Fprintln(log, message) }
	var router chain.Router
	chat := chain.ChatRouter{Service: cfg.Router.LLM, Roles: purposes, Instructions: cfg.Instructions}
	switch cfg.Router.Mode {
	case "llm":
		router = chat
	case "jev":
		router = chain.DecisionRouter{Judge: cfg.Router.Decision, Roles: purposes, Instructions: cfg.Instructions}
		if cfg.Router.LLM.Model != "" {
			router = chain.Alternate{Primary: router, Secondary: chat, Observe: observe}
		}
	default:
		return errors.New("choose router.mode jev or llm")
	}
	executor := chain.Processes{Roles: roles}
	if cfg.ModelSelection != nil {
		executor.SelectModel = cfg.ModelSelection.choose
	}
	engine := chain.Chain{
		Router:   router,
		Executor: executor, Store: store,
		Observe: observe,
	}
	return engine.Run(ctx)
}
