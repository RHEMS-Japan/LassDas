package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"automation.internal/ticket-ingress/internal/decisions"
)

// The reception's own judge: a second reading of a request, asked only where
// it can take questions away.
//
// The reception writes its questions by reading the request against its
// rules and answering in prose. Those rules read "ask" onto anything a
// request left open, and a request that left nothing open has never been
// written. What this asks is the same question with the alternative stated
// honestly - proceeding means recording each choice where the requester
// reads it and can stop the run, not deciding in silence - and it is asked
// of a model that answers with an option and a number rather than prose.
//
// Where it is consulted is what makes it safe rather than the number it
// answers with. It is reached only when the reception has already decided
// to ask something, so the most a "no" can do is leave those questions
// exactly where they were. There is no path from anything here to a
// question being added, raised, or asked of a requester who was not going
// to be asked anyway - which is why the threshold is a number an operator
// may move without having to reason about what a lower one would let
// through.

// ReceptionOpinion is one answer to the reception's own question about a
// request.
type ReceptionOpinion struct {
	// Model is the decision model that answered. It is carried back rather
	// than read from the configuration by the caller, so the record names
	// the model that really answered.
	Model      string
	Answer     string
	Confidence float64
}

// ReceptionJudge answers whether a request is one to get on with.
//
// Every failure is an error, and an error means the model has no opinion:
// a caller that gets one does exactly what it would have done with no judge
// configured at all. Nothing here may end a run.
type ReceptionJudge interface {
	Proceedable(ctx context.Context, request string) (ReceptionOpinion, error)
}

// decisionsJudge asks the decisions service the reception's fixed question
// set and reads the one answer that decides anything.
type decisionsJudge struct {
	client *decisions.Client
	model  string
}

// NewReceptionJudge builds the judge a destination configured, and reports
// false when it configured none. An absent role is not an error: it is the
// ordinary state of every destination that has not asked for this.
func NewReceptionJudge(config Config, httpClient *http.Client) (ReceptionJudge, bool, error) {
	role, present := config.Models.ReceptionJudgeRole()
	if !present {
		return nil, false, nil
	}
	client, err := decisions.New(decisions.Endpoint{
		Provider:  role.Provider,
		Model:     role.Model,
		BaseURL:   role.BaseURL,
		APIKeyEnv: role.APIKeyEnv,
	}, httpClient)
	if err != nil {
		// The address and the key variable's name are already held to a
		// shape by the configuration, so a failure here is a configuration
		// this engine cannot call at all. It is reported rather than
		// swallowed: swallowing it would make a misconfigured role look
		// exactly like a model that is never sure enough.
		return nil, false, err
	}
	return &decisionsJudge{client: client, model: role.Model}, true, nil
}

// Proceedable asks the fixed set and returns the answer to the one question
// that decides anything. The others are asked because they come back in the
// same round trip and cost nothing extra; nothing reads them yet.
func (j *decisionsJudge) Proceedable(ctx context.Context, request string) (ReceptionOpinion, error) {
	if j == nil || j.client == nil {
		return ReceptionOpinion{}, errors.New("reception judge: no client")
	}
	answers, err := j.client.Judge(ctx, decisions.NewReceptionState(request), decisions.ReceptionQuestions())
	if err != nil {
		return ReceptionOpinion{}, err
	}
	option, confidence, ok := answers.Choice(decisions.QuestionProceedable)
	if !ok {
		return ReceptionOpinion{}, errors.New("reception judge: the answer settles nothing")
	}
	model := answers.Model
	if model == "" {
		model = j.model
	}
	return ReceptionOpinion{Model: model, Answer: option, Confidence: confidence}, nil
}

// consultReceptionJudge asks the judge about a request whose questions were
// about to be put to its requester, and returns the judgment worth sealing.
//
// It returns nothing at all unless the judgment settles something. A judge
// that said no, that was not sure enough, that could not be reached, that
// timed out or that was never configured leaves the decision exactly as it
// was - including the bytes it is sealed as, because a record nobody acts
// on is still a difference in a sealed artifact.
func consultReceptionJudge(ctx context.Context, judge ReceptionJudge, outcome string, questions []ReadinessQuestion, request TicketRequest, role ReceptionJudgeConfig) *ReceptionJudgment {
	// The guard, and the whole asymmetry: a request with nothing to ask
	// never reaches the model. A run that was going to proceed is not
	// judged again, and neither is one that was rejected or that nobody
	// could resolve into a question.
	if judge == nil || outcome != ReadinessOutcomeClarification || len(questions) == 0 {
		return nil
	}
	text := readinessTicketText(request)
	if text == "" {
		return nil
	}
	opinion, err := judge.Proceedable(ctx, text)
	if err != nil {
		return nil
	}
	// An answer from a model this destination did not name is not this
	// destination's judgment - a gateway that routed the call elsewhere, or
	// a service that answered from a substitute. It is dropped here, where
	// dropping it costs the run nothing, rather than sealed and refused at
	// the gate, where it would end a delivery over a model's own metadata.
	if opinion.Model != role.Model {
		return nil
	}
	judgment := ReceptionJudgment{
		Model:      opinion.Model,
		Answer:     boundedHead(opinion.Answer, 32),
		Confidence: opinion.Confidence,
		Threshold:  role.Threshold(),
	}
	if !isConfidence(judgment.Confidence) || !judgment.Settled() {
		return nil
	}
	return &judgment
}

// applyReceptionJudgment turns the questions the reception was about to ask
// into the decision a settling judgment allows, and is the only place that
// conversion happens. Sealing a decision and re-deriving it later both go
// through here, so a decision cannot be sealed under one rule and read back
// under another.
//
// A question is settled only by the default the reception itself proposed
// for it. One the reception proposed no default for is not settled by
// anything here - it keeps being asked, and the run keeps asking - because
// the alternative would be to invent a decision and show the requester a
// choice nobody made.
func applyReceptionJudgment(outcome string, questions []ReadinessQuestion, judgment *ReceptionJudgment) (string, []ReadinessQuestion, []ReadinessAssumption) {
	if judgment == nil || !judgment.Settled() ||
		outcome != ReadinessOutcomeClarification || len(questions) == 0 {
		return outcome, questions, nil
	}
	kept := make([]ReadinessQuestion, 0, len(questions))
	var settled []ReadinessAssumption
	for _, question := range questions {
		choice, offered := choiceByID(question.Choices, question.ProposedDefault)
		if question.ProposedDefault == "" || !offered {
			// Renumbered as it is kept, the way a question that outlives a
			// blamed one is: the ids an answer names have to be the ids the
			// requester is shown, counting from one.
			carried := question
			carried.ID = questionID(len(kept) + 1)
			kept = append(kept, carried)
			continue
		}
		settled = append(settled, ReadinessAssumption{
			Kind: AssumptionDefensibleDefault,
			// What was decided, in the reception's own two sentences: the
			// point it was going to ask about, and the answer it said it
			// would defend. Bounded together so the pair still fits the
			// limit either one of them could fill on its own.
			Statement: boundedHead(question.Question+" → "+choice.Label, 1900),
			Evidence:  boundedHead(choice.Effect, 1900),
		})
	}
	if len(kept) == 0 {
		return ReadinessOutcomeReady, []ReadinessQuestion{}, settled
	}
	return outcome, kept, settled
}

// questionID is the id a question is asked under, counting from one.
func questionID(position int) string {
	return fmt.Sprintf("Q%d", position)
}
