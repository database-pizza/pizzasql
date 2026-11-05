// Package pgerror holds SQLSTATE metadata and sentinel errors shared by the
// parser, executor, and protocol servers.
package pgerror

import (
	"errors"
	"fmt"
)

// SQLSTATE codes used by PizzaSQL's protocol and HTTP surfaces.
const (
	CodeFeatureNotSupported  = "0A000"
	CodeInvalidText          = "22P02"
	CodeUniqueViolation      = "23505"
	CodeForeignKeyViolation  = "23503"
	CodeCheckViolation       = "23514"
	CodeSerializationFailure = "40001"
	CodeSyntaxError          = "42601"
	CodeInternalError        = "XX000"
)

// ErrFeatureNotSupported marks SQL that PizzaSQL intentionally rejects instead
// of accepting with different semantics. Protocol layers map it to SQLSTATE
// 0A000 (feature_not_supported) rather than a generic syntax or internal error.
var ErrFeatureNotSupported = errors.New("feature not supported")

// Unsupported returns an error that wraps ErrFeatureNotSupported with a
// client-facing explanation and migration hint.
func Unsupported(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrFeatureNotSupported, fmt.Sprintf(format, args...))
}

// ErrInvalidText marks input that is valid SQL but not valid text for the
// target type, such as a malformed array literal. Protocol layers map it to
// SQLSTATE 22P02 (invalid_text_representation).
var ErrInvalidText = errors.New("invalid text representation")

// InvalidText returns an error that wraps ErrInvalidText with a client-facing
// explanation of the rejected input.
func InvalidText(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidText, fmt.Sprintf(format, args...))
}
