// Package pgerror holds SQLSTATE metadata and sentinel errors shared by the
// parser, executor, and protocol servers.
package pgerror

import (
	"errors"
	"fmt"
)

// SQLSTATE codes used by PizzaSQL's protocol and HTTP surfaces.
const (
	CodeFeatureNotSupported    = "0A000"
	CodeInvalidText            = "22P02"
	CodeInvalidParameter       = "22023"
	CodeDatatypeMismatch       = "42804"
	CodeUniqueViolation        = "23505"
	CodeForeignKeyViolation    = "23503"
	CodeCheckViolation         = "23514"
	CodeSerializationFailure   = "40001"
	CodeSyntaxError            = "42601"
	CodeUndefinedTable         = "42P01"
	CodeInvalidColumnReference = "42P10"
	CodeInternalError          = "XX000"
)

// ErrUndefinedTable marks a reference to a relation or type name that does not
// exist. Protocol layers map it to SQLSTATE 42P01 (undefined_table) instead of a
// generic internal error, which is what a regclass lookup typo must report.
var ErrUndefinedTable = errors.New("undefined table")

// UndefinedTable returns an error that wraps ErrUndefinedTable with a
// client-facing explanation of the name that did not resolve.
func UndefinedTable(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrUndefinedTable, fmt.Sprintf(format, args...))
}

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

// ErrInvalidParameter marks a well-formed value that is not valid for the
// operation it is used in, such as jsonb_array_length() on a non-array or a
// jsonb path element that is NULL. Protocol layers map it to SQLSTATE 22023
// (invalid_parameter_value).
var ErrInvalidParameter = errors.New("invalid parameter value")

// InvalidParameter returns an error that wraps ErrInvalidParameter with a
// client-facing explanation of the rejected value.
func InvalidParameter(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidParameter, fmt.Sprintf(format, args...))
}

// ErrInvalidColumnReference marks a reference whose shape does not match the
// relation it names, such as a table-function column alias list with the wrong
// arity. Protocol layers map it to SQLSTATE 42P10 (invalid_column_reference).
var ErrInvalidColumnReference = errors.New("invalid column reference")

// InvalidColumnReference returns an error that wraps
// ErrInvalidColumnReference with a client-facing explanation.
func InvalidColumnReference(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidColumnReference, fmt.Sprintf(format, args...))
}

// ErrDatatypeMismatch marks a value whose type is wrong for the operation it is
// used in, such as a non-integer array subscript. Protocol layers map it to
// SQLSTATE 42804 (datatype_mismatch).
var ErrDatatypeMismatch = errors.New("datatype mismatch")

// DatatypeMismatch returns an error that wraps ErrDatatypeMismatch with a
// client-facing explanation of the incompatible value.
func DatatypeMismatch(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrDatatypeMismatch, fmt.Sprintf(format, args...))
}
