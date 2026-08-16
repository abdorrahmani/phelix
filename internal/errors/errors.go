package phelixerr

import (
	"errors"
	"fmt"
)

// Error is Phelix's structured error. It pairs a stable [Code] with a concise
// Message and preserves an optional underlying cause in Err so the full chain
// stays inspectable via errors.Is/As and Unwrap.
type Error struct {
	Code    Code
	Message string
	Err     error
}

// Error returns the message. The message is intentionally concise; the cause
// is available separately via Unwrap and the CLI renderer.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Unwrap returns the underlying cause, if any, so errors.Is/As can traverse
// the chain.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// New builds a structured error with the given code and message and no cause.
func New(code Code, message string) error {
	return &Error{Code: code, Message: message}
}

// Newf is New with fmt.Sprintf-style formatting for the message.
func Newf(code Code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds a structured error carrying code and a human message, wrapping
// the original cause err. The cause remains reachable via Unwrap.
func Wrap(code Code, message string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: message, Err: err}
}

// Wrapf is Wrap with fmt.Sprintf-style formatting for the message.
func Wrapf(code Code, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

// AsError extracts the inner *Error from err, if any, using errors.As.
// Returns nil when err is not (or does not wrap) a *Error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// CodeOf returns the first structured code found by walking the error chain,
// or CodeUnknown when err is nil or carries no structured code.
func CodeOf(err error) Code {
	if err == nil {
		return CodeUnknown
	}
	if e := AsError(err); e != nil {
		return e.Code
	}
	return CodeUnknown
}

// IsCode reports whether err (or any wrapped error) is tagged with code.
func IsCode(err error, code Code) bool {
	return CodeOf(err) == code
}

// Cause unwraps to the deepest underlying error in the chain. This is the
// "root cause" shown in --debug output. It returns err itself when err does
// not wrap anything.
func Cause(err error) error {
	for err != nil {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		inner := u.Unwrap()
		if inner == nil {
			break
		}
		err = inner
	}
	return err
}
