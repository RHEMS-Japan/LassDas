package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// What the engine creates outside the repository has to be written down, or
// it is not findable afterwards. A change delivered as a pull request is
// reviewable by reading it; a queue, a bucket or a database the engine
// brought into existence to make that change work is invisible in the diff,
// outlives the delivery, and costs money until somebody knows it is there.
//
// So a card that creates one records it, and the record travels with
// everything else the run decided: into the trail, and from there into the
// pull request body and the ticket's closing comment.
//
// The card is what records it, not the agent. What an agent writes is a
// claim; the time it happened and the card it happened in are facts this
// process holds, and stamping them here keeps a report from saying a
// resource was created in a stage that never ran.

// ResourcesFile is where a run's created resources accumulate, one JSON
// object per line, under the run directory.
const ResourcesFile = "history/resources.jsonl"

// AgentResourcesFile is what an agent declares its creations in; the
// instruction that asks for it names the same constant.
const AgentResourcesFile = worker.AgentResourcesFile

// maxResourceRecords bounds one run's list, and maxResourceFileBytes one
// agent's claim file. A run that reports creating hundreds of resources has
// gone wrong in a way the report cannot usefully carry; the bound keeps a
// runaway loop out of the ticket comment rather than out of the cloud.
const (
	maxResourceRecords   = 64
	maxResourceFileBytes = worker.MaxCreatedResourcesBytes
)

// resourceClaim is the shape an agent writes: what it made, without the
// two fields it is in no position to assert.
type resourceClaim struct {
	Kind       string `json:"kind"`
	Identifier string `json:"identifier"`
	Provider   string `json:"provider,omitempty"`
	// Stage and CreatedAt are accepted and discarded. An agent that writes
	// them is not refused — it is being helpful — but what it wrote is a
	// claim about this process's own timeline, and this process knows.
	Stage     string `json:"stage,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// RecordCreatedResources moves an agent's claims into the run's record. It
// is called after the card's agent returns, whatever the agent's outcome: a
// card that failed after creating something has still created it, and a
// resource left out of the record is one nobody knows to remove.
//
// The claim file goes whether or not anything could be read from it. It
// sits in the destination's working copy, and the next card seals that
// working copy as the candidate change.
func (p *Pipeline) RecordCreatedResources(stage, workingCopy string) error {
	claimPath := filepath.Join(workingCopy, AgentResourcesFile)
	raw, readErr := readWorkspaceFile(claimPath, maxResourceFileBytes)
	if readErr != nil {
		// No declarations, or a file too large to be a list of them.
		// Neither is this run's failure, and neither is worth ending a card
		// over. The file still goes, in case something is there.
		p.discardDeclarations(claimPath)
		return nil
	}
	created := make([]worker.CreatedResource, 0, 8)
	at := time.Now().UTC()
	// What this destination allows. A kind it did not name is kept as a
	// refused declaration rather than as something the run created: the
	// engine may create what it was allowed to create, and a report that
	// listed the rest as created would tell a reader the destination agreed
	// to it. A configuration that cannot be read allows nothing — the
	// permission has to be established, not assumed.
	permitted := p.permittedResourceKinds()
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var claim resourceClaim
		if json.Unmarshal([]byte(line), &claim) != nil {
			continue
		}
		record := worker.CreatedResource{
			Kind:       plainResourceWord(claim.Kind, 64),
			Identifier: plainResourceWord(claim.Identifier, 256),
			Provider:   plainResourceWord(claim.Provider, 64),
			Stage:      stage,
			CreatedAt:  at,
		}
		record.Refused = !permitted(record.Kind)
		if record.Kind == "" || record.Identifier == "" {
			// A line naming neither what was made nor where to find it
			// records nothing; keeping it would put an empty bullet on the
			// ticket.
			continue
		}
		created = append(created, record)
		if len(created) >= maxResourceRecords {
			break
		}
	}
	// Written down before the file goes. The other order lost every
	// declaration whenever the removal failed, and turned a card that had
	// finished its work into a failed one — for a file the engine itself
	// was tidying up.
	if err := p.appendResources(created); err != nil {
		return err
	}
	p.discardDeclarations(claimPath)
	return nil
}

// discardDeclarations takes the file out of the working copy the next card
// seals as the change being proposed. A removal that fails is said aloud
// and not returned: what the agent declared is already in the run's record
// by this point, and failing the card would throw away work that was done
// over a file nobody asked about. The content goes first, so a file this
// process cannot unlink cannot carry a declaration — or a credential an
// agent wrote into it — into the pull request.
func (p *Pipeline) discardDeclarations(claimPath string) {
	if err := os.Remove(claimPath); err == nil || errors.Is(err, os.ErrNotExist) {
		return
	}
	emptied := os.WriteFile(claimPath, nil, 0o600)
	if p.Logger != nil {
		p.Logger.Error("resource declarations not removed from the working copy",
			"path", claimPath, "emptied", emptied == nil)
	}
}

// permittedResourceKinds answers whether this run's destination allows a
// kind of resource to exist because of it.
//
// The configuration is read the lenient way the chain reads it elsewhere —
// the file's other sections are the worker's business and validated there,
// and what is needed here is a list of words. It is read at collection time
// rather than carried on the pipeline because the collection runs at the
// end of every card, including ones that never resolved a consumer.
//
// A file that cannot be read allows nothing. The permission has to be
// established: reporting a resource as one the destination agreed to,
// because nothing could say otherwise, is the failure this answers.
func (p *Pipeline) permittedResourceKinds() func(kind string) bool {
	deny := func(string) bool { return false }
	raw, err := readWorkspaceFile(p.Config.ConsumerConfigPath, maxWorkspaceReadBytes)
	if err != nil {
		return deny
	}
	var parsed struct {
		Consumers []struct {
			Repository     string `json:"repository"`
			Infrastructure *struct {
				Resources []string `json:"resources"`
			} `json:"infrastructure"`
		} `json:"consumers"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return deny
	}
	repository := p.consumerRepository
	if repository == "" {
		repository, _ = p.readJSONField("ticket-draft.json", "repository")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository != repository || consumer.Infrastructure == nil {
			continue
		}
		allowed := append([]string(nil), consumer.Infrastructure.Resources...)
		return func(kind string) bool {
			for _, permitted := range allowed {
				if permitted == kind {
					return true
				}
			}
			return false
		}
	}
	return deny
}

// appendResources adds lines to the run's record, creating it on the first
// one. The file lives beside the run's other sealed records and is read
// back by the trail.
func (p *Pipeline) appendResources(created []worker.CreatedResource) error {
	if len(created) == 0 {
		return nil
	}
	path := p.path(ResourcesFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o711); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, record := range created {
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(file, "%s\n", encoded); err != nil {
			return err
		}
	}
	return nil
}

// plainResourceWord keeps a claim's text to what can be printed in a
// record that travels to a ticket: one line, bounded, no control
// characters. An agent's output is untrusted text like any other.
func plainResourceWord(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		builder.WriteRune(r)
		if builder.Len() >= limit {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}
