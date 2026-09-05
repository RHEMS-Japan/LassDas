package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// preserveAnswers writes the adopted answers of a resumed run into the
// knowledge tree the reception reads (worker.LoadPreservedAnswers), so the
// next ticket to the same destination is not asked a point the requester
// already settled. The workflow did this by committing the record to the
// instance repository from a job of its own; the pod's knowledge tree is the
// operator's copy on the state volume (knowledge_root, which must be
// writable for this), so the record goes there directly, at the path the
// instance configured (answer_knowledge.to). Best-effort after the report
// sealed: a run that ended is ended, and a failure here is logged for the
// operator, never turned into a failed terminal (issue #59 — the record was
// never written on the pod, and a requester answered the same scope
// question on consecutive tickets).
func (t *Terminal) preserveAnswers() {
	artifact, err := renderPreservedAnswers(t.config, t.envelope)
	if err != nil {
		t.logger.Error("adopted answers not preserved", "reason", err.Error())
		return
	}
	if !artifact.Enabled {
		return
	}
	path, written, err := writePreservedAnswers(t.config.KnowledgeRoot, artifact)
	if err != nil {
		t.logger.Error("adopted answers not preserved", "reason", err.Error())
		return
	}
	if written {
		t.logger.Info("adopted answers preserved", "issue_key", artifact.IssueKey, "path", path)
	}
}

// renderPreservedAnswers renders the run's adopted answers from the sealed
// envelope the run was claimed with: the snapshot, re-sealed into the raw
// ticket exactly as read-ticket seals it, and the cumulative clarification
// record. The workspace is not consulted — a resumed run that died before
// read-ticket (a pin mismatch at Prepare, seen live) left nothing there,
// and what the requester decided must not depend on how far the run got.
// A never-resumed envelope carries no clarification and nothing to preserve
// (Enabled false, no error); so does an instance that named no destination.
func renderPreservedAnswers(config runtime.Config, envelope hook.DispatchEnvelope) (worker.AnswerKnowledgeArtifact, error) {
	if envelope.ClarificationJSON == "" {
		return worker.AnswerKnowledgeArtifact{}, nil
	}
	if len(envelope.ClarificationJSON) > worker.MaxClarificationJSONBytes {
		return worker.AnswerKnowledgeArtifact{}, errors.New("clarification record is larger than allowed")
	}
	clarification, err := worker.DecodeClarificationContext([]byte(envelope.ClarificationJSON))
	if err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("clarification record is invalid")
	}
	workerConfig, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("worker configuration could not be loaded")
	}
	raw, err := worker.ReadRawTicket(envelope, workerConfig, config.Identity.EngineSHA)
	if err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("raw ticket could not be sealed from the envelope")
	}
	return worker.PreserveAnswerKnowledge(workerConfig, raw, clarification)
}

// writePreservedAnswers places the rendered record under knowledgeRoot at
// the artifact's path. The path was validated by the worker (a relative
// destination without hidden components, plus the ticket key), and is
// checked again to stay inside the tree; a symlink at the record or its
// directory is refused, as the reader skips symlinks. An identical record
// already there is left alone; a different one — a later revision of the
// same ticket — is replaced through a rename, so a reader never sees a
// partial file.
func writePreservedAnswers(knowledgeRoot string, artifact worker.AnswerKnowledgeArtifact) (string, bool, error) {
	if knowledgeRoot == "" || artifact.Path == "" || len(artifact.Content) == 0 {
		return "", false, errors.New("answer knowledge destination is incomplete")
	}
	root, err := filepath.Abs(knowledgeRoot)
	if err != nil {
		return "", false, errors.New("knowledge root is invalid")
	}
	target := filepath.Join(root, filepath.FromSlash(artifact.Path))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || strings.HasPrefix(relative, "..") {
		return "", false, errors.New("answer knowledge path leaves the knowledge tree")
	}
	if err := refuseSymlink(filepath.Dir(target)); err != nil {
		return "", false, err
	}
	if err := refuseSymlink(target); err != nil {
		return "", false, err
	}
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, artifact.Content) {
		return target, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", false, fmt.Errorf("answer knowledge directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return "", false, fmt.Errorf("answer knowledge write: %w", err)
	}
	name := temporary.Name()
	if _, err := temporary.Write(artifact.Content); err != nil {
		_ = temporary.Close()
		_ = os.Remove(name)
		return "", false, fmt.Errorf("answer knowledge write: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		_ = os.Remove(name)
		return "", false, fmt.Errorf("answer knowledge write: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(name)
		return "", false, fmt.Errorf("answer knowledge write: %w", err)
	}
	if err := os.Rename(name, target); err != nil {
		_ = os.Remove(name)
		return "", false, fmt.Errorf("answer knowledge write: %w", err)
	}
	return target, true, nil
}

// refuseSymlink fails when path exists and is a symbolic link; a missing
// path is fine (it is about to be created).
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("answer knowledge path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("answer knowledge path is a symbolic link")
	}
	return nil
}
