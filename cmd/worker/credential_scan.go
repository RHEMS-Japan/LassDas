package main

import (
	"errors"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/cardsecret"
	runtimecfg "automation.internal/ticket-ingress/internal/runtime"
)

// The last gate before a change leaves refuses one that carries a handed
// credential (internal/worker/artifact.go). For that gate to mean anything
// it has to know every credential this deployment provisions, not only the
// ones the sealing card happens to have been handed: the ordinary
// configuration hands a credential to the card that writes the change,
// while a different card seals it.
//
// Handing the value to the sealing card to make the check work would be the
// wrong repair — the list of stages exists so that a value reaches the
// cards that need it and no others. What is true instead is that the
// sealing worker is the engine's own process, running as the engine's user.
// The guarded-files check that closes a credential file is about the AI
// user, not this one; every card's entry point already reads the files it
// hands out the same way. So this process reads them all, and keeps them
// for the comparison and for nothing else: no agent environment, no
// validation sandbox, no mask changes.

// registerCredentialsForScan reads every credential the runtime
// configuration names and registers it for the candidate check.
//
// A file that cannot be read refuses the seal, naming the variable. The
// alternative is sealing a change nothing compared against that credential,
// which is the silence this whole gate exists to end — and a credential the
// operator provisioned and the engine cannot read is a broken deployment
// either way, said here rather than at the card that needed it.
func registerCredentialsForScan() error {
	path := os.Getenv("LASSDAS_RUNTIME_CONFIG")
	if path == "" {
		// Nothing dispatched this process as a card of a pod: the local
		// paths that run a verb by hand have no runtime configuration and
		// no credentials to compare against.
		return nil
	}
	config, err := runtimecfg.Load(path)
	if err != nil {
		return errors.New("the credentials to check this change against could not be read")
	}
	entries := make([]cardsecret.Entry, 0, len(config.Chain.Credentials))
	for _, credential := range config.Chain.Credentials {
		contents, err := readCredentialForScan(credential)
		if err != nil {
			return err
		}
		entries = append(entries, cardsecret.Entry{Name: credentialVariable(credential), Secret: contents})
	}
	cardsecret.RegisterForScan(entries)
	return nil
}

// readCredentialForScan reads one provisioned file. The variable is in the
// error and the contents never are: a refusal travels into the round's
// record and onto the ticket.
func readCredentialForScan(credential runtimecfg.Credential) (string, error) {
	raw, err := cardsecret.ReadCredentialFile(credential.Path)
	if err != nil {
		return "", errors.New("the credential in " + credentialVariable(credential) + " is " + err.Error() + ", so this change cannot be checked against it")
	}
	return strings.TrimRight(string(raw), " \t\r\n"), nil
}

// credentialVariable names a credential the way a person reading a refusal
// looks for it: the variable it is exported as, or its configured name
// where it has no variable at all.
func credentialVariable(credential runtimecfg.Credential) string {
	if len(credential.Env) > 0 {
		return credential.Env[0]
	}
	return credential.Name
}
