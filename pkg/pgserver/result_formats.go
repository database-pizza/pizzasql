package pgserver

import (
	"errors"
	"fmt"
)

// resultFormatError is a Bind result-format rejection with the SQLSTATE the
// client must receive.
type resultFormatError struct {
	code    string
	message string
}

func (e *resultFormatError) Error() string { return e.message }

// resolveResultFormats expands a portal's Bind result format list to one format
// code per result column and rejects binary requests the wire layer cannot
// encode.
//
// The PostgreSQL Bind rules are: an empty list means text for every column, a
// single entry applies to every column, and any other list length must equal the
// number of result columns. The result that is resolved here is the one this
// server emits at Execute; column metadata only exists then because Describe
// returns NoData. Formats are stored per portal, so two portals bound to the
// same statement can use different formats.
//
// An all-text result returns nil, which keeps the text row path allocation free.
func (c *Connection) resolveResultFormats(bindFormats []int16, columnTypes []string, columnCount int) ([]int16, error) {
	if len(bindFormats) == 0 {
		return nil, nil
	}
	if len(bindFormats) > 1 && len(bindFormats) != columnCount {
		return nil, &resultFormatError{
			code:    ErrCodeProtocolViolation,
			message: fmt.Sprintf("bind message has %d result formats but the query returns %d columns", len(bindFormats), columnCount),
		}
	}
	formats := make([]int16, columnCount)
	if len(bindFormats) == 1 {
		for i := range formats {
			formats[i] = bindFormats[0]
		}
	} else {
		copy(formats, bindFormats)
	}
	allText := true
	for i, format := range formats {
		switch format {
		case 0:
			continue
		case 1:
			allText = false
		default:
			return nil, &resultFormatError{
				code:    ErrCodeProtocolViolation,
				message: fmt.Sprintf("invalid Bind result format code %d", format),
			}
		}
		oid := c.getOIDForType(typeNameAt(columnTypes, i))
		if !canEncodeBinaryResult(oid) {
			return nil, &resultFormatError{
				code:    ErrCodeFeatureNotSupported,
				message: fmt.Sprintf("binary result format is not supported for column %d (%s, OID %d)", i+1, typeNameAt(columnTypes, i), oid),
			}
		}
	}
	if allText {
		return nil, nil
	}
	return formats, nil
}

// failResultFormat reports a result-format error through failExtended so the
// connection sends an ErrorResponse, discards the rest of the cycle until Sync,
// and marks an explicit transaction block as failed.
func (c *Connection) failResultFormat(err error) error {
	code := ErrCodeProtocolViolation
	var formatErr *resultFormatError
	if errors.As(err, &formatErr) {
		code = formatErr.code
	}
	return c.failExtended(code, err)
}

// typeNameAt returns the declared type name of a result column, or "" when the
// result carries no metadata for it (the wire layer reports "" as TEXT).
func typeNameAt(columnTypes []string, index int) string {
	if index < 0 || index >= len(columnTypes) {
		return ""
	}
	return columnTypes[index]
}
