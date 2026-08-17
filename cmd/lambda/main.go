package main

import (
	"context"
	"log/slog"
	"os"

	"automation.internal/ticket-ingress/internal/app"
	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	awsConfig, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("startup failed", "code", "aws_configuration_failed")
		os.Exit(1)
	}
	runtimeSecrets, err := app.LoadRuntimeSecrets(context.Background(), os.Getenv("RUNTIME_SECRET_ID"), secretsmanager.NewFromConfig(awsConfig))
	if err != nil {
		logger.Error("startup failed", "code", "runtime_secret_invalid")
		os.Exit(1)
	}
	config, err := app.LoadConfig(os.Getenv, runtimeSecrets)
	if err != nil {
		logger.Error("startup failed", "code", "configuration_invalid")
		os.Exit(1)
	}
	queueStore, err := state.NewDynamoStore(config.TableName, dynamodb.NewFromConfig(awsConfig))
	if err != nil {
		logger.Error("startup failed", "code", "queue_store_invalid")
		os.Exit(1)
	}
	backlogClient, err := backlog.NewClient(config.Backlog, nil)
	if err != nil {
		logger.Error("startup failed", "code", "backlog_client_invalid")
		os.Exit(1)
	}
	service, err := hook.NewService(config.Hook, backlogClient, queueStore, logger)
	if err != nil {
		logger.Error("startup failed", "code", "hook_service_invalid")
		os.Exit(1)
	}
	if config.Dispatch != nil {
		dispatcher, err := app.NewWorkflowDispatcher(*config.Dispatch, nil)
		if err != nil {
			logger.Error("startup failed", "code", "dispatcher_invalid")
			os.Exit(1)
		}
		service.UseDispatcher(dispatcher)
	}
	reportService, err := hook.NewTerminalReportService(config.FunctionURL.Report, queueStore, backlogClient, logger)
	if err != nil {
		logger.Error("startup failed", "code", "terminal_report_service_invalid")
		os.Exit(1)
	}
	questionService, err := hook.NewQuestionReportService(config.FunctionURL.Report, queueStore, backlogClient, logger)
	if err != nil {
		logger.Error("startup failed", "code", "question_report_service_invalid")
		os.Exit(1)
	}
	tickService, err := hook.NewQuestionTickService(config.FunctionURL.Report, queueStore, backlogClient, reportService, service, logger)
	if err != nil {
		logger.Error("startup failed", "code", "question_tick_service_invalid")
		os.Exit(1)
	}
	service.UseAnswerSignal(tickService)
	if config.Board != nil {
		projection, err := backlog.NewBoardProjection(backlogClient, *config.Board)
		if err != nil {
			logger.Error("startup failed", "code", "board_projection_invalid")
			os.Exit(1)
		}
		service.UseBoard(projection)
		questionService.UseBoard(projection)
		tickService.UseBoard(projection)
		reportService.UseBoard(projection)
	}
	handler, err := hook.NewFunctionURLHandlerWithQuestions(config.FunctionURL, service, queueStore, reportService, questionService, tickService)
	if err != nil {
		logger.Error("startup failed", "code", "function_url_handler_invalid")
		os.Exit(1)
	}
	lambda.Start(handler.Handle)
}
