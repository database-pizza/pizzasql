package pgserver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

// Connection represents a client connection
type Connection struct {
	conn           net.Conn
	reader         *bufio.Reader
	writer         *bufio.Writer
	executor       *executor.Executor
	schema         *storage.SchemaManager
	table          *storage.TableManager
	dbManager      *storage.DatabaseManager
	database       string
	params         map[string]string
	txStatus       byte
	quiet          bool // Disable query logging
	statements     map[string]*preparedStatement
	portals        map[string]*portal
	extendedFailed bool
	backendPID     int32
	// extendedCycle is true between Parse/Bind/Execute/Describe/Close and the
	// next Sync. Notifications are not interleaved into an extended-protocol
	// sequence; they are held until Sync.
	extendedCycle bool
	// catalogFallback serves catalog emulation before the executor exists, when
	// a connection has managers but has not completed startup.
	catalogFallback *storage.Session
}

// backendPIDSeq gives each connection a distinct synthetic backend PID. It is
// seeded with the process ID so NotificationResponse frames look plausible and
// do not collide across server processes.
var backendPIDSeq atomic.Int32

func nextBackendPID() int32 {
	return int32(os.Getpid()) + backendPIDSeq.Add(1)
}

type preparedStatement struct {
	query string
	// paramOIDs is the negotiated parameter type list: the client's non-zero
	// Parse OIDs plus inferred types for the zeros. Describe reports it and
	// Bind decodes with it.
	paramOIDs []int32
}

// portal is one Bind result. Its lifetime follows PostgreSQL: a portal is
// reusable until Close('P'), a Bind of the same name, or the end of the
// transaction it was created in (see Connection.clearPortals).
type portal struct {
	query string
	// resultFormats is the Bind result format list exactly as the client sent
	// it: empty (all text), one entry that repeats for every column, or one
	// entry per column. It is resolved once, when the first Execute produces the
	// result.
	resultFormats []int16
	// execution is nil until the first Execute. Materializing once lets a
	// resumed portal stream rows from execution.result instead of running the
	// statement again.
	execution *portalExecution
}

// portalExecution is the result a portal materialized on its first Execute,
// plus the stream cursor a later Execute continues from.
type portalExecution struct {
	result *executor.Result
	// columnTypes, oids, and formats are the result metadata resolved once for
	// every Execute of this portal.
	columnTypes []string
	oids        []int32
	formats     []int16
	// commandTag is the completion tag sent when the rows are exhausted.
	commandTag string
	// clearsPortals reports that this statement invalidated every open portal
	// (COMMIT, ROLLBACK, or ROLLBACK TO SAVEPOINT); see resetsPortals.
	clearsPortals bool
	rowOffset     int
	// described is true once the RowDescription was sent, so a resumed Execute
	// sends only DataRows.
	described bool
	// completed is true once CommandComplete was sent; a further Execute
	// repeats the tag without re-running the statement.
	completed bool
}

// NewConnection creates a new connection handler
func NewConnection(conn net.Conn, dbManager *storage.DatabaseManager, quiet bool) *Connection {
	return &Connection{
		conn:       conn,
		reader:     bufio.NewReader(conn),
		writer:     bufio.NewWriter(conn),
		dbManager:  dbManager,
		params:     make(map[string]string),
		statements: make(map[string]*preparedStatement),
		portals:    make(map[string]*portal),
		txStatus:   TxStatusIdle,
		quiet:      quiet,
		backendPID: nextBackendPID(),
	}
}

// Handle processes the connection
func (c *Connection) Handle() error {
	defer func() {
		if c.executor != nil {
			if err := c.executor.Close(); err != nil && !c.quiet {
				log.Printf("failed to close session: %v", err)
			}
		}
		c.conn.Close()
	}()

	// First, check for SSL request (sent before startup message)
	// SSL request is 8 bytes: length(4) + code(4) where code = 80877103
	firstBytes := make([]byte, 8)
	n, err := io.ReadFull(c.reader, firstBytes)
	if err != nil {
		return fmt.Errorf("failed to read initial bytes: %w", err)
	}

	// Check if it's an SSL request (code 80877103 = 0x04D2162F)
	if n == 8 {
		length := binary.BigEndian.Uint32(firstBytes[0:4])
		code := binary.BigEndian.Uint32(firstBytes[4:8])

		if length == 8 && code == 80877103 {
			// SSL request - we don't support SSL, send 'N'
			if !c.quiet {
				log.Printf("Client requested SSL, sending rejection")
			}
			if _, err := c.conn.Write([]byte{'N'}); err != nil {
				return fmt.Errorf("failed to send SSL rejection: %w", err)
			}
			// Now read the actual startup message
		} else {
			// Not SSL request, this is part of startup message
			// We need to prepend these bytes back for ReadStartupMessage
			// Create a multi-reader that first reads our buffered bytes, then continues with the reader
			c.reader = bufio.NewReader(io.MultiReader(bytes.NewReader(firstBytes), c.reader))
		}
	}

	// Read startup message
	if !c.quiet {
		log.Printf("Reading startup message...")
	}
	params, err := ReadStartupMessage(c.reader)
	if err != nil {
		return fmt.Errorf("failed to read startup message: %w", err)
	}

	c.params = params
	if !c.quiet {
		log.Printf("Startup params: %+v", params)
	}

	// Get database name from params (default to "pizzasql")
	dbName := params["database"]
	if dbName == "" {
		dbName = "pizzasql"
	}
	c.database = dbName

	if !c.quiet {
		log.Printf("New connection: user=%s database=%s", params["user"], dbName)
	}

	// Initialize database
	if err := c.initDatabase(dbName); err != nil {
		c.sendError("FATAL", ErrCodeConnectionFailure, fmt.Sprintf("Failed to initialize database: %v", err))
		return err
	}

	// Send authentication OK (no auth for now)
	if err := c.sendAuthenticationOk(); err != nil {
		return err
	}

	// Send parameter status messages
	if err := c.sendParameterStatus("server_version", version.PostgreSQLCompatVersion+" (PizzaSQL)"); err != nil {
		return err
	}
	// Build identity is a separate, custom ParameterStatus so the PostgreSQL
	// compatibility string above stays stable for version-sniffing drivers.
	if err := c.sendParameterStatus("pizzasql.version", version.String()); err != nil {
		return err
	}
	if err := c.sendParameterStatus("pizzasql.commit", version.Commit); err != nil {
		return err
	}
	if err := c.sendParameterStatus("pizzasql.proxy_errors", "1"); err != nil {
		return err
	}
	if err := c.sendParameterStatus("server_encoding", "UTF8"); err != nil {
		return err
	}
	if err := c.sendParameterStatus("client_encoding", "UTF8"); err != nil {
		return err
	}
	if err := c.sendParameterStatus("DateStyle", "ISO, MDY"); err != nil {
		return err
	}
	if err := c.sendParameterStatus("TimeZone", "UTC"); err != nil {
		return err
	}

	// Send backend key data (for cancellation - we don't implement this yet)
	if err := c.sendBackendKeyData(c.backendPID, 67890); err != nil {
		return err
	}

	// Send ready for query
	if err := c.sendReadyForQuery(); err != nil {
		return err
	}

	// Read messages on a separate goroutine so an idle listener can receive
	// asynchronous NotificationResponse frames while blocked waiting for input.
	// Only this goroutine writes to c.writer, which keeps frame order intact.
	type readResult struct {
		msg *Message
		err error
	}
	inbox := make(chan readResult, 16)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			msg, err := ReadMessage(c.reader)
			select {
			case inbox <- readResult{msg: msg, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	notifications := c.executor.NotificationChannel()
	overflow := c.executor.NotificationOverflow()

	for {
		// Do not interleave notifications into an extended-protocol cycle. The
		// channel is disabled until Sync closes the cycle.
		var notifyCh <-chan storage.Notification
		if !c.extendedCycle {
			notifyCh = notifications
		}

		select {
		case res := <-inbox:
			if res.err != nil {
				if res.err == io.EOF {
					log.Printf("Connection closed by client")
					return nil
				}
				return fmt.Errorf("failed to read message: %w", res.err)
			}
			switch res.msg.Type {
			case MsgParse, MsgBind, MsgDescribe, MsgExecute, MsgClose:
				c.extendedCycle = true
			}
			if err := c.handleMessage(res.msg); err != nil {
				if err == io.EOF {
					log.Printf("Connection closed normally")
					return nil
				}
				log.Printf("Error handling message: %v", err)
				return err
			}

		case n := <-notifyCh:
			if err := c.sendNotification(n); err != nil {
				return err
			}
			if err := c.writer.Flush(); err != nil {
				return err
			}

		case <-overflow:
			return fmt.Errorf("notification queue overflow: slow consumer disconnected")
		}
	}
}

// initDatabase initializes the database connection
func (c *Connection) initDatabase(dbName string) error {
	db, err := c.dbManager.GetDatabase(dbName)
	if err != nil {
		return err
	}

	c.schema = db.Schema
	c.table = db.Table
	c.executor = executor.New(db.Schema, db.Table)
	c.executor.SetBackendPID(c.backendPID)
	c.executor.SetNotificationHub(db.Hub)
	c.executor.SyncCatalog()

	return nil
}

// handleMessage processes a client message
func (c *Connection) handleMessage(msg *Message) error {
	if c.extendedFailed && msg.Type != MsgSync && msg.Type != MsgFlush && msg.Type != MsgTerminate {
		return nil
	}
	switch msg.Type {
	case MsgProxyError:
		return c.handleProxyError(msg)
	case MsgQuery:
		return c.handleQuery(msg)

	case MsgTerminate:
		log.Printf("Client requested termination")
		return io.EOF

	case MsgParse:
		return c.handleParse(msg)
	case MsgBind:
		return c.handleBind(msg)
	case MsgDescribe:
		return c.handleDescribe(msg)
	case MsgExecute:
		return c.handleExecute(msg)
	case MsgClose:
		return c.handleClose(msg)
	case MsgFlush:
		return c.writer.Flush()
	case MsgSync:
		c.extendedFailed = false
		c.extendedCycle = false
		if err := c.sendReadyForQuery(); err != nil {
			return err
		}
		return c.drainNotifications()

	default:
		log.Printf("Unknown message type: %c (%d)", msg.Type, msg.Type)
		c.sendError("ERROR", ErrCodeProtocolViolation, fmt.Sprintf("Unknown message type: %c", msg.Type))
		return c.sendReadyForQuery()
	}
}

// handleQuery processes a simple query
func (c *Connection) handleQuery(msg *Message) error {
	// A Query message carries a single NUL-terminated query string.
	if len(msg.Data) == 0 || msg.Data[len(msg.Data)-1] != 0 {
		c.sendError("ERROR", ErrCodeProtocolViolation, "invalid Query message: missing null terminator")
		return c.sendReadyForQuery()
	}
	sql := string(msg.Data[:len(msg.Data)-1])

	if !c.quiet {
		log.Printf("Query: %s", sql)
	}

	// Handle empty query
	sqlTrimmed := strings.TrimSpace(sql)
	if sqlTrimmed == "" || sqlTrimmed == ";" {
		if err := c.sendEmptyQueryResponse(); err != nil {
			return err
		}
		return c.sendReadyForQuery()
	}

	// Handle special PostgreSQL system queries that drivers send
	singleStatement := isSingleStatementSQL(sql)

	// In a failed transaction only ROLLBACK (or ROLLBACK TO SAVEPOINT) is
	// accepted. Gate before honoring the special driver queries below so they
	// return 25P02 instead of a result. Multi-statement batches are gated
	// per statement in the execution loop below.
	if c.txStatus == TxStatusFailed && singleStatement && !isRollbackStatement(sql) {
		c.sendError("ERROR", ErrCodeTransactionAborted, "current transaction is aborted, commands ignored until end of transaction block")
		return c.sendReadyForQuery()
	}

	// Recognize only complete initialization-query token sequences.
	if singleStatement {
		switch kind := systemQueryKind(sql); kind {
		case "version":
			return c.handleVersionQuery()
		case "current_user":
			return c.handleCurrentUserQuery()
		case "server_version", "server_encoding", "client_encoding":
			return c.handleShowCommand(strings.ToUpper(kind))
		}
	}
	// Parse the complete batch before executing its first statement. This keeps
	// unsupported trailing clauses from turning into committed writes.
	l := lexer.New(sql)
	p := parser.New(l)
	stmts, err := p.ParseMultiple()
	if err != nil {
		code := ErrCodeSyntaxError
		if errors.Is(err, pgerror.ErrFeatureNotSupported) {
			code = ErrCodeFeatureNotSupported
		}
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
		c.sendError("ERROR", code, fmt.Sprintf("Syntax error: %v", err))
		return c.sendReadyForQuery()
	}

	for _, stmt := range stmts {
		if c.txStatus == TxStatusFailed {
			if _, rollback := stmt.(*parser.RollbackStmt); !rollback {
				c.sendError("ERROR", ErrCodeTransactionAborted, "current transaction is aborted, commands ignored until end of transaction block")
				return c.sendReadyForQuery()
			}
		}
		result, err := c.executor.Execute(stmt)
		if err != nil && isIdleRollback(stmt, c.txStatus, err) {
			result, err = executor.NewResult("ROLLBACK"), nil
		}
		if err != nil {
			code := ErrCodeInternalError
			message := fmt.Sprintf("Execution error: %v", err)
			var primaryKeyErr *storage.PrimaryKeyViolationError
			var uniqueErr *storage.UniqueViolationError
			var notNullErr *storage.NotNullViolationError
			var checkErr *storage.CheckViolationError
			var foreignKeyErr *storage.ForeignKeyViolationError
			if errors.Is(err, storage.ErrSerialization) {
				code = ErrCodeSerializationFailure
				c.txStatus = TxStatusIdle
			} else if errors.Is(err, pgerror.ErrInvalidText) {
				code = ErrCodeInvalidText
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.Is(err, pgerror.ErrInvalidParameter) {
				code = ErrCodeInvalidParameter
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.Is(err, pgerror.ErrDatatypeMismatch) {
				code = ErrCodeDatatypeMismatch
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.Is(err, pgerror.ErrUndefinedTable) || analysisUndefinedTable(err) {
				code = ErrCodeUndefinedTable
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if analysisErr := analysisTypeMismatch(err); analysisErr {
				code = ErrCodeDatatypeMismatch
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.As(err, &notNullErr) {
				code = "23502"
				if _, commit := stmt.(*parser.CommitStmt); commit {
					c.txStatus = TxStatusIdle
				} else if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.As(err, &checkErr) {
				code = ErrCodeCheckViolation
				if _, commit := stmt.(*parser.CommitStmt); commit {
					c.txStatus = TxStatusIdle
				} else if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.As(err, &foreignKeyErr) {
				code = ErrCodeForeignKeyViolation
				if _, commit := stmt.(*parser.CommitStmt); commit {
					c.txStatus = TxStatusIdle
				} else if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.As(err, &uniqueErr) || errors.As(err, &primaryKeyErr) {
				code = ErrCodeUniqueViolation
				if uniqueErr != nil {
					message = fmt.Sprintf("Execution error: UNIQUE constraint failed: %s", uniqueErr.Constraint)
				}
				if _, commit := stmt.(*parser.CommitStmt); commit {
					c.txStatus = TxStatusIdle
				} else if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if errors.Is(err, pgerror.ErrFeatureNotSupported) || errors.Is(err, executor.ErrUnsupportedFeature) {
				code = ErrCodeFeatureNotSupported
				if c.txStatus == TxStatusInBlock {
					c.txStatus = TxStatusFailed
				}
			} else if _, commit := stmt.(*parser.CommitStmt); commit {
				// A failed COMMIT has already rolled the transaction back in
				// storage; leaving the session in the failed state would make
				// every following statement (including ROLLBACK) fail.
				c.txStatus = TxStatusIdle
			} else if c.txStatus == TxStatusInBlock {
				c.txStatus = TxStatusFailed
			}
			c.sendError("ERROR", code, message)
			return c.sendReadyForQuery()
		}
		if err := c.sendResult(result, stmt, wireColumnTypes(result), nil); err != nil {
			return err
		}
		if result.LSN > 0 {
			if err := c.sendLSN(result.LSN); err != nil {
				return err
			}
		}
		if resetsPortals(stmt) {
			// Same rule as the extended path: a transaction end or a savepoint
			// rollback invalidates every open portal. sendReadyForQuery covers
			// the idle case; ROLLBACK TO SAVEPOINT keeps the block open and
			// needs this explicit clear.
			c.clearPortals()
		}
	}

	return c.sendReadyForQuery()
}

func (c *Connection) handleParse(msg *Message) error {
	if c.extendedFailed {
		return nil
	}
	name, pos, err := readCString(msg.Data, 0)
	if err != nil {
		return c.failExtended(ErrCodeProtocolViolation, err)
	}
	query, pos, err := readCString(msg.Data, pos)
	if err != nil || pos+2 > len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Parse message"))
	}
	count := int(binary.BigEndian.Uint16(msg.Data[pos : pos+2]))
	pos += 2
	if pos+count*4 != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Parse parameter list"))
	}
	oids := make([]int32, count)
	for i := range oids {
		oids[i] = int32(binary.BigEndian.Uint32(msg.Data[pos : pos+4]))
		pos += 4
	}
	if !isSingleStatementSQL(query) {
		return c.failExtended(ErrCodeSyntaxError, fmt.Errorf("prepared statements must contain one command"))
	}
	paramOIDs, err := c.negotiateParameterOIDs(query, oids)
	if err != nil {
		return c.failExtended(ErrCodeProgramLimitExceeded, err)
	}
	c.statements[name] = &preparedStatement{query: query, paramOIDs: paramOIDs}
	return c.writeMessage(MsgParseComplete, nil)
}

func (c *Connection) handleBind(msg *Message) error {
	if c.extendedFailed {
		return nil
	}
	portalName, pos, err := readCString(msg.Data, 0)
	if err != nil {
		return c.failExtended(ErrCodeProtocolViolation, err)
	}
	statementName, pos, err := readCString(msg.Data, pos)
	if err != nil {
		return c.failExtended(ErrCodeProtocolViolation, err)
	}
	statement, ok := c.statements[statementName]
	if !ok {
		return c.failExtended(ErrCodeInvalidParameter, fmt.Errorf("prepared statement %q does not exist", statementName))
	}
	// Named portals cannot be replaced: PostgreSQL reports 42P03 and leaves the
	// existing portal in place. Only the unnamed portal is re-bound freely,
	// because the new Bind replaces it.
	if portalName != "" {
		if _, exists := c.portals[portalName]; exists {
			return c.failExtended(ErrCodeDuplicateCursor, fmt.Errorf("portal %q already exists", portalName))
		}
	}
	formats, pos, err := readInt16List(msg.Data, pos)
	if err != nil || pos+2 > len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind format list"))
	}
	paramCount := int(binary.BigEndian.Uint16(msg.Data[pos : pos+2]))
	pos += 2
	if len(formats) > 1 && len(formats) != paramCount {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("Bind parameter format count does not match parameters"))
	}
	for _, format := range formats {
		if format != 0 && format != 1 {
			return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind parameter format code"))
		}
	}
	params := make([]boundParameter, paramCount)
	for i := range params {
		if pos+4 > len(msg.Data) {
			return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind parameter"))
		}
		length := int32(binary.BigEndian.Uint32(msg.Data[pos : pos+4]))
		pos += 4
		params[i].oid = parameterOID(statement.paramOIDs, i)
		params[i].format = parameterFormat(formats, i)
		if length == -1 {
			params[i].null = true
			continue
		}
		if length < 0 || pos+int(length) > len(msg.Data) {
			return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind parameter length"))
		}
		params[i].value = append([]byte(nil), msg.Data[pos:pos+int(length)]...)
		pos += int(length)
	}
	resultFormats, pos, err := readInt16List(msg.Data, pos)
	if err != nil || pos != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind result format list"))
	}
	for _, format := range resultFormats {
		if format != 0 && format != 1 {
			return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind result format code"))
		}
	}
	query, err := bindQuery(statement.query, params)
	if err != nil {
		code := ErrCodeInvalidParameter
		if errors.Is(err, pgerror.ErrFeatureNotSupported) {
			code = ErrCodeFeatureNotSupported
		}
		if errors.Is(err, pgerror.ErrInvalidText) {
			code = ErrCodeInvalidText
		}
		if errors.Is(err, pgerror.ErrInvalidParameter) {
			code = ErrCodeInvalidParameter
		}
		return c.failExtended(code, err)
	}
	c.portals[portalName] = &portal{query: query, resultFormats: resultFormats}
	return c.writeMessage(MsgBindComplete, nil)
}

func (c *Connection) handleDescribe(msg *Message) error {
	if c.extendedFailed {
		return nil
	}
	if len(msg.Data) < 2 {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Describe message"))
	}
	name, pos, err := readCString(msg.Data, 1)
	if err != nil || pos != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Describe name"))
	}
	switch msg.Data[0] {
	case 'S':
		statement, ok := c.statements[name]
		if !ok {
			return c.failExtended(ErrCodeInvalidParameter, fmt.Errorf("prepared statement %q does not exist", name))
		}
		if err := c.sendParameterDescription(statement.paramOIDs); err != nil {
			return err
		}
	case 'P':
		// PostgreSQL reports parameter types only for Describe('S'). Portal
		// Describe returns row metadata (here NoData) without a
		// ParameterDescription.
		if _, ok := c.portals[name]; !ok {
			return c.failExtended(ErrCodeInvalidParameter, fmt.Errorf("portal %q does not exist", name))
		}
	default:
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Describe target"))
	}
	// Row metadata requires analysis without side effects. Return NoData for
	// compatibility; Execute still emits the real RowDescription. This is the
	// pre-0.4.0 behavior used by common extended-protocol clients.
	return c.writeMessage(MsgNoData, nil)
}

func (c *Connection) handleExecute(msg *Message) error {
	if c.extendedFailed {
		return nil
	}
	name, pos, err := readCString(msg.Data, 0)
	if err != nil || pos+4 != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Execute message"))
	}
	portal, ok := c.portals[name]
	if !ok {
		return c.failExtended(ErrCodeUndefinedCursor, fmt.Errorf("portal %q does not exist", name))
	}
	maxRows := int32(binary.BigEndian.Uint32(msg.Data[pos : pos+4]))
	if maxRows < 0 {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Execute row limit: %d", maxRows))
	}
	// Check the transaction state before catalog emulation. Catalog queries do
	// not all parse as regular PizzaSQL statements, but must still return 25P02
	// while the transaction is aborted.
	if c.txStatus == TxStatusFailed && !isRollbackStatement(portal.query) {
		return c.failExtended(ErrCodeTransactionAborted, fmt.Errorf("current transaction is aborted, commands ignored until end of transaction block"))
	}
	if portal.execution == nil {
		if failure := c.materializePortal(portal); failure != nil {
			return c.failExtended(failure.code, failure.err)
		}
	}
	if err := c.sendPortalRows(portal, maxRows); err != nil {
		return err
	}
	if portal.execution.clearsPortals {
		c.clearPortals()
	}
	return nil
}

// executeError is a rejected Execute. Its transaction-state effects have already
// been applied by the time it is built, so handleExecute only reports it.
type executeError struct {
	code string
	err  error
}

// materializePortal runs a portal's statement once and stores everything the
// Execute responses need. The executor already produces a complete result, so a
// resumed Execute stays a slice of that result and never runs the SQL again;
// column metadata and the completion tag are also computed once. A nil return
// means the portal is ready to stream.
func (c *Connection) materializePortal(portal *portal) *executeError {
	execution := &portalExecution{}
	stmt, err := parser.New(lexer.New(portal.query)).Parse()
	if err != nil {
		code := ErrCodeSyntaxError
		if errors.Is(err, pgerror.ErrFeatureNotSupported) {
			code = ErrCodeFeatureNotSupported
			if c.txStatus == TxStatusInBlock {
				c.txStatus = TxStatusFailed
			}
		}
		return &executeError{code: code, err: fmt.Errorf("syntax error: %w", err)}
	}
	result, err := c.executor.Execute(stmt)
	if err != nil && isIdleRollback(stmt, c.txStatus, err) {
		result, err = executor.NewResult("ROLLBACK"), nil
	}
	if err != nil {
		return c.classifyExecuteError(stmt, err)
	}
	execution.result = result
	// getCommandTag applies the statement's transaction transition, so it
	// belongs here: a repeated Execute must not replay BEGIN/COMMIT/ROLLBACK.
	execution.commandTag = c.getCommandTag(stmt, result)
	execution.clearsPortals = resetsPortals(stmt)
	columnTypes := wireColumnTypes(execution.result)
	formats, formatErr := c.resolveResultFormats(portal.resultFormats, columnTypes, len(execution.result.Columns))
	if formatErr != nil {
		return &executeError{code: resultFormatSQLState(formatErr), err: formatErr}
	}
	execution.columnTypes = columnTypes
	execution.oids = c.resultColumnOIDs(columnTypes, len(execution.result.Columns))
	execution.formats = formats
	portal.execution = execution
	return nil
}

// classifyExecuteError maps a statement execution error to the SQLSTATE the
// client receives and leaves the transaction state consistent.
func (c *Connection) classifyExecuteError(stmt parser.Statement, err error) *executeError {
	code := ErrCodeInternalError
	var primaryKeyErr *storage.PrimaryKeyViolationError
	var uniqueErr *storage.UniqueViolationError
	var notNullErr *storage.NotNullViolationError
	var checkErr *storage.CheckViolationError
	var foreignKeyErr *storage.ForeignKeyViolationError
	if errors.Is(err, storage.ErrSerialization) {
		code = ErrCodeSerializationFailure
		c.txStatus = TxStatusIdle
	} else if errors.Is(err, pgerror.ErrInvalidText) {
		code = ErrCodeInvalidText
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.Is(err, pgerror.ErrInvalidParameter) {
		code = ErrCodeInvalidParameter
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.Is(err, pgerror.ErrDatatypeMismatch) {
		code = ErrCodeDatatypeMismatch
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.Is(err, pgerror.ErrUndefinedTable) || analysisUndefinedTable(err) {
		code = ErrCodeUndefinedTable
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if analysisTypeMismatch(err) {
		code = ErrCodeDatatypeMismatch
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.As(err, &notNullErr) {
		code = "23502"
		if _, commit := stmt.(*parser.CommitStmt); commit {
			c.txStatus = TxStatusIdle
		} else if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.As(err, &checkErr) {
		code = ErrCodeCheckViolation
		if _, commit := stmt.(*parser.CommitStmt); commit {
			c.txStatus = TxStatusIdle
		} else if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.As(err, &foreignKeyErr) {
		code = ErrCodeForeignKeyViolation
		if _, commit := stmt.(*parser.CommitStmt); commit {
			c.txStatus = TxStatusIdle
		} else if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.As(err, &uniqueErr) || errors.As(err, &primaryKeyErr) {
		code = ErrCodeUniqueViolation
		if _, commit := stmt.(*parser.CommitStmt); commit {
			c.txStatus = TxStatusIdle
		} else if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if errors.Is(err, pgerror.ErrFeatureNotSupported) || errors.Is(err, executor.ErrUnsupportedFeature) {
		code = ErrCodeFeatureNotSupported
		if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
	} else if _, commit := stmt.(*parser.CommitStmt); commit {
		c.txStatus = TxStatusIdle
	} else if c.txStatus == TxStatusInBlock {
		c.txStatus = TxStatusFailed
	}
	if uniqueErr != nil {
		return &executeError{code: code, err: fmt.Errorf("execution error: UNIQUE constraint failed: %s", uniqueErr.Constraint)}
	}
	return &executeError{code: code, err: fmt.Errorf("execution error: %w", err)}
}

// sendPortalRows writes one Execute response from the portal's materialized
// result. The RowDescription is sent once, by the first Execute that has
// columns, so a resumed Execute emits only DataRows. With maxRows > 0 the
// response is PortalSuspended once the limit was reached; PostgreSQL reports
// PortalSuspended even when the last row falls exactly on the limit and
// completes on the next Execute, which is what this does too.
func (c *Connection) sendPortalRows(portal *portal, maxRows int32) error {
	execution := portal.execution
	if len(execution.result.Columns) > 0 && !execution.described {
		if err := c.sendRowDescriptionFormats(execution.result.Columns, execution.columnTypes, execution.oids, execution.formats); err != nil {
			return err
		}
		execution.described = true
	}
	remaining := len(execution.result.Rows) - execution.rowOffset
	limit := remaining
	if maxRows > 0 && int(maxRows) < remaining {
		limit = int(maxRows)
	}
	for i := 0; i < limit; i++ {
		if err := c.sendDataRowFormats(execution.result.Rows[execution.rowOffset+i], execution.oids, execution.formats); err != nil {
			return c.failBinaryEncode(err)
		}
	}
	execution.rowOffset += limit
	if maxRows > 0 && limit == int(maxRows) {
		return c.sendPortalSuspended()
	}
	firstCompletion := !execution.completed
	execution.completed = true
	if err := c.sendCommandComplete(execution.commandTag); err != nil {
		return err
	}
	if firstCompletion && execution.result.LSN > 0 {
		if err := c.sendLSN(execution.result.LSN); err != nil {
			return err
		}
	}
	// The portal is exhausted: keep its metadata for a repeated Execute, but
	// release the row data so a transaction that leaves portals open does not
	// retain every result.
	execution.result.Rows = nil
	execution.rowOffset = 0
	return nil
}

func (c *Connection) sendTabularResult(result *executor.Result, tag string, bindFormats []int16) error {
	formats, err := c.resolveResultFormats(bindFormats, result.ColumnTypes, len(result.Columns))
	if err != nil {
		return c.failResultFormat(err)
	}
	oids := c.resultColumnOIDs(result.ColumnTypes, len(result.Columns))
	if err := c.sendRowDescriptionFormats(result.Columns, result.ColumnTypes, oids, formats); err != nil {
		return err
	}
	for _, row := range result.Rows {
		if err := c.sendDataRowFormats(row, oids, formats); err != nil {
			return c.failBinaryEncode(err)
		}
	}
	return c.sendCommandComplete(fmt.Sprintf("%s %d", tag, len(result.Rows)))
}

func (c *Connection) handleClose(msg *Message) error {
	if c.extendedFailed {
		return nil
	}
	if len(msg.Data) < 2 {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Close message"))
	}
	name, pos, err := readCString(msg.Data, 1)
	if err != nil || pos != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Close name"))
	}
	if msg.Data[0] == 'S' {
		delete(c.statements, name)
	} else if msg.Data[0] == 'P' {
		delete(c.portals, name)
	} else {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Close target"))
	}
	return c.writeMessage(MsgCloseComplete, nil)
}

// clearPortals drops every portal. PostgreSQL binds a portal to the transaction
// that created it: Sync ends the implicit transaction outside an explicit
// block, and COMMIT/ROLLBACK end an explicit one. Prepared statements are not
// affected, so a client re-binds after the transaction ends.
func (c *Connection) clearPortals() {
	clear(c.portals)
}

// resetsPortals reports whether stmt invalidates every open portal. COMMIT and
// ROLLBACK end the transaction, and ROLLBACK TO SAVEPOINT undoes writes a
// suspended portal may already have materialized: resuming it would stream rows
// and a completion tag from a removed state. Clearing every portal on ROLLBACK
// TO SAVEPOINT is stricter than PostgreSQL, which keeps portals created before
// the savepoint; the coarse rule is what keeps a failed or rewound statement
// from leaking into a resumed stream.
func resetsPortals(stmt parser.Statement) bool {
	switch stmt.(type) {
	case *parser.CommitStmt, *parser.RollbackStmt:
		return true
	}
	return false
}

func (c *Connection) failExtended(code string, err error) error {
	c.extendedFailed = true
	if c.txStatus == TxStatusInBlock {
		c.txStatus = TxStatusFailed
	}
	return c.sendError("ERROR", code, err.Error())
}

func (c *Connection) handleProxyError(msg *Message) error {
	if len(msg.Data) != 1 || (msg.Data[0] != MsgQuery && msg.Data[0] != MsgParse) {
		_ = c.sendError("FATAL", ErrCodeProtocolViolation, "invalid proxy error relay")
		return io.EOF
	}
	code, message := "42501", "permission denied by API key scopes"
	if c.txStatus == TxStatusFailed {
		code, message = ErrCodeTransactionAborted, "current transaction is aborted, commands ignored until end of transaction block"
	}
	if msg.Data[0] == MsgParse {
		c.extendedCycle = true
		return c.failExtended(code, errors.New(message))
	}
	if c.txStatus == TxStatusInBlock {
		c.txStatus = TxStatusFailed
	}
	if err := c.sendError("ERROR", code, message); err != nil {
		return err
	}
	return c.sendReadyForQuery()
}

// analysisTypeMismatch reports whether the error is an analyzer type-mismatch,
// which is reported with SQLSTATE 42804 instead of a generic internal error.
func analysisTypeMismatch(err error) bool {
	var analysisErr *analyzer.AnalysisError
	return errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTypeMismatch
}

// analysisUndefinedTable reports whether the error is the analyzer's
// table-not-found, which PostgreSQL reports as 42P01 (undefined_table).
func analysisUndefinedTable(err error) bool {
	var analysisErr *analyzer.AnalysisError
	return errors.As(err, &analysisErr) && analysisErr.Type == analyzer.ErrTableNotFound
}

type boundParameter struct {
	value  []byte
	oid    int32
	format int16
	null   bool
}

func readCString(data []byte, pos int) (string, int, error) {
	if pos < 0 || pos >= len(data) {
		return "", pos, fmt.Errorf("missing null-terminated string")
	}
	end := bytes.IndexByte(data[pos:], 0)
	if end < 0 {
		return "", pos, fmt.Errorf("unterminated string")
	}
	return string(data[pos : pos+end]), pos + end + 1, nil
}

func readInt16List(data []byte, pos int) ([]int16, int, error) {
	if pos+2 > len(data) {
		return nil, pos, fmt.Errorf("missing list length")
	}
	count := int(binary.BigEndian.Uint16(data[pos : pos+2]))
	pos += 2
	if pos+count*2 > len(data) {
		return nil, pos, fmt.Errorf("truncated list")
	}
	result := make([]int16, count)
	for i := range result {
		result[i] = int16(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
	}
	return result, pos, nil
}

func parameterOID(oids []int32, index int) int32 {
	if index < len(oids) {
		return oids[index]
	}
	return 0
}

func parameterFormat(formats []int16, index int) int16 {
	if len(formats) == 1 {
		return formats[0]
	}
	if index < len(formats) {
		return formats[index]
	}
	return 0
}

func bindQuery(query string, params []boundParameter) (string, error) {
	var result strings.Builder
	last := 0
	for _, reference := range scanParameters(query) {
		result.WriteString(query[last:reference.start])
		if reference.number < 1 || reference.number > len(params) {
			return "", fmt.Errorf("parameter %s was not provided", query[reference.start:reference.end])
		}
		literal, err := parameterLiteral(params[reference.number-1])
		if err != nil {
			return "", fmt.Errorf("parameter $%d: %w", reference.number, err)
		}
		result.WriteString(literal)
		last = reference.end
	}
	result.WriteString(query[last:])
	return result.String(), nil
}

func parameterLiteral(param boundParameter) (string, error) {
	if param.null {
		return "NULL", nil
	}
	if param.format == 1 {
		if element, isArray := arrayParamElementType(param.oid); isArray {
			return binaryArrayLiteral(element, param.value)
		}
		if param.oid == 3802 {
			// PostgreSQL's binary jsonb format is a version byte followed by the
			// document text. pgx sends this form by default.
			if len(param.value) == 0 {
				return "", pgerror.InvalidText("jsonb binary parameter is empty")
			}
			if param.value[0] != 1 {
				return "", pgerror.InvalidText("unsupported jsonb binary format version %d", param.value[0])
			}
			canonical, err := executor.CanonicalizeJSONB(string(param.value[1:]))
			if err != nil {
				return "", err
			}
			return "'" + strings.ReplaceAll(canonical, "'", "''") + "'::jsonb", nil
		}
		switch param.oid {
		case 700, 701:
			return binaryFloatText(param.oid, param.value)
		case 1700:
			return binaryNumericText(param.value)
		case 16:
			if len(param.value) != 1 || param.value[0] > 1 {
				return "", pgerror.InvalidText("invalid binary boolean parameter")
			}
			if param.value[0] == 0 {
				return "FALSE", nil
			}
			return "TRUE", nil
		case 21:
			if len(param.value) != 2 {
				return "", fmt.Errorf("invalid binary int2")
			}
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(param.value))), 10), nil
		case 23:
			if len(param.value) != 4 {
				return "", fmt.Errorf("invalid binary int4")
			}
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(param.value))), 10), nil
		case 20:
			if len(param.value) != 8 {
				return "", fmt.Errorf("invalid binary int8")
			}
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(param.value)), 10), nil
		default:
			return "", fmt.Errorf("%w: binary parameter format for OID %d", pgerror.ErrFeatureNotSupported, param.oid)
		}
	}
	value := string(param.value)
	if param.oid == 3802 {
		// A text-format jsonb parameter is canonicalized at Bind, so the executor
		// receives the same bytes a jsonb column stores and an invalid document
		// fails before execution.
		canonical, err := executor.CanonicalizeJSONB(value)
		if err != nil {
			return "", err
		}
		return "'" + strings.ReplaceAll(canonical, "'", "''") + "'::jsonb", nil
	}
	if element, isArray := arrayParamElementType(param.oid); isArray {
		// The bound value is PostgreSQL array text. Validate and canonicalize it
		// now so a malformed array fails at Bind instead of during row
		// evaluation, then emit a typed literal the executor accepts unchanged.
		arrayType := element + "[]"
		canonical, err := executor.CanonicalizeArrayText(value, arrayType)
		if err != nil {
			return "", err
		}
		return "'" + strings.ReplaceAll(canonical, "'", "''") + "'::" + arrayType, nil
	}
	switch param.oid {
	case 0:
		if strings.EqualFold(value, "true") || strings.EqualFold(value, "false") {
			return strings.ToUpper(value), nil
		}
		if _, err := strconv.ParseFloat(value, 64); err == nil && value != "" {
			return value, nil
		}
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	case 16:
		if strings.EqualFold(value, "true") || value == "1" || value == "t" {
			return "TRUE", nil
		}
		return "FALSE", nil
	case 20, 21, 23, 26, 700, 701, 1700:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return "", fmt.Errorf("invalid numeric value")
		}
		return value, nil
	default:
		return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
	}
}

// sendResult sends a statement's result. columnTypes are the types reported in
// RowDescription (already corrected for BLOB values) and formats are the
// portal's resolved per-column format codes, or nil for all-text results.
func (c *Connection) sendResult(result *executor.Result, stmt parser.Statement, columnTypes []string, formats []int16) error {
	// Any statement that produced a row set (SELECT, or INSERT/UPDATE/DELETE
	// with RETURNING) sends a row description and its data rows. Other
	// statements only send the command completion tag.
	if len(result.Columns) > 0 {
		oids := c.resultColumnOIDs(columnTypes, len(result.Columns))
		if err := c.sendRowDescriptionFormats(result.Columns, columnTypes, oids, formats); err != nil {
			return err
		}
		for _, row := range result.Rows {
			if err := c.sendDataRowFormats(row, oids, formats); err != nil {
				return c.failBinaryEncode(err)
			}
		}
		return c.sendCommandComplete(c.getCommandTag(stmt, result))
	}
	// For other statements, just send command complete
	tag := c.getCommandTag(stmt, result)
	return c.sendCommandComplete(tag)
}

// wireColumnTypes corrects declared-affinity metadata when SQLite has stored a
// value of another type. In particular, binding []byte to a VARCHAR column is
// still a BLOB in SQLite; advertising TEXT while encoding PostgreSQL bytea text
// would make clients receive the literal "\\x..." instead of the original
// bytes.
func wireColumnTypes(result *executor.Result) []string {
	types := append([]string(nil), result.ColumnTypes...)
	if len(types) < len(result.Columns) {
		types = append(types, make([]string, len(result.Columns)-len(types))...)
	}
	for _, row := range result.Rows {
		for i, value := range row {
			if i >= len(types) {
				break
			}
			if _, ok := value.([]byte); ok {
				types[i] = "BLOB"
			}
		}
	}
	return types
}

// getCommandTag returns the command completion tag
func (c *Connection) getCommandTag(stmt parser.Statement, result *executor.Result) string {
	switch s := stmt.(type) {
	case *parser.SelectStmt:
		return fmt.Sprintf("SELECT %d", len(result.Rows))
	case *parser.CreateTableStmt:
		return "CREATE TABLE"
	case *parser.DropTableStmt:
		return "DROP TABLE"
	case *parser.CreateIndexStmt:
		return "CREATE INDEX"
	case *parser.DropIndexStmt:
		return "DROP INDEX"
	case *parser.AlterTableStmt:
		return "ALTER TABLE"
	case *parser.InsertStmt:
		return fmt.Sprintf("INSERT 0 %d", result.RowsAffected)
	case *parser.UpdateStmt:
		return fmt.Sprintf("UPDATE %d", result.RowsAffected)
	case *parser.DeleteStmt:
		return fmt.Sprintf("DELETE %d", result.RowsAffected)
	case *parser.BeginStmt:
		c.txStatus = TxStatusInBlock
		return "BEGIN"
	case *parser.CommitStmt:
		c.txStatus = TxStatusIdle
		return "COMMIT"
	case *parser.RollbackStmt:
		if s.Savepoint != "" {
			c.txStatus = TxStatusInBlock
			return "ROLLBACK"
		}
		c.txStatus = TxStatusIdle
		return "ROLLBACK"
	case *parser.SavepointStmt:
		c.txStatus = TxStatusInBlock
		return "SAVEPOINT"
	case *parser.ReleaseStmt:
		c.txStatus = TxStatusInBlock
		return "RELEASE"
	case *parser.ListenStmt:
		return "LISTEN"
	case *parser.UnlistenStmt:
		return "UNLISTEN"
	case *parser.NotifyStmt:
		return "NOTIFY"
	default:
		return "OK"
	}
}

// sendAuthenticationOk sends authentication OK message
func (c *Connection) sendAuthenticationOk() error {
	mb := NewMessageBuilder()
	mb.WriteInt32(0) // Auth OK
	return c.writeMessage(MsgAuthenticationOk, mb.Bytes())
}

// drainNotifications writes every notification currently buffered for this
// session. It is called at protocol-safe points (after Sync/ReadyForQuery).
func (c *Connection) drainNotifications() error {
	ch := c.executor.NotificationChannel()
	for {
		select {
		case n := <-ch:
			if err := c.sendNotification(n); err != nil {
				return err
			}
		default:
			return c.writer.Flush()
		}
	}
}

// sendNotification writes a NotificationResponse frame. The frame is only
// written by the connection's main goroutine, never from a publisher.
func (c *Connection) sendNotification(n storage.Notification) error {
	mb := NewMessageBuilder()
	mb.WriteInt32(n.PID)
	mb.WriteString(n.Channel)
	mb.WriteString(n.Payload)
	return c.writeMessage(MsgNotificationResponse, mb.Bytes())
}

// sendLSN exposes the last commit LSN as a ParameterStatus. Drivers that do
// not know the key ignore it; subscribers can use it as an ordering cursor.
func (c *Connection) sendLSN(lsn uint64) error {
	return c.sendParameterStatus("pizzasql.lsn", strconv.FormatUint(lsn, 10))
}

// sendParameterStatus sends a parameter status message
func (c *Connection) sendParameterStatus(name, value string) error {
	mb := NewMessageBuilder()
	mb.WriteString(name)
	mb.WriteString(value)
	return c.writeMessage(MsgParameterStatus, mb.Bytes())
}

// sendBackendKeyData sends backend key data
func (c *Connection) sendBackendKeyData(processID, secretKey int32) error {
	mb := NewMessageBuilder()
	mb.WriteInt32(processID)
	mb.WriteInt32(secretKey)
	return c.writeMessage(MsgBackendKeyData, mb.Bytes())
}

// isIdleRollback reports whether a bare ROLLBACK found no transaction to roll
// back. A COMMIT rejected by storage has already ended the transaction, so the
// client's cleanup ROLLBACK must complete (PostgreSQL reports a warning and
// succeeds) instead of surfacing an internal error on an idle session.
func isIdleRollback(stmt parser.Statement, status byte, err error) bool {
	rollback, ok := stmt.(*parser.RollbackStmt)
	if !ok || rollback.Savepoint != "" {
		return false
	}
	return status == TxStatusIdle && errors.Is(err, executor.ErrNoTransaction)
}

// sendReadyForQuery sends ready for query message
func (c *Connection) sendReadyForQuery() error {
	if c.txStatus == TxStatusIdle {
		// The transaction this cycle ran in ends here, so its portals do too.
		// A portal inside an explicit block survives (status T) until
		// COMMIT/ROLLBACK, and a failed block (status E) keeps its portals so
		// Execute still reports 25P02 until the transaction is rolled back.
		c.clearPortals()
	}
	mb := NewMessageBuilder()
	mb.AppendByte(c.txStatus)
	if err := c.writeMessage(MsgReadyForQuery, mb.Bytes()); err != nil {
		return err
	}
	// ReadyForQuery closes out a response cycle, so flush everything buffered
	// so far. This is what makes simple-query results and Sync responses
	// visible to the client.
	return c.writer.Flush()
}

// sendEmptyQueryResponse sends empty query response
func (c *Connection) sendEmptyQueryResponse() error {
	return c.writeMessage(MsgEmptyQueryResponse, []byte{})
}

// sendParameterDescription reports the negotiated parameter types of a
// prepared statement. The protocol count field is 16-bit and the negotiated
// list never exceeds maxParameterNumber, so the conversion cannot lose a count.
func (c *Connection) sendParameterDescription(oids []int32) error {
	if len(oids) > maxParameterNumber {
		return fmt.Errorf("parameter description has %d entries; the protocol limit is %d", len(oids), maxParameterNumber)
	}
	mb := NewMessageBuilder()
	mb.WriteInt16(int16(uint16(len(oids))))
	for _, oid := range oids {
		mb.WriteInt32(oid)
	}
	return c.writeMessage(MsgParameterDescription, mb.Bytes())
}

// sendRowDescription sends row description (column metadata) in text format.
func (c *Connection) sendRowDescription(columns []string, columnTypes []string) error {
	return c.sendRowDescriptionFormats(columns, columnTypes, c.resultColumnOIDs(columnTypes, len(columns)), nil)
}

// sendRowDescriptionFormats sends row description with the per-column formats a
// portal resolved at Bind. PostgreSQL reports the bound formats in a portal
// RowDescription; this server emits its RowDescription at Execute instead of
// Describe, so the portal's formats travel with the row metadata. OIDs are the
// same for text and binary results.
func (c *Connection) sendRowDescriptionFormats(columns []string, columnTypes []string, oids []int32, formats []int16) error {
	mb := NewMessageBuilder()
	mb.WriteInt16(int16(len(columns)))

	for i, col := range columns {
		colType := typeNameAt(columnTypes, i)
		format := int16(0)
		if i < len(formats) {
			format = formats[i]
		}
		mb.WriteString(col)
		mb.WriteInt32(0)                             // table OID
		mb.WriteInt16(0)                             // column attribute number
		mb.WriteInt32(oids[i])                       // type OID
		mb.WriteInt16(c.getTypeSizeForType(colType)) // type size
		mb.WriteInt32(-1)                            // type modifier
		mb.WriteInt16(format)                        // format code
	}

	return c.writeMessage(MsgRowDescription, mb.Bytes())
}

// resultColumnOIDs returns the PostgreSQL OID reported for every result column.
func (c *Connection) resultColumnOIDs(columnTypes []string, columnCount int) []int32 {
	oids := make([]int32, columnCount)
	for i := range oids {
		oids[i] = c.getOIDForType(typeNameAt(columnTypes, i))
	}
	return oids
}

// sendDataRow sends a data row in text format
func (c *Connection) sendDataRow(row []interface{}, columns []string) error {
	return c.sendDataRowFormats(row, nil, nil)
}

// sendDataRowFormats sends one DataRow, choosing text or binary encoding per
// column from formats. NULL is always length -1. Binary values are appended
// directly into the DataRow buffer; a value that cannot be encoded fails before
// the row is written, so the client never receives a partial DataRow.
func (c *Connection) sendDataRowFormats(row []interface{}, oids []int32, formats []int16) error {
	mb := NewMessageBuilder()
	mb.WriteInt16(int16(len(row)))

	for i, value := range row {
		if value == nil {
			mb.WriteInt32(-1) // NULL indicator
			continue
		}
		format := int16(0)
		if i < len(formats) {
			format = formats[i]
		}
		if format == 0 {
			strValue := c.valueToString(value)
			mb.WriteInt32(int32(len(strValue)))
			mb.WriteBytes([]byte(strValue))
			continue
		}
		oid := int32(25) // TEXT, the fallback RowDescription OID
		if i < len(oids) {
			oid = oids[i]
		}
		err := mb.AppendBinary(func(dst []byte) ([]byte, error) {
			return appendBinaryResultValue(dst, oid, value)
		})
		if err != nil {
			return fmt.Errorf("binary result column %d: %w", i+1, err)
		}
	}

	return c.writeMessage(MsgDataRow, mb.Bytes())
}

// sendCommandComplete sends command complete message
func (c *Connection) sendCommandComplete(tag string) error {
	mb := NewMessageBuilder()
	mb.WriteString(tag)
	return c.writeMessage(MsgCommandComplete, mb.Bytes())
}

// sendPortalSuspended reports that a bounded Execute stopped at its row limit
// with rows left to fetch.
func (c *Connection) sendPortalSuspended() error {
	return c.writeMessage(MsgPortalSuspended, nil)
}

// sendError sends an error response
func (c *Connection) sendError(severity, code, message string) error {
	if severity == "ERROR" && c.txStatus == TxStatusInBlock {
		c.txStatus = TxStatusFailed
	}
	mb := NewMessageBuilder()
	mb.AppendByte(ErrorFieldSeverity)
	mb.WriteString(severity)
	mb.AppendByte(ErrorFieldCode)
	mb.WriteString(code)
	mb.AppendByte(ErrorFieldMessage)
	mb.WriteString(message)
	mb.AppendByte(0) // Terminator

	if err := c.writeMessage(MsgErrorResponse, mb.Bytes()); err != nil {
		return err
	}
	// Errors must be visible promptly, including FATAL startup failures that
	// are not followed by a ReadyForQuery before the connection closes.
	return c.writer.Flush()
}

// writeMessage buffers a message for the connection. It does not flush, so
// callers that need to make a response visible to the client must flush at the
// appropriate protocol boundary (ReadyForQuery, an explicit Flush message, or
// an error response). Buffering amortizes the per-message syscalls that a
// result set would otherwise incur; the underlying bufio.Writer bounds memory
// use so large result sets cannot grow the buffer without limit.
func (c *Connection) writeMessage(msgType byte, data []byte) error {
	if !c.quiet {
		log.Printf("Sending message type=%c length=%d", msgType, len(data)+4)
	}
	return WriteMessage(c.writer, msgType, data)
}

// arrayElementTypeName reports the element type of an array type name such as
// "INTEGER[]". It reports false for scalar type names.
func arrayElementTypeName(typeName string) (string, bool) {
	upper := strings.ToUpper(strings.TrimSpace(typeName))
	if !strings.HasSuffix(upper, "[]") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(upper, "[]")), true
}

// arrayOIDForElement maps an array element type name to its PostgreSQL array
// OID. Narrower integer types report int8[] because values are not
// range-checked, matching the scalar carrier rule; NUMERIC reports numeric[]
// because array elements stay text.
func arrayOIDForElement(element string) int32 {
	base := strings.ToUpper(strings.TrimSpace(element))
	if idx := strings.IndexByte(base, '('); idx >= 0 {
		base = strings.TrimSpace(base[:idx])
	}
	switch base {
	case "INTEGER", "INT", "INT4":
		return 1007 // INT4ARRAYOID
	case "BIGINT", "INT8", "SMALLINT", "INT2", "TINYINT", "MEDIUMINT":
		return 1016 // INT8ARRAYOID
	case "BOOLEAN", "BOOL":
		return 1000 // BOOLARRAYOID
	case "REAL", "FLOAT", "FLOAT4":
		return 1021 // FLOAT4ARRAYOID
	case "DOUBLE", "DOUBLE PRECISION", "FLOAT8":
		return 1022 // FLOAT8ARRAYOID
	case "NUMERIC", "DECIMAL":
		return 1231 // NUMERICARRAYOID
	case "VARCHAR", "NVARCHAR", "CHARACTER VARYING":
		return 1015 // VARCHARARRAYOID
	default:
		return 1009 // TEXTARRAYOID
	}
}

// arrayParamElementType maps an array parameter OID to the element type the
// bound literal is cast to. Unknown OIDs are handled by callers.
func arrayParamElementType(oid int32) (string, bool) {
	switch oid {
	case 1009:
		return "TEXT", true
	case 1015:
		return "VARCHAR", true
	case 1007:
		return "INTEGER", true
	case 1016:
		return "BIGINT", true
	case 1005:
		return "SMALLINT", true
	case 1000:
		return "BOOLEAN", true
	case 1021:
		return "REAL", true
	case 1022:
		return "DOUBLE", true
	case 1231:
		return "NUMERIC", true
	default:
		return "", false
	}
}

// getOIDForType returns PostgreSQL OID for type.
//
// Types narrower than the value the engine carries are reported as the carrier
// type, because the engine does not range-check stored or cast values: a value
// that does not fit int2/int4 or float4 would fail client-side decoding rather
// than having been rejected on write. INTEGER and REAL keep their declared
// mappings, which predate this rule and are what existing clients expect.
func (c *Connection) getOIDForType(typeName string) int32 {
	if element, ok := arrayElementTypeName(typeName); ok {
		return arrayOIDForElement(element)
	}
	switch strings.ToUpper(typeName) {
	case "INT2":
		// The executor's exact-width spelling for a SMALLINT cast whose
		// constant value fits int2. A SMALLINT column is grouped below because
		// its stored values are not range-checked.
		return 21 // INT2OID
	case "SMALLINT", "TINYINT", "MEDIUMINT":
		// Carried as int64 and not range-checked, so int8 is the honest type.
		return 20 // INT8OID
	case "INTEGER", "INT", "INT4":
		return 23 // INT4OID
	case "BIGINT", "INT8":
		return 20 // INT8OID
	case "NUMERIC", "DECIMAL":
		// Values round-trip through JSON as float64; there is no exact decimal
		// carrier, so NUMERIC is not advertised.
		return 701 // FLOAT8OID
	case "JSONB":
		return 3802 // JSONBOID
	case "OID":
		return 26 // OIDOID
	case "REGCLASS":
		return 2205 // REGCLASSOID
	case "REGTYPE":
		return 2206 // REGTYPEOID
	case "REGPROC":
		// pg_type.typinput and friends are regproc attributes.
		return 24 // REGPROCOID
	case "NAME":
		return 19 // NAMEOID
	case "TEXT", "VARCHAR", "CHAR", "UUID":
		// UUID is stored as text (SQLite-style), so it reports TEXTOID rather
		// than the native PG UUID OID.
		return 25 // TEXTOID
	case "REAL", "FLOAT", "FLOAT4":
		return 700 // FLOAT4OID
	case "DOUBLE", "FLOAT8":
		return 701 // FLOAT8OID
	case "BOOLEAN", "BOOL":
		return 16 // BOOLOID
	case "BLOB":
		return 17 // BYTEAOID
	case "DATETIME", "TIMESTAMP":
		// Datetime values are exchanged as UTC RFC3339 (ISO8601 with a timezone
		// offset), which timestamptz decodes directly.
		return 1184 // TIMESTAMPTZOID
	case "DATE":
		return 1082 // DATEOID
	default:
		return 25 // Default to TEXT
	}
}

// getTypeSizeForType returns type size in bytes, or -1 for variable length.
func (c *Connection) getTypeSizeForType(typeName string) int16 {
	if _, ok := arrayElementTypeName(typeName); ok {
		return -1 // arrays are variable length
	}
	switch strings.ToUpper(typeName) {
	case "INT2":
		return 2 // Exact int2 cast result; see getOIDForType.
	case "SMALLINT", "TINYINT", "MEDIUMINT":
		return 8 // Reported as int8; see getOIDForType.
	case "INTEGER", "INT", "INT4":
		return 4
	case "BIGINT", "INT8":
		return 8
	case "NUMERIC", "DECIMAL":
		return 8 // Reported as float8; see getOIDForType.
	case "REAL", "FLOAT", "FLOAT4":
		return 4
	case "DOUBLE", "FLOAT8":
		return 8
	case "BOOLEAN", "BOOL":
		return 1
	case "DATETIME", "TIMESTAMP":
		return 8
	case "DATE":
		return 4
	case "OID", "REGCLASS", "REGTYPE", "REGPROC":
		return 4
	case "NAME":
		return 64
	default:
		return -1 // Variable length
	}
}

// valueToString converts a value to its PostgreSQL text representation
func (c *Connection) valueToString(value interface{}) string {
	return textValueString(value)
}

// textValueString is the text representation the wire layer sends for a value.
// Binary text and bytea results reuse it so both formats carry the same bytes
// for values without a native binary form. PostgreSQL's text format for bytea is
// \x followed by hex, which lets libpq/pgx decode a BLOB column (OID 17)
// losslessly.
func textValueString(value interface{}) string {
	if value == nil {
		return ""
	}
	if b, ok := value.([]byte); ok {
		return `\x` + hex.EncodeToString(b)
	}
	return fmt.Sprintf("%v", value)
}

// handleVersionQuery handles SELECT version()
func (c *Connection) handleVersionQuery() error {
	columns := []string{"version"}
	columnTypes := []string{"TEXT"}

	if err := c.sendRowDescription(columns, columnTypes); err != nil {
		return err
	}

	row := []interface{}{"PostgreSQL " + version.PostgreSQLCompatVersion + " (PizzaSQL)"}
	if err := c.sendDataRow(row, columns); err != nil {
		return err
	}

	if err := c.sendCommandComplete("SELECT 1"); err != nil {
		return err
	}

	return c.sendReadyForQuery()
}

// handleCurrentUserQuery handles SELECT current_user
func (c *Connection) handleCurrentUserQuery() error {
	columns := []string{"current_user"}
	columnTypes := []string{"TEXT"}

	if err := c.sendRowDescription(columns, columnTypes); err != nil {
		return err
	}

	user := c.params["user"]
	if user == "" {
		user = "pizzasql"
	}

	row := []interface{}{user}
	if err := c.sendDataRow(row, columns); err != nil {
		return err
	}

	if err := c.sendCommandComplete("SELECT 1"); err != nil {
		return err
	}

	return c.sendReadyForQuery()
}

// handleShowCommand handles SHOW commands
func (c *Connection) handleShowCommand(sqlUpper string) error {
	var value string
	var name string

	if sqlUpper == "SERVER_VERSION" {
		name = "server_version"
		value = version.PostgreSQLCompatVersion
	} else if sqlUpper == "SERVER_ENCODING" {
		name = "server_encoding"
		value = "UTF8"
	} else if sqlUpper == "CLIENT_ENCODING" {
		name = "client_encoding"
		value = "UTF8"
	} else {
		// Unknown SHOW command
		c.sendError("ERROR", ErrCodeFeatureNotSupported, "SHOW command not supported")
		return c.sendReadyForQuery()
	}

	columns := []string{name}
	columnTypes := []string{"TEXT"}

	if err := c.sendRowDescription(columns, columnTypes); err != nil {
		return err
	}

	row := []interface{}{value}
	if err := c.sendDataRow(row, columns); err != nil {
		return err
	}

	if err := c.sendCommandComplete("SHOW"); err != nil {
		return err
	}

	return c.sendReadyForQuery()
}

// isRollbackStatement reports whether sql parses as a single ROLLBACK
// statement, including ROLLBACK TO SAVEPOINT.
func isRollbackStatement(sql string) bool {
	stmt, err := parser.New(lexer.New(sql)).Parse()
	if err != nil {
		return false
	}
	_, ok := stmt.(*parser.RollbackStmt)
	return ok
}
