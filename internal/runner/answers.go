package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// preserveAnswers writes the adopted answers of a resumed run into the
// knowledge tree the reception reads (worker.LoadPreservedAnswers), so the
// next ticket to the same destination is not asked a point the requester
// already settled. The workflow did this by committing the record to the
// instance repository from a job of its own; the pod reads a copy of that
// tree on its state volume, so the record goes there directly, at the path
// the instance configured (answer_knowledge.to). Best-effort after the
// report sealed: a run that ended is ended, and a failure here is logged
// for the operator, never turned into a failed terminal (issue #59 — the
// record was never written on the pod, and a requester answered the same
// scope question on consecutive tickets).
func (t *Terminal) preserveAnswers() {
	artifact, err := renderPreservedAnswers(t.config, t.workspace)
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
// artifacts in its workspace. A run that was never resumed has no
// clarification artifact and nothing to preserve (Enabled false, no error);
// so does an instance that named no destination.
func renderPreservedAnswers(config runtime.Config, workspace string) (worker.AnswerKnowledgeArtifact, error) {
	encoded, err := os.ReadFile(filepath.Join(workspace, "clarification.json"))
	if errors.Is(err, os.ErrNotExist) {
		return worker.AnswerKnowledgeArtifact{}, nil
	}
	if err != nil || len(encoded) == 0 || len(encoded) > worker.MaxClarificationJSONBytes {
		return worker.AnswerKnowledgeArtifact{}, errors.New("clarification artifact could not be read")
	}
	clarification, err := worker.DecodeClarificationContext(encoded)
	if err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("clarification artifact is invalid")
	}
	workerConfig, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("worker configuration could not be loaded")
	}
	var raw worker.RawTicket
	if err := worker.ReadJSONFile(filepath.Join(workspace, "raw-ticket.json"), worker.MaxTicketJSONBytes, &raw); err != nil {
		return worker.AnswerKnowledgeArtifact{}, errors.New("raw ticket artifact could not be read")
	}
	return worker.PreserveAnswerKnowledge(workerConfig, raw, clarification)
}

// writePreservedAnswers places the rendered record under knowledgeRoot at
// the artifact's path. The path was validated by the worker (a relative
// destination without hidden components, plus the ticket key), and is
// checked again to stay inside the tree. An identical record already there
// is left alone; a different one — a later revision of the same ticket — is
// replaced through a rename, so a reader never sees a partial file.
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
