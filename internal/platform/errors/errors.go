// Package errors defines the unified application error model.
// See docs/specs/00-platform.md §2.2.
package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier returned to clients.
type Code string

const (
	OK                   Code = "OK"
	Unauthenticated      Code = "UNAUTHENTICATED"
	Forbidden            Code = "FORBIDDEN"
	ResourceUnavailable  Code = "RESOURCE_UNAVAILABLE"
	InvalidArgument      Code = "INVALID_ARGUMENT"
	StateConflict        Code = "STATE_CONFLICT"
	QuotaExceeded        Code = "QUOTA_EXCEEDED"
	SyncCursorExpired    Code = "SYNC_CURSOR_EXPIRED"
	InvalidCredentials   Code = "INVALID_CREDENTIALS"
	AlreadyFriend        Code = "ALREADY_FRIEND"
	ContentUnavailable   Code = "CONTENT_UNAVAILABLE"
	RateLimited          Code = "RATE_LIMITED"
	MustChangePassword   Code = "MUST_CHANGE_PASSWORD"
	InternalError        Code = "INTERNAL_ERROR"
)

// AppError is the single error type crossing module boundaries.
type AppError struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *AppError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.cause }

// New builds an AppError without an internal cause.
func New(code Code, message string) *AppError {
	return &AppError{Code: code, Message: message}
}

// Wrap builds an AppError carrying an internal cause (never serialized).
func Wrap(code Code, message string, cause error) *AppError {
	return &AppError{Code: code, Message: message, cause: cause}
}

// AsApp extracts an AppError, mapping unknown errors to INTERNAL_ERROR (R6).
func AsApp(err error) *AppError {
	var ae *AppError
	if stderrors.As(err, &ae) {
		return ae
	}
	return Wrap(InternalError, "internal error", err)
}

// HTTPStatus maps a code to its fixed HTTP status (R5).
func HTTPStatus(code Code) int {
	switch code {
	case OK:
		return http.StatusOK
	case Unauthenticated, InvalidCredentials:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case ResourceUnavailable:
		return http.StatusNotFound
	case InvalidArgument:
		return http.StatusBadRequest
	case StateConflict, AlreadyFriend, SyncCursorExpired, MustChangePassword:
		return http.StatusConflict
	case QuotaExceeded:
		return http.StatusRequestEntityTooLarge
	case ContentUnavailable:
		return http.StatusGone
	case RateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// Convenience constructors used across modules.
func Unauth(msg string) *AppError  { return New(Unauthenticated, msg) }
func Invalid(msg string) *AppError { return New(InvalidArgument, msg) }
func Unavail(msg string) *AppError { return New(ResourceUnavailable, msg) }
func Conflict(msg string) *AppError {
	return New(StateConflict, msg)
}
