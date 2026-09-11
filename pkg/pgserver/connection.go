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
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

// Connection represents a client connection
type Connection struct {
	conn           net.Conn
	reader         *bufio.Reader
	writer         *bufio.Writer
	executor       *executor.Executor
	schema         *storage.SchemaManager
	dbManager      *storage.DatabaseManager
	database       string
	params         map[string]string
	txStatus       byte
	quiet          bool // Disable query logging
	statements     map[string]*preparedStatement
	portals        map[string]*portal
	extendedFailed bool
}

type preparedStatement struct {
	query     string
	paramOIDs []int32
}

type portal struct {
	query    string
	executed bool
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
	}
}

// Handle processes the connection
func (c *Connection) Handle() error {
	defer func() {
		if c.executor != nil {
			if err := c.executor.RollbackActive(); err != nil && !c.quiet {
				log.Printf("failed to roll back disconnected transaction: %v", err)
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
	if err := c.sendParameterStatus("server_version", "14.0 (PizzaSQL)"); err != nil {
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
	if err := c.sendBackendKeyData(12345, 67890); err != nil {
		return err
	}

	// Send ready for query
	if err := c.sendReadyForQuery(); err != nil {
		return err
	}

	// Message loop
	for {
		msg, err := ReadMessage(c.reader)
		if err != nil {
			if err == io.EOF {
				log.Printf("Connection closed by client")
				return nil
			}
			return fmt.Errorf("failed to read message: %w", err)
		}

		if err := c.handleMessage(msg); err != nil {
			if err == io.EOF {
				// Normal termination
				log.Printf("Connection closed normally")
				return nil
			}
			log.Printf("Error handling message: %v", err)
			return err
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
	c.executor = executor.New(db.Schema, db.Table)
	c.executor.SyncCatalog()

	return nil
}

// handleMessage processes a client message
func (c *Connection) handleMessage(msg *Message) error {
	switch msg.Type {
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
		return c.sendReadyForQuery()

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
	sqlUpper := strings.ToUpper(strings.TrimSpace(sql))
	singleStatement := isSingleStatementSQL(sql)

	// In a failed transaction only ROLLBACK (or ROLLBACK TO SAVEPOINT) is
	// accepted. Gate before honoring the special driver queries below so they
	// return 25P02 instead of a result. Multi-statement batches are gated
	// per statement in the execution loop below.
	if c.txStatus == TxStatusFailed && singleStatement && !isRollbackStatement(sql) {
		c.sendError("ERROR", ErrCodeTransactionAborted, "current transaction is aborted, commands ignored until end of transaction block")
		return c.sendReadyForQuery()
	}

	// lib/pq and other drivers query these for connection validation
	if singleStatement && strings.Contains(sqlUpper, "SELECT VERSION()") {
		// Return a fake PostgreSQL version
		return c.handleVersionQuery()
	}

	if singleStatement && strings.Contains(sqlUpper, "SELECT CURRENT_USER") {
		// Return the current user
		return c.handleCurrentUserQuery()
	}

	if singleStatement && strings.Contains(sqlUpper, "SHOW") && (strings.Contains(sqlUpper, "SERVER_VERSION") ||
		strings.Contains(sqlUpper, "SERVER_ENCODING") ||
		strings.Contains(sqlUpper, "CLIENT_ENCODING")) {
		// Handle SHOW commands
		return c.handleShowCommand(sqlUpper)
	}
	if result, handled, err := c.catalogResult(sql); handled {
		if err != nil {
			c.sendError("ERROR", ErrCodeInternalError, err.Error())
			return c.sendReadyForQuery()
		}
		if err := c.sendTabularResult(result, "SELECT"); err != nil {
			return err
		}
		return c.sendReadyForQuery()
	}

	// Parse the complete batch before executing its first statement. This keeps
	// unsupported trailing clauses from turning into committed writes.
	l := lexer.New(sql)
	p := parser.New(l)
	stmts, err := p.ParseMultiple()
	if err != nil {
		c.sendError("ERROR", ErrCodeSyntaxError, fmt.Sprintf("Syntax error: %v", err))
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
		if err != nil {
			code := ErrCodeInternalError
			if errors.Is(err, storage.ErrSerialization) {
				code = ErrCodeSerializationFailure
				c.txStatus = TxStatusIdle
			} else if c.txStatus == TxStatusInBlock {
				c.txStatus = TxStatusFailed
			}
			c.sendError("ERROR", code, fmt.Sprintf("Execution error: %v", err))
			return c.sendReadyForQuery()
		}
		if err := c.sendResult(result, stmt); err != nil {
			return err
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
	c.statements[name] = &preparedStatement{query: query, paramOIDs: oids}
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
	formats, pos, err := readInt16List(msg.Data, pos)
	if err != nil || pos+2 > len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind format list"))
	}
	paramCount := int(binary.BigEndian.Uint16(msg.Data[pos : pos+2]))
	pos += 2
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
	_, pos, err = readInt16List(msg.Data, pos) // result formats; text output is currently used
	if err != nil || pos != len(msg.Data) {
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Bind result format list"))
	}
	query, err := bindQuery(statement.query, params)
	if err != nil {
		return c.failExtended(ErrCodeInvalidParameter, err)
	}
	c.portals[portalName] = &portal{query: query}
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
		mb := NewMessageBuilder()
		mb.WriteInt16(int16(len(statement.paramOIDs)))
		for _, oid := range statement.paramOIDs {
			mb.WriteInt32(oid)
		}
		if err := c.writeMessage(MsgParameterDescription, mb.Bytes()); err != nil {
			return err
		}
	case 'P':
		if _, ok := c.portals[name]; !ok {
			return c.failExtended(ErrCodeInvalidParameter, fmt.Errorf("portal %q does not exist", name))
		}
	default:
		return c.failExtended(ErrCodeProtocolViolation, fmt.Errorf("invalid Describe target"))
	}
	// Execute sends the row description once the bound statement has been
	// analyzed, avoiding side effects during Describe.
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
		return c.failExtended(ErrCodeInvalidParameter, fmt.Errorf("portal %q does not exist", name))
	}
	if portal.executed {
		return c.failExtended(ErrCodeFeatureNotSupported, fmt.Errorf("portal can only be executed once"))
	}
	portal.executed = true
	// Check the transaction state before catalog emulation. Catalog queries do
	// not all parse as regular PizzaSQL statements, but must still return 25P02
	// while the transaction is aborted.
	if c.txStatus == TxStatusFailed && !isRollbackStatement(portal.query) {
		return c.failExtended(ErrCodeTransactionAborted, fmt.Errorf("current transaction is aborted, commands ignored until end of transaction block"))
	}
	if result, handled, catalogErr := c.catalogResult(portal.query); handled {
		if catalogErr != nil {
			return c.failExtended(ErrCodeInternalError, catalogErr)
		}
		return c.sendTabularResult(result, "SELECT")
	}
	l := lexer.New(portal.query)
	stmt, err := parser.New(l).Parse()
	if err != nil {
		return c.failExtended(ErrCodeSyntaxError, fmt.Errorf("syntax error: %w", err))
	}
	result, err := c.executor.Execute(stmt)
	if err != nil {
		code := ErrCodeInternalError
		if errors.Is(err, storage.ErrSerialization) {
			code = ErrCodeSerializationFailure
			c.txStatus = TxStatusIdle
		} else if c.txStatus == TxStatusInBlock {
			c.txStatus = TxStatusFailed
		}
		return c.failExtended(code, fmt.Errorf("execution error: %w", err))
	}
	return c.sendResult(result, stmt)
}

var catalogFilterPattern = regexp.MustCompile(`(?i)\b(table_name|tablename|table_schema|schemaname|constraint_name|indexname)\s*=\s*'((?:''|[^'])*)'`)

func (c *Connection) catalogResult(sql string) (*executor.Result, bool, error) {
	if !isSingleStatementSQL(sql) {
		return nil, false, nil
	}
	upper := strings.ToUpper(sql)
	var source string
	for _, candidate := range []string{
		"INFORMATION_SCHEMA.TABLES", "INFORMATION_SCHEMA.COLUMNS",
		"INFORMATION_SCHEMA.TABLE_CONSTRAINTS", "INFORMATION_SCHEMA.KEY_COLUMN_USAGE",
		"PG_TABLES", "PG_INDEXES",
	} {
		if strings.Contains(upper, "FROM "+candidate) {
			source = candidate
			break
		}
	}
	if source == "" {
		return nil, false, nil
	}
	if c.txStatus == TxStatusIdle {
		c.schema.LockStatement()
		defer c.schema.UnlockStatement()
	}

	tables, err := c.schema.ListTables()
	if err != nil {
		return nil, true, err
	}
	rows := make([]map[string]interface{}, 0)
	switch source {
	case "INFORMATION_SCHEMA.TABLES":
		for _, table := range tables {
			rows = append(rows, map[string]interface{}{
				"table_catalog": c.database, "table_schema": "public", "table_name": table, "table_type": "BASE TABLE",
			})
		}
	case "PG_TABLES":
		for _, table := range tables {
			indexes, _ := c.schema.ListTableIndexes(table)
			rows = append(rows, map[string]interface{}{
				"schemaname": "public", "tablename": table, "tableowner": c.params["user"], "tablespace": nil,
				"hasindexes": len(indexes) > 0, "hasrules": false, "hastriggers": false, "rowsecurity": false,
			})
		}
	case "INFORMATION_SCHEMA.COLUMNS":
		for _, table := range tables {
			schema, schemaErr := c.schema.GetSchema(table)
			if schemaErr != nil {
				continue
			}
			for i, column := range schema.Columns {
				rows = append(rows, map[string]interface{}{
					"table_catalog": c.database, "table_schema": "public", "table_name": table,
					"column_name": column.Name, "ordinal_position": int64(i + 1), "column_default": column.Default,
					"is_nullable": yesNo(column.Nullable), "data_type": strings.ToLower(column.Type),
				})
			}
		}
	case "INFORMATION_SCHEMA.TABLE_CONSTRAINTS", "INFORMATION_SCHEMA.KEY_COLUMN_USAGE":
		for _, table := range tables {
			schema, schemaErr := c.schema.GetSchema(table)
			if schemaErr != nil || schema.PrimaryKey == "" || schema.PrimaryKey == "_rowid_" {
				continue
			}
			row := map[string]interface{}{
				"constraint_catalog": c.database, "constraint_schema": "public", "constraint_name": table + "_pkey",
				"table_catalog": c.database, "table_schema": "public", "table_name": table,
			}
			if source == "INFORMATION_SCHEMA.TABLE_CONSTRAINTS" {
				row["constraint_type"] = "PRIMARY KEY"
				row["is_deferrable"] = "NO"
				row["initially_deferred"] = "NO"
			} else {
				row["column_name"] = schema.PrimaryKey
				row["ordinal_position"] = int64(1)
			}
			rows = append(rows, row)
		}
	case "PG_INDEXES":
		for _, table := range tables {
			indexes, _ := c.schema.ListTableIndexes(table)
			for _, index := range indexes {
				columns := make([]string, len(index.Columns))
				for i, column := range index.Columns {
					columns[i] = column.Name
				}
				unique := ""
				if index.Unique {
					unique = "UNIQUE "
				}
				rows = append(rows, map[string]interface{}{
					"schemaname": "public", "tablename": table, "indexname": index.Name, "tablespace": nil,
					"indexdef": fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", unique, index.Name, table, strings.Join(columns, ", ")),
				})
			}
		}
	}

	for _, match := range catalogFilterPattern.FindAllStringSubmatch(sql, -1) {
		column, expected := strings.ToLower(match[1]), strings.ReplaceAll(match[2], "''", "'")
		filtered := rows[:0]
		for _, row := range rows {
			if value, ok := row[column]; ok && strings.EqualFold(fmt.Sprintf("%v", value), expected) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	columns := catalogProjection(sql, rows)
	result := executor.NewResult("SELECT")
	if len(columns) == 1 && columns[0] == "count(*)" {
		result.AddColumnWithType("count", "INTEGER")
		result.AddRow(int64(len(rows)))
		return result, true, nil
	}
	for _, column := range columns {
		columnType := "TEXT"
		if column == "ordinal_position" {
			columnType = "INTEGER"
		} else if strings.HasPrefix(column, "has") || column == "rowsecurity" {
			columnType = "BOOLEAN"
		}
		result.AddColumnWithType(column, columnType)
	}
	for _, row := range rows {
		values := make([]interface{}, len(columns))
		for i, column := range columns {
			values[i] = row[column]
		}
		result.AddRow(values...)
	}
	return result, true, nil
}

func isSingleStatementSQL(sql string) bool {
	trimmed := strings.TrimSpace(sql)
	if strings.HasSuffix(trimmed, ";") {
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
	}
	inString, inIdent := false, false
	for i := 0; i < len(trimmed); i++ {
		switch trimmed[i] {
		case '\'':
			if !inIdent {
				if inString && i+1 < len(trimmed) && trimmed[i+1] == '\'' {
					i++
					continue
				}
				inString = !inString
			}
		case '"':
			if !inString {
				inIdent = !inIdent
			}
		case ';':
			if !inString && !inIdent {
				return false
			}
		}
	}
	return true
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

func catalogProjection(sql string, rows []map[string]interface{}) []string {
	upper := strings.ToUpper(sql)
	selectPos, fromPos := strings.Index(upper, "SELECT"), strings.Index(upper, " FROM ")
	if selectPos < 0 || fromPos < 0 || fromPos <= selectPos+6 {
		return nil
	}
	projection := strings.TrimSpace(sql[selectPos+6 : fromPos])
	projection = strings.TrimSpace(strings.TrimPrefix(strings.ToUpper(projection), "DISTINCT "))
	if projection == "*" && len(rows) > 0 {
		columns := make([]string, 0, len(rows[0]))
		for column := range rows[0] {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		return columns
	}
	parts := strings.Split(projection, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.TrimSpace(part)
		if index := strings.Index(strings.ToUpper(column), " AS "); index >= 0 {
			column = strings.TrimSpace(column[:index])
		}
		if index := strings.LastIndex(column, "."); index >= 0 {
			column = column[index+1:]
		}
		columns = append(columns, strings.ToLower(strings.Trim(column, `"`)))
	}
	return columns
}

func yesNo(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}

func (c *Connection) sendTabularResult(result *executor.Result, tag string) error {
	if err := c.sendRowDescription(result.Columns, result.ColumnTypes); err != nil {
		return err
	}
	for _, row := range result.Rows {
		if err := c.sendDataRow(row, result.Columns); err != nil {
			return err
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

func (c *Connection) failExtended(code string, err error) error {
	c.extendedFailed = true
	return c.sendError("ERROR", code, err.Error())
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
	inString, inIdent := false, false
	for i := 0; i < len(query); {
		ch := query[i]
		if ch == '\'' && !inIdent {
			result.WriteByte(ch)
			if inString && i+1 < len(query) && query[i+1] == '\'' {
				result.WriteByte(query[i+1])
				i += 2
				continue
			}
			inString = !inString
			i++
			continue
		}
		if ch == '"' && !inString {
			inIdent = !inIdent
			result.WriteByte(ch)
			i++
			continue
		}
		if ch == '$' && !inString && !inIdent && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			end := i + 1
			for end < len(query) && query[end] >= '0' && query[end] <= '9' {
				end++
			}
			n, _ := strconv.Atoi(query[i+1 : end])
			if n < 1 || n > len(params) {
				return "", fmt.Errorf("parameter $%d was not provided", n)
			}
			literal, err := parameterLiteral(params[n-1])
			if err != nil {
				return "", fmt.Errorf("parameter $%d: %w", n, err)
			}
			result.WriteString(literal)
			i = end
			continue
		}
		result.WriteByte(ch)
		i++
	}
	return result.String(), nil
}

func parameterLiteral(param boundParameter) (string, error) {
	if param.null {
		return "NULL", nil
	}
	if param.format == 1 {
		switch param.oid {
		case 16:
			if len(param.value) != 1 {
				return "", fmt.Errorf("invalid binary boolean")
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
			return "", fmt.Errorf("binary format is unsupported for OID %d", param.oid)
		}
	}
	value := string(param.value)
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

// sendResult sends query results
func (c *Connection) sendResult(result *executor.Result, stmt parser.Statement) error {
	// Any statement that produced a row set (SELECT, or INSERT/UPDATE/DELETE
	// with RETURNING) sends a row description and its data rows. Other
	// statements only send the command completion tag.
	if len(result.Columns) > 0 {
		if err := c.sendRowDescription(result.Columns, wireColumnTypes(result)); err != nil {
			return err
		}
		for _, row := range result.Rows {
			if err := c.sendDataRow(row, result.Columns); err != nil {
				return err
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

// sendReadyForQuery sends ready for query message
func (c *Connection) sendReadyForQuery() error {
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

// sendRowDescription sends row description (column metadata)
func (c *Connection) sendRowDescription(columns []string, columnTypes []string) error {
	mb := NewMessageBuilder()
	mb.WriteInt16(int16(len(columns)))

	for i, col := range columns {
		colType := ""
		if i < len(columnTypes) {
			colType = columnTypes[i]
		}
		mb.WriteString(col)
		mb.WriteInt32(0)                             // table OID
		mb.WriteInt16(0)                             // column attribute number
		mb.WriteInt32(c.getOIDForType(colType))      // type OID
		mb.WriteInt16(c.getTypeSizeForType(colType)) // type size
		mb.WriteInt32(-1)                            // type modifier
		mb.WriteInt16(0)                             // format code (text)
	}

	return c.writeMessage(MsgRowDescription, mb.Bytes())
}

// sendDataRow sends a data row
func (c *Connection) sendDataRow(row []interface{}, columns []string) error {
	mb := NewMessageBuilder()
	mb.WriteInt16(int16(len(row)))

	for _, value := range row {
		if value == nil {
			mb.WriteInt32(-1) // NULL indicator
			continue
		}

		// Convert value to string
		strValue := c.valueToString(value)
		mb.WriteInt32(int32(len(strValue)))
		mb.WriteBytes([]byte(strValue))
	}

	return c.writeMessage(MsgDataRow, mb.Bytes())
}

// sendCommandComplete sends command complete message
func (c *Connection) sendCommandComplete(tag string) error {
	mb := NewMessageBuilder()
	mb.WriteString(tag)
	return c.writeMessage(MsgCommandComplete, mb.Bytes())
}

// sendError sends an error response
func (c *Connection) sendError(severity, code, message string) error {
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

// getOIDForType returns PostgreSQL OID for type
func (c *Connection) getOIDForType(typeName string) int32 {
	switch strings.ToUpper(typeName) {
	case "INTEGER", "INT":
		return 23 // INT4OID
	case "BIGINT":
		return 20 // INT8OID
	case "TEXT", "VARCHAR", "CHAR", "UUID":
		// UUID is stored as text (SQLite-style), so it reports TEXTOID rather
		// than the native PG UUID OID.
		return 25 // TEXTOID
	case "REAL", "FLOAT":
		return 700 // FLOAT4OID
	case "DOUBLE":
		return 701 // FLOAT8OID
	case "BOOLEAN", "BOOL":
		return 16 // BOOLOID
	case "BLOB":
		return 17 // BYTEAOID
	case "DATETIME", "TIMESTAMP":
		// Datetime values are exchanged as UTC RFC3339 (ISO8601 with a timezone
		// offset), which timestamptz decodes directly.
		return 1184 // TIMESTAMPTZOID
	default:
		return 25 // Default to TEXT
	}
}

// getTypeSizeForType returns type size
func (c *Connection) getTypeSizeForType(typeName string) int16 {
	switch strings.ToUpper(typeName) {
	case "INTEGER", "INT":
		return 4
	case "BIGINT":
		return 8
	case "REAL", "FLOAT":
		return 4
	case "DOUBLE":
		return 8
	case "BOOLEAN", "BOOL":
		return 1
	case "DATETIME", "TIMESTAMP":
		return 8
	default:
		return -1 // Variable length
	}
}

// valueToString converts a value to string
func (c *Connection) valueToString(value interface{}) string {
	if value == nil {
		return ""
	}
	// PostgreSQL's text format for bytea is \x followed by hex. Emitting that
	// lets libpq/pgx decode a BLOB column (OID 17) losslessly.
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

	row := []interface{}{"PostgreSQL 14.0 (PizzaSQL)"}
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

	if strings.Contains(sqlUpper, "SERVER_VERSION") {
		name = "server_version"
		value = "14.0"
	} else if strings.Contains(sqlUpper, "SERVER_ENCODING") {
		name = "server_encoding"
		value = "UTF8"
	} else if strings.Contains(sqlUpper, "CLIENT_ENCODING") {
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
