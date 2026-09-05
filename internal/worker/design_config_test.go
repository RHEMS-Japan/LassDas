package worker

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Absent configuration is the safe reading: design on, no trigger vocabulary
// (so the skip can never fire), investigation reports reviewed.
func TestDesignConfigDefaultsAreTheSafeReading(t *testing.T) {
	consumer := validTestConfig().Consumers[0]
	if !consumer.DesignEnabled() || consumer.DesignTriggerWords() != nil || !consumer.ReviewsInvestigation() {
		t.Fatalf("absent design config read as (%v, %v, %v)", consumer.DesignEnabled(), consumer.DesignTriggerWords(), consumer.ReviewsInvestigation())
	}
	consumer.Design = &DesignConfig{}
	if !consumer.DesignEnabled() || len(consumer.DesignTriggerWords()) != 0 || !consumer.ReviewsInvestigation() {
		t.Fatalf("empty design config read as (%v, %v, %v)", consumer.DesignEnabled(), consumer.DesignTriggerWords(), consumer.ReviewsInvestigation())
	}
	off := false
	consumer.Design = &DesignConfig{Default: DesignDefaultOff, TriggerWords: []string{"slow"}, ReviewInvestigation: &off}
	if consumer.DesignEnabled() || len(consumer.DesignTriggerWords()) != 1 || consumer.ReviewsInvestigation() {
		t.Fatalf("explicit design config read as (%v, %v, %v)", consumer.DesignEnabled(), consumer.DesignTriggerWords(), consumer.ReviewsInvestigation())
	}
	// The accessor hands out a copy: a caller cannot edit the configuration.
	consumer.DesignTriggerWords()[0] = "edited"
	if consumer.Design.TriggerWords[0] != "slow" {
		t.Fatal("DesignTriggerWords exposed the configuration slice")
	}
}

func TestDesignConfigValidation(t *testing.T) {
	valid := func() Config {
		config := validTestConfig()
		config.Consumers[0].Design = &DesignConfig{Default: DesignDefaultOn, TriggerWords: []string{"slow", "in production", "遅い"}}
		return config
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid design config was refused: %v", err)
	}
	tooMany := make([]string, maxDesignTriggerWords+1)
	for index := range tooMany {
		tooMany[index] = fmt.Sprintf("word-%d", index)
	}
	cases := map[string]func(*DesignConfig){
		"default outside on/off":  func(d *DesignConfig) { d.Default = "maybe" },
		"too many words":          func(d *DesignConfig) { d.TriggerWords = tooMany },
		"empty word":              func(d *DesignConfig) { d.TriggerWords = []string{""} },
		"untrimmed word":          func(d *DesignConfig) { d.TriggerWords = []string{" slow"} },
		"word with a newline":     func(d *DesignConfig) { d.TriggerWords = []string{"slow\nlogs"} },
		"overlong word":           func(d *DesignConfig) { d.TriggerWords = []string{strings.Repeat("s", maxDesignTriggerWordSize+1)} },
		"duplicate ignoring case": func(d *DesignConfig) { d.TriggerWords = []string{"Slow", "slow"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := valid()
			mutate(config.Consumers[0].Design)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate() accepted the design config")
			}
		})
	}
}

// The shipped example configuration carries a design block, so the example a
// consumer copies from already names a trigger vocabulary.
func TestExampleConsumerConfigCarriesADesignPolicy(t *testing.T) {
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	consumer := config.Consumers[0]
	if consumer.Design == nil || !consumer.DesignEnabled() || len(consumer.DesignTriggerWords()) == 0 || !consumer.ReviewsInvestigation() {
		t.Fatalf("example design policy = %+v", consumer.Design)
	}
	// The second destination leaves it out and gets the safe reading.
	if config.Consumers[1].Design != nil || !config.Consumers[1].DesignEnabled() {
		t.Fatalf("second consumer design = %+v", config.Consumers[1].Design)
	}
}
