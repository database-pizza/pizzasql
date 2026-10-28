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
	CodeUniqueViolation      = "23505"
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
