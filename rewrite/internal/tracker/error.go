package tracker

import "fmt"

// trackerError is an answer with a status other than the one expected, kept
// whole so an adapter can tell one refusal from another.
type trackerError struct {
	Status int
	Body   string
}

func (e *trackerError) Error() string {
	// Both adapters redact before storing the body. Keep it whole for refusal
	// handling, but do not put the entire response into logs or notices.
	return fmt.Sprintf("tracker returned HTTP %d: %s", e.Status, clip(e.Body, 200))
}

// clip is text cut to its first limit characters, marked where it was cut.
func clip(text string, limit int) string {
	count := 0
	for index := range text {
		if count == limit {
			return text[:index] + "…"
		}
		count++
	}
	return text
}
