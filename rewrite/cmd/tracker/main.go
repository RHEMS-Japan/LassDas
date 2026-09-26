// A configured role may use this command to communicate with its issue. This
// command neither judges the report nor marks a request complete.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"ticket-runner/internal/tracker"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output, log io.Writer) error {
	flags := flag.NewFlagSet("tracker", flag.ContinueOnError)
	flags.SetOutput(log)
	base := flags.String("base-url", "", "configured Backlog API base URL")
	key := flags.String("key-env", "", "name of the credential environment variable, never its value")
	issue := flags.String("issue", "", "the assigned issue id or key")
	after := flags.Int64("after-id", 0, "list comments after this API id")
	id := flags.Int64("comment-id", 0, "read this posted comment")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *base == "" || *key == "" || *issue == "" || flags.NArg() != 1 {
		return errors.New("provide --base-url, --key-env, --issue and one action: read, comments, comment, or post")
	}
	action := flags.Arg(0)
	if *after < 0 || (*after != 0 && action != "comments") || (*id != 0 && action != "comment") {
		return errors.New("comment selection flags do not match the action")
	}
	b := tracker.Backlog{BaseURL: *base, KeyEnv: *key}
	var data any
	var err error
	switch action {
	case "read":
		var text string
		text, err = b.Request(ctx, *issue)
		if err == nil {
			_, err = io.WriteString(output, text)
		}
		return err
	case "comments":
		data, err = b.Comments(ctx, *issue, *after)
	case "comment":
		data, err = b.Comment(ctx, *issue, *id)
	case "post":
		var content []byte
		content, err = io.ReadAll(input)
		if err == nil {
			data, err = b.AddComment(ctx, *issue, string(content))
		}
	default:
		return errors.New("choose read, comments, comment, or post")
	}
	if err != nil {
		return err
	}
	// Native API receipts are observations, not a required working-answer shape.
	return json.NewEncoder(output).Encode(data)
}
