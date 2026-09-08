package initsmoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/attendant"
	"automation.internal/ticket-ingress/internal/initwizard"
	"automation.internal/ticket-ingress/internal/localrun"
	"automation.internal/ticket-ingress/internal/worker"
)

// RuntimeObserver reads the owned container and verifies the remote PR. It
// never polls the tracker as a replacement attendant, nor changes runtime state.
type RuntimeObserver struct {
	Manager localrun.Manager
	Process initwizard.Process
	API     initwizard.API
	Dir     string
}

var deliveryIDPattern = regexp.MustCompile(`^delivery_[a-f0-9]{32}$`)

func (o RuntimeObserver) Observe(ctx context.Context, s *initwizard.State, record Record, secrets initwizard.Secrets) (Observation, error) {
	i := localrun.Instance{ID: s.Project, Dir: o.Dir, Image: s.Image, EngineSHA: s.EngineSHA, DockerContext: s.DockerContext, BoardPort: s.BoardPort}
	status, err := o.Manager.Status(ctx, i)
	if err != nil {
		return Observation{}, err
	}
	if status.State != "ready" {
		return Observation{}, errors.New("本体が起動時検査を通った状態ではありません。run status を確認してください")
	}
	process := o.Process
	if process == nil {
		process = initwizard.ExecProcess{}
	}
	docker := func(script string, args ...string) ([]byte, error) {
		argv := []string{"--context", s.DockerContext, "exec", "--user", "1000:1000", status.ContainerID, "python3", "-c", script}
		argv = append(argv, args...)
		return process.Run(ctx, "docker", argv, "")
	}
	raw, err := docker(`import os,sys; p='/data/status/board.json'; assert os.path.getsize(p)<4194304; sys.stdout.buffer.write(open(p,'rb').read())`)
	if err != nil {
		return Observation{}, errors.New("本体の現在地を読めません")
	}
	var board attendant.BoardSnapshot
	if json.Unmarshal(raw, &board) != nil || board.GeneratedAt.IsZero() || time.Since(board.GeneratedAt) > 130*time.Second || board.GeneratedAt.After(time.Now().Add(time.Minute)) {
		return Observation{}, errors.New("本体の現在地の記録が古いか不正です")
	}
	var run *attendant.RunStatus
	for index := range board.Runs {
		value := &board.Runs[index]
		if value.IssueID == record.IssueID && (record.DeliveryID == "" || value.DeliveryID == record.DeliveryID) {
			if run != nil {
				return Observation{}, errors.New("同じ課題の実行を一意に照合できません")
			}
			run = value
		}
	}
	if run == nil {
		return Observation{Step: "受付待ち", Detail: board.Notice}, nil
	}
	if !deliveryIDPattern.MatchString(run.DeliveryID) || run.IssueKey != record.IssueKey {
		return Observation{}, errors.New("課題と実行の身元が一致しません")
	}
	observation := Observation{DeliveryID: run.DeliveryID, Step: run.Step, Detail: run.StepTitle + " " + run.Detail, Terminal: run.Terminal}
	if run.Terminal != "success" {
		return observation, nil
	}
	record.DeliveryID = run.DeliveryID
	raw, err = docker(readArtifactsScript, run.DeliveryID)
	if err != nil {
		return observation, errors.New("納品された実行の確定記録を読めません")
	}
	var files map[string]json.RawMessage
	if len(raw) > 16*1024*1024 || json.Unmarshal(raw, &files) != nil {
		return observation, errors.New("納品記録の一覧が不正です")
	}
	config, err := worker.LoadConfig(filepath.Join(o.Dir, "config", "consumer.json"))
	if err != nil {
		return observation, err
	}
	proof, err := validateProof(files, config, record)
	if err != nil {
		return observation, err
	}
	if err := o.verifyRemote(ctx, s, record, proof, secrets); err != nil {
		return observation, err
	}
	observation.PRURL = proof.Payload.PullRequest.HTMLURL
	observation.Verified = true
	return observation, nil
}

// Select only bounded JSON evidence, never transcripts, env or secret files.
// The script is fixed; the validated delivery identifier is a separate argv.
const readArtifactsScript = `
import json, os, pathlib, re, stat, sys
assert re.fullmatch(r'delivery_[a-f0-9]{32}',sys.argv[1])
root=pathlib.Path('/data/runs')/sys.argv[1]
assert not root.is_symlink()
names=['feature-pr.json','validation.json']
history=root/'history'
assert not history.is_symlink()
for d in history.iterdir():
    if re.fullmatch(r'(stage|design)-[1-9][0-9]?',d.name):
        assert d.is_dir() and not d.is_symlink()
        for p in d.iterdir():
            if p.name in ['ticket.json','source.json','candidate.json','decision.json','design.json','applier-run.json'] or re.fullmatch(r'[a-z][a-z0-9-]{1,63}(-design-review)?\.json',p.name):
                names.append(str(p.relative_to(root)))
result={}; total=0
assert len(names)<512
for name in names:
    p=root/name
    if not p.exists(): continue
    info=p.lstat()
    assert stat.S_ISREG(info.st_mode) and info.st_uid==1000 and info.st_size<=8388608
    total+=info.st_size
    assert total<12582912
    result[name]=json.loads(p.read_bytes())
sys.stdout.write(json.dumps(result,separators=(',',':')))
`

func (o RuntimeObserver) verifyRemote(ctx context.Context, s *initwizard.State, record Record, proof featureProof, secrets initwizard.Secrets) error {
	base := fmt.Sprintf("/repos/%s/pulls/%d", record.Repository, proof.Payload.PullRequest.Number)
	type repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	type ref struct {
		Ref, SHA string
		Repo     repository
	}
	var pr struct {
		Number       int64
		HTMLURL      string `json:"html_url"`
		Head, Base   ref
		ChangedFiles int `json:"changed_files"`
	}
	key := secrets["TARGET_GITHUB_TOKEN"]
	if err := o.API.GitHub(ctx, base, key, &pr); err != nil {
		return err
	}
	feature := proof.Payload.Feature
	if pr.Number != proof.Payload.PullRequest.Number || pr.HTMLURL != proof.Payload.PullRequest.HTMLURL || pr.Base.Ref != record.Branch || pr.Base.SHA != feature.Base.SHA || pr.Head.Ref != feature.Branch || pr.Head.SHA != feature.HeadSHA || pr.Base.Repo.ID != record.RepositoryID || pr.Head.Repo.ID != record.RepositoryID || !strings.EqualFold(pr.Base.Repo.FullName, record.Repository) || !strings.EqualFold(pr.Head.Repo.FullName, record.Repository) || pr.ChangedFiles != 1 {
		return errors.New("実在 PR の repo・base・head・変更数が記録と一致しません")
	}
	var files []struct {
		Filename, Status, SHA string
		PreviousFilename      string `json:"previous_filename"`
	}
	if err := o.API.GitHub(ctx, base+"/files?per_page=2", key, &files); err != nil {
		return err
	}
	if len(files) != 1 || files[0].Filename != record.Path || files[0].Status != "modified" || files[0].PreviousFilename != "" {
		return errors.New("実在 PR の差分が確認した 1 ファイルだけではありません")
	}
	after, err := o.API.File(ctx, record.Repository, record.Path, feature.HeadSHA, key)
	if err != nil {
		return err
	}
	before, err := o.API.File(ctx, record.Repository, record.Path, feature.Base.SHA, key)
	if err != nil {
		return err
	}
	if string(after) != record.After || digest(string(before)) != record.BeforeSHA256 || string(before)+record.Addition != record.After {
		return errors.New("実在 PR の変更前後の内容が期待差分と一致しません")
	}
	var commit struct {
		SHA     string
		Tree    struct{ SHA string }
		Parents []struct{ SHA string }
	}
	if err := o.API.GitHub(ctx, "/repos/"+record.Repository+"/git/commits/"+feature.HeadSHA, key, &commit); err != nil {
		return err
	}
	if commit.SHA != feature.HeadSHA || commit.Tree.SHA != feature.TreeSHA || len(commit.Parents) != 1 || commit.Parents[0].SHA != feature.Base.SHA {
		return errors.New("実在 PR の commit と検証した tree・親が一致しません")
	}
	return nil
}
