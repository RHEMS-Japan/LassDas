package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestApprovalWaitingReturnsBeforeRouting(t *testing.T) {
	store := &approvalMemoryStore{state: State{}}
	c := Chain{Store: store, Router: approvalNoRouter{t}, Executor: approvalNoExecutor{t}}
	// Decode through the public checkpoint shape, so the pre-implementation
	// failure demonstrates ignored state rather than a missing Go identifier.
	decodeApprovalState(t, &store.state, `{"awaiting_approval":{"stage":"execute","post_n":1,"post_comment_id":9,"deadline":"2030-01-01T00:00:00Z"}}`)
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("approval wait was ignored")
	}
}

type approvalMemoryStore struct{ state State }

func (s *approvalMemoryStore) Load() (State, error) { return s.state, nil }
func (s *approvalMemoryStore) Save(v State) error   { s.state = v; return nil }

type approvalNoRouter struct{ t *testing.T }

func (r approvalNoRouter) Next(context.Context, State) (Assignment, error) {
	r.t.Fatal("approval wait routed")
	return Assignment{}, nil
}

type approvalNoExecutor struct{ t *testing.T }

func (r approvalNoExecutor) Execute(context.Context, Assignment, State) []Result {
	r.t.Fatal("approval wait launched")
	return nil
}

func decodeApprovalState(t *testing.T, state *State, data string) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), state); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalDigestVectors(t *testing.T) {
	paths, err := filepath.Glob("testdata/approval-digest-*.json")
	if err != nil || len(paths) != 3 {
		t.Fatalf("vectors: %v %v", paths, err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var vector struct {
				Files []struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				Digest string
				Error  string
			}
			if err = json.Unmarshal(raw, &vector); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, f := range vector.Files {
				files[f.Path] = []byte(f.Content)
			}
			digest, err := ApprovalDigest(files)
			if vector.Error != "" {
				if err == nil || !strings.Contains(err.Error(), vector.Error) {
					t.Fatalf("%v", err)
				}
			} else if err != nil || digest != vector.Digest {
				t.Fatalf("digest=%s err=%v", digest, err)
			}
		})
	}
}
func approvalWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestApprovalBundleRefusals(t *testing.T) {
	tests := []struct {
		name, reason string
		change       func(*testing.T, string)
	}{
		{"name", "bundle_name", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, ".hidden"), "x") }},
		{"nonascii_name", "bundle_name", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "日本語"), "x") }},
		{"symlink", "bundle_kind", func(t *testing.T, p string) {
			if err := os.Symlink("a", filepath.Join(p, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory_link", "bundle_kind", func(t *testing.T, p string) {
			if err := os.Symlink(".", filepath.Join(p, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", "bundle_kind", func(t *testing.T, p string) {
			if err := os.Link(filepath.Join(p, "a"), filepath.Join(p, "link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", "bundle_kind", func(t *testing.T, p string) {
			if err := syscall.Mkfifo(filepath.Join(p, "pipe"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"bytes", "bundle_limit", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), strings.Repeat("x\n", 16385)) }},
		{"count", "bundle_limit", func(t *testing.T, p string) {
			for i := 0; i < 16; i++ {
				approvalWrite(t, filepath.Join(p, fmt.Sprintf("f%d", i)), "x")
			}
		}},
		{"depth", "bundle_limit", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "b/c/d/e/f"), "x") }},
		{"line", "bundle_limit", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), strings.Repeat("日", 201)) }},
		{"spaces", "bundle_limit", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "x"+strings.Repeat(" \t", 9)) }},
		{"blank", "bundle_limit", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "x\n\n \n\t\nx") }},
		{"missing", "show_missing", func(t *testing.T, p string) {
			if err := os.Remove(filepath.Join(p, "a")); err != nil {
				t.Fatal(err)
			}
		}},
		{"credential", "bundle_credential", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "test-credential-value") }},
		{"masked", "post_masked", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "plan"), "test-credential-value") }},
		{"fence", "post_fence", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "```") }},
		{"utf8", "post_fence", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), string([]byte{0xff})) }},
		{"format", "post_fence", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "x\u202ey") }},
		{"control", "post_fence", func(t *testing.T, p string) { approvalWrite(t, filepath.Join(p, "a"), "x\ry") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			bundle := filepath.Join(root, "bundle")
			approvalWrite(t, filepath.Join(bundle, "a"), "x\n")
			tt.change(t, bundle)
			dest := filepath.Join(t.TempDir(), "bundle-1")
			_, err := CollectApprovalBundle(root, &Approval{Bundle: "bundle", Show: []string{"a"}}, dest, []string{"test-credential-value"})
			if err == nil || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("want %s got %v", tt.reason, err)
			}
			if _, err = os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("left refused copy")
			}
			if tt.name == "credential" {
				if _, err = os.Stat(filepath.Join(bundle, "a")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("credential remained")
				}
			}
		})
	}
}
func TestApprovalBundleCopyAndCollision(t *testing.T) {
	root := t.TempDir()
	approvalWrite(t, filepath.Join(root, "bundle/a"), "x\n")
	approvalWrite(t, filepath.Join(root, "bundle/b/c"), "second\n")
	dest := filepath.Join(t.TempDir(), "bundle-1")
	b, err := CollectApprovalBundle(root, &Approval{Bundle: "bundle", Show: []string{"a"}}, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	approvalWrite(t, filepath.Join(root, "bundle/a"), "changed")
	copy, err := ReadApprovalBundle(dest)
	if err != nil || copy.Digest != b.Digest {
		t.Fatal("copy changed")
	}
	if err = CheckApprovalCollision(b.Digest, []ApprovalPost{{Digest: b.Digest}}); err != nil {
		t.Fatal(err)
	}
	other := b.Digest[:8] + strings.Repeat("0", 56)
	if err = CheckApprovalCollision(b.Digest, []ApprovalPost{{Digest: other}}); err == nil {
		t.Fatal("prefix collision accepted")
	}
}
func TestApprovalCleanupConfinesPaths(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	approvalWrite(t, outside, "keep")
	approvalWrite(t, filepath.Join(root, "dir/a"), "old")
	if err := RemoveApprovalFile(root, filepath.Join(root, "dir"), "../dir/a"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveApprovalFile(root, root, "link"); err == nil {
		t.Fatal("followed link")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Fatal("outside changed")
	}
	if err := RemoveApprovalFile(root, root, "../escape"); err == nil {
		t.Fatal("escaped")
	}
}
func approvalLaunchFixture(t *testing.T) (Processes, State, ApprovalGO) {
	t.Helper()
	root := t.TempDir()
	directory := t.TempDir()
	approvalWrite(t, filepath.Join(root, "bundle/a"), "x\n")
	b, err := CollectApprovalBundle(root, &Approval{Bundle: "bundle", Show: []string{"a"}}, filepath.Join(directory, "bundle-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	post := ApprovalPost{N: 1, Stage: "execute", CommentID: 90, Digest: b.Digest, DigestPrefix: b.Digest[:8], Files: b.Files, Deadline: now.Add(time.Hour), CreatedAt: now}
	if err = post.Save(directory); err != nil {
		t.Fatal(err)
	}
	goRecord := ApprovalGO{Result: "go", PostN: 1, PostCommentID: 90, GOCommentID: 91, AuthorID: 7, Body: "GO " + b.Digest[:8], Digest: b.Digest, AcceptedAt: now, PendingSince: now, ConsumedAt: now, LaunchHistoryLen: 0}
	if err = goRecord.Save(directory); err != nil {
		t.Fatal(err)
	}
	state := State{Workflow: &Workflow{Stages: []Stage{{Name: "execute", Kind: CommandStage, Approval: &Approval{Bundle: "bundle"}}}}, Pending: &Assignment{Role: "execute"}, PendingSince: now}
	p := Processes{ApprovalDirectory: directory, Roles: map[string]Role{"execute": {Name: "execute", Processes: []Process{{Name: "p", Directory: root, Command: []string{"/bin/sh", "-c", "printf '%s\\n%s' \"$TASK_APPROVED_BUNDLE\" \"$TASK_APPROVED_DIGEST\""}, Env: map[string]string{"KEEP": "yes"}}}}}}
	return p, state, goRecord
}
func TestApprovalEnvironmentOnlyExactLaunch(t *testing.T) {
	for _, variant := range []string{"match", "position", "time", "stage", "unconsumed", "expired", "copy_changed", "rejection", "unreadable_go"} {
		t.Run(variant, func(t *testing.T) {
			p, state, g := approvalLaunchFixture(t)
			role := "execute"
			switch variant {
			case "copy_changed":
				approvalWrite(t, filepath.Join(p.ApprovalDirectory, "bundle-1/a"), "tampered after consumption\n")
			case "rejection", "unreadable_go":
				g.Result = variant
				if err := g.Save(p.ApprovalDirectory); err != nil {
					t.Fatal(err)
				}
			case "position":
				state.History = append(state.History, Result{})
			case "time":
				state.PendingSince = state.PendingSince.Add(time.Second)
			case "stage":
				role = "other"
				p.Roles[role] = p.Roles["execute"]
			case "unconsumed":
				g.ConsumedAt = time.Time{}
				if err := g.Save(p.ApprovalDirectory); err != nil {
					t.Fatal(err)
				}
			case "expired":
				g.ExpiredAt = time.Now()
				if err := g.Save(p.ApprovalDirectory); err != nil {
					t.Fatal(err)
				}
			}
			results := p.Execute(context.Background(), Assignment{Role: role}, state)
			if len(results) != 1 {
				t.Fatal(results)
			}
			if variant == "match" {
				if results[0].Error != "" || !strings.Contains(results[0].Output, g.Digest) {
					t.Fatalf("%+v", results)
				}
			} else if strings.Contains(results[0].Output, g.Digest) {
				t.Fatal("approval leaked")
			}
			if variant == "copy_changed" && results[0].Error == "" {
				t.Fatal("changed copy launched")
			}
			if len(p.Roles["execute"].Processes[0].Env) != 1 {
				t.Fatal("shared env mutated")
			}
		})
	}
}
func TestApprovalReceiptAndPrepareCleanup(t *testing.T) {
	p, state, _ := approvalLaunchFixture(t)
	role := p.Roles["execute"]
	role.Processes[0].Receipt = "receipt"
	role.Processes[0].Command = []string{"/bin/true"}
	p.Roles["execute"] = role
	file := filepath.Join(role.Processes[0].Directory, "receipt")
	approvalWrite(t, file, "old")
	p.Prepare = func(ctx context.Context, p Process) (Process, func(), error) {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("receipt survives Prepare")
		}
		p.Env["NEW"] = "value"
		return p, nil, nil
	}
	results := p.Execute(context.Background(), Assignment{Role: "execute"}, state)
	if results[0].ApprovalReceiptFailure != "receipt_unreadable" {
		t.Fatal(results)
	}
	if len(p.Roles["execute"].Processes[0].Env) != 1 {
		t.Fatal("Prepare mutated configured map")
	}
	p.CredentialValues = func() []string { return []string{"test-secret"} }
	role.Processes[0].Command = []string{"/bin/sh", "-c", "printf test-secret > receipt"}
	p.Roles["execute"] = role
	results = p.Execute(context.Background(), Assignment{Role: "execute"}, state)
	if results[0].ApprovalReceiptFailure != "bundle_credential" || strings.Contains(results[0].Receipt, "test-secret") {
		t.Fatalf("%+v", results)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential receipt retained")
	}
}
func TestApprovalFailureEnding(t *testing.T) {
	for _, tc := range []struct{ name, reason, text string }{
		{"receipt_unreadable", "receipt_unreadable", "起動した・結果は読めない (効果不明): receipt_unreadable"},
		{"receipt_credential", "bundle_credential", "起動した・結果は読めない (効果不明): bundle_credential"},
		{"bundle_credential", "bundle_credential", "承認条件が成立しなかった: bundle_credential"},
		{"post_fence", "post_fence", "承認条件が成立しなかった: post_fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := ApprovalFailureRecord("execute", tc.reason)
			record.Error = tc.text
			state := State{Workflow: &Workflow{Stages: []Stage{{Name: "plan", Kind: ModelStage}, {Name: "execute", Kind: CommandStage, OnFailure: "plan", Approval: &Approval{}}}}, History: []Result{{Role: "execute", Speaker: "p"}, {Role: "execute", Speaker: "runtime"}, record}}
			ending, ok := state.LastEnding()
			if !ok || ending.ApprovalFailure != tc.reason || ending.Error != tc.text || ending.Attempt != 1 {
				t.Fatalf("%+v", ending)
			}
			want := "execute: " + tc.text + "\n"
			if text := ending.instruction(); text != want {
				t.Fatalf("instruction = %q, want %q", text, want)
			}
			text := state.stageInstruction("plan")
			if !strings.Contains(text, want) || strings.Contains(text, "did not exit 0") {
				t.Fatal(text)
			}
		})
	}
}

func TestApprovalPredecessorCleansOnlyShowAndOwnReceipt(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"bundle/show", "bundle/plan", "prior-receipt", "later-receipt"} {
		approvalWrite(t, filepath.Join(root, name), "old")
	}
	workflow := &Workflow{Stages: []Stage{{Name: "plan", Kind: ModelStage}, {Name: "check", Kind: CommandStage, OnFailure: "plan"}, {Name: "execute", Kind: CommandStage, OnFailure: "plan", Approval: &Approval{Bundle: "bundle", Show: []string{"show"}}}, {Name: "deliver", Kind: CommandStage, OnFailure: "plan"}}}
	p := Processes{Roles: map[string]Role{"check": {Name: "check", Processes: []Process{{Name: "p", Directory: root, Receipt: "prior-receipt", Command: []string{"/bin/true"}}}}, "deliver": {Name: "deliver", Processes: []Process{{Name: "p", Directory: root, Receipt: "later-receipt", Command: []string{"/bin/true"}}}}}}
	p.Prepare = func(ctx context.Context, p Process) (Process, func(), error) {
		if p.Receipt == "prior-receipt" {
			for _, name := range []string{"bundle/show", "prior-receipt"} {
				if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("not cleared before Prepare: %s", name)
				}
			}
		}
		return p, nil, nil
	}
	p.Execute(context.Background(), Assignment{Role: "check"}, State{Workflow: workflow})
	for _, name := range []string{"bundle/plan", "later-receipt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("removed unrelated %s", name)
		}
	}
	results := p.Execute(context.Background(), Assignment{Role: "deliver"}, State{Workflow: workflow})
	if !strings.Contains(results[0].Receipt, "old") {
		t.Fatal("delivery receipt removed")
	}
}
func TestApprovalReceiptFailureIsRecordedAfterStage(t *testing.T) {
	for _, variant := range []string{"receipt_unreadable", "bundle_credential", "predecessor_credential", "predecessor_unreadable"} {
		t.Run(variant, func(t *testing.T) {
			role, reason := "execute", variant
			if strings.HasPrefix(variant, "predecessor") {
				role = "check"
				reason = "bundle_credential"
				if variant == "predecessor_unreadable" {
					reason = "receipt_unreadable"
				}
			}
			workflow := &Workflow{Stages: []Stage{{Name: "plan", Kind: ModelStage}, {Name: "check", Kind: CommandStage, OnFailure: "plan"}, {Name: "execute", Kind: CommandStage, OnFailure: "plan", Approval: &Approval{}}}}
			store := &memoryStore{state: State{Workflow: workflow, History: []Result{{Role: "plan", Speaker: "runtime"}, {Role: "check", Speaker: "runtime"}}}}
			if role == "check" {
				store.state.History = []Result{{Role: "plan", Speaker: "runtime"}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			launched := false
			runner := Chain{Store: store, BeforeApproval: func(context.Context, *State, Assignment) error { return nil }, Router: testRouter(func(context.Context, State) (Assignment, error) {
				if launched {
					cancel()
					return Assignment{}, context.Canceled
				}
				return Assignment{Role: role}, nil
			}), Executor: testExecutor(func(context.Context, Assignment, State) []Result {
				launched = true
				return []Result{{Role: role, Speaker: "p", ApprovalReceiptFailure: reason, FinishedAt: time.Now().UTC()}}
			})}
			_ = runner.Run(ctx)
			if store.state.Recovering || store.state.Step != role {
				t.Fatal("approval refusal changed the completed command state")
			}
			last := store.state.History[len(store.state.History)-1]
			if variant == "bundle_credential" && last.Error != approvalReceiptPrefix+reason {
				t.Fatalf("receipt result: %s", last.Error)
			}
			if role == "execute" {
				ending, ok := store.state.LastEnding()
				if !ok || ending.Error != last.Error {
					t.Fatalf("ending = %+v, record = %+v", ending, last)
				}
			}
			if variant == "predecessor_unreadable" {
				if ApprovalFailureReason(last) != "" {
					t.Fatal("predecessor unreadable is not an approval failure")
				}
			} else if last.Role != "execute" || ApprovalFailureReason(last) != reason {
				t.Fatalf("%+v", store.state.History)
			}
		})
	}
}
