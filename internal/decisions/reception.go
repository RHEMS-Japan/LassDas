package decisions

import "unicode/utf8"

// The reception's questions, as one fixed set.
//
// The reception already decides all of this: whether a request can be acted
// on without asking its author anything, what kind of request it is, and how
// far it reaches. It decides it by writing a prompt, reading prose back, and
// holding that prose to a schema. This set asks the same things of a model
// that answers with an option id and a number instead, so the two judgments
// can be put side by side on the same request.
//
// The criteria below are the reception's own rules, restated. Each one is
// written so that the option an implementer would defend is the option the
// rule names - a criterion that drifts from the rule makes the two judgments
// disagree about wording rather than about the request. They are constants
// because the same words have to reach both the run and the measurement: a
// set measured under one wording and asked under another has been measured
// for nothing.

// The question ids. They are the names answers come back under, and the
// names any record of a judgment is read by afterwards.
const (
	// QuestionProceedable is the one that decides whether the reception has
	// anything to ask at all.
	QuestionProceedable = "proceedable"
	// QuestionTargetNamed and QuestionScopeClosed are the two properties a
	// request that can be acted on almost always has, asked separately so a
	// request that has one and not the other can be seen.
	QuestionTargetNamed = "target_named"
	QuestionScopeClosed = "scope_closed"
	// QuestionKind and QuestionSize are what the request is and how much of
	// it there is. The kind decides which chain runs; the size is what the
	// measurement needs to tell a one-line change from a module.
	QuestionKind = "kind"
	QuestionSize = "size"
)

// The options each question offers. An option id is part of the contract:
// it is what an answer names, so it is read back by a caller's switch and
// written into whatever record the judgment leaves.
const (
	AnswerYes    = "yes"
	AnswerNo     = "no"
	AnswerPartly = "partly"

	KindDocs          = "docs"
	KindCode          = "code"
	KindInvestigation = "investigation"
	KindDesign        = "design"

	SizeSmall  = "small"
	SizeMedium = "medium"
	SizeLarge  = "large"
)

// proceedable restates the conditions the reception asks under, and states
// the two sides truthfully.
//
// The first wording asked whether every open point could be settled, and
// read "no" onto anything a request had left open. That is the wrong
// question, because it weighs proceeding against a costless alternative
// that does not exist. Proceeding is not deciding in silence: each point
// the run settles is written down as a stated assumption the requester
// reads before any of the work is delivered, and any one of them is a
// reason for them to stop the run. Not proceeding puts the request back to
// them and does nothing until they answer.
//
// So yes is the ordinary answer - a request that left something open is
// every request - and no is the exception, available only where a
// particular point can be named whose two readings deliver different
// things and where a recorded assumption would come too late to help.
const (
	proceedableInstructions = "An automated implementer will carry out `request` on its own, reading the repository as it goes, and cannot ask the requester anything while it works. " +
		"Whatever it settles on the way is written down as a stated assumption the requester reads before any of the work is delivered, and any one of them is a reason for them to stop the run. " +
		"The alternative is not a safer version of the same thing: it puts the request back to the requester and nothing happens until they answer. " +
		"Is this a request to get on with, or is there a particular point where getting on with it would deliver something they did not ask for?"
	proceedableYes = "Get on with it. Points are left open - they are in every request - and each of them is ordinary implementation judgement: " +
		"settled by reading the repository, or by a default an engineer could defend in one sentence, recorded where the requester sees it and can stop the run over it. " +
		"This is the ordinary answer, and it does not ask that the request left nothing open - only that what it left open is the kind of thing an implementer decides."
	proceedableNo = "Ask first. A particular point can be named here - not a general thinness of detail, not that the work looks large or unfamiliar - " +
		"which has two defensible readings that lead to materially different results: which user-visible behaviour is wanted, what would count as done, how far the change may reach, or what becomes of somebody's data or safety. " +
		"The answer is in neither the request nor the repository, only the requester has it, and a recorded assumption does not save them: by the time they read it the wrong thing is built."
)

// targetNamed is how much of the starting point the request hands over.
// The reception does not ask about this - it reads the repository instead -
// so the answer is a measure of how much reading the run is about to do,
// not of whether it can proceed.
const (
	targetNamedInstructions = "Does `request` name what has to change, or only what should be different afterwards?"
	targetNamedYes          = "The request names what is to change - the file, the document section, the component or the named behaviour - precisely enough that an implementer starts in one place."
	targetNamedPartly       = "The request names an area but not the point inside it, so an implementer must first find which part of that area it means."
	targetNamedNo           = "The request says what should be different afterwards and names nothing that has to change to get there."
)

// scopeClosed is whether the request says where the work stops. An open
// boundary is not a reason to ask the requester anything - the writable
// scope bounds the change either way - but it is what turns a small request
// into an unbounded one.
const (
	scopeClosedInstructions = "Does `request` bound what may change, or could it be satisfied by changing an open-ended set of things?"
	scopeClosedYes          = "The request bounds the work: it says what is in scope, or the change it asks for is confined to what it names, and nothing in it invites work beyond that."
	scopeClosedNo           = "The boundary is open: the outcome it asks for could be reached by changing an unbounded set of things, or it invites related improvements without saying where they stop."
)

// kind splits the reception's two kinds into the four a run is actually
// shaped by. The reception seals change or investigation and, for a change,
// whether a design must precede the code; docs, code and design are that
// second decision made visible, and investigation is the reception's own -
// a request asking to find out and to change something is a change.
const (
	kindInstructions   = "What kind of work does `request` ask for?"
	kindDocsMeaning    = "A change to prose only - documentation, comments, messages or other text a reader sees. Nothing the program does changes."
	kindCodeMeaning    = "A change to what the program does, and the request itself states how the change is to be made: which part changes, and to what."
	kindInvestigationM = "The request asks only to find out, measure or explain what the running system does, and asks for nothing to be changed. A request asking for both a finding and a change is not this."
	kindDesignMeaning  = "A change to what the program does whose approach the request does not state, or one that calls for observing the running system first - slowness, intermittence, behaviour in production, log contents, a root cause. The approach has to be worked out before anything is written."
)

// size is what a minimal correct implementation costs. It is the one
// question here the reception does not already answer, and the reason for
// asking it is the run after the reception: a one-line change and a new
// module are not worth the same seat.
const (
	sizeInstructions  = "How much work is a minimal correct implementation of `request` in a large repository?"
	sizeSmallMeaning  = "One file, or a few lines across files the request names. A paragraph of prose, one message, one flag."
	sizeMediumMeaning = "Several files, or one new component built to a pattern the repository already has."
	sizeLargeMeaning  = "Many files across more than one area, a new module wired into what is already there, or work whose extent is not knowable until the repository has been read."
)

// ReceptionQuestions is the set the reception asks about one request. It is
// built fresh on every call: the maps are the caller's to hold, and a set
// shared between a run and a measurement could otherwise be edited by one of
// them for the other.
func ReceptionQuestions() Questions {
	return Questions{
		QuestionProceedable: {
			Type:         KindChoice,
			Instructions: proceedableInstructions,
			Criteria: map[string]string{
				AnswerYes: proceedableYes,
				AnswerNo:  proceedableNo,
			},
		},
		QuestionTargetNamed: {
			Type:         KindChoice,
			Instructions: targetNamedInstructions,
			Criteria: map[string]string{
				AnswerYes:    targetNamedYes,
				AnswerPartly: targetNamedPartly,
				AnswerNo:     targetNamedNo,
			},
		},
		QuestionScopeClosed: {
			Type:         KindChoice,
			Instructions: scopeClosedInstructions,
			Criteria: map[string]string{
				AnswerYes: scopeClosedYes,
				AnswerNo:  scopeClosedNo,
			},
		},
		QuestionKind: {
			Type:         KindChoice,
			Instructions: kindInstructions,
			Criteria: map[string]string{
				KindDocs:          kindDocsMeaning,
				KindCode:          kindCodeMeaning,
				KindInvestigation: kindInvestigationM,
				KindDesign:        kindDesignMeaning,
			},
		},
		QuestionSize: {
			Type:         KindChoice,
			Instructions: sizeInstructions,
			Criteria: map[string]string{
				SizeSmall:  sizeSmallMeaning,
				SizeMedium: sizeMediumMeaning,
				SizeLarge:  sizeLargeMeaning,
			},
		},
	}
}

// MaxReceptionRequestBytes bounds the request text one judgment carries.
// The whole state has to fit the model's context beside the criteria, and a
// request longer than this has said what it is asking for many times over.
const MaxReceptionRequestBytes = 20000

// ReceptionState is what the questions are asked about. The field name is
// the one the criteria refer to by name, so renaming it here without
// rewriting them would leave every criterion pointing at nothing.
type ReceptionState struct {
	Request string `json:"request"`
}

// NewReceptionState bounds the request text. It cuts on a character
// boundary: a request in a language whose characters are several bytes long
// would otherwise end in half a character, and the invalid byte travels to
// the service as the last thing it reads.
func NewReceptionState(request string) ReceptionState {
	if len(request) <= MaxReceptionRequestBytes {
		return ReceptionState{Request: request}
	}
	cut := MaxReceptionRequestBytes
	for cut > 0 && !utf8.RuneStart(request[cut]) {
		cut--
	}
	return ReceptionState{Request: request[:cut]}
}
