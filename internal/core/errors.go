package core

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// RateLimited returns a rate_limited error saying when to retry.
func RateLimited(what string, wait time.Duration) *Error {
	secs := int(math.Ceil(wait.Seconds()))
	return &Error{
		Code: CodeRateLimited, RetryAfter: time.Duration(secs) * time.Second,
		Message: fmt.Sprintf("too many %s; retry in %ds", what, secs),
	}
}

// Code identifies a class of failure. Codes are part of the public API: the
// HTTP and MCP layers report them verbatim.
type Code string

// Error codes.
const (
	CodeInvalid               Code = "invalid"
	CodeForbidden             Code = "forbidden"
	CodeNamespaceNotFound     Code = "namespace_not_found"
	CodeNamespaceExists       Code = "namespace_exists"
	CodeConversationNotFound  Code = "conversation_not_found"
	CodeConversationExists    Code = "conversation_exists"
	CodeConversationConcluded Code = "conversation_concluded"
	CodeNotParticipant        Code = "not_participant"
	CodeNameActive            Code = "name_active"
	CodeStaleContext          Code = "stale_context"
	CodeMessageTooLarge       Code = "message_too_large"
	CodeNotCoordinator        Code = "not_coordinator"
	CodeTooManyWaiters        Code = "too_many_waiters"
	CodeTooManyWatchers       Code = "too_many_watchers"
	CodeUnauthorized          Code = "unauthorized"
	CodeAccountNotFound       Code = "account_not_found"
	CodeAccountExists         Code = "account_exists"
	CodeTokenNotFound         Code = "token_not_found"
	CodeFeedbackNotFound      Code = "feedback_not_found"
	CodeLastAdmin             Code = "last_admin"
	CodeRateLimited           Code = "rate_limited"
	// CodeBusy is a write that could not get the database (held by another
	// process) in time; retrying shortly is expected to succeed.
	CodeBusy Code = "busy"
	// CodeIdempotencyConflict is a post that reuses an idempotency key
	// for a different message.
	CodeIdempotencyConflict Code = "idempotency_conflict"
	// CodeOwnCredential is a revocation, made without force, that would
	// revoke the token the request is using.
	CodeOwnCredential Code = "own_credential"
)

// Error is a failure the caller can act on. Message is short, ASCII, and
// says what to do next where there is something to do.
type Error struct {
	Code    Code
	Message string
	// LastSeq is set for CodeStaleContext: the conversation's current last seq.
	LastSeq int64
	// RetryAfter is set for CodeRateLimited: how long until a retry can
	// succeed.
	RetryAfter time.Duration
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns the Code of err, or "" if err is not a core error.
func CodeOf(err error) Code {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
