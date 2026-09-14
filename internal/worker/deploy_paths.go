package worker

import (
	"errors"
	"regexp"
	"strings"
)

// compileDeployPath turns one deploy_paths entry into the matcher it means,
// with the reading a workflow's own paths filter gives it: an entry without
// glob characters is a prefix — "docs" or "docs/" covers everything under
// docs — "*" and "?" stay inside one path segment, "**" spans segments
// wherever it appears ("docs/**", "**/*.go", "**.js"), "[...]" is a class,
// and a trailing "/" means "anything under such a directory". Two readings
// differ from GitHub's on purpose and are documented on the field: "?" is
// exactly one character (not an optional one) and "+" is literal.
//
// An entry that could not mean what it says — absolute, "!"-negated,
// carrying a backslash, an empty, "." or ".." segment, padding, or an
// unparsable class — is refused, because accepted it would look declared
// while covering nothing, and the runner would end deliveries the
// deployment reacts to.
func compileDeployPath(pattern string) (*regexp.Regexp, error) {
	invalid := errors.New("consumer workflow deploy_paths entry is invalid")
	if pattern == "" || strings.TrimSpace(pattern) != pattern || strings.HasPrefix(pattern, "/") ||
		strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "\\") {
		return nil, invalid
	}
	directory := strings.HasSuffix(pattern, "/")
	body := strings.TrimSuffix(pattern, "/")
	for _, segment := range strings.Split(body, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, invalid
		}
	}
	var expression strings.Builder
	expression.WriteString("^")
	if !strings.ContainsAny(body, "*?[") {
		expression.WriteString(regexp.QuoteMeta(body))
		if directory {
			expression.WriteString("/.+")
		} else {
			expression.WriteString("(?:/.*)?")
		}
		expression.WriteString("$")
		return regexp.Compile(expression.String())
	}
	for i := 0; i < len(body); {
		rest := body[i:]
		switch {
		case strings.HasPrefix(rest, "**/") && (i == 0 || body[i-1] == '/'):
			expression.WriteString("(?:.*/)?")
			i += 3
		case rest == "/**":
			expression.WriteString("(?:/.*)?")
			i += 3
		case strings.HasPrefix(rest, "**"):
			expression.WriteString(".*")
			i += 2
		case rest[0] == '*':
			expression.WriteString("[^/]*")
			i++
		case rest[0] == '?':
			expression.WriteString("[^/]")
			i++
		case rest[0] == '[':
			end := strings.IndexByte(rest, ']')
			if end < 1 {
				return nil, errors.New("consumer workflow deploy_paths pattern is invalid")
			}
			class := rest[:end+1]
			if strings.HasPrefix(class, "[!") {
				class = "[^" + class[2:]
			}
			if strings.ContainsAny(class[1:end], "/[") {
				return nil, errors.New("consumer workflow deploy_paths pattern is invalid")
			}
			expression.WriteString(class)
			i += end + 1
		default:
			expression.WriteString(regexp.QuoteMeta(rest[:1]))
			i++
		}
	}
	if directory {
		expression.WriteString("/.+")
	}
	expression.WriteString("$")
	compiled, err := regexp.Compile(expression.String())
	if err != nil {
		return nil, errors.New("consumer workflow deploy_paths pattern is invalid")
	}
	return compiled, nil
}

// DeployPathCovered reports whether one delivered path falls inside a
// declared scope. Entries the validator would refuse cover nothing.
func DeployPathCovered(patterns []string, filePath string) bool {
	for _, pattern := range patterns {
		compiled, err := compileDeployPath(pattern)
		if err != nil {
			continue
		}
		if compiled.MatchString(filePath) {
			return true
		}
	}
	return false
}
