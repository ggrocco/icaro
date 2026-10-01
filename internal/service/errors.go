package service

import (
	"errors"
	"fmt"

	"github.com/ggrocco/icaro/internal/workflow"
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
