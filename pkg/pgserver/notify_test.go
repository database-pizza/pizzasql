package pgserver

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/testkv"
)

func newNotifyDatabase(t *testing.T) (*storage.DatabaseManager, *storage.DatabaseInstance) {
	t.Helper()
	kv := testkv.New(t)
	pool := kv.Pool(8)
	t.Cleanup(func() { pool.Close() })
	dm := storage.NewDatabaseManager(pool, &storage.DatabaseManagerConfig{
		DefaultDatabase: "pg_notify",
		AutoCreate:      true,
	})
	db, err := dm.GetDatabase("pg_notify")
	if err != nil {
		t.Fatal(err)
	}
	return dm, db
}

func queryMessage(sql string) []byte {
	data := append([]byte(sql), 0)
	var msg bytes.Buffer
	msg.WriteByte(MsgQuery)
	_ = binary.Write(&msg, binary.BigEndian, uint32(len(data)+4))
	msg.Write(data)
	return msg.Bytes()
}

func writeAndReadUntilReady(t *testing.T, client net.Conn, r *bufio.Reader, payload []byte) []*Message {
	t.Helper()
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	var msgs []*Message
	for {
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		msgs = append(msgs, m)
		if m.Type == MsgReadyForQuery {
			return msgs
		}
	}
}

func startNotifyConnection(t *testing.T, dm *storage.DatabaseManager) (net.Conn, *bufio.Reader) {
	t.Helper()
	server, client := net.Pipe()
	c := NewConnection(server, dm, true)
	done := make(chan error, 1)
	go func() { done <- c.Handle() }()
	t.Cleanup(func() {
		client.Close()
		<-done
	})

	if _, err := client.Write(startupMessage(map[string]string{"user": "tester", "database": "pg_notify"})); err != nil {
		t.Fatalf("startup: %v", err)
	}
	r := bufio.NewReader(client)
	for {
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("read startup: %v", err)
		}
		if m.Type == MsgReadyForQuery {
			return client, r
		}
	}
}

func publishFromNewSession(t *testing.T, db *storage.DatabaseInstance, channel, payload string) {
	t.Helper()
	e := executor.New(db.Schema, db.Table)
	e.SetBackendPID(4242)
	e.SetNotificationHub(db.Hub)
	t.Cleanup(func() { _ = e.Close() })
	l := lexer.New(fmt.Sprintf("NOTIFY %s, '%s'", channel, payload))
	stmt, err := parser.New(l).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(stmt); err != nil {
		t.Fatal(err)
	}
}

func TestIdleListenerReceivesNotification(t *testing.T) {
	dm, db := newNotifyDatabase(t)
	client, r := startNotifyConnection(t, dm)

	writeAndReadUntilReady(t, client, r, queryMessage("LISTEN updates"))
	publishFromNewSession(t, db, "updates", "hello")

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, err := ReadMessage(r)
	if err != nil {
		t.Fatalf("read notification: %v", err)
	}
	if m.Type != MsgNotificationResponse {
		t.Fatalf("message type = %c, want NotificationResponse", m.Type)
	}
	if len(m.Data) < 6 {
		t.Fatalf("short notification frame: %v", m.Data)
	}
	pid := int32(binary.BigEndian.Uint32(m.Data[:4]))
	if pid != 4242 {
		t.Fatalf("pid = %d, want 4242", pid)
	}
	pos := 4
	channel, pos, err := readCString(m.Data, pos)
	if err != nil || channel != "updates" {
		t.Fatalf("channel = %q err=%v", channel, err)
	}
	payload, _, err := readCString(m.Data, pos)
	if err != nil || payload != "hello" {
		t.Fatalf("payload = %q err=%v", payload, err)
	}
}

func TestExtendedProtocolDeliversNotificationAfterSync(t *testing.T) {
	dm, db := newNotifyDatabase(t)
	client, r := startNotifyConnection(t, dm)

	// Parse + Bind + Execute, then Flush. The connection remains in an
	// extended-protocol cycle, so a notification published now must not be
	// written until Sync.
	var buf bytes.Buffer
	parseBody := []byte("stmt\x00LISTEN ext\x00\x00\x00")
	buf.WriteByte(MsgParse)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(parseBody)+4))
	buf.Write(parseBody)

	bindBody := []byte("portal\x00stmt\x00\x00\x00\x00\x00\x00\x00")
	buf.WriteByte(MsgBind)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(bindBody)+4))
	buf.Write(bindBody)

	execBody := []byte("portal\x00\x00\x00\x00\x00")
	buf.WriteByte(MsgExecute)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(execBody)+4))
	buf.Write(execBody)

	buf.WriteByte(MsgFlush)
	_ = binary.Write(&buf, binary.BigEndian, uint32(4))
	if _, err := client.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}

	for {
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("read extended response: %v", err)
		}
		if m.Type == MsgCommandComplete {
			break
		}
		if m.Type == MsgErrorResponse {
			t.Fatalf("extended protocol error: %+v", m.Data)
		}
	}

	publishFromNewSession(t, db, "ext", "during-extended")

	// The notification is queued in the subscriber but must not be written
	// before Sync closes the cycle.
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if m, err := ReadMessage(r); err == nil {
		t.Fatalf("notification interleaved into extended cycle: type %c", m.Type)
	}

	var syncMsg bytes.Buffer
	syncMsg.WriteByte(MsgSync)
	_ = binary.Write(&syncMsg, binary.BigEndian, uint32(4))
	if _, err := client.Write(syncMsg.Bytes()); err != nil {
		t.Fatal(err)
	}

	var sawNotification bool
	for {
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("read sync response: %v", err)
		}
		if m.Type == MsgNotificationResponse {
			sawNotification = true
		}
		if m.Type == MsgReadyForQuery {
			break
		}
	}
	if !sawNotification {
		// Notifications are emitted after ReadyForQuery, so the frame follows the
		// Ready message that closed the extended cycle.
		_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
		m, err := ReadMessage(r)
		if err != nil {
			t.Fatalf("notification was not delivered after Sync: %v", err)
		}
		if m.Type != MsgNotificationResponse {
			t.Fatalf("post-Sync message type = %c, want NotificationResponse", m.Type)
		}
	}
}

func TestCommitLSNParameterStatus(t *testing.T) {
	dm, _ := newNotifyDatabase(t)
	client, r := startNotifyConnection(t, dm)

	writeAndReadUntilReady(t, client, r, queryMessage("CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"))
	writeAndReadUntilReady(t, client, r, queryMessage("BEGIN"))
	writeAndReadUntilReady(t, client, r, queryMessage("INSERT INTO t VALUES (1, 'a')"))
	msgs := writeAndReadUntilReady(t, client, r, queryMessage("COMMIT"))

	found := false
	for _, m := range msgs {
		if m.Type != MsgParameterStatus {
			continue
		}
		name, pos, err := readCString(m.Data, 0)
		if err != nil || name != "pizzasql.lsn" {
			continue
		}
		value, _, err := readCString(m.Data, pos)
		if err != nil || value == "" || value == "0" {
			t.Fatalf("invalid lsn value %q", value)
		}
		found = true
	}
	if !found {
		t.Fatal("COMMIT did not expose pizzasql.lsn")
	}
}

func TestNotificationFrameEncoding(t *testing.T) {
	c, _ := newTestConnection(t)
	var out bytes.Buffer
	c.writer = bufio.NewWriter(&out)
	if err := c.sendNotification(storage.Notification{PID: 7, Channel: "ch", Payload: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := c.writer.Flush(); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMessage(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgNotificationResponse {
		t.Fatalf("type = %c", m.Type)
	}
	channel, pos, _ := readCString(m.Data, 4)
	payload, _, _ := readCString(m.Data, pos)
	if channel != "ch" || payload != "p" {
		t.Fatalf("decoded %q %q", channel, payload)
	}
}

func TestHandleStartupDrainsNotificationsOnDisconnect(t *testing.T) {
	dm, db := newNotifyDatabase(t)
	client, r := startNotifyConnection(t, dm)
	writeAndReadUntilReady(t, client, r, queryMessage("LISTEN cleanup"))
	if len(db.Hub.Channels()) != 1 {
		t.Fatalf("channels = %v", db.Hub.Channels())
	}
	// Terminate and give Handle time to run its cleanup defer.
	if _, err := client.Write([]byte{MsgTerminate, 0, 0, 0, 4}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(db.Hub.Channels()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscription not cleaned up: %v", db.Hub.Channels())
}
