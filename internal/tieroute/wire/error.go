package wire

import (
	"errors"
	"fmt"
)

// Error codes from the registry in the API conventions that a client
// branches on (§4.4).
const (
	CodeBadRequest         = "bad-request"
	CodeUnknownKind        = "unknown-kind"
	CodeBadContentType     = "bad-content-type"
	CodeBadSignature       = "bad-signature"
	CodeInvalidChain       = "invalid-chain"
	CodeClockSkew          = "clock-skew"
	CodeUnauthenticated    = "unauthenticated"
	CodeNotAllowed         = "not-allowed"
	CodeRegistrationClosed = "registration-closed"
	CodeNotFound           = "not-found"
	CodeHandleTaken        = "handle-taken"
	CodeStaleHead          = "stale-head"
	CodeStaleEpoch         = "stale-epoch"
	CodeWrongHome          = "wrong-home"
	CodeAwaitingKey        = "awaiting-key"
	CodeStaleVersion       = "stale-version"
	CodeBadInvite          = "bad-invite"
	CodeAlreadyExists      = "already-exists"
	CodeExpired            = "expired"
	CodeRemoved            = "removed"
	CodeMigrated           = "migrated"
	CodeTooLarge           = "too-large"
	CodeOverQuota          = "over-quota"
	CodeRateLimited        = "rate-limited"
	CodeUnavailable        = "unavailable"
	CodeNotImplemented     = "not-implemented"
)

// Error is the body of every non-2xx response (§4.4), with its HTTP status.
// Clients branch on Code, never on Msg.
type Error struct {
	Status int      `json:"-"`
	Code   string   `json:"error"`
	Msg    string   `json:"msg,omitempty"`
	Retry  int64    `json:"retry,omitempty"`
	IDs    []string `json:"ids,omitempty"`
	Now    int64    `json:"now,omitempty"`
	Group  string   `json:"group,omitempty"`
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("tieroute: %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("tieroute: %d %s", e.Status, e.Code)
}

// IsCode reports whether err is a Tieroute error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// AsError returns err as a Tieroute error, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
