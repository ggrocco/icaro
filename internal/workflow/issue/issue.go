// Package issue is the validation issue type the API reports, kept apart
// from package workflow so API clients can decode it without linking the
// validator.
package issue

import (
	"fmt"
	"strings"
)

// Issue is one validation problem at a path in the document.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (i Issue) String() string {
	if i.Hint != "" {
		return fmt.Sprintf("%s: %s (hint: %s)", i.Path, i.Message, i.Hint)
	}
	return fmt.Sprintf("%s: %s", i.Path, i.Message)
}

// Issues is a list of issues; it is an error.
type Issues []Issue

func (is Issues) Error() string {
	parts := make([]string, len(is))
	for i, it := range is {
		parts[i] = it.String()
	}
	return strings.Join(parts, "; ")
}
