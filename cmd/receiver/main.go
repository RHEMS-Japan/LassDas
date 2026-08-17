package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/receiver"
)

func main() {
	if err := run(); err != nil {
		encoded, _ := json.Marshal(map[string]string{"decision": "rejected", "code": safeCode(err)})
		_, _ = fmt.Fprintln(os.Stderr, string(encoded))
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 {
		return errors.New("arguments_not_allowed")
	}
	config, err := receiver.LoadConfig(os.Getenv)
	if err != nil {
		return errors.New("configuration_invalid")
	}
	client, err := receiver.NewClient(config, nil)
	if err != nil {
		return errors.New("configuration_invalid")
	}
	result, err := client.Pull(context.Background(), time.Now())
	if err != nil {
		return err
	}
	if result.NoWork {
		_, err = fmt.Fprintln(os.Stdout, `{"decision":"no_work"}`)
		return err
	}
	outputPath := os.Getenv("CLAIM_ENVELOPE_PATH")
	if !filepath.IsAbs(outputPath) || filepath.Clean(outputPath) != outputPath {
		return errors.New("output_path_invalid")
	}
	encodedEnvelope, err := json.Marshal(result.Envelope)
	if err != nil {
		return errors.New("envelope_encode_failed")
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("envelope_write_failed")
	}
	if _, err := file.Write(encodedEnvelope); err != nil {
		_ = file.Close()
		return errors.New("envelope_write_failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("envelope_write_failed")
	}
	output, err := json.Marshal(struct {
		Decision string `json:"decision"`
		receiver.Receipt
	}{Decision: "accepted", Receipt: result.Receipt})
	if err != nil {
		return errors.New("receipt_encode_failed")
	}
	_, err = fmt.Fprintln(os.Stdout, string(output))
	return err
}

func safeCode(err error) string {
	var validation *receiver.ValidationError
	if errors.As(err, &validation) {
		return validation.Code
	}
	switch err.Error() {
	case "arguments_not_allowed", "configuration_invalid", "output_path_invalid", "envelope_encode_failed", "envelope_write_failed", "receipt_encode_failed":
		return err.Error()
	default:
		return "unexpected_failure"
	}
}
