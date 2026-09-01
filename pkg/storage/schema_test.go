package storage

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type testScan struct {
	keys     []string
	offset   int
	limit    uint32
	keysOnly bool
}

type testKVServer struct {
	mu           sync.Mutex
	data         map[string][]byte
	lsns         map[string]uint64
	writes       map[string]int
	nextLSN      uint64
	maxScanPage  uint32
	scanOpens    int
	scanNexts    int
	scanCloses   int
	keyOnlyOpens int
	gets         int
	multiGets    int
	closers      []net.Conn
}

func newTestKVServer(t *testing.T) *testKVServer {
	t.Helper()

	return &testKVServer{
		data:   make(map[string][]byte),
		lsns:   make(map[string]uint64),
		writes: make(map[string]int),
	}
}

func newTestKVPool(kv *testKVServer, size int, timeout time.Duration) *KVPool {
	pool := &KVPool{
		pool:    make(chan *KVClient, size),
		size:    size,
		timeout: timeout,
	}
	for i := 0; i < size; i++ {
		pool.pool <- kv.client()
	}
	return pool
}

func (s *testKVServer) close() {
	s.mu.Lock()
	closers := append([]net.Conn(nil), s.closers...)
	s.mu.Unlock()

	for _, conn := range closers {
		_ = conn.Close()
	}
}

func (s *testKVServer) client() *KVClient {
	clientConn, serverConn := net.Pipe()

	s.mu.Lock()
	s.closers = append(s.closers, clientConn, serverConn)
	s.mu.Unlock()

	go s.handle(serverConn)
	return &KVClient{
		conn:     clientConn,
		reader:   bufio.NewReader(clientConn),
		writer:   bufio.NewWriter(clientConn),
		nextID:   1,
		lastUsed: time.Now(),
	}
}

func (s *testKVServer) writeCount(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	for key, writes := range s.writes {
		if strings.Contains(key, prefix) {
			count += writes
		}
	}
	return count
}

func (s *testKVServer) hasKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.data[key]
	return ok
}

func (s *testKVServer) countKeys(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	for key := range s.data {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

func (s *testKVServer) scanStats() (opens, nexts, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scanOpens, s.scanNexts, s.scanCloses
}

func (s *testKVServer) keyOnlyOpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyOnlyOpens
}

func (s *testKVServer) readStats() (gets, multiGets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, s.multiGets
}

func (s *testKVServer) handle(conn net.Conn) {
	defer conn.Close()

	r := bufio.NewReader(conn)
	scans := make(map[uint64]*testScan)
	var nextScan uint64 = 1
	for {
		opcode, _, requestID, payload, err := readFrame(r)
		if err != nil {
			return
		}
		body := s.execute(opcode, payload, scans, &nextScan)
		if _, err := conn.Write(encodeResponse(opcode, requestID, body)); err != nil {
			return
		}
	}
}

func (s *testKVServer) execute(opcode uint16, payload []byte, scans map[uint64]*testScan, nextScan *uint64) []byte {
	switch opcode {
	case opPing:
		body := make([]byte, 2+len(payload))
		putU16(body[0:2], statusOK)
		copy(body[2:], payload)
		return body
	case opGet:
		key, ok := parseOneKey(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		s.gets++
		value, found := s.data[string(key)]
		lsn := s.lsns[string(key)]
		s.mu.Unlock()
		if !found {
			body := make([]byte, 2)
			putU16(body[0:2], statusNotFound)
			return body
		}
		body := make([]byte, 14+len(value))
		putU16(body[0:2], statusOK)
		putU64(body[2:10], lsn)
		putU32(body[10:14], uint32(len(value)))
		copy(body[14:], value)
		return body
	case opPut:
		key, value, ok := parsePut(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		s.nextLSN++
		lsn := s.nextLSN
		s.data[string(key)] = append([]byte(nil), value...)
		s.lsns[string(key)] = lsn
		s.writes[string(key)]++
		s.mu.Unlock()
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		putU64(body[2:10], lsn)
		return body
	case opDelete:
		key, ok := parseOneKey(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		_, found := s.data[string(key)]
		delete(s.data, string(key))
		delete(s.lsns, string(key))
		s.mu.Unlock()
		body := make([]byte, 3)
		putU16(body[0:2], statusOK)
		if found {
			body[2] = 1
		}
		return body
	case opExists:
		key, ok := parseOneKey(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		_, found := s.data[string(key)]
		s.mu.Unlock()
		body := make([]byte, 3)
		putU16(body[0:2], statusOK)
		if found {
			body[2] = 1
		}
		return body
	case opMultiGet:
		keys, ok := parseMultiGetKeys(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		body := make([]byte, 6)
		putU16(body[0:2], statusOK)
		putU32(body[2:6], uint32(len(keys)))
		s.mu.Lock()
		s.multiGets++
		for _, key := range keys {
			value, found := s.data[string(key)]
			if !found {
				body = append(body, make([]byte, 16)...)
				continue
			}
			lsn := s.lsns[string(key)]
			entry := make([]byte, 16+len(value))
			entry[0] = 1
			putU32(entry[4:8], uint32(len(value)))
			putU64(entry[8:16], lsn)
			copy(entry[16:], value)
			body = append(body, entry...)
		}
		s.mu.Unlock()
		return body
	case opBatchWrite:
		ops, ok := parseBatchOps(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		s.nextLSN++
		lsn := s.nextLSN
		for _, op := range ops {
			if op.Op == batchPut {
				s.data[string(op.Key)] = append([]byte(nil), op.Value...)
				s.lsns[string(op.Key)] = lsn
				s.writes[string(op.Key)]++
			} else {
				delete(s.data, string(op.Key))
				delete(s.lsns, string(op.Key))
			}
		}
		s.mu.Unlock()
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		putU64(body[2:10], lsn)
		return body
	case opScanOpen:
		includeValues, limit, prefix, ok := parseScanOpen(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		s.scanOpens++
		if !includeValues {
			s.keyOnlyOpens++
		}
		s.mu.Unlock()
		s.mu.Lock()
		keys := make([]string, 0)
		for key := range s.data {
			if strings.HasPrefix(key, string(prefix)) {
				keys = append(keys, key)
			}
		}
		s.mu.Unlock()
		sort.Strings(keys)
		if s.maxScanPage > 0 && limit > s.maxScanPage {
			limit = s.maxScanPage
		}
		id := *nextScan
		*nextScan = id + 1
		scans[id] = &testScan{keys: keys, limit: limit, keysOnly: !includeValues}
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		putU64(body[2:10], id)
		return body
	case opScanNext:
		id, ok := parseScanID(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		scan := scans[id]
		if scan == nil {
			return errorBody("ScanNotFound")
		}
		s.mu.Lock()
		s.scanNexts++
		s.mu.Unlock()
		remaining := len(scan.keys) - scan.offset
		count := int(scan.limit)
		if count > remaining {
			count = remaining
		}
		end := scan.offset + count
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		if end >= len(scan.keys) {
			body[2] = 1
		}
		putU32(body[6:10], uint32(count))
		s.mu.Lock()
		for _, key := range scan.keys[scan.offset:end] {
			value := s.data[key]
			if scan.keysOnly {
				value = nil
			}
			lsn := s.lsns[key]
			entry := make([]byte, 16+len(key)+len(value))
			putU32(entry[0:4], uint32(len(key)))
			putU32(entry[4:8], uint32(len(value)))
			putU64(entry[8:16], lsn)
			copy(entry[16:], key)
			copy(entry[16+len(key):], value)
			body = append(body, entry...)
		}
		s.mu.Unlock()
		scan.offset = end
		return body
	case opScanClose:
		id, ok := parseScanID(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		s.scanCloses++
		s.mu.Unlock()
		delete(scans, id)
		body := make([]byte, 3)
		putU16(body[0:2], statusOK)
		body[2] = 1
		return body
	default:
		return errorBody("UnknownOpcode")
	}
}

func parsePut(payload []byte) ([]byte, []byte, bool) {
	if len(payload) < 8 {
		return nil, nil, false
	}
	keyLen := getU32(payload[0:4])
	valueLen := getU32(payload[4:8])
	if keyLen > maxKeySize || valueLen > maxValueSize {
		return nil, nil, false
	}
	if uint64(8)+uint64(keyLen)+uint64(valueLen) != uint64(len(payload)) {
		return nil, nil, false
	}
	return payload[8 : 8+keyLen], payload[8+keyLen:], true
}

func parseMultiGetKeys(payload []byte) ([][]byte, bool) {
	if len(payload) < 4 {
		return nil, false
	}
	count := getU32(payload[0:4])
	if count > maxOperations {
		return nil, false
	}
	keys := make([][]byte, 0, count)
	pos := 4
	for i := uint32(0); i < count; i++ {
		if len(payload)-pos < 4 {
			return nil, false
		}
		length := getU32(payload[pos : pos+4])
		pos += 4
		if length > maxKeySize || len(payload)-pos < int(length) {
			return nil, false
		}
		keys = append(keys, payload[pos:pos+int(length)])
		pos += int(length)
	}
	return keys, pos == len(payload)
}

func parseBatchOps(payload []byte) ([]BatchOp, bool) {
	if len(payload) < 8 {
		return nil, false
	}
	count := getU32(payload[0:4])
	metadataLen := getU32(payload[4:8])
	if count == 0 || count > maxOperations || uint64(metadataLen) > uint64(len(payload)-8) {
		return nil, false
	}
	pos := 8 + int(metadataLen)
	ops := make([]BatchOp, 0, count)
	for i := uint32(0); i < count; i++ {
		if len(payload)-pos < 12 {
			return nil, false
		}
		opcode := payload[pos]
		keyLen := getU32(payload[pos+4 : pos+8])
		valueLen := getU32(payload[pos+8 : pos+12])
		pos += 12
		if opcode != batchPut && opcode != batchDelete {
			return nil, false
		}
		if keyLen > maxKeySize || valueLen > maxValueSize {
			return nil, false
		}
		if opcode == batchDelete && valueLen != 0 {
			return nil, false
		}
		if len(payload)-pos < int(keyLen)+int(valueLen) {
			return nil, false
		}
		key := payload[pos : pos+int(keyLen)]
		pos += int(keyLen)
		value := payload[pos : pos+int(valueLen)]
		pos += int(valueLen)
		ops = append(ops, BatchOp{Op: opcode, Key: key, Value: value})
	}
	return ops, pos == len(payload)
}

func parseScanOpen(payload []byte) (bool, uint32, []byte, bool) {
	if len(payload) < 12 {
		return false, 0, nil, false
	}
	includeValues := payload[0] != 0
	limit := getU32(payload[4:8])
	prefixLen := getU32(payload[8:12])
	if limit == 0 || limit > 4096 || prefixLen > maxKeySize {
		return false, 0, nil, false
	}
	if uint64(12)+uint64(prefixLen) != uint64(len(payload)) {
		return false, 0, nil, false
	}
	return includeValues, limit, payload[12:], true
}

func parseScanID(payload []byte) (uint64, bool) {
	if len(payload) < 8 {
		return 0, false
	}
	return getU64(payload[0:8]), true
}

func TestInsertDoesNotRewriteSchemaForRowIDUpdates(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "name", Type: "TEXT", Nullable: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	initialSchemaWrites := kv.writeCount(":_schema:")
	if initialSchemaWrites != 1 {
		t.Fatalf("expected create table to write schema once, got %d", initialSchemaWrites)
	}

	for i := int64(1); i <= 3; i++ {
		err := tables.Insert("users", Row{"id": i, "name": fmt.Sprintf("user-%d", i)})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if got := kv.writeCount(":_schema:"); got != initialSchemaWrites {
		t.Fatalf("expected inserts not to rewrite schema, got %d schema writes", got)
	}
	if got := kv.writeCount(":_sys:rowid:"); got != 0 {
		t.Fatalf("expected no rowid counter writes for inserts, got %d", got)
	}
}

func TestRowIDIsDerivedFromRowsAfterRestart(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "events",
		Columns: []Column{
			{Name: "name", Type: "TEXT", Nullable: true},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	for i := 1; i <= 2; i++ {
		err := tables.Insert("events", Row{"name": fmt.Sprintf("event-%d", i)})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Simulate a process restart: new managers have empty in-memory ROWID state
	// but the same durable KV rows.
	restartedSchemas := NewSchemaManager(pool, "testdb")
	restartedTables := NewTableManager(pool, restartedSchemas, "testdb")
	if err := restartedTables.Insert("events", Row{"name": "event-3"}); err != nil {
		t.Fatalf("insert after restart: %v", err)
	}

	if !kv.hasKey("testdb:_data:events:3") {
		t.Fatalf("expected restart insert to continue at rowid 3")
	}
	if got := kv.writeCount(":_sys:rowid:"); got != 0 {
		t.Fatalf("expected no rowid counter writes, got %d", got)
	}
}

func TestInsertDoesNotWriteDurableIndexEntries(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "status", Type: "TEXT", Nullable: false},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	err = schemas.CreateIndex(&Index{
		Name:  "idx_users_status",
		Table: "users",
		Columns: []IndexColumn{
			{Name: "status"},
		},
	})
	if err != nil {
		t.Fatalf("create index: %v", err)
	}

	for i := int64(1); i <= 3; i++ {
		status := "active"
		if i == 2 {
			status = "inactive"
		}
		err := tables.Insert("users", Row{"id": i, "status": status})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	if got := kv.writeCount(":idx:"); got != 0 {
		t.Fatalf("expected no durable index entry writes, got %d", got)
	}

	rows, err := tables.SelectByIndex("users", "idx_users_status", "active")
	if err != nil {
		t.Fatalf("select by index: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 active rows from derived index, got %d", len(rows))
	}
}

func TestIndexIsDerivedFromRowsAfterRestart(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()

	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	tables := NewTableManager(pool, schemas, "testdb")

	err := schemas.CreateTable(&Schema{
		Name: "users",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", Nullable: false, PrimaryKey: true},
			{Name: "status", Type: "TEXT", Nullable: false},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	err = schemas.CreateIndex(&Index{
		Name:  "idx_users_status",
		Table: "users",
		Columns: []IndexColumn{
			{Name: "status"},
		},
	})
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	for i := int64(1); i <= 3; i++ {
		status := "active"
		if i == 3 {
			status = "inactive"
		}
		if err := tables.Insert("users", Row{"id": i, "status": status}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	restartedSchemas := NewSchemaManager(pool, "testdb")
	restartedTables := NewTableManager(pool, restartedSchemas, "testdb")

	rows, err := restartedTables.SelectByIndex("users", "idx_users_status", "active")
	if err != nil {
		t.Fatalf("select by index after restart: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 active rows from restart-derived index, got %d", len(rows))
	}
	if got := kv.writeCount(":idx:"); got != 0 {
		t.Fatalf("expected no durable index entry writes, got %d", got)
	}
}

func TestListTableIndexesCachesMetadata(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	pool := newTestKVPool(kv, 2, 5*time.Second)
	defer pool.Close()

	schemas := NewSchemaManager(pool, "testdb")
	if err := schemas.CreateTable(&Schema{
		Name: "items",
		Columns: []Column{
			{Name: "id", Type: "INTEGER", PrimaryKey: true},
			{Name: "kind", Type: "TEXT"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := schemas.CreateIndex(&Index{
		Name: "idx_items_kind", Table: "items", Columns: []IndexColumn{{Name: "kind"}},
	}); err != nil {
		t.Fatal(err)
	}

	restarted := NewSchemaManager(pool, "testdb")
	getsBefore, _ := kv.readStats()
	if indexes, err := restarted.ListTableIndexes("items"); err != nil || len(indexes) != 1 {
		t.Fatalf("first list: indexes=%v err=%v", indexes, err)
	}
	getsAfterFirst, _ := kv.readStats()
	if getsAfterFirst <= getsBefore {
		t.Fatal("first index metadata lookup did not read durable metadata")
	}
	for i := 0; i < 10; i++ {
		if indexes, err := restarted.ListTableIndexes("items"); err != nil || len(indexes) != 1 {
			t.Fatalf("cached list %d: indexes=%v err=%v", i, indexes, err)
		}
	}
	getsAfterCached, _ := kv.readStats()
	if getsAfterCached != getsAfterFirst {
		t.Fatalf("cached index metadata issued %d extra reads", getsAfterCached-getsAfterFirst)
	}
}
