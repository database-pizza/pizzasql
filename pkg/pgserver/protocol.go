package pgserver

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Maximum message sizes enforced before any allocation, to bound memory use
// against malformed or hostile clients. Regular protocol messages (queries,
// bind parameters, etc.) are capped at 16 MiB; startup messages are much
// smaller and capped at 1 MiB.
const (
	MaxMessageSize        = 16 * 1024 * 1024 // 16 MiB
	MaxStartupMessageSize = 1 * 1024 * 1024  // 1 MiB
)

// Message type constants (first byte of message)
const (
	// Frontend (client) messages
	MsgStartup   = 0   // Startup message (no type byte)
	MsgQuery     = 'Q' // Simple query
	MsgTerminate = 'X' // Terminate
	MsgPassword  = 'p' // Password message
	MsgParse     = 'P' // Parse (prepared statement)
	MsgBind      = 'B' // Bind
	MsgDescribe  = 'D' // Describe
	MsgExecute   = 'E' // Execute
	MsgSync      = 'S' // Sync
	MsgFlush     = 'H' // Flush
	MsgClose     = 'C' // Close

	// Backend (server) messages
	MsgAuthenticationOk     = 'R' // Authentication request
	MsgBackendKeyData       = 'K' // Backend key data
	MsgBindComplete         = '2' // Bind complete
	MsgCloseComplete        = '3' // Close complete
	MsgCommandComplete      = 'C' // Command complete
	MsgDataRow              = 'D' // Data row
	MsgEmptyQueryResponse   = 'I' // Empty query response
	MsgErrorResponse        = 'E' // Error response
	MsgNoData               = 'n' // No data
	MsgNoticeResponse       = 'N' // Notice response
	MsgParameterDescription = 't' // Parameter description
	MsgParameterStatus      = 'S' // Parameter status
	MsgParseComplete        = '1' // Parse complete
	MsgReadyForQuery        = 'Z' // Ready for query
	MsgRowDescription       = 'T' // Row description
	MsgNotificationResponse = 'A' // Notification response
)

// Transaction status
const (
	TxStatusIdle    = 'I' // Idle (not in transaction)
	TxStatusInBlock = 'T' // In transaction block
	TxStatusFailed  = 'E' // In failed transaction block
)

// Error field types
const (
	ErrorFieldSeverity         = 'S'
	ErrorFieldCode             = 'C'
	ErrorFieldMessage          = 'M'
	ErrorFieldDetail           = 'D'
	ErrorFieldHint             = 'H'
	ErrorFieldPosition         = 'P'
	ErrorFieldInternalPosition = 'p'
	ErrorFieldInternalQuery    = 'q'
	ErrorFieldWhere            = 'W'
	ErrorFieldSchemaName       = 's'
	ErrorFieldTableName        = 't'
	ErrorFieldColumnName       = 'c'
	ErrorFieldDataTypeName     = 'd'
	ErrorFieldConstraintName   = 'n'
	ErrorFieldFile             = 'F'
	ErrorFieldLine             = 'L'
	ErrorFieldRoutine          = 'R'
)

// PostgreSQL error codes (subset)
const (
	ErrCodeSuccess              = "00000"
	ErrCodeSyntaxError          = "42601"
	ErrCodeUndefinedTable       = "42P01"
	ErrCodeUndefinedColumn      = "42703"
	ErrCodeDuplicateTable       = "42P07"
	ErrCodeDuplicateColumn      = "42701"
	ErrCodeInvalidParameter     = "22023"
	ErrCodeInternalError        = "XX000"
	ErrCodeConnectionFailure    = "08006"
	ErrCodeProtocolViolation    = "08P01"
	ErrCodeFeatureNotSupported  = "0A000"
	ErrCodeTransactionAborted   = "25P02"
	ErrCodeSerializationFailure = "40001"
)

// Message represents a PostgreSQL protocol message
type Message struct {
	Type byte
	Data []byte
}

// WriteMessage writes a message to the writer
func WriteMessage(w io.Writer, msgType byte, data []byte) error {
	// Write message type
	if _, err := w.Write([]byte{msgType}); err != nil {
		return err
	}

	// Write message length (includes itself, 4 bytes)
	length := uint32(len(data) + 4)
	if err := binary.Write(w, binary.BigEndian, length); err != nil {
		return err
	}

	// Write message data
	if _, err := w.Write(data); err != nil {
		return err
	}

	return nil
}

// ReadMessage reads a message from the reader
func ReadMessage(r io.Reader) (*Message, error) {
	// Read message type
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(r, typeBuf); err != nil {
		return nil, err
	}

	// Read message length
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}

	if length < 4 {
		return nil, fmt.Errorf("invalid message length: %d", length)
	}
	if length > MaxMessageSize {
		return nil, fmt.Errorf("message length %d exceeds maximum %d", length, MaxMessageSize)
	}

	// Read message data
	data := make([]byte, length-4)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	return &Message{
		Type: typeBuf[0],
		Data: data,
	}, nil
}

// ReadStartupMessage reads the initial startup message (no type byte)
func ReadStartupMessage(r io.Reader) (map[string]string, error) {
	// Read message length
	var length uint32
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return nil, err
	}

	if length < 8 {
		return nil, fmt.Errorf("invalid startup message length: %d", length)
	}
	if length > MaxStartupMessageSize {
		return nil, fmt.Errorf("startup message length %d exceeds maximum %d", length, MaxStartupMessageSize)
	}

	// Read protocol version
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil {
		return nil, err
	}

	// Read parameters
	data := make([]byte, length-8)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	params := make(map[string]string)
	params["protocol_version"] = fmt.Sprintf("%d", version)

	// Parse null-terminated key-value pairs
	i := 0
	for i < len(data) {
		if data[i] == 0 {
			break
		}

		// Read key
		keyStart := i
		for i < len(data) && data[i] != 0 {
			i++
		}
		if i >= len(data) {
			break
		}
		key := string(data[keyStart:i])
		i++ // skip null

		// Read value
		valueStart := i
		for i < len(data) && data[i] != 0 {
			i++
		}
		if i > len(data) {
			break
		}
		value := string(data[valueStart:i])
		i++ // skip null

		params[key] = value
	}

	return params, nil
}

// MessageBuilder helps build protocol messages
type MessageBuilder struct {
	data []byte
}

// NewMessageBuilder creates a new message builder
func NewMessageBuilder() *MessageBuilder {
	return &MessageBuilder{
		data: make([]byte, 0, 1024),
	}
}

// AppendByte appends a single byte.
func (mb *MessageBuilder) AppendByte(b byte) {
	mb.data = append(mb.data, b)
}

// WriteInt16 writes a 16-bit integer
func (mb *MessageBuilder) WriteInt16(n int16) {
	mb.data = append(mb.data, byte(n>>8), byte(n))
}

// WriteInt32 writes a 32-bit integer
func (mb *MessageBuilder) WriteInt32(n int32) {
	mb.data = append(mb.data, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// WriteString writes a null-terminated string
func (mb *MessageBuilder) WriteString(s string) {
	mb.data = append(mb.data, []byte(s)...)
	mb.data = append(mb.data, 0)
}

// WriteBytes writes raw bytes
func (mb *MessageBuilder) WriteBytes(b []byte) {
	mb.data = append(mb.data, b...)
}

// Bytes returns the built message data
func (mb *MessageBuilder) Bytes() []byte {
	return mb.data
}

// Reset resets the builder for reuse
func (mb *MessageBuilder) Reset() {
	mb.data = mb.data[:0]
}
