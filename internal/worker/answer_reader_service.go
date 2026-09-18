package worker

import (
	"context"
	"errors"
	"net/http"

	"automation.internal/ticket-ingress/internal/hook"
)

// AnswerReaderService reads a requester's comment against the questions that
// were asked. It is what the question tick uses in place of the regular
// expressions that used to decide whether a person had answered.
//
// The consumer configuration is read on every call rather than held: a tick
// runs for as long as the process does, and the seat that reads answers can
// be changed without restarting it.
type AnswerReaderService struct {
	configPath string
	client     ChatCompletionsAPI
}

func NewAnswerReaderService(configPath string, client ChatCompletionsAPI) (*AnswerReaderService, error) {
	if configPath == "" {
		return nil, errors.New("answer reader needs a consumer configuration")
	}
	if client == nil {
		gateway, err := NewGatewayClient(&http.Client{Timeout: ModelInvocationTimeout})
		if err != nil {
			return nil, errors.New("answer reader model client could not be created")
		}
		client = gateway
	}
	return &AnswerReaderService{configPath: configPath, client: client}, nil
}

// ReadAnswer satisfies hook.AnswerReader.
func (s *AnswerReaderService) ReadAnswer(ctx context.Context, questionsJSON, body string) (hook.AnswerReading, error) {
	if s == nil {
		return hook.AnswerReading{}, errors.New("answer reader is not configured")
	}
	config, err := LoadConfig(s.configPath)
	if err != nil {
		return hook.AnswerReading{}, err
	}
	invoker, err := NewModelInvoker(s.client)
	if err != nil {
		return hook.AnswerReading{}, errors.New("answer reader model client could not be created")
	}
	reading, _, err := invoker.ReadAnswer(ctx, config.Models.Readiness.Assessor, questionsJSON, body)
	if err != nil {
		return hook.AnswerReading{}, err
	}
	return hook.AnswerReading{
		Kind: reading.Kind, Answers: reading.Answers,
		NotNeeded: reading.NotNeeded, Unanswered: reading.Unanswered, Reason: reading.Reason,
	}, nil
}
