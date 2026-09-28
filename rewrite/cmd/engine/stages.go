package main

import (
	"errors"
	"fmt"

	"ticket-runner/internal/chain"
)

// An ordered run decides progress from results the runtime observed, so the
// declared kind of each stage has to match what its role actually launches: a
// command stage must launch no model, because its exit status is the fact that
// satisfies it, and a model stage must launch one, because nothing else in it
// is being observed. The entrance's question role comes from the same intake
// setting the graph form uses; attaching it here keeps the accepted run
// self-describing without asking operators to write the same name twice.
func prepareStages(cfg *config) error {
	staged := cfg.Workflow != nil && len(cfg.Workflow.Stages) > 0
	if (cfg.Router.Mode == "stages") != staged {
		return errors.New("router.mode stages and workflow.stages are one setting; configure both or neither")
	}
	if !staged {
		return nil
	}
	cfg.Workflow.Question = ""
	if cfg.Intake != nil {
		cfg.Workflow.Question = cfg.Intake.QuestionRole
	}
	roles := map[string]chain.Role{}
	for _, role := range cfg.Roles {
		roles[role.Name] = role
	}
	for _, stage := range cfg.Workflow.Stages {
		role, configured := roles[stage.Name]
		if !configured {
			return fmt.Errorf("stage %q names no configured role", stage.Name)
		}
		models := 0
		for _, process := range role.Processes {
			if process.ModelEnv != "" {
				models++
			}
		}
		if stage.Kind == chain.CommandStage && models > 0 {
			return fmt.Errorf("command stage %q launches a model, so its exit status would not be the fact that satisfies it", stage.Name)
		}
		if stage.Kind == chain.ModelStage && models == 0 {
			return fmt.Errorf("model stage %q launches no model", stage.Name)
		}
	}
	return nil
}
