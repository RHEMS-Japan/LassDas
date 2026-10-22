package chain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var ErrAwaitingApproval = errors.New("the request is waiting for approval")

// ApprovalFailure is a runtime refusal, never a command's exit status.
type ApprovalFailure string

func (r ApprovalFailure) Error() string { return ApprovalFailureText(string(r)) }

var approvalReasons = []string{"bundle_name", "bundle_kind", "bundle_limit", "bundle_credential", "show_missing", "post_size", "post_fence", "post_masked", "digest_collision", "receipt_unreadable"}

const approvalFailurePrefix = "承認条件が成立しなかった: "
const approvalReceiptPrefix = "起動した・結果は読めない (効果不明): "

func ApprovalFailureText(reason string) string {
	if reason == "receipt_unreadable" {
		return approvalReceiptPrefix + reason
	}
	return approvalFailurePrefix + reason
}
func ApprovalFailureReason(result Result) string {
	if result.Speaker != "runtime" {
		return ""
	}
	for _, reason := range approvalReasons {
		if result.Error == ApprovalFailureText(reason) || reason == "bundle_credential" && result.Error == approvalReceiptPrefix+reason {
			return reason
		}
	}
	return ""
}
func ApprovalFailureRecord(stage, reason string) Result {
	return Result{Role: stage, Speaker: "runtime", Error: ApprovalFailureText(reason), FinishedAt: time.Now().UTC()}
}

type AwaitingApproval struct {
	Stage         string    `json:"stage"`
	PostN         int       `json:"post_n"`
	PostCommentID int64     `json:"post_comment_id"`
	Deadline      time.Time `json:"deadline"`
}
type ApprovalFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type ApprovalPost struct {
	N              int            `json:"n"`
	Stage          string         `json:"stage"`
	CommentID      int64          `json:"comment_id"`
	SentTextSHA256 string         `json:"sent_text_sha256"`
	Digest         string         `json:"digest"`
	DigestPrefix   string         `json:"digest_prefix"`
	Deadline       time.Time      `json:"deadline"`
	CreatedAt      time.Time      `json:"created_at"`
	Files          []ApprovalFile `json:"files"`
}
type ApprovalGO struct {
	Result           string    `json:"result"`
	PostN            int       `json:"post_n"`
	PostCommentID    int64     `json:"post_comment_id"`
	GOCommentID      int64     `json:"go_comment_id"`
	AuthorID         int64     `json:"author_id"`
	Body             string    `json:"body"`
	AcceptedAt       time.Time `json:"accepted_at"`
	Digest           string    `json:"digest"`
	LaunchHistoryLen int       `json:"launch_history_len"`
	PendingSince     time.Time `json:"pending_since"`
	ConsumedAt       time.Time `json:"consumed_at,omitzero"`
	ExpiredAt        time.Time `json:"expired_at,omitzero"`
}

func (p ApprovalPost) BundlePath(directory string) string {
	return filepath.Join(directory, fmt.Sprintf("bundle-%d", p.N))
}
func (p ApprovalPost) Save(directory string) error {
	return SaveApprovalJSON(filepath.Join(directory, fmt.Sprintf("post-%d.json", p.N)), p)
}
func (g ApprovalGO) Save(directory string) error {
	return SaveApprovalJSON(filepath.Join(directory, fmt.Sprintf("go-%d.json", g.GOCommentID)), g)
}
func SaveApprovalJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".approval-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func ReadApprovalRecords[T any](directory, prefix string) ([]T, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var values []T
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix+"-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := readReceipt(directory, directory, entry.Name())
		if err != nil {
			return nil, err
		}
		var value T
		if err = json.Unmarshal(raw, &value); err != nil {
			return nil, errors.New("approval record is unreadable")
		}
		expected := ""
		switch record := any(value).(type) {
		case ApprovalPost:
			expected = fmt.Sprintf("post-%d.json", record.N)
		case ApprovalGO:
			expected = fmt.Sprintf("go-%d.json", record.GOCommentID)
		}
		if expected != "" && entry.Name() != expected {
			return nil, errors.New("approval record filename does not match its identity")
		}
		values = append(values, value)
	}
	return values, nil
}
func (s State) ApprovalStage(role string) (Stage, string, bool) {
	if s.Workflow != nil {
		for i, stage := range s.Workflow.Stages {
			if stage.Name == role && stage.Approval != nil {
				previous := ""
				if i > 0 {
					previous = s.Workflow.Stages[i-1].Name
				}
				return stage, previous, true
			}
		}
	}
	return Stage{}, "", false
}
func (s State) approvalReceiptRole(role string) bool {
	if s.Workflow != nil {
		for i, stage := range s.Workflow.Stages {
			if stage.Approval != nil && (stage.Name == role || i > 0 && s.Workflow.Stages[i-1].Name == role) {
				return true
			}
		}
	}
	return false
}
func (p Processes) approvedLaunch(state State, role string) (string, string, error) {
	stage, _, approval := state.ApprovalStage(role)
	if !approval {
		return "", "", nil
	}
	if p.ApprovalDirectory == "" || state.Pending == nil || state.Pending.Role != stage.Name || state.PendingSince.IsZero() {
		return "", "", errors.New("approval launch has no consumed GO")
	}
	gos, err := ReadApprovalRecords[ApprovalGO](p.ApprovalDirectory, "go")
	if err != nil {
		return "", "", err
	}
	posts, err := ReadApprovalRecords[ApprovalPost](p.ApprovalDirectory, "post")
	if err != nil {
		return "", "", err
	}
	for _, g := range gos {
		if g.Result != "go" || g.ConsumedAt.IsZero() || !g.ExpiredAt.IsZero() || g.LaunchHistoryLen != len(state.History) || !g.PendingSince.Equal(state.PendingSince) {
			continue
		}
		for _, post := range posts {
			if post.Stage == role && post.N == g.PostN && post.CommentID == g.PostCommentID && post.Digest == g.Digest {
				bundle, err := ReadApprovalBundle(post.BundlePath(p.ApprovalDirectory))
				if err != nil {
					return "", "", err
				}
				if bundle.Digest != post.Digest || !slices.Equal(bundle.Files, post.Files) {
					return "", "", errors.New("approved bundle changed")
				}
				path, err := filepath.Abs(post.BundlePath(p.ApprovalDirectory))
				return path, g.Digest, err
			}
		}
	}
	return "", "", errors.New("approval launch does not match a consumed GO")
}
