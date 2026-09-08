// Package testkv provides an in-memory PizzaKV-compatible server for tests. It
// speaks the PKBFI wire protocol over a real TCP listener so production
// KVPool/KVClient code paths can be exercised without a separate binary.
package testkv

import (
	"bufio"
	"hash/crc32"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

const (
	headerMagic   = "PKBF"
	headerVersion = 1
	headerSize    = 32

	opPing         = 1
	opGet          = 3
	opPut          = 4
	opDelete       = 5
	opExists       = 6
	opMultiGet     = 7
	opBatchWrite   = 8
	opScanOpen     = 9
	opScanNext     = 10
	opScanClose    = 11
	opCompareBatch = 12

	batchPut    = 1
	batchDelete = 2

	statusOK       = 0
	statusNotFound = 1
	statusError    = 2
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type scan struct {
	keys     []string
	offset   int
	limit    uint32
	keysOnly bool
}

// Server is an in-memory KV server.
type Server struct {
	mu      sync.Mutex
	data    map[string][]byte
	lsns    map[string]uint64
	nextLSN uint64

	ln       net.Listener
	wg       sync.WaitGroup
	closed   bool
	scans    map[uint64]*scan
	nextScan uint64
}

// New starts an in-memory KV server on an ephemeral TCP port.
func New(t testing.TB) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		data:  make(map[string][]byte),
		lsns:  make(map[string]uint64),
		ln:    ln,
		scans: make(map[uint64]*scan),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(func() { s.Close() })
	return s
}

// Addr returns the server's listen address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Pool creates a KV pool connected to the server.
func (s *Server) Pool(size int) *storage.KVPool {
	pool, err := storage.NewKVPool(s.Addr(), size, 5*time.Second)
	if err != nil {
		panic(err)
	}
	return pool
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		opcode, _, requestID, payload, err := readFrame(r)
		if err != nil {
			return
		}
		body := s.execute(opcode, payload)
		if _, err := conn.Write(encodeResponse(opcode, requestID, body)); err != nil {
			return
		}
	}
}

func (s *Server) execute(opcode uint16, payload []byte) []byte {
	switch opcode {
	case opPing:
		return statusOKBody(nil)
	case opGet:
		key, ok := parseOneKey(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		v, found := s.data[string(key)]
		lsn := s.lsns[string(key)]
		s.mu.Unlock()
		if !found {
			return statusBody(statusNotFound, nil)
		}
		body := make([]byte, 14+len(v))
		putU16(body[0:2], statusOK)
		putU64(body[2:10], lsn)
		putU32(body[10:14], uint32(len(v)))
		copy(body[14:], v)
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
		keys, ok := parseMultiGet(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		body := make([]byte, 6)
		putU16(body[0:2], statusOK)
		putU32(body[2:6], uint32(len(keys)))
		s.mu.Lock()
		for _, key := range keys {
			v, found := s.data[string(key)]
			if !found {
				body = append(body, make([]byte, 16)...)
				continue
			}
			entry := make([]byte, 16+len(v))
			entry[0] = 1
			putU32(entry[4:8], uint32(len(v)))
			putU64(entry[8:16], s.lsns[string(key)])
			copy(entry[16:], v)
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
		applyOps(s.data, s.lsns, ops, lsn)
		s.mu.Unlock()
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		putU64(body[2:10], lsn)
		return body
	case opCompareBatch:
		checks, ops, ok := parseCompareBatch(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		committed := true
		for _, c := range checks {
			if c.LSN == 0 {
				if _, found := s.data[string(c.Key)]; found {
					committed = false
					break
				}
			} else if s.lsns[string(c.Key)] != c.LSN {
				committed = false
				break
			}
		}
		var lsn uint64
		if committed {
			s.nextLSN++
			lsn = s.nextLSN
			applyOps(s.data, s.lsns, ops, lsn)
		}
		s.mu.Unlock()
		body := make([]byte, 18)
		putU16(body[0:2], statusOK)
		if committed {
			body[2] = 1
		}
		putU64(body[10:18], lsn)
		return body
	case opScanOpen:
		includeValues, limit, prefix, ok := parseScanOpen(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		keys := make([]string, 0)
		for k := range s.data {
			if strings.HasPrefix(k, string(prefix)) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		s.nextScan++
		id := s.nextScan
		s.scans[id] = &scan{keys: keys, limit: limit, keysOnly: !includeValues}
		s.mu.Unlock()
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		putU64(body[2:10], id)
		return body
	case opScanNext:
		id, ok := parseScanID(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		sc := s.scans[id]
		if sc == nil {
			s.mu.Unlock()
			return errorBody("ScanNotFound")
		}
		remaining := len(sc.keys) - sc.offset
		count := int(sc.limit)
		if count > remaining {
			count = remaining
		}
		end := sc.offset + count
		body := make([]byte, 10)
		putU16(body[0:2], statusOK)
		if end >= len(sc.keys) {
			body[2] = 1
		}
		putU32(body[6:10], uint32(count))
		for _, key := range sc.keys[sc.offset:end] {
			value := s.data[key]
			if sc.keysOnly {
				value = nil
			}
			entry := make([]byte, 16+len(key)+len(value))
			putU32(entry[0:4], uint32(len(key)))
			putU32(entry[4:8], uint32(len(value)))
			putU64(entry[8:16], s.lsns[key])
			copy(entry[16:], key)
			copy(entry[16+len(key):], value)
			body = append(body, entry...)
		}
		sc.offset = end
		s.mu.Unlock()
		return body
	case opScanClose:
		id, ok := parseScanID(payload)
		if !ok {
			return errorBody("InvalidPayload")
		}
		s.mu.Lock()
		delete(s.scans, id)
		s.mu.Unlock()
		body := make([]byte, 3)
		putU16(body[0:2], statusOK)
		body[2] = 1
		return body
	default:
		return errorBody("UnknownOpcode")
	}
}

func applyOps(data map[string][]byte, lsns map[string]uint64, ops []storage.BatchOp, lsn uint64) {
	for _, op := range ops {
		if op.Op == batchPut {
			data[string(op.Key)] = append([]byte(nil), op.Value...)
			lsns[string(op.Key)] = lsn
		} else {
			delete(data, string(op.Key))
			delete(lsns, string(op.Key))
		}
	}
}

// ── wire helpers ────────────────────────────────────────────────────────────

func crc32c(p []byte) uint32 { return crc32.Checksum(p, crcTable) }

func putU16(b []byte, v uint16) { b[0] = byte(v); b[1] = byte(v >> 8) }
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
func getU16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
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
	putU16(frame[8:10], opcode)
	putU16(frame[10:12], flags)
	putU64(frame[12:20], requestID)
	putU32(frame[20:24], uint32(len(payload)))
	putU32(frame[24:28], crc32c(payload))
	putU32(frame[28:32], crc32c(frame[0:32]))
	copy(frame[32:], payload)
	return frame
}

func encodeResponse(opcode uint16, requestID uint64, body []byte) []byte {
	return encodeFrame(opcode|0x8000, 1, requestID, body)
}

func readFrame(r *bufio.Reader) (uint16, uint16, uint64, []byte, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	payloadLen := getU32(header[20:24])
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, 0, 0, nil, err
	}
	return getU16(header[8:10]), getU16(header[10:12]), getU64(header[12:20]), payload, nil
}

func statusOKBody(p []byte) []byte {
	body := make([]byte, 2+len(p))
	putU16(body[0:2], statusOK)
	copy(body[2:], p)
	return body
}

func statusBody(status uint16, p []byte) []byte {
	body := make([]byte, 2+len(p))
	putU16(body[0:2], status)
	copy(body[2:], p)
	return body
}

func errorBody(msg string) []byte {
	body := make([]byte, 2+len(msg))
	putU16(body[0:2], statusError)
	copy(body[2:], msg)
	return body
}

func parseOneKey(p []byte) ([]byte, bool) {
	if len(p) < 4 {
		return nil, false
	}
	l := getU32(p[0:4])
	if 4+int(l) != len(p) {
		return nil, false
	}
	return p[4:], true
}

func parsePut(p []byte) ([]byte, []byte, bool) {
	if len(p) < 8 {
		return nil, nil, false
	}
	kl := getU32(p[0:4])
	vl := getU32(p[4:8])
	if 8+int(kl)+int(vl) != len(p) {
		return nil, nil, false
	}
	return p[8 : 8+kl], p[8+kl:], true
}

func parseMultiGet(p []byte) ([][]byte, bool) {
	if len(p) < 4 {
		return nil, false
	}
	n := getU32(p[0:4])
	keys := make([][]byte, 0, n)
	pos := 4
	for i := uint32(0); i < n; i++ {
		if len(p)-pos < 4 {
			return nil, false
		}
		l := getU32(p[pos : pos+4])
		pos += 4
		if len(p)-pos < int(l) {
			return nil, false
		}
		keys = append(keys, p[pos:pos+int(l)])
		pos += int(l)
	}
	return keys, pos == len(p)
}

func parseBatchOps(p []byte) ([]storage.BatchOp, bool) {
	if len(p) < 8 {
		return nil, false
	}
	n := getU32(p[0:4])
	ml := getU32(p[4:8])
	pos := 8 + int(ml)
	ops := make([]storage.BatchOp, 0, n)
	for i := uint32(0); i < n; i++ {
		if len(p)-pos < 12 {
			return nil, false
		}
		op := p[pos]
		kl := getU32(p[pos+4 : pos+8])
		vl := getU32(p[pos+8 : pos+12])
		pos += 12
		if op != batchPut && op != batchDelete {
			return nil, false
		}
		if len(p)-pos < int(kl)+int(vl) {
			return nil, false
		}
		key := p[pos : pos+int(kl)]
		pos += int(kl)
		value := p[pos : pos+int(vl)]
		pos += int(vl)
		ops = append(ops, storage.BatchOp{Op: op, Key: key, Value: value})
	}
	return ops, pos == len(p)
}

func parseCompareBatch(p []byte) ([]storage.CompareCheck, []storage.BatchOp, bool) {
	if len(p) < 16 {
		return nil, nil, false
	}
	nc := getU32(p[0:4])
	no := getU32(p[4:8])
	ml := getU32(p[8:12])
	pos := 16
	checks := make([]storage.CompareCheck, 0, nc)
	for i := uint32(0); i < nc; i++ {
		if len(p)-pos < 16 {
			return nil, nil, false
		}
		kl := getU32(p[pos : pos+4])
		lsn := getU64(p[pos+8 : pos+16])
		pos += 16
		if len(p)-pos < int(kl) {
			return nil, nil, false
		}
		checks = append(checks, storage.CompareCheck{Key: p[pos : pos+int(kl)], LSN: lsn})
		pos += int(kl)
	}
	pos += int(ml)
	ops := make([]storage.BatchOp, 0, no)
	for i := uint32(0); i < no; i++ {
		if len(p)-pos < 12 {
			return nil, nil, false
		}
		op := p[pos]
		kl := getU32(p[pos+4 : pos+8])
		vl := getU32(p[pos+8 : pos+12])
		pos += 12
		if op != batchPut && op != batchDelete {
			return nil, nil, false
		}
		if len(p)-pos < int(kl)+int(vl) {
			return nil, nil, false
		}
		key := p[pos : pos+int(kl)]
		pos += int(kl)
		value := p[pos : pos+int(vl)]
		pos += int(vl)
		ops = append(ops, storage.BatchOp{Op: op, Key: key, Value: value})
	}
	return checks, ops, pos == len(p)
}

func parseScanOpen(p []byte) (bool, uint32, []byte, bool) {
	if len(p) < 12 {
		return false, 0, nil, false
	}
	include := p[0] != 0
	limit := getU32(p[4:8])
	pl := getU32(p[8:12])
	if 12+int(pl) != len(p) {
		return false, 0, nil, false
	}
	return include, limit, p[12:], true
}

func parseScanID(p []byte) (uint64, bool) {
	if len(p) < 8 {
		return 0, false
	}
	return getU64(p[0:8]), true
}
