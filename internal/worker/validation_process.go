package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

type cappedCommandOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (w *cappedCommandOutput) Write(value []byte) (int, error) {
	original := len(value)
	remaining := w.limit + 1 - w.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = w.buffer.Write(value)
	}
	return original, nil
}

func runCredentialFreeCommand(
	ctx context.Context,
	directory string,
	environment []string,
	timeout time.Duration,
	maxOutputBytes int,
	arguments []string,
) ([]byte, error) {
	if ctx == nil || timeout <= 0 || !validFixedArguments(arguments) || maxOutputBytes < 0 {
		return nil, errors.New("command contract is invalid")
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, arguments[0], arguments[1:]...)
	command.Dir = directory
	command.Env = append([]string(nil), environment...)
	command.Stdin = nil
	command.Stderr = io.Discard
	var output cappedCommandOutput
	if maxOutputBytes > 0 {
		output.limit = maxOutputBytes
		command.Stdout = &output
	} else {
		command.Stdout = io.Discard
	}
	configureProcessGroup(command)
	err := command.Run()
	terminateProcessGroup(command)
	if err != nil || commandContext.Err() != nil || output.buffer.Len() > maxOutputBytes {
		return nil, errors.New("command failed")
	}
	return append([]byte(nil), output.buffer.Bytes()...), nil
}

func validFixedArguments(arguments []string) bool {
	if len(arguments) == 0 || len(arguments) > 32 {
		return false
	}
	for _, argument := range arguments {
		if argument == "" || len(argument) > 1024 || strings.ContainsAny(argument, "\r\n\x00") {
			return false
		}
	}
	return true
}
