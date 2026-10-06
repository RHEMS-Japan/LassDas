package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Kubernetes templates in deploy/ticket-engine and the operator
// configurations in rewrite/examples are copied and filled in one by one,
// and what one of them names another has to provide: SETUP.md lists these
// references under "Keep object names and references together". The tests
// below read the shipped files, each <placeholder> filled in, for those
// references, and start the engine on every example as the StatefulSet
// starts it, on a new volume.
//
// The module has no YAML library and these tests add none. A template is
// read as environment_delivery_fixture_test.go reads one, by its own lines:
// each reading below takes one shape the templates are written in, and a
// line of the kind it reads in any other shape is reported, not passed over,
// so a template rewritten in another shape fails here instead of passing
// unread.

const shippedTemplates = "../../../deploy/ticket-engine/"

var (
	templatePlaceholder = regexp.MustCompile(`<([a-z][a-z0-9-]*)>`)
	templateDocument    = regexp.MustCompile(`(?m)^---$`)
	templateSecretRef   = regexp.MustCompile(`(?m)^\s*- name: (\S+)\n\s+valueFrom:\n\s+secretKeyRef: \{ name: ([^,\s]+), key: ([^,\s}]+) \}$`)
	templateMount       = regexp.MustCompile(`^\s*- \{ name: ([^,\s]+), mountPath: ([^,\s}]+)(?:, readOnly: (?:true|false))? \}$`)
	templateConfigMap   = regexp.MustCompile(`(?m)^\s*- name: (\S+)\n\s+configMap:(?: \{ name: ([^,\s}]+) \}|\n\s+name: (\S+))$`)
	templateName        = regexp.MustCompile(`^\s+name: (\S+)$`)
	templateKey         = regexp.MustCompile(`^\s*([A-Za-z0-9_.-]+):(?:\s|$)`)
	setupConfigMap      = regexp.MustCompile(`create configmap (\S+) \\\n\s*--from-file=([^=\s]+)=`)
)

// shippedSet is the text of every file the checks read, by name: the
// templates and SETUP.md with each <placeholder> filled in the same way, as
// one operator's copies would be, and the examples as shipped.
func shippedSet(t *testing.T) map[string]string {
	t.Helper()
	set := map[string]string{}
	for _, name := range []string{"statefulset.yaml.example", "secrets.yaml.example", "egress-configmap.yaml.example", "operator-scripts-configmap.yaml.example", "SETUP.md"} {
		data, err := os.ReadFile(shippedTemplates + name)
		if err != nil {
			t.Fatal(err)
		}
		set[name] = templatePlaceholder.ReplaceAllString(string(data), "fixture-$1")
	}
	examples, err := filepath.Glob("../../examples/*.json")
	if err != nil || len(examples) == 0 {
		t.Fatalf("no shipped example found: %v", err)
	}
	for _, path := range examples {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		set["examples/"+filepath.Base(path)] = string(data)
	}
	return set
}

// templateLines are a template's lines without the blank ones and the
// comments.
func templateLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			lines = append(lines, strings.TrimRight(line, " \r"))
		}
	}
	return lines
}

func templateIndent(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// templateUnder is the lines indented deeper than the one line that reads
// header, up to the next line that is not; ok is false unless exactly one
// line reads header.
func templateUnder(lines []string, header string) (under []string, ok bool) {
	at := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header {
			if at >= 0 {
				return nil, false
			}
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	for _, line := range lines[at+1:] {
		if templateIndent(line) <= templateIndent(lines[at]) {
			break
		}
		under = append(under, line)
	}
	return under, true
}

// templateObjects reads each document of a template file as an object of
// the given kind: its metadata name and the keys directly under section.
// ok is false when a document is not one.
func templateObjects(text, kind, section string) (objects map[string]map[string]bool, ok bool) {
	objects = map[string]map[string]bool{}
	for _, document := range templateDocument.Split(text, -1) {
		lines := templateLines(document)
		if len(lines) == 0 {
			continue
		}
		metadata, named := templateUnder(lines, "metadata:")
		block, held := templateUnder(lines, section)
		if !slices.Contains(lines, "kind: "+kind) || !named || !held || len(block) == 0 {
			return nil, false
		}
		name := ""
		for _, line := range metadata {
			if m := templateName.FindStringSubmatch(line); m != nil && name == "" {
				name = m[1]
			}
		}
		keys := map[string]bool{}
		for _, line := range block {
			// A line indented deeper than the first is part of a key's value.
			if templateIndent(line) != templateIndent(block[0]) {
				continue
			}
			m := templateKey.FindStringSubmatch(line)
			if m == nil || name == "" {
				return nil, false
			}
			keys[m[1]] = true
		}
		objects[name] = keys
	}
	return objects, len(objects) > 0
}

// templateFlag is the item after flag in a command, or "" without one.
func templateFlag(command []string, flag string) string {
	if i := slices.Index(command, flag); i >= 0 && i+1 < len(command) {
		return command[i+1]
	}
	return ""
}

type templateSecretUse struct{ env, secret, key string }

type templateContainer struct {
	command []string
	secrets []templateSecretUse
	mounts  map[string]string // each volume's mount path in this container
}

type shippedReading struct {
	containers map[string]templateContainer // mirror, engine and status
	secrets    map[string]map[string]bool   // each Secret's keys
	configMaps map[string]map[string]bool   // each ConfigMap's keys: the two example ConfigMaps and the one SETUP.md creates
	scripts    string                       // the name of the operator-scripts ConfigMap
	volumes    map[string]string            // the ConfigMap of each configMap volume
	mounts     map[string][]string          // each volume's mount paths, in any container
	pod        string                       // the StatefulSet without its comments
}

// readShipped reads what the checks compare out of the set, with a problem
// for each part it cannot read.
func readShipped(set map[string]string) (shippedReading, []string) {
	var problems []string
	say := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	r := shippedReading{containers: map[string]templateContainer{}, configMaps: map[string]map[string]bool{}, volumes: map[string]string{}, mounts: map[string][]string{}}
	var ok bool
	if r.secrets, ok = templateObjects(set["secrets.yaml.example"], "Secret", "stringData:"); !ok {
		say("secrets.yaml.example: a document this check cannot read as a Secret with stringData")
	}
	for _, file := range []string{"egress-configmap.yaml.example", "operator-scripts-configmap.yaml.example"} {
		objects, ok := templateObjects(set[file], "ConfigMap", "data:")
		if !ok || len(objects) != 1 {
			say("%s: not one ConfigMap with data this check can read", file)
		}
		for name, keys := range objects {
			r.configMaps[name] = keys
			if file == "operator-scripts-configmap.yaml.example" {
				r.scripts = name
			}
		}
	}
	created := setupConfigMap.FindAllStringSubmatch(set["SETUP.md"], -1)
	if len(created) == 0 {
		say("SETUP.md: no command that creates the operator configuration's ConfigMap")
	}
	for _, m := range created {
		if r.configMaps[m[1]] == nil {
			r.configMaps[m[1]] = map[string]bool{}
		}
		r.configMaps[m[1]][m[2]] = true
	}
	pod := templateLines(set["statefulset.yaml.example"])
	r.pod = strings.Join(pod, "\n")
	read := 0
	for _, name := range []string{"mirror", "engine", "status"} {
		block, ok := templateUnder(pod, "- name: "+name)
		if !ok {
			say("statefulset.yaml.example: not one container named %s", name)
			continue
		}
		c := templateContainer{mounts: map[string]string{}}
		text := strings.Join(block, "\n")
		for _, m := range templateSecretRef.FindAllStringSubmatch(text, -1) {
			c.secrets = append(c.secrets, templateSecretUse{env: m[1], secret: m[2], key: m[3]})
		}
		if strings.Count(text, "secretKeyRef") != len(c.secrets) {
			say("statefulset.yaml.example: container %s has a secretKeyRef this check cannot read", name)
		}
		read += strings.Count(text, "secretKeyRef")
		if name != "mirror" {
			command, ok := templateUnder(block, "command:")
			if !ok || len(command) == 0 {
				say("statefulset.yaml.example: container %s has no command written one item to a line", name)
			}
			for _, line := range command {
				item, found := strings.CutPrefix(strings.TrimSpace(line), "- ")
				if !found {
					say("statefulset.yaml.example: container %s has a command line this check cannot read: %s", name, strings.TrimSpace(line))
				}
				c.command = append(c.command, item)
			}
		}
		// A mount line in another shape is reported once, below.
		mounts, _ := templateUnder(block, "volumeMounts:")
		for _, line := range mounts {
			if m := templateMount.FindStringSubmatch(line); m != nil {
				c.mounts[m[1]] = m[2]
			}
		}
		r.containers[name] = c
	}
	if strings.Count(r.pod, "secretKeyRef") != read {
		say("statefulset.yaml.example: a secretKeyRef outside the mirror, engine and status containers, which this check does not read")
	}
	for _, line := range pod {
		if !strings.Contains(line, "mountPath") {
			continue
		}
		if m := templateMount.FindStringSubmatch(line); m != nil {
			r.mounts[m[1]] = append(r.mounts[m[1]], m[2])
		} else {
			say("statefulset.yaml.example: a volume mount this check cannot read: %s", strings.TrimSpace(line))
		}
	}
	volumes, ok := templateUnder(pod, "volumes:")
	text := strings.Join(volumes, "\n")
	found := templateConfigMap.FindAllStringSubmatch(text, -1)
	if !ok || strings.Count(text, "configMap:") != len(found) {
		say("statefulset.yaml.example: a configMap volume this check cannot read")
	}
	for _, m := range found {
		r.volumes[m[1]] = m[2] + m[3]
	}
	return r, problems
}

// shippedProblems is every reference among the shipped files that does not
// agree, each said with the letter of the check it belongs to.
func shippedProblems(set map[string]string) []string {
	r, problems := readShipped(set)
	say := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	engine, status := r.containers["engine"], r.containers["status"]

	// (a) What the engine and the mirror take from a Secret is a key of a
	// Secret that secrets.yaml.example defines.
	credentials := map[string]bool{}
	for _, name := range []string{"mirror", "engine"} {
		for _, use := range r.containers[name].secrets {
			credentials[use.secret] = true
			if !r.secrets[use.secret][use.key] {
				say("(a) container %s takes %s from key %s of Secret %s, which secrets.yaml.example does not hold", name, use.env, use.key, use.secret)
			}
		}
	}
	if len(engine.secrets) == 0 {
		say("(a) the engine container takes no credential from a Secret")
	}

	// (b) Each configMap volume mounts a ConfigMap that an example ConfigMap
	// or SETUP.md creates, each of those is mounted, and a file a container
	// names under a ConfigMap's mount is one of that ConfigMap's keys.
	mounted := map[string]bool{}
	for volume, configMap := range r.volumes {
		mounted[configMap] = true
		keys, created := r.configMaps[configMap]
		if !created {
			say("(b) volume %s mounts ConfigMap %s, which neither an example ConfigMap nor SETUP.md creates", volume, configMap)
			continue
		}
		for _, mount := range r.mounts[volume] {
			named := regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(mount) + `/([A-Za-z0-9_.-]+)`)
			for _, m := range named.FindAllStringSubmatch(r.pod, -1) {
				if !keys[m[1]] {
					say("(b) %s/%s is read through volume %s, but ConfigMap %s has no key %s", mount, m[1], volume, configMap, m[1])
				}
			}
		}
	}
	for configMap := range r.configMaps {
		if !mounted[configMap] {
			say("(b) ConfigMap %s is created, but no volume of the StatefulSet mounts it", configMap)
		}
	}

	// (c) The engine reads the configuration the config volume holds and
	// writes its log inside its queue. The status page reads the same
	// configuration and queue and shows <run-dir>/engine.log (cmd/status).
	config, queue, log := templateFlag(engine.command, "--config"), templateFlag(engine.command, "--run-dir"), templateFlag(engine.command, "--log-file")
	if config != "/etc/ticket-automation/operator.json" {
		say("(c) the engine reads --config %q, not /etc/ticket-automation/operator.json", config)
	}
	if queue == "" || log == "" {
		say("(c) the engine is started without --run-dir or --log-file")
	} else if within, err := filepath.Rel(queue, filepath.Dir(log)); err != nil || within == ".." || strings.HasPrefix(within, "../") {
		say("(c) the engine's --log-file %s is outside its --run-dir %s", log, queue)
	}
	if templateFlag(status.command, "--config") != config || templateFlag(status.command, "--run-dir") != queue {
		say("(c) the status page reads --config %q and --run-dir %q, the engine %q and %q", templateFlag(status.command, "--config"), templateFlag(status.command, "--run-dir"), config, queue)
	}
	if log != filepath.Join(queue, "engine.log") {
		say("(c) the status page shows %s, but the engine writes its log to %q", filepath.Join(queue, "engine.log"), log)
	}

	// (d) The programs the examples run from the operator-scripts
	// ConfigMap's mount are its keys, and each of its keys is run by one.
	var scripts []string
	for volume, configMap := range r.volumes {
		if configMap == r.scripts {
			scripts = append(scripts, r.mounts[volume]...)
		}
	}
	ran := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		example, isExample := strings.CutPrefix(name, "examples/")
		if !isExample {
			continue
		}
		cfg, err := readConfig([]byte(set[name]))
		if err != nil {
			say("(d) %s cannot be read: %v", example, err)
			continue
		}
		for _, role := range cfg.Roles {
			for _, process := range role.Processes {
				commands := slices.Concat(process.Command, slices.Collect(maps.Values(process.Env)))
				for _, mount := range scripts {
					named := regexp.MustCompile(regexp.QuoteMeta(mount) + `/([A-Za-z0-9_.-]+)`)
					for _, command := range commands {
						for _, m := range named.FindAllStringSubmatch(command, -1) {
							ran[m[1]] = true
							if !r.configMaps[r.scripts][m[1]] {
								say("(d) %s runs %s/%s, which the operator-scripts ConfigMap does not hold", example, mount, m[1])
							}
						}
					}
				}
			}
		}
	}
	if len(ran) == 0 {
		say("(d) no example runs a program from the operator-scripts ConfigMap's mount")
	}
	for key := range r.configMaps[r.scripts] {
		if !ran[key] {
			say("(d) the operator-scripts ConfigMap holds %s, which no example runs", key)
		}
	}

	// (e) The status page's two basic-authentication values are keys of a
	// Secret that secrets.yaml.example defines and that neither the engine
	// nor the mirror reads.
	for _, flag := range []string{"--auth-user-env", "--auth-password-env"} {
		env, taken := templateFlag(status.command, flag), false
		for _, use := range status.secrets {
			if use.env != env {
				continue
			}
			taken = true
			if !r.secrets[use.secret][use.key] {
				say("(e) the status page's %s %s is key %s of Secret %s, which secrets.yaml.example does not hold", flag, env, use.key, use.secret)
			}
			if credentials[use.secret] {
				say("(e) the status page's %s %s comes from Secret %s, which the engine or the mirror reads as well", flag, env, use.secret)
			}
		}
		if !taken {
			say("(e) the status page is started with %s %q, which the status container does not take from a Secret", flag, env)
		}
	}
	slices.Sort(problems)
	return slices.Compact(problems)
}

func TestShippedTemplatesAgreeWithEachOtherAndTheExamples(t *testing.T) {
	set := shippedSet(t)
	r, _ := readShipped(set)
	t.Logf("Secrets %v; ConfigMaps %v; configMap volumes %v; mounts %v", r.secrets, r.configMaps, r.volumes, r.mounts)
	for _, name := range []string{"mirror", "engine", "status"} {
		t.Logf("%s: command %q, from Secrets %v, mounts %v", name, r.containers[name].command, r.containers[name].secrets, r.containers[name].mounts)
	}
	for _, problem := range shippedProblems(set) {
		t.Error(problem)
	}
}

// Each check notices a copy broken the way an edit could break it, so that
// a check that read nothing cannot pass for one that found nothing wrong.
func TestShippedTemplateChecksNoticeABrokenCopy(t *testing.T) {
	for _, broken := range []struct{ name, file, old, new, want string }{
		{"renamed Secret key", "secrets.yaml.example", "  TRACKER_API_KEY: \"\"\n", "  TRACKER_KEY: \"\"\n",
			"(a) container engine takes TRACKER_API_KEY from key TRACKER_API_KEY of Secret fixture-consumer-ticket-engine,"},
		{"secretKeyRef in another shape", "statefulset.yaml.example", "secretKeyRef: { name: fixture-consumer-ticket-engine, key: MODEL_API_KEY }",
			"secretKeyRef:\n                  name: fixture-consumer-ticket-engine\n                  key: MODEL_API_KEY", "container engine has a secretKeyRef this check cannot read"},
		{"renamed ConfigMap", "egress-configmap.yaml.example", "  name: fixture-consumer-ticket-engine-egress\n", "  name: fixture-consumer-ticket-engine-network\n",
			"(b) volume network-policy mounts ConfigMap fixture-consumer-ticket-engine-egress,"},
		{"renamed rules key", "egress-configmap.yaml.example", "  ipv6.rules: |\n", "  ip6.rules: |\n",
			"(b) /policy/ipv6.rules is read through volume network-policy"},
		{"log outside the queue", "statefulset.yaml.example", "            - /var/lib/ticket-automation/queue/engine.log\n", "            - /var/lib/ticket-automation/engine.log\n",
			"(c) the engine's --log-file /var/lib/ticket-automation/engine.log is outside its --run-dir /var/lib/ticket-automation/queue"},
		{"renamed operator program", "operator-scripts-configmap.yaml.example", "  confirm-report: |\n", "  confirm: |\n",
			"(d) operator-stages.json runs /opt/ticket-automation/operator/confirm-report,"},
		{"status credential from elsewhere", "statefulset.yaml.example", "            - STATUS_PASSWORD\n", "            - STATUS_SECRET\n",
			"(e) the status page is started with --auth-password-env \"STATUS_SECRET\","},
	} {
		t.Run(broken.name, func(t *testing.T) {
			set := shippedSet(t)
			if strings.Count(set[broken.file], broken.old) != 1 {
				t.Fatalf("%s no longer holds %q exactly once", broken.file, broken.old)
			}
			set[broken.file] = strings.Replace(set[broken.file], broken.old, broken.new, 1)
			problems := shippedProblems(set)
			if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, broken.want) }) {
				t.Fatalf("%q in place of %q was not noticed: %q", broken.new, broken.old, problems)
			}
		})
	}
}

// finishedExample is a shipped example as SETUP.md section 4 has an
// operator finish it for the first start: the same replacements, the
// project's guidance in place of the example's first sentence, and the
// intake still closed. Hosts stay fictitious, under example.test where the
// examples have example.invalid.
func finishedExample(t *testing.T, example string) []byte {
	t.Helper()
	text := strings.NewReplacer(
		`"project_id": 0,`, `"project_id": 17,`,
		"REPLACE_WITH_RFC3339_ACCEPTANCE_START", "2100-01-01T00:00:00Z",
		"REPLACE_WITH_OWNER/REPLACE_WITH_REPOSITORY", "fixture-owner/fixture-intake",
		"https://repository.example.invalid/example-owner/example-repository.git", "/var/lib/ticket-automation/mirror/fixture-owner/fixture-repository.git",
		"example-owner/example-repository", "fixture-owner/fixture-repository",
		"example-integration-branch", "integration",
		"example.invalid", "example.test",
	).Replace(example)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		t.Fatal(err)
	}
	if raw, ok := fields["instructions"]; ok {
		var instructions string
		if err := json.Unmarshal(raw, &instructions); err != nil {
			t.Fatal(err)
		}
		if first, shared, found := strings.Cut(instructions, ". "); found && strings.HasPrefix(first, "Operator setup is incomplete:") {
			instructions = "Read the project's CONTRIBUTING.md before changing its source.\n\n" + shared
		}
		fields["instructions"], _ = json.Marshal(instructions)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// onVolumes is a command with each path under one of the mounts moved to
// the same place under that volume's own directory in root.
func onVolumes(command []string, mounts map[string]string, root string) []string {
	moved := slices.Clone(command)
	for i, item := range moved {
		longest := ""
		for volume, mount := range mounts {
			if (item == mount || strings.HasPrefix(item, mount+"/")) && len(mount) > len(longest) {
				longest = mount
				moved[i] = filepath.Join(root, volume, strings.TrimPrefix(item, mount))
			}
		}
	}
	return moved
}

// A first start on a new volume, as SETUP.md section 6 makes one: each
// shipped example finished as section 4 has an operator finish it, started
// with the StatefulSet's own command and with only the credentials its
// Secret gives the engine container. The engine creates its queue and its
// log on the empty volume, says first which issues it takes up, and asks the
// tracker for them with the tracker's credential; the test stops it at that
// first question.
func TestEveryShippedExampleStartsOnANewVolumeAsTheStatefulSetStartsIt(t *testing.T) {
	set := shippedSet(t)
	shipped, problems := readShipped(set)
	engine := shipped.containers["engine"]
	if len(problems) > 0 || len(engine.command) < 2 {
		t.Fatalf("the engine container cannot be read: %q", problems)
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if example, ok := strings.CutPrefix(name, "examples/"); ok {
			t.Run(example, func(t *testing.T) { startOnNewVolume(t, engine, set[name]) })
		}
	}
}

func startOnNewVolume(t *testing.T, engine templateContainer, example string) {
	root := t.TempDir()
	args := onVolumes(engine.command[1:], engine.mounts, root)
	configPath, queue, logFile := templateFlag(args, "--config"), templateFlag(args, "--run-dir"), templateFlag(args, "--log-file")
	for _, path := range []string{configPath, queue, logFile} {
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatalf("%q is on none of the engine container's volumes: %q", path, args)
		}
	}
	// Every volume is there and empty, as in a new Pod, and the
	// configuration is the one file of the config volume.
	for volume := range engine.mounts {
		if err := os.MkdirAll(filepath.Join(root, volume), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := finishedExample(t, example)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	address, keyEnv := cfg.Backlog.BaseURL, cfg.Backlog.KeyEnv
	if cfg.GitHub != nil {
		address, keyEnv = cfg.GitHub.APIURL, cfg.GitHub.KeyEnv
	}
	trackerURL, err := url.Parse(address)
	if err != nil || trackerURL.Host == "" || keyEnv == "" {
		t.Fatalf("the example names no tracker address or credential: %q %q", address, keyEnv)
	}
	// Only what the StatefulSet gives the engine container: the tracker's
	// credential is set only if it is one of those.
	t.Setenv(keyEnv, "")
	for _, use := range engine.secrets {
		t.Setenv(use.env, "fixture-"+strings.ToLower(use.env))
	}
	credential := os.Getenv(keyEnv)
	if credential == "" {
		t.Fatalf("the tracker's credential %s is not one the StatefulSet gives the engine container", keyEnv)
	}

	type question struct {
		method, path, authorization string
		query                       url.Values
	}
	var (
		mu        sync.Mutex
		questions []question
		elsewhere []string
		asked     = make(chan struct{})
	)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		questions = append(questions, question{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.URL.Query()})
		first := len(questions) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[]")
		if first {
			close(asked)
		}
	}))
	defer tracker.Close()
	local, err := url.Parse(tracker.URL)
	if err != nil {
		t.Fatal(err)
	}
	network := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != trackerURL.Host {
			mu.Lock()
			elsewhere = append(elsewhere, r.URL.Host)
			mu.Unlock()
			return nil, fmt.Errorf("only the tracker answers in this test, not %s", r.URL.Host)
		}
		forwarded := r.Clone(r.Context())
		forwarded.URL.Scheme, forwarded.URL.Host = local.Scheme, local.Host
		return network.RoundTrip(forwarded)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- run(ctx, args, io.Discard, io.Discard) }()
	select {
	case <-asked:
	case err := <-result:
		said, _ := os.ReadFile(logFile)
		t.Fatalf("the engine ended before it asked the tracker: %v\nits log:\n%s", err, said)
	case <-time.After(30 * time.Second):
		cancel()
		<-result
		said, _ := os.ReadFile(logFile)
		t.Fatalf("the engine did not ask the tracker within 30 seconds\nits log:\n%s", said)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("the engine ended with %v, not because it was stopped", err)
	}

	mu.Lock()
	first, others := questions[0], slices.Clone(elsewhere)
	mu.Unlock()
	// Nothing but the tracker is asked before a request is accepted: no model
	// is paid for while the intake is closed.
	if len(others) > 0 {
		t.Errorf("the engine asked %q as well as the tracker", others)
	}
	base := strings.TrimRight(trackerURL.Path, "/")
	if cfg.GitHub != nil {
		if first.method != http.MethodGet || first.path != base+"/repos/"+cfg.GitHub.Repository+"/issues" || first.authorization != "Bearer "+credential {
			t.Errorf("the first question to the tracker was %s %s with authorization %q", first.method, first.path, first.authorization)
		}
	} else if first.method != http.MethodGet || first.path != base+"/issues" || first.query.Get("projectId[]") != "17" || first.query.Get("apiKey") != credential {
		t.Errorf("the first question to the tracker was %s %s?%s", first.method, first.path, first.query.Encode())
	}
	if info, err := os.Stat(queue); err != nil || !info.IsDir() {
		t.Fatalf("the queue %s was not created: %v", queue, err)
	}
	said, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	since, err := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(said), "\n")
	if want := intakeScope(cfg, since); line != want {
		t.Errorf("the log begins %q, not with the intake line %q", line, want)
	}
	if strings.Contains(string(said), "the runtime stopped") {
		t.Errorf("the log says the runtime stopped:\n%s", said)
	}
	if entries, err := os.ReadDir(filepath.Join(queue, "jobs")); err != nil || len(entries) != 0 {
		t.Errorf("the queue holds %v after an empty issue list: %v", entries, err)
	}
	t.Logf("%s %s; the log begins %q", first.method, first.path, line)
}
