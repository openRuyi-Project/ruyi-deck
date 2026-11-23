// Package apperror distinguishes invalid input from failures during execution.
package apperror

import (
	"errors"
	"fmt"
)

// InputError describes an invalid command argument or configuration.
type InputError struct {
	Err error
}

// Error returns the underlying error message.
func (e *InputError) Error() string { return e.Err.Error() }

// Unwrap lets callers inspect the underlying error.
func (e *InputError) Unwrap() error { return e.Err }

// Input marks an error as an input error, which has exit code 2.
func Input(err error) error { return &InputError{Err: err} }

// Invalid formats an input error.
func Invalid(format string, args ...any) error {
	return Input(fmt.Errorf(format, args...))
}

// ExitCode returns the documented exit code for an error.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var input *InputError
	if errors.As(err, &input) {
		return 2
	}
	return 1
}
