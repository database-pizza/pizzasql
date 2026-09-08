package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func getResponseBody(value []byte, lsn uint64) []byte {
	body := make([]byte, 14+len(value))
	putU16(body[0:2], statusOK)
	putU64(body[2:10], lsn)
	putU32(body[10:14], uint32(len(value)))
	copy(body[14:], value)
	return body
}

func pipeClient(conn net.Conn) *KVClient {
	return &KVClient{
		conn:     conn,
		reader:   bufio.NewReader(conn),
		writer:   bufio.NewWriter(conn),
		nextID:   1,
		lastUsed: time.Now(),
	}
}

func TestCRC32C(t *testing.T) {
	if got := crc32c([]byte("123456789")); got != 0xe3069283 {
		t.Fatalf("crc32c = %#x, want 0xe3069283", got)
	}
}

func TestEncodeReadFrameRoundTrip(t *testing.T) {
	payload := []byte{0x00, 0x01, 0xfe, '\n', '\r'}
	frame := encodeFrame(opPut, 0, 42, payload)
	r := bufio.NewReader(bytes.NewReader(frame))
	opcode, flags, requestID, body, err := readFrame(r)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if opcode != opPut || flags != 0 || requestID != 42 {
		t.Fatalf("opcode=%d flags=%d requestID=%d", opcode, flags, requestID)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body = %x, want %x", body, payload)
	}
}

func TestReadFrameRejectsOversizedFrame(t *testing.T) {
	var header [headerSize]byte
	copy(header[0:4], headerMagic)
	putU16(header[4:6], headerVersion)
	putU32(header[20:24], maxFrameSize+1)
	r := bufio.NewReader(strings.NewReader(string(header[:])))
	if _, _, _, _, err := readFrame(r); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestClientRejectsOversizedRequests(t *testing.T) {
	c := &KVClient{}
	largeKey := make([]byte, maxKeySize+1)
	largeValue := make([]byte, maxValueSize+1)

	if _, err := c.Put(largeKey, nil); err == nil {
		t.Fatal("Put accepted an oversized key")
	}
	if _, err := c.Put(nil, largeValue); err == nil {
		t.Fatal("Put accepted an oversized value")
	}
	if _, err := c.Get(largeKey); err == nil {
		t.Fatal("Get accepted an oversized key")
	}
	if _, err := c.MultiGet([][]byte{largeKey}); err == nil {
		t.Fatal("MultiGet accepted an oversized key")
	}
	if _, err := c.BatchWrite([]BatchOp{{Op: batchPut, Key: []byte("k"), Value: largeValue}}, nil); err == nil {
		t.Fatal("BatchWrite accepted an oversized value")
	}
	if _, err := c.Scan(largeKey); err == nil {
		t.Fatal("Scan accepted an oversized prefix")
	}
	if _, err := c.ScanWithLimit(nil, 0); err == nil {
		t.Fatal("ScanWithLimit accepted a zero page size")
	}
	if _, err := c.ScanWithLimit(nil, 4097); err == nil {
		t.Fatal("ScanWithLimit accepted an oversized page size")
	}
}

func TestPutGetBinaryRoundTrip(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	key := []byte{0x00, 0x01, 0x02, 'k', '\n'}
	value := []byte{0xff, 0x00, '\r', '\n', 'v', 0x80}
	lsn, err := c.Put(key, value)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if lsn == 0 {
		t.Fatalf("put returned zero lsn")
	}
	res, err := c.Get(key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !res.Found {
		t.Fatalf("expected found")
	}
	if res.LSN != lsn {
		t.Fatalf("lsn = %d, want %d", res.LSN, lsn)
	}
	if !bytes.Equal(res.Value, value) {
		t.Fatalf("value = %x, want %x", res.Value, value)
	}
}

func TestGetNotFound(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	if _, err := c.Get([]byte("missing")); err != ErrKeyNotFound {
		t.Fatalf("err = %v, want ErrKeyNotFound", err)
	}
	if _, err := c.Read("missing"); err != ErrKeyNotFound {
		t.Fatalf("read err = %v, want ErrKeyNotFound", err)
	}
}

func TestDeleteAndExists(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	if _, err := c.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}
	found, err := c.Exists([]byte("k"))
	if err != nil || !found {
		t.Fatalf("exists = %v, %v", found, err)
	}
	deleted, err := c.Del([]byte("k"))
	if err != nil || !deleted {
		t.Fatalf("del = %v, %v", deleted, err)
	}
	found, err = c.Exists([]byte("k"))
	if err != nil || found {
		t.Fatalf("exists after delete = %v, %v", found, err)
	}
	deleted, err = c.Del([]byte("k"))
	if err != nil || deleted {
		t.Fatalf("del missing = %v, %v", deleted, err)
	}
}

func TestExistsManyPipelinesRequests(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			kv.handle(conn)
		}
	}()
	c, err := NewKVClient(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	keys := make([][]byte, 300)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("key:%03d", i))
		if i%2 == 0 {
			if _, err := c.Put(keys[i], []byte("v")); err != nil {
				t.Fatal(err)
			}
		}
	}
	exists, err := c.ExistsMany(keys)
	if err != nil {
		t.Fatal(err)
	}
	for i, found := range exists {
		if found != (i%2 == 0) {
			t.Fatalf("exists[%d]=%v", i, found)
		}
	}
}

func TestMultiGet(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	if _, err := c.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("put a: %v", err)
	}
	if _, err := c.Put([]byte("b"), []byte{0x00, 0x02}); err != nil {
		t.Fatalf("put b: %v", err)
	}
	results, err := c.MultiGet([][]byte{[]byte("a"), []byte("missing"), []byte("b")})
	if err != nil {
		t.Fatalf("multi_get: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len = %d", len(results))
	}
	if !results[0].Found || string(results[0].Value) != "1" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if results[1].Found {
		t.Fatalf("results[1] should be missing")
	}
	if !results[2].Found || !bytes.Equal(results[2].Value, []byte{0x00, 0x02}) {
		t.Fatalf("results[2] = %+v", results[2])
	}
}

func TestBatchWrite(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	if _, err := c.Put([]byte("d"), []byte("old")); err != nil {
		t.Fatalf("put d: %v", err)
	}
	lsn, err := c.BatchWrite([]BatchOp{
		{Op: batchPut, Key: []byte("a"), Value: []byte("1")},
		{Op: batchPut, Key: []byte("b"), Value: []byte("2")},
		{Op: batchDelete, Key: []byte("d")},
	}, []byte("meta"))
	if err != nil {
		t.Fatalf("batch_write: %v", err)
	}
	if lsn == 0 {
		t.Fatalf("zero lsn")
	}
	ra, err := c.Get([]byte("a"))
	if err != nil || !ra.Found || string(ra.Value) != "1" {
		t.Fatalf("a = %+v, %v", ra, err)
	}
	rb, err := c.Get([]byte("b"))
	if err != nil || !rb.Found || string(rb.Value) != "2" {
		t.Fatalf("b = %+v, %v", rb, err)
	}
	rd, err := c.Get([]byte("d"))
	if err != ErrKeyNotFound {
		t.Fatalf("d err = %v, want ErrKeyNotFound", err)
	}
	if rd.Found {
		t.Fatalf("d should be deleted")
	}
}

func TestScanPagination(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	kv.maxScanPage = 2
	c := kv.client()
	defer c.Close()

	expected := make(map[string]string)
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("pre:%02d", i)
		value := fmt.Sprintf("v%d", i)
		if _, err := c.Put([]byte(key), []byte(value)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		expected[key] = value
	}
	scan, err := c.Scan([]byte("pre:"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer scan.Close()

	var entries []KVEntry
	pages := 0
	for {
		batch, done, err := scan.Next()
		if err != nil {
			t.Fatalf("scan next: %v", err)
		}
		pages++
		entries = append(entries, batch...)
		if done {
			break
		}
	}
	if pages < 3 {
		t.Fatalf("expected pagination across multiple pages, got %d", pages)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(entries))
	}
	for _, e := range entries {
		if want := expected[string(e.Key)]; string(e.Value) != want {
			t.Fatalf("key %q value = %q, want %q", e.Key, e.Value, want)
		}
		if e.LSN == 0 {
			t.Fatalf("key %q has zero lsn", e.Key)
		}
	}
}

func TestScanBinaryValues(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	key := []byte{0x00, 0x01, 'p'}
	value := []byte{0xff, 0x00, '\n', '\r'}
	if _, err := c.Put(key, value); err != nil {
		t.Fatalf("put: %v", err)
	}
	scan, err := c.Scan([]byte{0x00})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer scan.Close()
	batch, done, err := scan.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !done || len(batch) != 1 {
		t.Fatalf("done=%v len=%d", done, len(batch))
	}
	if !bytes.Equal(batch[0].Key, key) {
		t.Fatalf("key = %x, want %x", batch[0].Key, key)
	}
	if !bytes.Equal(batch[0].Value, value) {
		t.Fatalf("value = %x, want %x", batch[0].Value, value)
	}
}

func TestReadsNoDelimiterAssumptions(t *testing.T) {
	kv := newTestKVServer(t)
	defer kv.close()
	c := kv.client()
	defer c.Close()

	values := []string{"a\nb", "c\r\nd", "", "e\rf\ng"}
	for i, v := range values {
		if _, err := c.Put([]byte(fmt.Sprintf("p:%d", i)), []byte(v)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	got, err := c.Reads("p:")
	if err != nil {
		t.Fatalf("reads: %v", err)
	}
	if len(got) != len(values) {
		t.Fatalf("reads returned %d values, want %d", len(got), len(values))
	}
	for i, v := range values {
		if got[i] != v {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestHeaderCRCError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		resp := encodeResponse(opGet, requestID, getResponseBody([]byte("v"), 1))
		resp[6] ^= 0xff
		serverConn.Write(resp)
	}()

	if _, err := c.Read("k"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestPayloadCRCError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		resp := encodeResponse(opGet, requestID, getResponseBody([]byte("v"), 1))
		resp[headerSize] ^= 0xff
		serverConn.Write(resp)
	}()

	if _, err := c.Read("k"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestServerErrorStatus(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		serverConn.Write(encodeResponse(opGet, requestID, errorBody("Boom")))
	}()

	_, err := c.Read("k")
	if err == nil {
		t.Fatalf("expected error")
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("server error should not be ErrProtocol: %v", err)
	}
	if !strings.Contains(err.Error(), "Boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestIDMismatch(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		serverConn.Write(encodeResponse(opGet, requestID+1, getResponseBody([]byte("v"), 1)))
	}()

	if _, err := c.Read("k"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestOpcodeMismatch(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		serverConn.Write(encodeResponse(opPut, requestID, getResponseBody([]byte("v"), 1)))
	}()

	if _, err := c.Read("k"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

func TestPartialReads(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	go func() {
		r := bufio.NewReader(serverConn)
		_, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		resp := encodeResponse(opGet, requestID, getResponseBody([]byte("hello"), 7))
		for _, b := range resp {
			if _, err := serverConn.Write([]byte{b}); err != nil {
				return
			}
		}
	}()

	res, err := c.Get([]byte("k"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !res.Found || string(res.Value) != "hello" || res.LSN != 7 {
		t.Fatalf("res = %+v", res)
	}
}

func TestMonotonicRequestIDs(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := pipeClient(clientConn)

	ids := make(chan uint64, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		for i := 0; i < 4; i++ {
			_, _, requestID, _, err := readFrame(r)
			if err != nil {
				return
			}
			ids <- requestID
			serverConn.Write(encodeResponse(opPing, requestID, []byte{0, 0}))
		}
	}()

	for i := 0; i < 4; i++ {
		if _, _, err := c.request(opPing, nil); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
	}
	var prev uint64
	for i := 0; i < 4; i++ {
		id := <-ids
		if i > 0 && id <= prev {
			t.Fatalf("request id not monotonic: %d then %d", prev, id)
		}
		prev = id
	}
}

func TestPoolReconnectsAfterBrokenConnection(t *testing.T) {
	s := newTestKVServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn
			go s.handle(conn)
		}
	}()

	pool, err := NewKVPool(ln.Addr().String(), 1, 2*time.Second)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	if err := pool.WithClient(func(c *KVClient) error { return c.Write("k", "v") }); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := pool.WithClient(func(c *KVClient) error {
		v, err := c.Read("k")
		if err != nil {
			return err
		}
		if v != "v" {
			return fmt.Errorf("read = %q", v)
		}
		return nil
	}); err != nil {
		t.Fatalf("first read: %v", err)
	}

	first := <-accepted
	first.Close()

	if err := pool.WithClient(func(c *KVClient) error { return c.Write("k", "v") }); err == nil {
		t.Fatalf("expected write on broken connection to fail")
	}

	if err := pool.WithClient(func(c *KVClient) error {
		v, err := c.Read("k")
		if err != nil {
			return err
		}
		if v != "v" {
			return fmt.Errorf("read = %q", v)
		}
		return nil
	}); err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}

	client, err := pool.Get()
	if err != nil {
		t.Fatal(err)
	}
	client.lastUsed = time.Now().Add(-31 * time.Second)
	pool.Put(client)
	second := <-accepted
	second.Close()
	if err := pool.WithClient(func(c *KVClient) error {
		v, err := c.Read("k")
		if err != nil {
			return err
		}
		if v != "v" {
			return fmt.Errorf("read = %q", v)
		}
		return nil
	}); err != nil {
		t.Fatalf("stale idle connection was not replaced before use: %v", err)
	}
}

func compareBatchResponseBody(committed bool, lsn uint64) []byte {
	body := make([]byte, 18)
	putU16(body[0:2], statusOK)
	if committed {
		body[2] = 1
	}
	putU64(body[10:18], lsn)
	return body
}

func parseComparePayload(payload []byte) ([]CompareCheck, []BatchOp, bool) {
	if len(payload) < 16 {
		return nil, nil, false
	}
	checkCount := getU32(payload[0:4])
	opCount := getU32(payload[4:8])
	metadataLen := getU32(payload[8:12])
	pos := 16
	checks := make([]CompareCheck, 0, checkCount)
	for i := uint32(0); i < checkCount; i++ {
		if len(payload)-pos < 16 {
			return nil, nil, false
		}
		keyLen := getU32(payload[pos : pos+4])
		lsn := getU64(payload[pos+8 : pos+16])
		pos += 16
		if keyLen > maxKeySize || len(payload)-pos < int(keyLen) {
			return nil, nil, false
		}
		checks = append(checks, CompareCheck{Key: payload[pos : pos+int(keyLen)], LSN: lsn})
		pos += int(keyLen)
	}
	if len(payload)-pos < int(metadataLen) {
		return nil, nil, false
	}
	pos += int(metadataLen)
	ops := make([]BatchOp, 0, opCount)
	for i := uint32(0); i < opCount; i++ {
		if len(payload)-pos < 12 {
			return nil, nil, false
		}
		op := payload[pos]
		keyLen := getU32(payload[pos+4 : pos+8])
		valueLen := getU32(payload[pos+8 : pos+12])
		pos += 12
		if keyLen > maxKeySize || valueLen > maxValueSize || len(payload)-pos < int(keyLen)+int(valueLen) {
			return nil, nil, false
		}
		ops = append(ops, BatchOp{
			Op:    op,
			Key:   payload[pos : pos+int(keyLen)],
			Value: payload[pos+int(keyLen) : pos+int(keyLen)+int(valueLen)],
		})
		pos += int(keyLen) + int(valueLen)
	}
	if pos != len(payload) {
		return nil, nil, false
	}
	return checks, ops, true
}

func TestCompareBatchWriteClientSemantics(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	c := pipeClient(clientConn)

	state := make(map[string]uint64)
	var nextLSN uint64

	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		for i := 0; i < 3; i++ {
			opcode, _, requestID, payload, err := readFrame(r)
			if err != nil {
				return
			}
			if opcode != opCompareBatch {
				serverConn.Write(encodeResponse(opcode, requestID, errorBody("UnknownOpcode")))
				continue
			}
			checks, ops, ok := parseComparePayload(payload)
			if !ok {
				serverConn.Write(encodeResponse(opcode, requestID, errorBody("InvalidPayload")))
				continue
			}
			committed := true
			for _, check := range checks {
				current, found := state[string(check.Key)]
				if check.LSN == 0 {
					if found {
						committed = false
					}
				} else if !found || current != check.LSN {
					committed = false
				}
			}
			responseLSN := uint64(0)
			if committed {
				nextLSN++
				responseLSN = nextLSN
				for _, op := range ops {
					if op.Op == batchPut {
						state[string(op.Key)] = nextLSN
					} else {
						delete(state, string(op.Key))
					}
				}
			}
			serverConn.Write(encodeResponse(opcode, requestID, compareBatchResponseBody(committed, responseLSN)))
		}
	}()

	lsn, committed, err := c.CompareBatchWrite(
		[]CompareCheck{{Key: []byte("k"), LSN: 0}},
		[]BatchOp{{Op: batchPut, Key: []byte("k"), Value: []byte("v")}},
		[]byte("meta"),
	)
	if err != nil {
		t.Fatalf("absent check: %v", err)
	}
	if !committed || lsn != 1 {
		t.Fatalf("absent check committed=%v lsn=%d, want committed lsn=1", committed, lsn)
	}

	lsn, committed, err = c.CompareBatchWrite(
		[]CompareCheck{{Key: []byte("k"), LSN: 1}},
		[]BatchOp{{Op: batchPut, Key: []byte("k"), Value: []byte("v2")}},
		nil,
	)
	if err != nil {
		t.Fatalf("matching lsn: %v", err)
	}
	if !committed || lsn != 2 {
		t.Fatalf("matching lsn committed=%v lsn=%d, want committed lsn=2", committed, lsn)
	}

	lsn, committed, err = c.CompareBatchWrite(
		[]CompareCheck{{Key: []byte("k"), LSN: 1}},
		[]BatchOp{{Op: batchPut, Key: []byte("k"), Value: []byte("v3")}},
		nil,
	)
	if err != nil {
		t.Fatalf("stale lsn: %v", err)
	}
	if committed || lsn != 0 {
		t.Fatalf("stale lsn committed=%v lsn=%d, want conflict committed=false lsn=0", committed, lsn)
	}
}

func TestCompareBatchWriteWireFormat(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	c := pipeClient(clientConn)

	var captured []byte
	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		opcode, _, requestID, payload, err := readFrame(r)
		if err != nil {
			return
		}
		captured = payload
		serverConn.Write(encodeResponse(opcode, requestID, compareBatchResponseBody(true, 7)))
	}()

	checks := []CompareCheck{
		{Key: []byte("a"), LSN: 0},
		{Key: []byte("bb"), LSN: 42},
	}
	ops := []BatchOp{
		{Op: batchPut, Key: []byte("x"), Value: []byte("yy")},
		{Op: batchDelete, Key: []byte("z")},
	}
	if _, _, err := c.CompareBatchWrite(checks, ops, []byte("m")); err != nil {
		t.Fatalf("compare batch: %v", err)
	}

	if !bytes.Equal(captured[0:4], []byte{2, 0, 0, 0}) {
		t.Fatalf("check count = %v", captured[0:4])
	}
	if !bytes.Equal(captured[4:8], []byte{2, 0, 0, 0}) {
		t.Fatalf("op count = %v", captured[4:8])
	}
	if !bytes.Equal(captured[8:12], []byte{1, 0, 0, 0}) {
		t.Fatalf("metadata len = %v", captured[8:12])
	}
	if !bytes.Equal(captured[12:16], []byte{0, 0, 0, 0}) {
		t.Fatalf("reserved = %v", captured[12:16])
	}
	pos := 16
	if !bytes.Equal(captured[pos:pos+4], []byte{1, 0, 0, 0}) {
		t.Fatalf("check0 key len = %v", captured[pos:pos+4])
	}
	if getU64(captured[pos+8:pos+16]) != 0 {
		t.Fatalf("check0 lsn = %d", getU64(captured[pos+8:pos+16]))
	}
	if !bytes.Equal(captured[pos+16:pos+17], []byte("a")) {
		t.Fatalf("check0 key = %q", captured[pos+16:pos+17])
	}
	pos += 17
	if !bytes.Equal(captured[pos:pos+4], []byte{2, 0, 0, 0}) {
		t.Fatalf("check1 key len = %v", captured[pos:pos+4])
	}
	if getU64(captured[pos+8:pos+16]) != 42 {
		t.Fatalf("check1 lsn = %d", getU64(captured[pos+8:pos+16]))
	}
	if !bytes.Equal(captured[pos+16:pos+18], []byte("bb")) {
		t.Fatalf("check1 key = %q", captured[pos+16:pos+18])
	}
	pos += 18
	if !bytes.Equal(captured[pos:pos+1], []byte("m")) {
		t.Fatalf("metadata = %q", captured[pos:pos+1])
	}
	pos++
	if captured[pos] != batchPut {
		t.Fatalf("op0 opcode = %d", captured[pos])
	}
	if !bytes.Equal(captured[pos+4:pos+8], []byte{1, 0, 0, 0}) {
		t.Fatalf("op0 key len = %v", captured[pos+4:pos+8])
	}
	if !bytes.Equal(captured[pos+8:pos+12], []byte{2, 0, 0, 0}) {
		t.Fatalf("op0 value len = %v", captured[pos+8:pos+12])
	}
	pos += 12
	if !bytes.Equal(captured[pos:pos+3], []byte("xyy")) {
		t.Fatalf("op0 body = %q", captured[pos:pos+3])
	}
	pos += 3
	if captured[pos] != batchDelete {
		t.Fatalf("op1 opcode = %d", captured[pos])
	}
	if !bytes.Equal(captured[pos+4:pos+8], []byte{1, 0, 0, 0}) {
		t.Fatalf("op1 key len = %v", captured[pos+4:pos+8])
	}
	if !bytes.Equal(captured[pos+8:pos+12], []byte{0, 0, 0, 0}) {
		t.Fatalf("op1 value len = %v", captured[pos+8:pos+12])
	}
	pos += 12
	if !bytes.Equal(captured[pos:pos+1], []byte("z")) {
		t.Fatalf("op1 key = %q", captured[pos:pos+1])
	}
	pos++
	if pos != len(captured) {
		t.Fatalf("payload trailing bytes: got len %d want %d", len(captured), pos)
	}
}

func TestCompareBatchWriteRejectsInvalidInput(t *testing.T) {
	c := &KVClient{}
	largeKey := make([]byte, maxKeySize+1)
	if _, _, err := c.CompareBatchWrite(nil, nil, nil); err == nil {
		t.Fatal("accepted empty ops")
	}
	if _, _, err := c.CompareBatchWrite([]CompareCheck{{Key: largeKey}}, []BatchOp{{Op: batchPut, Key: []byte("k")}}, nil); err == nil {
		t.Fatal("accepted oversized check key")
	}
	if _, _, err := c.CompareBatchWrite(nil, []BatchOp{{Op: 99, Key: []byte("k")}}, nil); err == nil {
		t.Fatal("accepted invalid batch opcode")
	}
	if _, _, err := c.CompareBatchWrite(nil, []BatchOp{{Op: batchDelete, Key: []byte("k"), Value: []byte("v")}}, nil); err == nil {
		t.Fatal("accepted delete with value")
	}
}

func TestCompareBatchWriteMalformedResponse(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	c := pipeClient(clientConn)

	go func() {
		defer serverConn.Close()
		r := bufio.NewReader(serverConn)
		opcode, _, requestID, _, err := readFrame(r)
		if err != nil {
			return
		}
		serverConn.Write(encodeResponse(opcode, requestID, []byte{0, 0, 1}))
	}()

	if _, _, err := c.CompareBatchWrite(
		[]CompareCheck{{Key: []byte("k"), LSN: 0}},
		[]BatchOp{{Op: batchPut, Key: []byte("k"), Value: []byte("v")}},
		nil,
	); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}
