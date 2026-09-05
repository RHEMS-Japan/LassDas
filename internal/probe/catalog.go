// Package probe is the investigating designer's only tool: a catalogue of
// read-only measurements the consumer declares, executed by the kernel with
// the kernel's identities, and recorded line by line with a hash chain so
// that what was looked at and what came back can be checked later.
//
// The catalogue is the shape gate (docs/INVESTIGATING_DESIGNER.md §3.2). It
// is not the guard: the guard is the credentials the kernel holds (§3.3).
// The gate exists so that a request outside the declared shapes is refused
// before anything runs, and so that the refusal is itself recorded.
package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Kind names the executor a probe runs on.
type Kind string

const (
	// KindExec runs a fixed argv with regex-shaped slots. No shell.
	KindExec Kind = "exec"
	// KindSQL sends one read statement over the kernel's own connection.
	KindSQL Kind = "sql"
	// KindHTTP performs a GET or HEAD against an allowed host and returns
	// timing and status, never the body.
	KindHTTP Kind = "http"
	// KindRepo reads the baseline working copy. Built in; consumers do not
	// declare it.
	KindRepo Kind = "repo"
)

const (
	// DefaultMaxOutputBytes bounds one measurement's stored output.
	DefaultMaxOutputBytes = 256 * 1024
	// DefaultTimeoutSeconds bounds one measurement's wall time.
	DefaultTimeoutSeconds = 60
	// MaxSlotValueLength bounds a slot value; longer values are refused.
	MaxSlotValueLength = 4096
	// DefaultSQLStatementTimeoutMS is the statement timeout the kernel sets
	// on every read connection when the probe declares none.
	DefaultSQLStatementTimeoutMS = 10000
)

// CookiesObservationJar names the only jar an http probe may carry: the
// observation session the screen check signs in with. The kernel attaches
// it read-only; the model never sees a value.
const CookiesObservationJar = "observation-jar"

// SelectOnlyPattern is the statement shape a sql probe accepts when the
// consumer declares no narrower one: one SELECT, no statement separator.
const SelectOnlyPattern = `^\s*SELECT\b[^;]*$`

var (
	probeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	slotPattern    = regexp.MustCompile(`\{\{([a-z][a-z0-9_]{0,31})\}\}`)
	hostPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$`)
)

// Spec is one declared measurement shape.
type Spec struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`

	// Argv is the fixed command line of an exec probe. A {{slot}} marks a
	// value the model fills; every slot must have a pattern in Args.
	Argv []string `json:"argv,omitempty"`
	// Args maps a slot name to the anchored regular expression its value
	// must match in full. For sql the slot is "query"; for http it is
	// "path" (and optionally "method").
	Args map[string]string `json:"args,omitempty"`

	// DSNEnv names the environment variable, visible to the kernel alone,
	// that carries a sql probe's connection string.
	DSNEnv string `json:"dsn_env,omitempty"`
	// StatementTimeoutMS bounds one statement's execution on the server.
	StatementTimeoutMS int `json:"statement_timeout_ms,omitempty"`

	// Hosts lists the only hosts an http probe may address (lowercase ASCII,
	// no port, no userinfo). The identity provider's host must not be here.
	Hosts []string `json:"hosts,omitempty"`
	// Methods lists the allowed methods; GET and HEAD are the only choices.
	Methods []string `json:"methods,omitempty"`
	// Returns lists what the measurement reports: status, time_total, bytes.
	Returns []string `json:"returns,omitempty"`
	// Cookies names the jar to attach, or is empty for an anonymous request.
	Cookies string `json:"cookies,omitempty"`

	// MaxOutputBytes and TimeoutSeconds override the defaults for this probe.
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	compiled map[string]*regexp.Regexp
}

// Catalog is the validated set of shapes: the consumer's declarations plus
// the built-in repository probes.
type Catalog struct {
	specs map[string]Spec
	order []string
}

// Builtins are the repository probes every catalogue carries. They read the
// baseline working copy and nothing else.
func Builtins() []Spec {
	return []Spec{
		{ID: "repo.list", Kind: KindRepo, Args: map[string]string{"path": `[A-Za-z0-9_./-]{0,200}`}},
		{ID: "repo.read", Kind: KindRepo, Args: map[string]string{"path": `[A-Za-z0-9_./-]{1,200}`}},
		{ID: "repo.grep", Kind: KindRepo, Args: map[string]string{"pattern": `[^\x00-\x1f\x7f]{1,200}`, "path": `[A-Za-z0-9_./-]{0,200}`}},
	}
}

// ParseCatalog decodes the consumer's `probes` array and validates it
// together with the built-ins.
func ParseCatalog(encoded []byte) (Catalog, error) {
	var specs []Spec
	if len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &specs); err != nil {
			return Catalog{}, fmt.Errorf("probe catalogue: %w", err)
		}
	}
	return NewCatalog(specs)
}

// NewCatalog validates the consumer's specs, adds the built-ins and refuses
// any shape that could not be checked before execution.
func NewCatalog(consumer []Spec) (Catalog, error) {
	catalog := Catalog{specs: map[string]Spec{}}
	for _, spec := range Builtins() {
		if err := catalog.add(spec); err != nil {
			return Catalog{}, err
		}
	}
	for _, spec := range consumer {
		if spec.Kind == KindRepo {
			return Catalog{}, fmt.Errorf("probe %q: repo probes are built in and cannot be declared", spec.ID)
		}
		if err := catalog.add(spec); err != nil {
			return Catalog{}, err
		}
	}
	return catalog, nil
}

func (c *Catalog) add(spec Spec) error {
	if !probeIDPattern.MatchString(spec.ID) {
		return fmt.Errorf("probe id %q is invalid", spec.ID)
	}
	if _, exists := c.specs[spec.ID]; exists {
		return fmt.Errorf("probe %q is declared twice", spec.ID)
	}
	compiled, err := compileSlots(spec)
	if err != nil {
		return fmt.Errorf("probe %q: %w", spec.ID, err)
	}
	spec.compiled = compiled
	if spec.MaxOutputBytes < 0 || spec.MaxOutputBytes > 4*DefaultMaxOutputBytes {
		return fmt.Errorf("probe %q: max_output_bytes out of range", spec.ID)
	}
	if spec.TimeoutSeconds < 0 || spec.TimeoutSeconds > 10*DefaultTimeoutSeconds {
		return fmt.Errorf("probe %q: timeout_seconds out of range", spec.ID)
	}
	switch spec.Kind {
	case KindExec:
		if len(spec.Argv) == 0 || strings.TrimSpace(spec.Argv[0]) == "" {
			return fmt.Errorf("probe %q: exec argv is empty", spec.ID)
		}
		referenced := map[string]bool{}
		for _, word := range spec.Argv {
			for _, match := range slotPattern.FindAllStringSubmatch(word, -1) {
				referenced[match[1]] = true
				if _, ok := compiled[match[1]]; !ok {
					return fmt.Errorf("probe %q: slot %q has no pattern", spec.ID, match[1])
				}
			}
		}
		for slot := range compiled {
			if !referenced[slot] {
				return fmt.Errorf("probe %q: pattern %q is never used", spec.ID, slot)
			}
		}
		if spec.DSNEnv != "" || len(spec.Hosts) > 0 || len(spec.Methods) > 0 || spec.Cookies != "" {
			return fmt.Errorf("probe %q: exec probes carry argv and args only", spec.ID)
		}
	case KindSQL:
		if spec.DSNEnv == "" || !envNamePattern.MatchString(spec.DSNEnv) {
			return fmt.Errorf("probe %q: sql probes need dsn_env", spec.ID)
		}
		if len(spec.Argv) > 0 || len(spec.Hosts) > 0 || spec.Cookies != "" {
			return fmt.Errorf("probe %q: sql probes carry dsn_env and a query pattern only", spec.ID)
		}
		if _, ok := compiled["query"]; !ok {
			re, err := regexp.Compile(anchor(SelectOnlyPattern))
			if err != nil {
				return err
			}
			compiled["query"] = re
		}
		for slot := range compiled {
			if slot != "query" {
				return fmt.Errorf("probe %q: sql probes take the query slot only", spec.ID)
			}
		}
		if spec.StatementTimeoutMS < 0 || spec.StatementTimeoutMS > 60000 {
			return fmt.Errorf("probe %q: statement_timeout_ms out of range", spec.ID)
		}
	case KindHTTP:
		if len(spec.Hosts) == 0 {
			return fmt.Errorf("probe %q: http probes need hosts", spec.ID)
		}
		for _, host := range spec.Hosts {
			if !hostPattern.MatchString(host) || isIPLiteral(host) {
				return fmt.Errorf("probe %q: host %q is not a lowercase DNS name", spec.ID, host)
			}
		}
		if len(spec.Methods) == 0 {
			return fmt.Errorf("probe %q: http probes need methods", spec.ID)
		}
		for _, method := range spec.Methods {
			if method != "GET" && method != "HEAD" {
				return fmt.Errorf("probe %q: method %q is not allowed", spec.ID, method)
			}
		}
		for _, field := range spec.Returns {
			switch field {
			case "status", "time_total", "bytes":
			default:
				return fmt.Errorf("probe %q: return %q is unknown", spec.ID, field)
			}
		}
		if spec.Cookies != "" && spec.Cookies != CookiesObservationJar {
			return fmt.Errorf("probe %q: cookies %q is unknown", spec.ID, spec.Cookies)
		}
		if _, ok := compiled["path"]; !ok {
			return fmt.Errorf("probe %q: http probes need a path pattern", spec.ID)
		}
		for slot := range compiled {
			if slot != "path" && slot != "method" {
				return fmt.Errorf("probe %q: http probes take path and method slots only", spec.ID)
			}
		}
		if len(spec.Argv) > 0 || spec.DSNEnv != "" {
			return fmt.Errorf("probe %q: http probes carry hosts, methods, returns and cookies only", spec.ID)
		}
	case KindRepo:
	default:
		return fmt.Errorf("probe %q: kind %q is unknown", spec.ID, spec.Kind)
	}
	c.specs[spec.ID] = spec
	c.order = append(c.order, spec.ID)
	return nil
}

var envNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// isIPLiteral reports a host written as an address rather than a name; the
// catalogue wants names so that the dial-time address check has something
// to resolve and judge.
func isIPLiteral(host string) bool {
	return net.ParseIP(strings.Trim(host, "[]")) != nil
}

func compileSlots(spec Spec) (map[string]*regexp.Regexp, error) {
	compiled := map[string]*regexp.Regexp{}
	names := make([]string, 0, len(spec.Args))
	for name := range spec.Args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slotNamePattern.MatchString(name) {
			return nil, fmt.Errorf("slot name %q is invalid", name)
		}
		pattern := spec.Args[name]
		if strings.TrimSpace(pattern) == "" {
			return nil, fmt.Errorf("slot %q has an empty pattern", name)
		}
		re, err := regexp.Compile(anchor(pattern))
		if err != nil {
			return nil, fmt.Errorf("slot %q: %w", name, err)
		}
		compiled[name] = re
	}
	return compiled, nil
}

var slotNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// anchor wraps a consumer pattern so that it must match the whole value.
// Patterns that already carry anchors keep working; the wrap is idempotent
// in effect.
func anchor(pattern string) string {
	return `^(?:` + pattern + `)$`
}

// Specs returns the catalogue in declaration order, built-ins first.
func (c Catalog) Specs() []Spec {
	out := make([]Spec, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.specs[id])
	}
	return out
}

// Lookup returns a spec by id.
func (c Catalog) Lookup(id string) (Spec, bool) {
	spec, ok := c.specs[id]
	return spec, ok
}

// ForbidHosts refuses a catalogue whose http probes may address any of the
// given hosts — the identity provider's login entry, in practice, which
// rotates the session cookie on every use and would kill the shared jar.
func (c Catalog) ForbidHosts(hosts []string) error {
	forbidden := map[string]bool{}
	for _, host := range hosts {
		forbidden[strings.ToLower(host)] = true
	}
	for _, id := range c.order {
		spec := c.specs[id]
		for _, host := range spec.Hosts {
			if forbidden[host] {
				return fmt.Errorf("probe %q addresses forbidden host %q", id, host)
			}
		}
	}
	return nil
}

// Request is what the model asks for: a probe id and its slot values.
type Request struct {
	Probe string            `json:"probe"`
	Args  map[string]string `json:"args,omitempty"`
}

// Plan is a request the catalogue accepted, with every slot filled.
type Plan struct {
	Spec Spec
	Args map[string]string
	// Argv is the exec command line with slots substituted; empty for
	// other kinds.
	Argv []string
}

// Refusal says why a request was not executed. It is recorded like a
// measurement, so the model's attempt is visible afterwards.
type Refusal struct {
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// Resolve checks a request against the catalogue. A request outside the
// declared shape is not executed; the returned Refusal says why in words
// the model can act on ("that shape does not exist").
func (c Catalog) Resolve(request Request) (Plan, *Refusal) {
	spec, ok := c.specs[request.Probe]
	if !ok {
		return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q is not in the catalogue", request.Probe)}
	}
	args := map[string]string{}
	for name, value := range request.Args {
		if spec.Kind == KindHTTP && name == "method" {
			allowed := false
			for _, candidate := range spec.Methods {
				if candidate == value {
					allowed = true
				}
			}
			if !allowed {
				return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q does not allow method %q", spec.ID, value)}
			}
			args[name] = value
			continue
		}
		re, declared := spec.compiled[name]
		if !declared {
			return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q has no slot %q", spec.ID, name)}
		}
		if reason := slotValueProblem(value, spec.Kind == KindSQL || spec.Kind == KindRepo); reason != "" {
			return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q slot %q: %s", spec.ID, name, reason)}
		}
		if !re.MatchString(value) {
			return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q slot %q: value does not match the declared shape", spec.ID, name)}
		}
		args[name] = value
	}
	for name := range spec.compiled {
		if _, given := args[name]; !given {
			if spec.Kind == KindHTTP && name == "method" {
				continue
			}
			if spec.Kind == KindRepo && name == "path" {
				args[name] = ""
				continue
			}
			return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q needs slot %q", spec.ID, name)}
		}
	}
	plan := Plan{Spec: spec, Args: args}
	if spec.Kind == KindExec {
		plan.Argv = make([]string, 0, len(spec.Argv))
		for _, word := range spec.Argv {
			plan.Argv = append(plan.Argv, slotPattern.ReplaceAllStringFunc(word, func(match string) string {
				name := slotPattern.FindStringSubmatch(match)[1]
				return args[name]
			}))
		}
	}
	if spec.Kind == KindHTTP {
		if plan.Args["method"] == "" {
			plan.Args["method"] = spec.Methods[0]
		}
		// The path is appended to the host: without a leading slash a
		// value could carry a port or another authority.
		if path := plan.Args["path"]; !strings.HasPrefix(path, "/") || strings.Contains(path, "//") || strings.ContainsAny(path, "@:\\") {
			return Plan{}, &Refusal{Reason: fmt.Sprintf("probe %q slot \"path\": must start with / and carry no authority", spec.ID)}
		}
	}
	return plan, nil
}

// slotValueProblem names what makes a value unusable regardless of the
// declared pattern: emptiness, length, whitespace, control characters and
// the statement separator. The pattern is checked afterwards. A statement
// or a search pattern needs plain spaces between words (allowSpaces); an
// argv slot does not, so there a space is refused like any whitespace.
func slotValueProblem(value string, allowSpaces bool) string {
	switch {
	case value == "":
		return "value is empty"
	case len(value) > MaxSlotValueLength:
		return "value is too long"
	case strings.ContainsRune(value, ';'):
		return "value contains a statement separator"
	case strings.HasPrefix(value, "-"):
		// A value starting with "-" would be read as a flag by the command
		// it is handed to, whatever the pattern allowed.
		return "value starts with a dash"
	}
	for _, r := range value {
		if unicode.IsSpace(r) && (r != ' ' || !allowSpaces) {
			return "value contains whitespace"
		}
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return "value contains a control character"
		}
	}
	return ""
}

// MaxOutput returns the probe's output cap.
func (s Spec) MaxOutput() int {
	if s.MaxOutputBytes > 0 {
		return s.MaxOutputBytes
	}
	return DefaultMaxOutputBytes
}

// Timeout returns the probe's wall-time cap in seconds.
func (s Spec) Timeout() int {
	if s.TimeoutSeconds > 0 {
		return s.TimeoutSeconds
	}
	return DefaultTimeoutSeconds
}

// ErrNoCatalog is returned when a session has no catalogue to resolve with.
var ErrNoCatalog = errors.New("probe catalogue is empty")
