package worker

import "testing"

// A declared scope must be able to mean what it says: an absolute path or a
// ".." segment never matches a delivered path, a padded or empty entry
// covers nothing while looking declared, and a glob the matcher rejects
// would silently cover nothing.
func TestDeployPathsRefuseWhatCannotMatchADeliveredPath(t *testing.T) {
	for _, bad := range [][]string{{""}, {" docs/"}, {"docs/ "}, {"/docs"}, {"../docs"}, {"docs/../src"}, {"."}, {".."}, {"./docs"}, {"["}} {
		if err := (ConsumerWorkflow{DeployPaths: bad}).validateDeployPaths(); err == nil {
			t.Errorf("deploy_paths %q accepted", bad)
		}
	}
	for _, good := range [][]string{{"docs/"}, {"src", "app/"}, {"*.go"}, {"cmd/*/main.go"}, nil} {
		if err := (ConsumerWorkflow{DeployPaths: good}).validateDeployPaths(); err != nil {
			t.Errorf("deploy_paths %q refused: %v", good, err)
		}
	}
}
