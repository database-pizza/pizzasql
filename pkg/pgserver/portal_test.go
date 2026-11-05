package pgserver

import (
	"bufio"
	"bytes"
	"strconv"
	"testing"
)

// Bounded Execute is only reachable over the raw protocol. pgx v5.11.0 sends
// maxRows = 0 for every Execute (pgconn.ExecParams, ExecPrepared, and the
// Pipeline equivalents have no row-limit argument), and database/sql drives pgx,
// so neither a pgx nor a database/sql test can request a suspended portal. These
// tests therefore drive the protocol messages directly, one round trip per
// Execute.

// portalSession sends extended-protocol messages to a connection backed by a
// real executor and returns the frames the client would receive for that round
// trip.
type portalSession struct {
	t    *testing.T
	c    *Connection
	conn *countingConn
}

func newPortalSession(t *testing.T) *portalSession {
	t.Helper()
	c, _ := newDataConnection(t)
	conn := &countingConn{}
	c.conn = conn
	c.writer = bufio.NewWriter(conn)
	return &portalSession{t: t, c: c, conn: conn}
}

// send handles the messages, flushes buffered output the way a client's Flush
// message would, and returns the frames produced since the previous send.
func (s *portalSession) send(messages ...*Message) []*Message {
	s.t.Helper()
	for _, message := range messages {
		if err := s.c.handleMessage(message); err != nil {
			s.t.Fatalf("handle %c: %v", message.Type, err)
		}
	}
	if err := s.c.handleMessage(&Message{Type: MsgFlush}); err != nil {
		s.t.Fatalf("flush: %v", err)
	}
	frames := readAllMessages(s.t, s.conn.bytes())
	s.conn.reset()
	return frames
}

// setup runs SQL outside the protocol; the statements are the test fixture, not
// the behavior under test.
func (s *portalSession) setup(statements ...string) {
	s.t.Helper()
	for _, sql := range statements {
		if _, err := s.c.executor.Execute(parseStmt(s.t, sql)); err != nil {
			s.t.Fatalf("%s: %v", sql, err)
		}
	}
}

// countRows reports how many rows a table holds, so a test can prove a DML
// portal ran once.
func (s *portalSession) countRows(table string) int64 {
	s.t.Helper()
	result, err := s.c.executor.Execute(parseStmt(s.t, "SELECT COUNT(*) FROM "+table))
	if err != nil {
		s.t.Fatalf("count %s: %v", table, err)
	}
	return result.Rows[0][0].(int64)
}

func executeRowsMessage(portal string, maxRows int32) *Message {
	mb := NewMessageBuilder()
	mb.WriteString(portal)
	mb.WriteInt32(maxRows)
	return &Message{Type: MsgExecute, Data: mb.Bytes()}
}

func closePortalMessage(portal string) *Message {
	return &Message{Type: MsgClose, Data: append([]byte("P"+portal), 0)}
}

func queryMessageData(sql string) *Message {
	return &Message{Type: MsgQuery, Data: append([]byte(sql), 0)}
}

func syncMessage() *Message {
	return &Message{Type: MsgSync}
}

func commandTagOf(message *Message) string {
	if message == nil || message.Type != MsgCommandComplete {
		return ""
	}
	if end := bytes.IndexByte(message.Data, 0); end >= 0 {
		return string(message.Data[:end])
	}
	return string(message.Data)
}

func assertFrameTypes(t *testing.T, frames []*Message, want string) {
	t.Helper()
	if got := messageTypes(frames); got != want {
		t.Fatalf("frames = %q, want %q", got, want)
	}
}

func assertRowIDs(t *testing.T, frames []*Message, want ...string) {
	t.Helper()
	rows := dataRowsRaw(t, frames)
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, id := range want {
		if len(rows[i]) != 1 || string(rows[i][0]) != id {
			t.Fatalf("row %d = %q, want %q", i, rows[i], id)
		}
	}
}

func readyStatus(t *testing.T, frames []*Message) byte {
	t.Helper()
	for _, message := range frames {
		if message.Type == MsgReadyForQuery {
			if len(message.Data) != 1 {
				t.Fatalf("ReadyForQuery payload = %q", message.Data)
			}
			return message.Data[0]
		}
	}
	t.Fatalf("no ReadyForQuery in %q", messageTypes(frames))
	return 0
}

const portalSelectSQL = "SELECT id FROM t ORDER BY id"

func createNumberedTable(t *testing.T, s *portalSession, rows int) {
	t.Helper()
	s.setup("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	for i := 1; i <= rows; i++ {
		s.setup("INSERT INTO t (id) VALUES (" + strconv.Itoa(i) + ")")
	}
}

// A bounded Execute returns at most maxRows rows and suspends; each later
// Execute resumes where the previous one stopped. The RowDescription is sent by
// the first Execute only, and Describe('P') still answers NoData without
// disturbing that sequence.
func TestBoundedExecuteSuspendsAndResumes(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 5)

	frames := s.send(
		parseMessage("stmt", portalSelectSQL),
		bindResultMessage("p", "stmt", nil, nil),
		&Message{Type: MsgDescribe, Data: []byte("Pp\x00")},
	)
	assertFrameTypes(t, frames, "12n")

	first := s.send(executeRowsMessage("p", 2))
	assertFrameTypes(t, first, "TDDs")
	assertRowIDs(t, first, "1", "2")

	second := s.send(executeRowsMessage("p", 2))
	assertFrameTypes(t, second, "DDs")
	assertRowIDs(t, second, "3", "4")

	third := s.send(executeRowsMessage("p", 2))
	assertFrameTypes(t, third, "DC")
	assertRowIDs(t, third, "5")
	if tag := commandTagOf(third[len(third)-1]); tag != "SELECT 5" {
		t.Fatalf("completion tag = %q, want %q", tag, "SELECT 5")
	}

	// A completed portal repeats its tag without re-running the statement.
	fourth := s.send(executeRowsMessage("p", 0))
	assertFrameTypes(t, fourth, "C")
	if tag := commandTagOf(fourth[0]); tag != "SELECT 5" {
		t.Fatalf("repeated completion tag = %q, want %q", tag, "SELECT 5")
	}
	// Completion drops the row data, so an exhausted portal only keeps the
	// metadata a repeated Execute answers with.
	if rows := s.c.portals["p"].execution.result.Rows; len(rows) != 0 {
		t.Fatalf("completed portal still holds %d rows", len(rows))
	}
}

// maxRows = 0 keeps the pre-existing meaning: every remaining row in one
// Execute followed by CommandComplete.
func TestExecuteUnboundedStreamsAllRows(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))

	frames := s.send(executeRowsMessage("p", 0))
	assertFrameTypes(t, frames, "TDDDC")
	assertRowIDs(t, frames, "1", "2", "3")
	if tag := commandTagOf(frames[len(frames)-1]); tag != "SELECT 3" {
		t.Fatalf("completion tag = %q, want %q", tag, "SELECT 3")
	}
}

// A limit that falls exactly on the last row still suspends, as PostgreSQL
// does; the following Execute reports completion with no further rows.
func TestBoundedExecuteExactLimitSuspends(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))

	frames := s.send(executeRowsMessage("p", 3))
	assertFrameTypes(t, frames, "TDDDs")
	assertRowIDs(t, frames, "1", "2", "3")

	frames = s.send(executeRowsMessage("p", 0))
	assertFrameTypes(t, frames, "C")
	if tag := commandTagOf(frames[0]); tag != "SELECT 3" {
		t.Fatalf("completion tag = %q, want %q", tag, "SELECT 3")
	}
}

// Close('P') destroys a suspended portal, and a later Execute reports 34000
// until Sync recovers the cycle.
func TestCloseEndsSuspendedPortal(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))
	assertFrameTypes(t, s.send(executeRowsMessage("p", 1)), "TDs")

	assertFrameTypes(t, s.send(closePortalMessage("p")), "3")

	frames := s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	assertFrameTypes(t, s.send(syncMessage()), "Z")
	if s.c.extendedFailed {
		t.Fatal("Sync did not clear the extended-failure state")
	}
}

// The unnamed portal is replaced by every Bind, which restarts its materialized
// result, row cursor, and row metadata.
func TestUnnamedRebindRestartsPortal(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 5)
	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("", "stmt", nil, nil))
	assertFrameTypes(t, s.send(executeRowsMessage("", 2)), "TDDs")

	frames := s.send(bindResultMessage("", "stmt", nil, nil), executeRowsMessage("", 0))
	assertFrameTypes(t, frames, "2TDDDDDC")
	assertRowIDs(t, frames, "1", "2", "3", "4", "5")
}

// A named portal cannot be replaced by another Bind: PostgreSQL reports 42P03,
// and the rejected Bind fails the cycle, so an explicit transaction is aborted
// and the portal goes with the ROLLBACK.
func TestNamedRebindIsRejected(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	if status := readyStatus(t, s.send(queryMessageData("BEGIN"))); status != TxStatusInBlock {
		t.Fatalf("BEGIN status = %c, want %c", status, TxStatusInBlock)
	}
	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))
	assertFrameTypes(t, s.send(executeRowsMessage("p", 1)), "TDs")

	frames := s.send(bindResultMessage("p", "stmt", nil, nil))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeDuplicateCursor {
		t.Fatalf("SQLSTATE = %q, want %q", code, ErrCodeDuplicateCursor)
	}
	if status := readyStatus(t, s.send(syncMessage())); status != TxStatusFailed {
		t.Fatalf("status after rejected re-Bind = %c, want %c", status, TxStatusFailed)
	}

	if status := readyStatus(t, s.send(queryMessageData("ROLLBACK"))); status != TxStatusIdle {
		t.Fatalf("ROLLBACK status = %c, want %c", status, TxStatusIdle)
	}
	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE after ROLLBACK = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	s.send(syncMessage())
}

// Inside an explicit transaction a portal survives Sync and keeps resuming;
// COMMIT and ROLLBACK end the transaction and with it every portal.
func TestPortalLivesUntilTransactionEnd(t *testing.T) {
	for _, ending := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(ending, func(t *testing.T) {
			s := newPortalSession(t)
			createNumberedTable(t, s, 3)

			if status := readyStatus(t, s.send(queryMessageData("BEGIN"))); status != TxStatusInBlock {
				t.Fatalf("BEGIN status = %c, want %c", status, TxStatusInBlock)
			}
			frames := s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))
			assertFrameTypes(t, frames, "12")

			frames = s.send(executeRowsMessage("p", 2), syncMessage())
			assertFrameTypes(t, frames, "TDDsZ")
			if status := readyStatus(t, frames); status != TxStatusInBlock {
				t.Fatalf("Sync inside the transaction reported status %c, want %c", status, TxStatusInBlock)
			}

			frames = s.send(executeRowsMessage("p", 0), syncMessage())
			assertFrameTypes(t, frames, "DCZ")
			assertRowIDs(t, frames, "3")

			if status := readyStatus(t, s.send(queryMessageData(ending))); status != TxStatusIdle {
				t.Fatalf("%s status = %c, want %c", ending, status, TxStatusIdle)
			}
			frames = s.send(executeRowsMessage("p", 1))
			assertFrameTypes(t, frames, "E")
			if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
				t.Fatalf("SQLSTATE after %s = %q, want %q", ending, code, ErrCodeUndefinedCursor)
			}
			s.send(syncMessage())
		})
	}
}

// Outside an explicit block the portal belongs to the implicit transaction that
// Sync ends, so Execute afterwards reports 34000 (PostgreSQL behaves the same);
// the prepared statement survives and can be bound again.
func TestPortalEndsAtSyncOutsideTransaction(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	frames := s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))
	assertFrameTypes(t, frames, "12")

	frames = s.send(executeRowsMessage("p", 1), syncMessage())
	assertFrameTypes(t, frames, "TDsZ")

	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	s.send(syncMessage())

	frames = s.send(bindResultMessage("p", "stmt", nil, nil), executeRowsMessage("p", 0))
	assertFrameTypes(t, frames, "2TDDDC")
	assertRowIDs(t, frames, "1", "2", "3")
}

// A suspended INSERT ... RETURNING runs the statement exactly once: the resume
// streams the prefetched rows, and a completed portal never runs it again.
func TestBoundedExecuteDMLEvaluatesOnce(t *testing.T) {
	s := newPortalSession(t)
	s.setup("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	frames := s.send(
		parseMessage("stmt", "INSERT INTO t (id) VALUES (1), (2), (3) RETURNING id"),
		bindResultMessage("p", "stmt", nil, nil),
	)
	assertFrameTypes(t, frames, "12")

	frames = s.send(executeRowsMessage("p", 2))
	assertFrameTypes(t, frames, "TDDs")
	assertRowIDs(t, frames, "1", "2")

	frames = s.send(executeRowsMessage("p", 0))
	// The autocommit INSERT publishes its commit LSN with the completion.
	assertFrameTypes(t, frames, "DCS")
	assertRowIDs(t, frames, "3")
	if tag := commandTagOf(frames[len(frames)-2]); tag != "INSERT 0 3" {
		t.Fatalf("completion tag = %q, want %q", tag, "INSERT 0 3")
	}

	frames = s.send(executeRowsMessage("p", 0))
	assertFrameTypes(t, frames, "C")
	if rows := s.countRows("t"); rows != 3 {
		t.Fatalf("table has %d rows after three Executes, want 3", rows)
	}
}

// Per-portal result formats keep applying on every Execute of the same portal,
// and only the first Execute carries the RowDescription.
func TestPortalResumeKeepsBinaryResultFormats(t *testing.T) {
	s := newPortalSession(t)
	s.setup(
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO t VALUES (1, 'one')",
		"INSERT INTO t VALUES (2, 'two')",
	)
	frames := s.send(
		parseMessage("stmt", "SELECT id, name FROM t ORDER BY id"),
		bindResultMessage("p", "stmt", nil, []int16{1}),
	)
	assertFrameTypes(t, frames, "12")

	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "TDs")
	descriptions := 0
	for _, message := range frames {
		if message.Type == MsgRowDescription {
			descriptions++
			formats := rowDescriptionFormats(t, message.Data)
			if len(formats) != 2 || formats[0] != 1 || formats[1] != 1 {
				t.Fatalf("RowDescription formats = %v, want binary for both columns", formats)
			}
		}
	}
	if descriptions != 1 {
		t.Fatalf("got %d RowDescriptions, want 1", descriptions)
	}
	rows := dataRowsRaw(t, frames)
	assertBytes(t, "first id", rows[0][0], []byte{0, 0, 0, 1})

	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "Ds")
	rows = dataRowsRaw(t, frames)
	assertBytes(t, "second id", rows[0][0], []byte{0, 0, 0, 2})

	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "C")
	if tag := commandTagOf(frames[0]); tag != "SELECT 2" {
		t.Fatalf("completion tag = %q, want %q", tag, "SELECT 2")
	}
}

// Execute of a portal that was never bound is 34000, like PostgreSQL's
// "portal does not exist".
func TestExecuteUnknownPortalIsUndefinedCursor(t *testing.T) {
	s := newPortalSession(t)
	frames := s.send(executeRowsMessage("missing", 0))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	s.send(syncMessage())
}

// A failed Execute leaves the transaction aborted: a later Execute yields 25P02
// until ROLLBACK, which also destroys the portals.
func TestFailedExecuteLeavesTransactionConsistent(t *testing.T) {
	s := newPortalSession(t)
	s.setup("CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	if status := readyStatus(t, s.send(queryMessageData("BEGIN"))); status != TxStatusInBlock {
		t.Fatalf("BEGIN status = %c, want %c", status, TxStatusInBlock)
	}

	s.send(
		parseMessage("failing", "INSERT INTO t (id, name) VALUES (1, NULL)"),
		bindResultMessage("bad", "failing", nil, nil),
	)
	frames := s.send(executeRowsMessage("bad", 0))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != "23502" {
		t.Fatalf("SQLSTATE = %q, want 23502", code)
	}
	if status := readyStatus(t, s.send(syncMessage())); status != TxStatusFailed {
		t.Fatalf("status after failure = %c, want %c", status, TxStatusFailed)
	}

	s.send(
		parseMessage("reading", portalSelectSQL),
		bindResultMessage("ok", "reading", nil, nil),
	)
	frames = s.send(executeRowsMessage("ok", 0))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeTransactionAborted {
		t.Fatalf("SQLSTATE in the aborted transaction = %q, want %q", code, ErrCodeTransactionAborted)
	}
	if status := readyStatus(t, s.send(syncMessage())); status != TxStatusFailed {
		t.Fatalf("status after aborted Execute = %c, want %c", status, TxStatusFailed)
	}

	if status := readyStatus(t, s.send(queryMessageData("ROLLBACK"))); status != TxStatusIdle {
		t.Fatalf("ROLLBACK status = %c, want %c", status, TxStatusIdle)
	}
	frames = s.send(executeRowsMessage("ok", 0))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE after ROLLBACK = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	s.send(syncMessage())
}

// A ROLLBACK TO SAVEPOINT can undo writes a suspended portal already
// materialized, so it drops every portal. Resuming one would stream rows and a
// completion tag from a removed state; the client gets 34000 instead. Clearing
// on ROLLBACK TO SAVEPOINT is stricter than PostgreSQL, which keeps portals
// created before the savepoint.
func TestRollbackToSavepointClearsSuspendedPortals(t *testing.T) {
	s := newPortalSession(t)
	s.setup("CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if status := readyStatus(t, s.send(queryMessageData("BEGIN"))); status != TxStatusInBlock {
		t.Fatalf("BEGIN status = %c, want %c", status, TxStatusInBlock)
	}
	if status := readyStatus(t, s.send(queryMessageData("SAVEPOINT s"))); status != TxStatusInBlock {
		t.Fatalf("SAVEPOINT status = %c, want %c", status, TxStatusInBlock)
	}
	s.send(
		parseMessage("stmt", "INSERT INTO t (id) VALUES (1), (2) RETURNING id"),
		bindResultMessage("p", "stmt", nil, nil),
	)
	frames := s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "TDs")
	assertRowIDs(t, frames, "1")

	if status := readyStatus(t, s.send(queryMessageData("ROLLBACK TO SAVEPOINT s"))); status != TxStatusInBlock {
		t.Fatalf("ROLLBACK TO SAVEPOINT status = %c, want %c", status, TxStatusInBlock)
	}

	frames = s.send(executeRowsMessage("p", 1))
	assertFrameTypes(t, frames, "E")
	if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
		t.Fatalf("SQLSTATE = %q, want %q", code, ErrCodeUndefinedCursor)
	}
	s.send(syncMessage())

	if rows := s.countRows("t"); rows != 0 {
		t.Fatalf("table has %d rows after ROLLBACK TO SAVEPOINT, want 0", rows)
	}
	if status := readyStatus(t, s.send(queryMessageData("ROLLBACK"))); status != TxStatusIdle {
		t.Fatalf("ROLLBACK status = %c, want %c", status, TxStatusIdle)
	}
}

// Transaction commands executed through a portal emit their tag once and drop
// every open portal, so a suspended sibling can never replay rows or a
// completion tag from a transaction that ended or was rewound.
func TestTransactionCommandPortalClearsSiblings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    string
		wantTag    string
		wantStatus byte
	}{
		{name: "commit", command: "COMMIT", wantTag: "COMMIT", wantStatus: TxStatusIdle},
		{name: "rollback", command: "ROLLBACK", wantTag: "ROLLBACK", wantStatus: TxStatusIdle},
		{name: "rollback_to_savepoint", command: "ROLLBACK TO SAVEPOINT s", wantTag: "ROLLBACK", wantStatus: TxStatusInBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newPortalSession(t)
			createNumberedTable(t, s, 3)
			if status := readyStatus(t, s.send(queryMessageData("BEGIN"))); status != TxStatusInBlock {
				t.Fatalf("BEGIN status = %c, want %c", status, TxStatusInBlock)
			}
			if status := readyStatus(t, s.send(queryMessageData("SAVEPOINT s"))); status != TxStatusInBlock {
				t.Fatalf("SAVEPOINT status = %c, want %c", status, TxStatusInBlock)
			}
			s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("first", "stmt", nil, nil))
			assertFrameTypes(t, s.send(executeRowsMessage("first", 1)), "TDs")
			s.send(bindResultMessage("second", "stmt", nil, nil))
			assertFrameTypes(t, s.send(executeRowsMessage("second", 1)), "TDs")

			s.send(parseMessage("txn", tc.command), bindResultMessage("txn_portal", "txn", nil, nil))
			frames := s.send(executeRowsMessage("txn_portal", 0))
			assertFrameTypes(t, frames, "C")
			if tag := commandTagOf(frames[0]); tag != tc.wantTag {
				t.Fatalf("tag = %q, want %q", tag, tc.wantTag)
			}
			if status := readyStatus(t, s.send(syncMessage())); status != tc.wantStatus {
				t.Fatalf("status after %s = %c, want %c", tc.command, status, tc.wantStatus)
			}

			for _, name := range []string{"first", "second", "txn_portal"} {
				frames = s.send(executeRowsMessage(name, 1))
				assertFrameTypes(t, frames, "E")
				if code := errorCode(findError(frames)); code != ErrCodeUndefinedCursor {
					t.Fatalf("Execute %q after %s: SQLSTATE = %q, want %q", name, tc.command, code, ErrCodeUndefinedCursor)
				}
				s.send(syncMessage())
			}
		})
	}
}

// BEGIN through a portal starts the block and emits its tag. It does not end a
// transaction, so sibling portals are kept, and a repeated Execute repeats the
// stored tag without replaying BEGIN (the engine rejects a second BEGIN, so a
// replay would surface as an error).
func TestBeginPortalStartsTransaction(t *testing.T) {
	s := newPortalSession(t)
	createNumberedTable(t, s, 3)
	s.send(parseMessage("begin_stmt", "BEGIN"), bindResultMessage("begin_portal", "begin_stmt", nil, nil))
	frames := s.send(executeRowsMessage("begin_portal", 0))
	assertFrameTypes(t, frames, "C")
	if tag := commandTagOf(frames[0]); tag != "BEGIN" {
		t.Fatalf("tag = %q, want %q", tag, "BEGIN")
	}
	if status := readyStatus(t, s.send(syncMessage())); status != TxStatusInBlock {
		t.Fatalf("status after BEGIN = %c, want %c", status, TxStatusInBlock)
	}

	s.send(parseMessage("stmt", portalSelectSQL), bindResultMessage("p", "stmt", nil, nil))
	assertFrameTypes(t, s.send(executeRowsMessage("p", 1)), "TDs")

	frames = s.send(executeRowsMessage("begin_portal", 0))
	assertFrameTypes(t, frames, "C")
	if tag := commandTagOf(frames[0]); tag != "BEGIN" {
		t.Fatalf("repeated tag = %q, want %q", tag, "BEGIN")
	}

	frames = s.send(executeRowsMessage("p", 0))
	assertFrameTypes(t, frames, "DDC")
	assertRowIDs(t, frames, "2", "3")
	if status := readyStatus(t, s.send(queryMessageData("ROLLBACK"))); status != TxStatusIdle {
		t.Fatalf("ROLLBACK status = %c, want %c", status, TxStatusIdle)
	}
}
