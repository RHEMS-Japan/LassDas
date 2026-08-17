package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type commandOutput struct {
	Decision string `json:"decision"`
	Code     string `json:"code"`
}

func main() {
	result, err := run(os.Args[1:], os.Getenv, time.Now, nil)
	if err != nil {
		result = commandOutput{Decision: "rejected", Code: errorCode(err)}
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		encoded = []byte(`{"decision":"rejected","code":"output_failed"}`)
		err = reporterFailure("output_failed")
	}
	output := os.Stdout
	if err != nil {
		output = os.Stderr
	}
	_, _ = fmt.Fprintln(output, string(encoded))
	if err != nil {
		os.Exit(1)
	}
}
