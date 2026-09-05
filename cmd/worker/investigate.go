package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// The investigate verb is the investigating designer's card body: it builds
// the probe session from the consumer's catalogue and the kernel's own
// identities, drives the model, and seals the round's records into the run
// directory (docs/INVESTIGATING_DESIGNER.md §3, §4). Exit 0 means the
// records were sealed; exit 1 with `incomplete.json` in the round directory
// means the round ended honestly without them; any other failure is the
// kernel's own.

const (
	// investigationWallSeconds is the role's budget across the rounds of one
	// request (§3.1); the card wall outlasts it.
	investigationWallSeconds = 1800
	incompleteFile           = "incomplete.json"
)

func runInvestigate(ctx context.Context, args []string) error {
	flags := commandFlags("investigate")
	configPath := flags.String("config", "", "")
	toolSHA := flags.String("tool-sha", "", "")
	draftPath := flags.String("draft", "", "")
	repoRoot := flags.String("repo-root", "", "")
	baseSHA := flags.String("base-sha", "", "")
	round := flags.Int("round", 0, "")
	mode := flags.String("mode", "", "")
	measurementsPath := flags.String("measurements", "", "")
	outDir := flags.String("out-dir", "", "")
	previousDir := flags.String("previous-dir", "", "")
	sessionSeed := flags.String("session-seed", "", "")
	sessionState := flags.String("session-state", "", "")
	if !parseFlags(flags, args) || !allPresent(*configPath, *toolSHA, *draftPath, *repoRoot, *baseSHA, *measurementsPath, *outDir) ||
		!worker.ValidToolSHA(*toolSHA) || *round < 1 || *round > 20 || (*mode != worker.ModeInvestigation && *mode != worker.ModeDesign) {
		return errors.New("investigate arguments are invalid")
	}
	config, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	if config.Models.Designer == nil {
		return errors.New("consumer configuration names no designer model")
	}
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(*draftPath, worker.MaxTicketJSONBytes, &draft); err != nil || draft.ToolSHA != *toolSHA {
		return errors.New("ticket draft could not be read")
	}
	catalog, err := config.ProbeCatalog()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	recorder, err := probe.OpenRecorder(*measurementsPath)
	if err != nil {
		return err
	}
	identity := investigate.Identity{DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256, ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: *baseSHA}
	carry, previous, err := previousRound(*previousDir, identity, *measurementsPath)
	if err != nil {
		return err
	}
	if carry.ElapsedSeconds >= investigationWallSeconds || carry.ProbesUsed >= probe.DefaultLimits.MaxProbes {
		return writeIncomplete(*outDir, "the request's investigation budget was spent in earlier rounds")
	}
	consumer, err := config.ConsumerFor(draft.Repository)
	if err != nil {
		return errors.New("ticket repository is not a configured consumer")
	}
	session := &probe.Session{
		Catalog: catalog, Recorder: recorder, Limits: probe.DefaultLimits,
		Env:      probe.EnvFromProcess(probe.ExecEnvironmentNames),
		RepoRoot: *repoRoot,
		DSN:      dsnLookup(catalog),
		Jar:      observationJar(*sessionSeed, *sessionState),
		Used:     carry.ProbesUsed,
	}
	request := worker.TicketRequest{
		SchemaVersion: draft.SchemaVersion, DeliveryID: draft.DeliveryID, InputSHA256: draft.InputSHA256, ConfigSHA256: draft.ConfigSHA256,
		ToolSHA: draft.ToolSHA, IssueKey: draft.IssueKey, RunID: draft.RunID, Repository: draft.Repository, Mode: draft.Mode,
		Summary: draft.Summary, VerificationPath: draft.VerificationPath, ExpectedText: draft.ExpectedText, AbsentText: draft.AbsentText, Request: draft.Request,
	}
	input := worker.InvestigationInput{
		Identity: identity, Round: *round, Mode: *mode, Request: request, Session: session, MeasurementsPath: *measurementsPath,
		Bounds:       investigate.Bounds{AllowedFilePrefixes: consumer.Mode.AllowedFilePrefixes, MaxFiles: consumer.Mode.MaxFiles, Catalog: catalog, RepoRoot: *repoRoot},
		ElapsedCarry: carry.ElapsedSeconds, Previous: previous,
	}
	invoker, err := newModelInvoker(ctx, *config.Models.Designer)
	if err != nil {
		return err
	}
	remaining := time.Duration(investigationWallSeconds-carry.ElapsedSeconds) * time.Second
	roundCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	started := time.Now()
	result, err := invoker.Investigate(roundCtx, *config.Models.Designer, input, started)
	if errors.Is(err, worker.ErrInvestigationIncomplete) {
		return writeIncomplete(*outDir, result.Incomplete)
	}
	if err != nil {
		return err
	}
	if err := investigate.Write(filepath.Join(*outDir, "investigation.json"), result.Investigation); err != nil {
		return err
	}
	if result.Design != nil {
		if err := investigate.Write(filepath.Join(*outDir, "design.json"), *result.Design); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*outDir, "DESIGN.md"), []byte(investigate.RenderDesign(*result.Design, result.Investigation)), 0o644); err != nil {
			return err
		}
	}
	usage, _ := json.Marshal(struct {
		Turns int                    `json:"turns"`
		Usage worker.InvocationUsage `json:"usage"`
	}{Turns: result.Turns, Usage: result.Usage})
	if err := os.WriteFile(filepath.Join(*outDir, "invocation.json"), append(usage, '\n'), 0o644); err != nil {
		return err
	}
	// The report seals the budget spent up to the report; the design phase
	// spends more wall time after it. The round file carries the whole
	// round's spend so the next round is charged for all of it.
	spent, _ := json.Marshal(roundSpend{ProbesUsed: session.Used, ElapsedSeconds: carry.ElapsedSeconds + int(time.Since(started).Seconds())})
	return os.WriteFile(filepath.Join(*outDir, "round.json"), append(spent, '\n'), 0o644)
}

// roundSpend is the whole round's budget use, report and design phases
// together (§5: the next round continues from it).
type roundSpend struct {
	ProbesUsed     int `json:"probes_used"`
	ElapsedSeconds int `json:"elapsed_seconds"`
}

// previousRound reads the earlier round's sealed records, when there was
// one: the budget it spent (carried into this round) and, as the model's
// context, its design and the reviews that sent it back.
func previousRound(dir string, identity investigate.Identity, measurementsPath string) (investigate.Budget, []byte, error) {
	if dir == "" {
		return investigate.Budget{}, nil, nil
	}
	investigation, err := investigate.ReadInvestigation(filepath.Join(dir, "investigation.json"))
	if err != nil {
		return investigate.Budget{}, nil, errors.New("previous investigation could not be read")
	}
	if err := investigation.Validate(identity, measurementsPath); err != nil {
		return investigate.Budget{}, nil, fmt.Errorf("previous investigation does not verify: %w", err)
	}
	carry := investigate.Budget{ProbesUsed: investigation.ProbesUsed, ElapsedSeconds: investigation.ElapsedSeconds}
	if raw, err := os.ReadFile(filepath.Join(dir, "round.json")); err == nil {
		var spent roundSpend
		if json.Unmarshal(raw, &spent) == nil && spent.ProbesUsed >= carry.ProbesUsed && spent.ElapsedSeconds >= carry.ElapsedSeconds {
			carry = investigate.Budget{ProbesUsed: spent.ProbesUsed, ElapsedSeconds: spent.ElapsedSeconds}
		}
	}
	context := map[string]json.RawMessage{}
	for _, name := range []string{"design.json", "decision.json", "objection.json"} {
		if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil && json.Valid(raw) {
			context[strings.TrimSuffix(name, ".json")] = raw
		}
	}
	reviews, _ := filepath.Glob(filepath.Join(dir, "*-design-review.json"))
	var findings []json.RawMessage
	for _, path := range reviews {
		if raw, err := os.ReadFile(path); err == nil && json.Valid(raw) {
			findings = append(findings, raw)
		}
	}
	if len(findings) > 0 {
		encoded, _ := json.Marshal(findings)
		context["reviews"] = encoded
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		return carry, nil, err
	}
	return carry, encoded, nil
}

// dsnLookup hands the kernel's connection strings to the session by the
// names the catalogue declares — and no other variable.
func dsnLookup(catalog probe.Catalog) func(string) string {
	declared := map[string]bool{}
	for _, spec := range catalog.Specs() {
		if spec.DSNEnv != "" {
			declared[spec.DSNEnv] = true
		}
	}
	return func(name string) string {
		if !declared[name] {
			return ""
		}
		return os.Getenv(name)
	}
}

// observationJar loads the screen check's session for the http probes that
// declare it; the probes read it and never write it back.
func observationJar(seed, state string) []probe.Cookie {
	if seed == "" && state == "" {
		return nil
	}
	cookies, _ := visiblecheck.LoadSessionCookies(seed, state)
	out := make([]probe.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		out = append(out, probe.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure})
	}
	return out
}

// writeIncomplete records why the round sealed nothing and fails the card.
func writeIncomplete(outDir, reason string) error {
	encoded, _ := json.Marshal(map[string]string{"reason": reason})
	if err := os.WriteFile(filepath.Join(outDir, incompleteFile), append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	return fmt.Errorf("investigation incomplete: %s", reason)
}
