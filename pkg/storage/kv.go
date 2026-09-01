package storage

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	headerSize    = 32
	headerMagic   = "PKBF"
	headerVersion = 1

	opPing       = 1
	opStatus     = 2
	opGet        = 3
	opPut        = 4
	opDelete     = 5
	opExists     = 6
	opMultiGet   = 7
	opBatchWrite = 8
	opScanOpen   = 9
	opScanNext   = 10
	opScanClose  = 11

	batchPut    = 1
	batchDelete = 2

	statusOK       = 0
	statusNotFound = 1
	statusError    = 2

	maxKeySize         = 1024 * 1024
	maxValueSize       = 64 * 1024 * 1024
	maxTransactionSize = 64 * 1024 * 1024
	maxOperations      = 65535
	maxFrameSize       = maxKeySize + maxValueSize + 1024

	scanPageSize       = 1024
	existsPipelineSize = 128
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

var (
	ErrKeyNotFound = errors.New("key not found")
	ErrProtocol    = errors.New("pkbfi protocol error")
)

func crc32c(p []byte) uint32 {
	return crc32.Checksum(p, crc32cTable)
}

func putU16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU64(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

func getU16(b []byte) uint16 {
	return uint16(b[0]) | uint16(b[1])<<8
}

func getU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func getU64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func encodeFrame(opcode, flags uint16, requestID uint64, payload []byte) []byte {
	frame := make([]byte, headerSize+len(payload))
	copy(frame[0:4], headerMagic)
	putU16(frame[4:6], headerVersion)
	putU16(frame[6:8], 0)
	putU16(frame[8:10], opcode)
	putU16(frame[10:12], flags)
	putU64(frame[12:20], requestID)
	putU32(frame[20:24], uint32(len(payload)))
	putU32(frame[24:28], crc32c(payload))
	putU32(frame[28:32], 0)
	putU32(frame[28:32], crc32c(frame[0:32]))
	copy(frame[32:], payload)
	return frame
}

func readFrame(r *bufio.Reader) (uint16, uint16, uint64, []byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	if string(header[0:4]) != headerMagic {
		return 0, 0, 0, nil, fmt.Errorf("%w: invalid magic", ErrProtocol)
	}
	if getU16(header[4:6]) != headerVersion {
		return 0, 0, 0, nil, fmt.Errorf("%w: incompatible version", ErrProtocol)
	}
	payloadLen := getU32(header[20:24])
	if payloadLen > maxFrameSize {
		return 0, 0, 0, nil, fmt.Errorf("%w: frame too large", ErrProtocol)
	}
	headerCRC := getU32(header[28:32])
	var headerCopy [headerSize]byte
	copy(headerCopy[:], header[:])
	putU32(headerCopy[28:32], 0)
	if crc32c(headerCopy[:]) != headerCRC {
		return 0, 0, 0, nil, fmt.Errorf("%w: header checksum mismatch", ErrProtocol)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, 0, nil, err
	}
	if crc32c(payload) != getU32(header[24:28]) {
		return 0, 0, 0, nil, fmt.Errorf("%w: payload checksum mismatch", ErrProtocol)
	}
	return getU16(header[8:10]), getU16(header[10:12]), getU64(header[12:20]), payload, nil
}

func encodeResponse(opcode uint16, requestID uint64, body []byte) []byte {
	return encodeFrame(opcode|0x8000, 1, requestID, body)
}

func oneKeyPayload(key []byte) []byte {
	payload := make([]byte, 4+len(key))
	putU32(payload[0:4], uint32(len(key)))
	copy(payload[4:], key)
	return payload
}

func validateKey(key []byte) error {
	if len(key) > maxKeySize {
		return fmt.Errorf("pkbfi: key exceeds %d bytes", maxKeySize)
	}
	return nil
}

func validateValue(value []byte) error {
	if len(value) > maxValueSize {
		return fmt.Errorf("pkbfi: value exceeds %d bytes", maxValueSize)
	}
	return nil
}

func parseOneKey(payload []byte) ([]byte, bool) {
	if len(payload) < 4 {
		return nil, false
	}
	length := getU32(payload[0:4])
	if length > maxKeySize || uint64(4)+uint64(length) != uint64(len(payload)) {
		return nil, false
	}
	return payload[4:], true
}

func errorBody(message string) []byte {
	body := make([]byte, 2+len(message))
	putU16(body[0:2], statusError)
	copy(body[2:], message)
	return body
}

type KVClient struct {
	conn           net.Conn
	reader         *bufio.Reader
	writer         *bufio.Writer
	mu             sync.Mutex
	nextID         uint64
	requestTimeout time.Duration
	lastUsed       time.Time
}

type KVResult struct {
	Value []byte
	LSN   uint64
	Found bool
}

type KVEntry struct {
	Key   []byte
	Value []byte
	LSN   uint64
}

type BatchOp struct {
	Op    byte
	Key   []byte
	Value []byte
}

type ScanCursor struct {
	client *KVClient
	id     uint64
}

func NewKVClient(addr string) (*KVClient, error) {
	network, target := parseAddr(addr)
	conn, err := net.Dial(network, target)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to PizzaKV: %w", err)
	}
	return &KVClient{
		conn:     conn,
		reader:   bufio.NewReader(conn),
		writer:   bufio.NewWriter(conn),
		nextID:   1,
		lastUsed: time.Now(),
	}, nil
}

func parseAddr(addr string) (string, string) {
	if strings.HasPrefix(addr, "unix:") {
		return "unix", strings.TrimPrefix(addr, "unix:")
	}
	return "tcp", addr
}

func (c *KVClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *KVClient) SetDeadline(t time.Time) error {
	if c.conn == nil {
		return nil
	}
	return c.conn.SetDeadline(t)
}

func (c *KVClient) writeFrame(opcode, flags uint16, requestID uint64, payload []byte) error {
	frame := encodeFrame(opcode, flags, requestID, payload)
	if _, err := c.writer.Write(frame); err != nil {
		return err
	}
	return c.writer.Flush()
}

func (c *KVClient) request(opcode uint16, payload []byte) (uint16, []byte, error) {
	if c.requestTimeout > 0 {
		if err := c.conn.SetDeadline(time.Now().Add(c.requestTimeout)); err != nil {
			return 0, nil, err
		}
	}
	requestID := c.nextID
	c.nextID++
	if err := c.writeFrame(opcode, 0, requestID, payload); err != nil {
		return 0, nil, err
	}
	respOpcode, respFlags, respID, body, err := readFrame(c.reader)
	if err != nil {
		return 0, nil, err
	}
	if respOpcode != opcode|0x8000 {
		return 0, nil, fmt.Errorf("%w: unexpected response opcode %d", ErrProtocol, respOpcode)
	}
	if respFlags != 1 {
		return 0, nil, fmt.Errorf("%w: unexpected response flags %d", ErrProtocol, respFlags)
	}
	if respID != requestID {
		return 0, nil, fmt.Errorf("%w: response id %d does not match request %d", ErrProtocol, respID, requestID)
	}
	c.lastUsed = time.Now()
	if len(body) < 2 {
		return 0, nil, fmt.Errorf("%w: response too short", ErrProtocol)
	}
	status := getU16(body[0:2])
	if status == statusError {
		return status, nil, fmt.Errorf("pkbfi server error: %s", body[2:])
	}
	return status, body[2:], nil
}

func (c *KVClient) Put(key, value []byte) (uint64, error) {
	if err := validateKey(key); err != nil {
		return 0, err
	}
	if err := validateValue(value); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := make([]byte, 8+len(key)+len(value))
	putU32(payload[0:4], uint32(len(key)))
	putU32(payload[4:8], uint32(len(value)))
	copy(payload[8:], key)
	copy(payload[8+len(key):], value)
	status, body, err := c.request(opPut, payload)
	if err != nil {
		return 0, err
	}
	if status != statusOK || len(body) != 8 {
		return 0, fmt.Errorf("%w: malformed put response", ErrProtocol)
	}
	return getU64(body[0:8]), nil
}

func (c *KVClient) Get(key []byte) (KVResult, error) {
	if err := validateKey(key); err != nil {
		return KVResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status, body, err := c.request(opGet, oneKeyPayload(key))
	if err != nil {
		return KVResult{}, err
	}
	if status == statusNotFound {
		return KVResult{}, ErrKeyNotFound
	}
	if status != statusOK || len(body) < 12 {
		return KVResult{}, fmt.Errorf("%w: malformed get response", ErrProtocol)
	}
	lsn := getU64(body[0:8])
	valueLen := getU32(body[8:12])
	if valueLen > maxValueSize || uint64(len(body)) != 12+uint64(valueLen) {
		return KVResult{}, fmt.Errorf("%w: malformed get value length", ErrProtocol)
	}
	return KVResult{Value: body[12:], LSN: lsn, Found: true}, nil
}

func (c *KVClient) Del(key []byte) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status, body, err := c.request(opDelete, oneKeyPayload(key))
	if err != nil {
		return false, err
	}
	if status != statusOK || len(body) != 1 {
		return false, fmt.Errorf("%w: malformed delete response", ErrProtocol)
	}
	return body[0] != 0, nil
}

func (c *KVClient) Exists(key []byte) (bool, error) {
	if err := validateKey(key); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	status, body, err := c.request(opExists, oneKeyPayload(key))
	if err != nil {
		return false, err
	}
	if status != statusOK || len(body) != 1 {
		return false, fmt.Errorf("%w: malformed exists response", ErrProtocol)
	}
	return body[0] != 0, nil
}

func (c *KVClient) ExistsMany(keys [][]byte) ([]bool, error) {
	if len(keys) > maxOperations {
		return nil, fmt.Errorf("pkbfi: too many keys")
	}
	for _, key := range keys {
		if err := validateKey(key); err != nil {
			return nil, err
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	results := make([]bool, len(keys))
	for start := 0; start < len(keys); start += existsPipelineSize {
		end := start + existsPipelineSize
		if end > len(keys) {
			end = len(keys)
		}
		ids := make([]uint64, end-start)
		if c.requestTimeout > 0 {
			if err := c.conn.SetDeadline(time.Now().Add(c.requestTimeout)); err != nil {
				return nil, err
			}
		}
		for i, key := range keys[start:end] {
			ids[i] = c.nextID
			c.nextID++
			if _, err := c.writer.Write(encodeFrame(opExists, 0, ids[i], oneKeyPayload(key))); err != nil {
				return nil, err
			}
		}
		if err := c.writer.Flush(); err != nil {
			return nil, err
		}
		for i, requestID := range ids {
			opcode, flags, responseID, body, err := readFrame(c.reader)
			if err != nil {
				return nil, err
			}
			if opcode != opExists|0x8000 || flags != 1 || responseID != requestID {
				return nil, fmt.Errorf("%w: malformed exists response frame", ErrProtocol)
			}
			if len(body) < 2 {
				return nil, fmt.Errorf("%w: response too short", ErrProtocol)
			}
			status := getU16(body[0:2])
			if status == statusError {
				return nil, fmt.Errorf("pkbfi server error: %s", body[2:])
			}
			if status != statusOK || len(body) != 3 {
				return nil, fmt.Errorf("%w: malformed exists response", ErrProtocol)
			}
			results[start+i] = body[2] != 0
			c.lastUsed = time.Now()
		}
	}
	return results, nil
}

func (c *KVClient) MultiGet(keys [][]byte) ([]KVResult, error) {
	if len(keys) > maxOperations {
		return nil, fmt.Errorf("pkbfi: too many keys")
	}
	payloadSize := 4
	for _, key := range keys {
		if err := validateKey(key); err != nil {
			return nil, err
		}
		payloadSize += 4 + len(key)
		if payloadSize > maxFrameSize {
			return nil, fmt.Errorf("pkbfi: multi_get request exceeds frame limit")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := make([]byte, 4, payloadSize)
	putU32(payload[0:4], uint32(len(keys)))
	for _, key := range keys {
		var length [4]byte
		putU32(length[:], uint32(len(key)))
		payload = append(payload, length[:]...)
		payload = append(payload, key...)
	}
	status, body, err := c.request(opMultiGet, payload)
	if err != nil {
		return nil, err
	}
	if status != statusOK || len(body) < 4 {
		return nil, fmt.Errorf("%w: malformed multi_get response", ErrProtocol)
	}
	count := getU32(body[0:4])
	if count != uint32(len(keys)) {
		return nil, fmt.Errorf("%w: multi_get count mismatch", ErrProtocol)
	}
	results := make([]KVResult, count)
	pos := 4
	for i := uint32(0); i < count; i++ {
		if len(body)-pos < 16 {
			return nil, fmt.Errorf("%w: truncated multi_get entry", ErrProtocol)
		}
		present := body[pos] != 0
		valueLen := getU32(body[pos+4 : pos+8])
		lsn := getU64(body[pos+8 : pos+16])
		pos += 16
		results[i] = KVResult{LSN: lsn, Found: present}
		if present {
			if valueLen > maxValueSize || len(body)-pos < int(valueLen) {
				return nil, fmt.Errorf("%w: multi_get value length", ErrProtocol)
			}
			results[i].Value = body[pos : pos+int(valueLen)]
			pos += int(valueLen)
		}
	}
	if pos != len(body) {
		return nil, fmt.Errorf("%w: multi_get trailing bytes", ErrProtocol)
	}
	return results, nil
}

func (c *KVClient) BatchWrite(ops []BatchOp, metadata []byte) (uint64, error) {
	if len(ops) == 0 || len(ops) > maxOperations {
		return 0, fmt.Errorf("pkbfi: invalid operation count")
	}
	if len(metadata) > maxTransactionSize-8 {
		return 0, fmt.Errorf("pkbfi: batch metadata exceeds transaction limit")
	}
	payloadSize := 8 + len(metadata)
	for _, op := range ops {
		if op.Op != batchPut && op.Op != batchDelete {
			return 0, fmt.Errorf("pkbfi: invalid batch opcode %d", op.Op)
		}
		if err := validateKey(op.Key); err != nil {
			return 0, err
		}
		if err := validateValue(op.Value); err != nil {
			return 0, err
		}
		if op.Op == batchDelete && len(op.Value) != 0 {
			return 0, fmt.Errorf("pkbfi: delete operation with value")
		}
		payloadSize += 12 + len(op.Key) + len(op.Value)
		if payloadSize > maxTransactionSize {
			return 0, fmt.Errorf("pkbfi: batch exceeds transaction limit")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := make([]byte, 8, payloadSize)
	putU32(payload[0:4], uint32(len(ops)))
	putU32(payload[4:8], uint32(len(metadata)))
	payload = append(payload, metadata...)
	for _, op := range ops {
		var header [12]byte
		header[0] = op.Op
		putU32(header[4:8], uint32(len(op.Key)))
		putU32(header[8:12], uint32(len(op.Value)))
		payload = append(payload, header[:]...)
		payload = append(payload, op.Key...)
		payload = append(payload, op.Value...)
	}
	status, body, err := c.request(opBatchWrite, payload)
	if err != nil {
		return 0, err
	}
	if status != statusOK || len(body) != 8 {
		return 0, fmt.Errorf("%w: malformed batch_write response", ErrProtocol)
	}
	return getU64(body[0:8]), nil
}

func (c *KVClient) Scan(prefix []byte) (*ScanCursor, error) {
	return c.openScan(prefix, true, scanPageSize)
}

func (c *KVClient) ScanWithLimit(prefix []byte, pageSize uint32) (*ScanCursor, error) {
	return c.openScan(prefix, true, pageSize)
}

// ScanKeys opens a key-only scan: the server omits values from the returned
// pages. It is used when only the key set (or its size) is needed, such as the
// COUNT(*) fast path or bulk deletion, so a full-table scan does not pull row
// values across the wire.
func (c *KVClient) ScanKeys(prefix []byte) (*ScanCursor, error) {
	return c.openScan(prefix, false, scanPageSize)
}

func (c *KVClient) openScan(prefix []byte, includeValues bool, pageSize uint32) (*ScanCursor, error) {
	if err := validateKey(prefix); err != nil {
		return nil, err
	}
	if pageSize == 0 || pageSize > 4096 {
		return nil, fmt.Errorf("pkbfi: scan page size must be between 1 and 4096")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := make([]byte, 12+len(prefix))
	if includeValues {
		payload[0] = 1
	}
	putU32(payload[4:8], pageSize)
	putU32(payload[8:12], uint32(len(prefix)))
	copy(payload[12:], prefix)
	status, body, err := c.request(opScanOpen, payload)
	if err != nil {
		return nil, err
	}
	if status != statusOK || len(body) != 8 {
		return nil, fmt.Errorf("%w: malformed scan_open response", ErrProtocol)
	}
	return &ScanCursor{client: c, id: getU64(body[0:8])}, nil
}

func (s *ScanCursor) Next() ([]KVEntry, bool, error) {
	s.client.mu.Lock()
	defer s.client.mu.Unlock()
	var payload [12]byte
	putU64(payload[0:8], s.id)
	putU32(payload[8:12], 0)
	status, body, err := s.client.request(opScanNext, payload[:])
	if err != nil {
		return nil, false, err
	}
	if status != statusOK || len(body) < 8 {
		return nil, false, fmt.Errorf("%w: malformed scan_next response", ErrProtocol)
	}
	done := body[0] != 0
	count := getU32(body[4:8])
	entries := make([]KVEntry, 0, count)
	pos := 8
	for i := uint32(0); i < count; i++ {
		if len(body)-pos < 16 {
			return nil, false, fmt.Errorf("%w: truncated scan entry", ErrProtocol)
		}
		keyLen := getU32(body[pos : pos+4])
		valueLen := getU32(body[pos+4 : pos+8])
		lsn := getU64(body[pos+8 : pos+16])
		pos += 16
		if keyLen > maxKeySize || valueLen > maxValueSize || len(body)-pos < int(keyLen)+int(valueLen) {
			return nil, false, fmt.Errorf("%w: scan entry length", ErrProtocol)
		}
		key := body[pos : pos+int(keyLen)]
		pos += int(keyLen)
		value := body[pos : pos+int(valueLen)]
		pos += int(valueLen)
		entries = append(entries, KVEntry{Key: key, Value: value, LSN: lsn})
	}
	if pos != len(body) {
		return nil, false, fmt.Errorf("%w: scan trailing bytes", ErrProtocol)
	}
	return entries, done, nil
}

func (s *ScanCursor) Close() error {
	s.client.mu.Lock()
	defer s.client.mu.Unlock()
	var payload [8]byte
	putU64(payload[0:8], s.id)
	status, body, err := s.client.request(opScanClose, payload[:])
	if err != nil {
		return err
	}
	if status != statusOK || len(body) != 1 {
		return fmt.Errorf("%w: malformed scan_close response", ErrProtocol)
	}
	return nil
}

func (c *KVClient) Write(key, value string) error {
	_, err := c.Put([]byte(key), []byte(value))
	return err
}

func (c *KVClient) Read(key string) (string, error) {
	res, err := c.Get([]byte(key))
	if err != nil {
		return "", err
	}
	return string(res.Value), nil
}

func (c *KVClient) Delete(key string) error {
	_, err := c.Del([]byte(key))
	return err
}

func (c *KVClient) Reads(prefix string) ([]string, error) {
	scan, err := c.Scan([]byte(prefix))
	if err != nil {
		return nil, err
	}
	defer scan.Close()
	values := make([]string, 0)
	for {
		entries, done, err := scan.Next()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			values = append(values, string(entry.Value))
		}
		if done {
			return values, nil
		}
	}
}

func (c *KVClient) IsAlive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return false
	}
	c.conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	defer c.conn.SetDeadline(time.Time{})
	_, _, err := c.request(opPing, nil)
	return err == nil
}

type KVPool struct {
	addr    string
	pool    chan *KVClient
	size    int
	timeout time.Duration
	mu      sync.Mutex
	closed  bool
}

func (p *KVPool) replacementClient() (*KVClient, error) {
	client, err := NewKVClient(p.addr)
	if err == nil {
		client.requestTimeout = p.timeout
		return client, nil
	}
	p.mu.Lock()
	if !p.closed {
		select {
		case p.pool <- nil:
		default:
		}
	}
	p.mu.Unlock()
	return nil, err
}

func NewKVPool(addr string, size int, timeout time.Duration) (*KVPool, error) {
	p := &KVPool{
		addr:    addr,
		pool:    make(chan *KVClient, size),
		size:    size,
		timeout: timeout,
	}
	for i := 0; i < size; i++ {
		client, err := NewKVClient(addr)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("failed to create connection pool: %w", err)
		}
		p.pool <- client
	}
	return p, nil
}

func (p *KVPool) Get() (*KVClient, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool is closed")
	}
	p.mu.Unlock()

	select {
	case client := <-p.pool:
		if client != nil && client.conn != nil {
			if client.lastUsed.IsZero() {
				client.lastUsed = time.Now()
			} else if time.Since(client.lastUsed) >= 20*time.Second && !client.IsAlive() {
				client.Close()
				return p.replacementClient()
			}
			client.requestTimeout = p.timeout
			return client, nil
		}
		return p.replacementClient()
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("kv pool timeout: no connection available after 30s")
	}
}

func (p *KVPool) Put(client *KVClient) {
	if client == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		client.Close()
		return
	}
	p.mu.Unlock()

	client.requestTimeout = 0
	client.SetDeadline(time.Time{})

	select {
	case p.pool <- client:
	default:
		client.Close()
	}
}

func (p *KVPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	close(p.pool)
	for client := range p.pool {
		if client != nil {
			client.Close()
		}
	}
	return nil
}

func (p *KVPool) WithClient(fn func(*KVClient) error) error {
	client, err := p.Get()
	if err != nil {
		return err
	}
	if err := fn(client); err != nil {
		if !isConnectionError(err) {
			p.Put(client)
			return err
		}
		client.Close()
		p.mu.Lock()
		if !p.closed {
			select {
			case p.pool <- nil:
			default:
			}
		}
		p.mu.Unlock()
		return err
	}
	p.Put(client)
	return nil
}

func isConnectionError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return errors.Is(err, ErrProtocol)
}
