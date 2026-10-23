package service

import (
	"errors"
	"fmt"

	"icaro/internal/workflow"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// ValidationError carries structured issues.
type ValidationError struct {
	Issues workflow.Issues
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed: %s", e.Issues.Error())
}

// BadRequest is a client error with a message.
type BadRequest struct {
	Msg string
}

func (e *BadRequest) Error() string { return e.Msg }

func badRequest(format string, args ...any) error {
	return &BadRequest{Msg: fmt.Sprintf(format, args...)}
}

// Conflict is a 409 with a message (e.g. a concurrency policy refusal).
type Conflict struct {
	Msg string
}

func (e *Conflict) Error() string { return e.Msg }

func (e *Conflict) Is(target error) bool { return target == ErrConflict }

// Unavailable is a 503: a dependency the request needs (Docker) is not
// reachable. Msg says which, and how to fix it.
type Unavailable struct {
	Msg string
}

func (e *Unavailable) Error() string { return e.Msg }

// Upstream is a 502: a dependency (docker compose) refused the request.
// Msg is the dependency's own error; it is about the user's setup, not
// the daemon's internals, so it is passed on.
type Upstream struct {
	Msg string
}

func (e *Upstream) Error() string { return e.Msg }
