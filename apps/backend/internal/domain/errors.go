package domain

import (
	"errors"
	"time"
)

// AppError classifies operational failures with an optional retry directive
// without coupling domain/agent packages to any workflow engine.
type AppError struct {
	Code           string
	Message        string
	Cause          error
	NonRetryable   bool
	NextRetryDelay time.Duration
}

func (e *AppError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *AppError) Unwrap() error { return e.Cause }
func (e *AppError) Type() string  { return e.Code }

func NewNonRetryableError(message, code string, cause error) error {
	return &AppError{Code: code, Message: message, Cause: cause, NonRetryable: true}
}

func NewRetryableError(message, code string, nextDelay time.Duration, cause error) error {
	return &AppError{Code: code, Message: message, Cause: cause, NextRetryDelay: nextDelay}
}

func IsNonRetryable(err error) bool {
	var app *AppError
	return errors.As(err, &app) && app.NonRetryable
}

func RetryDelay(err error) (time.Duration, bool) {
	var app *AppError
	if errors.As(err, &app) && app.NextRetryDelay > 0 {
		return app.NextRetryDelay, true
	}
	return 0, false
}

func ErrorCode(err error) string {
	var app *AppError
	if errors.As(err, &app) && app.Code != "" {
		return app.Code
	}
	return ""
}
