package mls

import "fmt"

// ErrorCode says what kind of error the library returned. The numbers
// match the TRMLS_ERR_ codes in mls/include/tieroute_mls.h.
type ErrorCode int

const (
	// ErrBadInput means the request was malformed.
	ErrBadInput ErrorCode = 1
	// ErrNotFound means there's no such group, pending commit, staged
	// commit, or proposal.
	ErrNotFound ErrorCode = 2
	// ErrInvalid means OpenMLS rejected a message, key package, Welcome, or
	// GroupInfo.
	ErrInvalid ErrorCode = 3
	// ErrWrongEpoch means the message is for an epoch other than the
	// group's current one. A message for a later epoch waits until the
	// group gets there (§11.6).
	ErrWrongEpoch ErrorCode = 4
	// ErrPendingCommit means the group has a pending or staged commit to
	// merge or clear first.
	ErrPendingCommit ErrorCode = 5
	// ErrPendingProposal means the group has pending proposals, so it can't
	// send an application message until a commit covers them.
	ErrPendingProposal ErrorCode = 6
	// ErrInactive means this device is no longer a member of the group.
	ErrInactive ErrorCode = 7
	// ErrPastEpoch means the message is from a past epoch whose secrets are
	// gone.
	ErrPastEpoch ErrorCode = 8
	// ErrStorage means a snapshot is malformed.
	ErrStorage ErrorCode = 9
	// ErrInternal means a bug in tieroute-mls or OpenMLS.
	ErrInternal ErrorCode = 10
)

var codeNames = map[ErrorCode]string{
	ErrBadInput:        "bad input",
	ErrNotFound:        "not found",
	ErrInvalid:         "invalid",
	ErrWrongEpoch:      "wrong epoch",
	ErrPendingCommit:   "pending commit",
	ErrPendingProposal: "pending proposal",
	ErrInactive:        "inactive",
	ErrPastEpoch:       "past epoch",
	ErrStorage:         "storage",
	ErrInternal:        "internal",
}

func (c ErrorCode) String() string {
	if s, ok := codeNames[c]; ok {
		return s
	}
	return fmt.Sprintf("error %d", int(c))
}

// Error is an error from the library: a code and a message.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string {
	return "mls: " + e.Message
}

// Is reports whether target is an *Error with the same code, so
// errors.Is(err, &mls.Error{Code: mls.ErrWrongEpoch}) works, as does
// errors.Is(err, mls.ErrWrongEpoch).
func (e *Error) Is(target error) bool {
	switch t := target.(type) {
	case *Error:
		return t.Code == e.Code
	case ErrorCode:
		return t == e.Code
	}
	return false
}

// Error lets a bare code stand as a target for errors.Is.
func (c ErrorCode) Error() string {
	return "mls: " + c.String()
}
