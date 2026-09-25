package initwizard

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// The means are what the engine is handed beyond the destination's own
// repository: the secrets it may use, and the account it may create things
// in. Neither is asked for. A setup that needs none — every setup that
// delivers a pull request and nothing else — writes nothing and gets
// nothing, and a person at the terminal is never shown a question about a
// cloud account they do not have.
//
// They are read from the answers file, which is where the project's own
// facts already live, and carried in the journal so that the two generated
// configurations can be built from one place.

// Means is what one project hands its engine.
type Means struct {
	Credentials    []runtimeconfig.Credential   `json:"credentials,omitempty"`
	Infrastructure *worker.InfrastructureConfig `json:"infrastructure,omitempty"`
}

// credentialAnswer matches the per-credential answer ids. The name is the
// middle of the id, so one credential is three adjacent lines in the file
// rather than a nested object nobody can diff.
var credentialAnswer = regexp.MustCompile(`^credential-([a-z0-9][a-z0-9-]*)-(path|env|stages)$`)

// MeansFromAnswers reads the optional blocks. An answers file that names
// none returns the zero value, which is what every project delivering only
// pull requests has.
func MeansFromAnswers(answers Answers) (Means, error) {
	var means Means
	credentials, err := credentialsFromAnswers(answers)
	if err != nil {
		return Means{}, err
	}
	means.Credentials = credentials
	infrastructure, err := infrastructureFromAnswers(answers)
	if err != nil {
		return Means{}, err
	}
	means.Infrastructure = infrastructure
	if err := means.validate(); err != nil {
		return Means{}, err
	}
	return means, nil
}

// validate holds the answers to the same rules the engine's own load does,
// so a mistyped one is named here — where the file can still be edited —
// rather than by a body that will not start.
func (m Means) validate() error {
	if err := runtimeconfig.ValidateCredentials(m.Credentials); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "runtime config: "))
	}
	if m.Infrastructure == nil || m.Infrastructure.Credential == "" {
		return nil
	}
	for _, credential := range m.Credentials {
		if credential.Name == m.Infrastructure.Credential {
			return nil
		}
	}
	return fmt.Errorf("infrastructure-credential が %q を指していますが、credential-%s-path がありません", m.Infrastructure.Credential, m.Infrastructure.Credential)
}

func credentialsFromAnswers(answers Answers) ([]runtimeconfig.Credential, error) {
	names := make([]string, 0, 4)
	seen := map[string]bool{}
	for id := range answers.Answers {
		match := credentialAnswer.FindStringSubmatch(id)
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	// Sorted, because the map is not: the generated configuration is
	// digested, and a list whose order changed between two runs of apply
	// would read as a changed configuration.
	sort.Strings(names)
	credentials := make([]runtimeconfig.Credential, 0, len(names))
	for _, name := range names {
		path, ok := answers.Value("credential-" + name + "-path")
		if !ok {
			return nil, fmt.Errorf("credential-%s-path がありません (env と stages だけでは鍵の在り処が分かりません)", name)
		}
		variables, err := answerList(answers, "credential-"+name+"-env")
		if err != nil || len(variables) == 0 {
			return nil, fmt.Errorf("credential-%s-env に環境変数名を書いてください (1 つなら文字列、複数なら JSON 配列)", name)
		}
		stages, err := answerList(answers, "credential-"+name+"-stages")
		if err != nil || len(stages) == 0 {
			return nil, fmt.Errorf("credential-%s-stages に渡す工程を JSON 配列で書いてください (%s)", name, strings.Join(runtimeconfig.DispatchedStages(), " / "))
		}
		credentials = append(credentials, runtimeconfig.Credential{
			Name: name, Path: path, Env: variables, Stages: stages,
		})
	}
	if len(credentials) == 0 {
		return nil, nil
	}
	return credentials, nil
}

func infrastructureFromAnswers(answers Answers) (*worker.InfrastructureConfig, error) {
	provider, ok := answers.Value("infrastructure-provider")
	if !ok {
		// Nothing declared. The other infrastructure answers are refused
		// rather than ignored: a file that names a region and no provider
		// has lost a line, and silently delivering without the account is
		// the failure this whole block exists to avoid.
		for _, id := range []string{"infrastructure-region", "infrastructure-credential", "infrastructure-resources", "infrastructure-naming-prefix"} {
			if _, present := answers.Value(id); present {
				return nil, errors.New(id + " がありますが infrastructure-provider がありません")
			}
		}
		return nil, nil
	}
	resources, err := answerList(answers, "infrastructure-resources")
	if err != nil {
		return nil, errors.New("infrastructure-resources は資源の種類の JSON 配列です")
	}
	region, _ := answers.Value("infrastructure-region")
	credential, _ := answers.Value("infrastructure-credential")
	prefix, _ := answers.Value("infrastructure-naming-prefix")
	infrastructure := &worker.InfrastructureConfig{
		Provider: provider, Region: region, Credential: credential,
		Resources: resources, NamingPrefix: prefix,
	}
	if err := worker.ValidateInfrastructure(*infrastructure); err != nil {
		return nil, err
	}
	return infrastructure, nil
}

// answerList reads an answer that is either one name or a JSON array of
// them. One name is what a person writes for the common case, and refusing
// it would make the file's shape depend on how many of something there are.
func answerList(answers Answers, id string) ([]string, error) {
	value, ok := answers.Value(id)
	if !ok {
		return nil, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(value), "[") {
		return []string{value}, nil
	}
	var list []string
	if json.Unmarshal([]byte(value), &list) != nil {
		return nil, errors.New(id + " は文字列の JSON 配列です")
	}
	for index, entry := range list {
		list[index] = strings.TrimSpace(entry)
	}
	return list, nil
}
