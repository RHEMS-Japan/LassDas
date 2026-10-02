package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"ticket-runner/internal/chain"
)

// A stage's announcement is posted when the stage begins, and the requester
// reads it to know what is working on their request. The model that will do
// that work is chosen per launch, inside the engine, just before the child
// starts; the history learns it only once the launch has returned, which is
// after the sentence belongs on the tracker. So the engine writes the choice
// down here as it makes it, beside the request's own history, and the turn
// that posts the sentence reads it from there.
//
// The first launch of each stage is kept for that sentence. A runtime that
// declares the models it chooses keeps every launch as well, since a stage
// launched again is declared when its models change; one that does not keeps
// the first only, as a record written before there were declarations does.
type chosenRecord struct {
	First    map[string]string         `json:"first"`
	Launches map[string][]chosenLaunch `json:"launches,omitempty"`
}

// chosenLaunch is one launch of a role that chose a model: which launch it
// was, when its first model was chosen, and every model its processes chose,
// in the order they were chosen.
type chosenLaunch struct {
	Launch int       `json:"launch"`
	At     time.Time `json:"at"`
	Models []string  `json:"models"`
}

// chosenPath is where the record sits: in the request's run directory, beside
// the history it belongs to, so a restart and a removed cache both leave it.
func chosenPath(runDirectory string) string {
	return filepath.Join(runDirectory, "chosen.json")
}

func loadChosen(runDirectory string) (chosenRecord, error) {
	var record chosenRecord
	raw, err := os.ReadFile(chosenPath(runDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return chosenRecord{}, errors.New("the recorded model choices are unreadable; no stage names a model from a damaged record")
	}
	return record, nil
}

// recordChosen writes down the model one launch of a role will use, unless
// that role already has one written: the announcement names the launch that
// began the stage, not whichever launch ran last.
func recordChosen(runDirectory, role, model string) error {
	if role == "" || model == "" {
		return nil
	}
	record, err := loadChosen(runDirectory)
	if err != nil {
		return err
	}
	if _, written := record.First[role]; written {
		return nil
	}
	if record.First == nil {
		record.First = map[string]string{}
	}
	record.First[role] = model
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(chosenPath(runDirectory), data)
}

// recordLaunch writes down what recordChosen does and, besides, the launch
// this choice belongs to. The processes of one launch choose in turn, and
// their models are kept together, so the launch is declared once with all of
// them.
func recordLaunch(runDirectory, role, model string, launch int) error {
	if role == "" || model == "" {
		return nil
	}
	record, err := loadChosen(runDirectory)
	if err != nil {
		return err
	}
	if record.First == nil {
		record.First = map[string]string{}
	}
	if _, written := record.First[role]; !written {
		record.First[role] = model
	}
	if record.Launches == nil {
		record.Launches = map[string][]chosenLaunch{}
	}
	launches := record.Launches[role]
	if last := len(launches) - 1; last >= 0 && launches[last].Launch == launch {
		launches[last].Models = append(launches[last].Models, model)
	} else {
		launches = append(launches, chosenLaunch{Launch: launch, At: time.Now().UTC(), Models: []string{model}})
	}
	record.Launches[role] = launches
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(chosenPath(runDirectory), data)
}

// firstModel is the model the launch that began this stage chose: from the
// engine's own record, or, for a stage whose record predates it or was lost,
// from the earliest launch of it the history kept. Both name the same launch,
// and an unreadable record is read as no record, so the history answers
// instead of a damaged file deciding what a person is told. An empty answer
// means no launch of this stage has chosen a model yet.
func firstModel(directory, role string) string {
	run := filepath.Join(directory, "run")
	if record, err := loadChosen(run); err == nil {
		if model := record.First[role]; model != "" {
			return model
		}
	}
	state, err := savedHistory(directory)
	if err != nil {
		return ""
	}
	for _, result := range state.History {
		if result.Role == role && result.Model != "" {
			return result.Model
		}
	}
	return ""
}

// awaitsModel reports that a launch of this role chooses a model, so a
// sentence about the stage beginning can wait a tick and name it. A role
// whose processes launch no model, or a runtime with no selection configured
// at all, never will: there the sentence is posted as the operator wrote it.
func awaitsModel(cfg config, role string) bool {
	return modelProcesses(cfg, role) > 0
}

// modelProcesses is how many processes of a role choose a model at each
// launch: none for a role that launches no model, or under a runtime with no
// selection configured.
func modelProcesses(cfg config, role string) int {
	if cfg.ModelSelection == nil {
		return 0
	}
	count := 0
	for _, configured := range cfg.Roles {
		if configured.Name != role {
			continue
		}
		for _, process := range configured.Processes {
			if process.ModelEnv != "" {
				count++
			}
		}
	}
	return count
}

// modelLaunch is one launch of a model stage as the completion footer says
// it: what ran, for how long, and whether the process exited 0.
type modelLaunch struct {
	model    string
	duration string
	failed   bool
}

// modelLaunches groups the request's history into the launches that used a
// model, in the order they ran, by the stage they belong to. A record that
// carries a model is one launch of one process; the runtime's own notes and
// the command stages carry none and are not here.
func modelLaunches(state chain.State) ([]string, map[string][]modelLaunch) {
	var order []string
	launches := map[string][]modelLaunch{}
	for _, result := range state.History {
		if result.Model == "" {
			continue
		}
		if _, seen := launches[result.Role]; !seen {
			order = append(order, result.Role)
		}
		launches[result.Role] = append(launches[result.Role], modelLaunch{
			model: result.Model, duration: spentText(result.FinishedAt.Sub(result.StartedAt)),
			// The runtime's note after the launch says a process did not exit
			// 0 exactly when the launch's own record carries that error.
			failed: result.Error != "",
		})
	}
	return order, launches
}
