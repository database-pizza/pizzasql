package storage

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row represents a database row.
type Row map[string]interface{}

// TableManager manages table data operations.
type TableManager struct {
	pool     *KVPool
	schema   *SchemaManager
	database string

	cacheMu     sync.RWMutex
	indexCache  map[string]map[string][]int64 // index name → indexed value → rowids
	indexTable  map[string]string             // index name → table name
	rowKeyCache map[string]map[int64]string   // table name → rowid → primary key
	// disabledIndexes prevents a concurrent lookup from rebuilding an index
	// after DROP has cleared it but before the schema entry is removed.
	disabledIndexes map[string]bool

	// counts holds exact per-table row counts for the COUNT(*) fast path.
	// It is derived lazily from durable rows on first use and maintained
	// incrementally by Insert/InsertBulk/Delete thereafter.
	counts          map[string]int
	countsInit      map[string]bool
	countGeneration map[string]time.Time

	// stripes are deterministic per-key locks used by point operations
	// (GetByPK/Insert/UpdateByPK/DeleteByPK). They replace the table-wide lock
	// so point operations on different keys of the same table proceed
	// concurrently. Index is derived from the full data key (database+table+pk).
	stripes [64]sync.Mutex

	// generations tracks full-table scans. predicateGenerations narrows indexed
	// equality reads to one index value so unrelated writes do not conflict.
	genMu                sync.Mutex
	generations          map[string]uint64
	predicateGenerations map[string]uint64

	// exprMu guards exprEval, the pluggable evaluator for expression-index
	// columns. It is set by the SQL executor so storage stays independent of
	// the SQL front end.
	exprMu   sync.RWMutex
	exprEval ExpressionEvaluator
}

// ExpressionEvaluator computes the value of a persisted index expression for a
// row. The SQL executor registers one so the storage layer can enforce
// expression indexes without importing the parser/executor.
type ExpressionEvaluator func(expression string, row Row) (interface{}, error)

// SetExpressionEvaluator installs the evaluator used to compute expression-index
// values during uniqueness validation and index maintenance.
func (m *TableManager) SetExpressionEvaluator(eval ExpressionEvaluator) {
	m.exprMu.Lock()
	m.exprEval = eval
	m.exprMu.Unlock()
}

func (m *TableManager) expressionEvaluator() ExpressionEvaluator {
	m.exprMu.RLock()
	defer m.exprMu.RUnlock()
	return m.exprEval
}

// NewTableManager creates a new table manager.
func NewTableManager(pool *KVPool, schema *SchemaManager, database string) *TableManager {
	return &TableManager{
		pool:                 pool,
		schema:               schema,
		database:             database,
		indexCache:           make(map[string]map[string][]int64),
		indexTable:           make(map[string]string),
		rowKeyCache:          make(map[string]map[int64]string),
		disabledIndexes:      make(map[string]bool),
		counts:               make(map[string]int),
		countsInit:           make(map[string]bool),
		countGeneration:      make(map[string]time.Time),
		generations:          make(map[string]uint64),
		predicateGenerations: make(map[string]uint64),
	}
}

func (m *TableManager) tableLock(key string) *sync.RWMutex {
	return m.schema.tableLock(key)
}

// stripeKey returns the deterministic striped lock for a point operation on the
// given full data key. Point operations on different keys therefore serialize
// independently, while operations on the same key are mutually exclusive.
func (m *TableManager) stripeKey(key string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(key))
	return &m.stripes[h.Sum32()%uint32(len(m.stripes))]
}

// generation returns the current in-process generation for a table. It is
// bumped on every committed write and captured by transaction scans.
func (m *TableManager) generation(table string) uint64 {
	key := strings.ToLower(table)
	m.genMu.Lock()
	g := m.generations[key]
	m.genMu.Unlock()
	return g
}

// bumpGeneration advances a table's generation. Callers hold the table gate in
// shared mode for point writes or exclusive mode for scan-based writes.
func (m *TableManager) bumpGeneration(table string) {
	key := strings.ToLower(table)
	m.genMu.Lock()
	m.generations[key]++
	m.genMu.Unlock()
}

func indexPredicateKey(table, indexName, value string) string {
	return strings.ToLower(table) + "\x00" + strings.ToLower(indexName) + "\x00" + value
}

func indexPredicateWildcardKey(table string) string {
	return strings.ToLower(table) + "\x00*"
}

func (m *TableManager) predicateSnapshot(table, indexName, value string) (string, uint64, string, uint64) {
	valueKey := indexPredicateKey(table, indexName, value)
	wildcardKey := indexPredicateWildcardKey(table)
	m.genMu.Lock()
	valueGen := m.predicateGenerations[valueKey]
	wildcardGen := m.predicateGenerations[wildcardKey]
	m.genMu.Unlock()
	return valueKey, valueGen, wildcardKey, wildcardGen
}

func (m *TableManager) predicateGeneration(key string) uint64 {
	m.genMu.Lock()
	gen := m.predicateGenerations[key]
	m.genMu.Unlock()
	return gen
}

func (m *TableManager) bumpIndexPredicates(table string, rows ...Row) error {
	indexes, err := m.schema.ListTableIndexes(table)
	if err != nil || len(indexes) == 0 {
		return err
	}
	m.genMu.Lock()
	defer m.genMu.Unlock()
	for _, row := range rows {
		if row == nil {
			continue
		}
		for _, index := range indexes {
			value, err := m.buildIndexValue(row, index.Columns)
			if err != nil {
				return err
			}
			m.predicateGenerations[indexPredicateKey(table, index.Name, value)]++
		}
	}
	return nil
}

func (m *TableManager) bumpIndexPredicateWildcard(table string) {
	m.genMu.Lock()
	m.predicateGenerations[indexPredicateWildcardKey(table)]++
	m.genMu.Unlock()
}

// compareWritePoint issues a single CompareBatchWrite against the pooled KV.
// It returns whether the compare checks held and the ops committed.
func (m *TableManager) compareWritePoint(checks []CompareCheck, ops []BatchOp) (bool, error) {
	var committed bool
	err := m.pool.WithClient(func(c *KVClient) error {
		_, ok, err := c.CompareBatchWrite(checks, ops, nil)
		committed = ok
		return err
	})
	return committed, err
}

// invalidateCache removes a table's derived in-memory indexes.
func (m *TableManager) invalidateCache(table string) {
	m.cacheMu.Lock()
	key := strings.ToLower(table)
	delete(m.rowKeyCache, key)
	delete(m.counts, key)
	delete(m.countsInit, key)
	delete(m.countGeneration, key)
	for indexName, tableName := range m.indexTable {
		if tableName == key {
			delete(m.indexCache, indexName)
			delete(m.indexTable, indexName)
		}
	}
	m.cacheMu.Unlock()
}

// InvalidateCache is the exported version for use by the executor.
func (m *TableManager) InvalidateCache(table string) {
	m.invalidateCache(table)
}

// rowVisitFunc is invoked for each decoded row in a streaming scan. Return
// stop=true to end the scan early; a non-nil error aborts the scan.
type rowVisitFunc func(Row) (stop bool, err error)

// scanRows streams the rows of table by scanning durable KV rows one page at a
// time under a single pooled client. Each page is decoded as it arrives and
// passed to fn, which may stop the scan early. The cursor is always closed and
// the client always returned to the pool, even on error.
func (m *TableManager) scanRows(table string, fn rowVisitFunc) error {
	return m.scanRowsWithPageSize(table, scanPageSize, fn)
}

func (m *TableManager) scanRowsWithPageSize(table string, pageSize uint32, fn rowVisitFunc) error {
	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanWithLimit([]byte(m.dataPrefix(table)), pageSize)
		if err != nil {
			return err
		}
		defer func() {
			if err := cursor.Close(); retErr == nil {
				retErr = err
			}
		}()

		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, e := range entries {
				row, err := decodeRow(e.Value)
				if err != nil {
					return err
				}
				stop, err := fn(row)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
			if done {
				return nil
			}
		}
	})
}

// rowWithLSNVisitFunc is like rowVisitFunc but also passes the durable row LSN.
type rowWithLSNVisitFunc func(row Row, lsn uint64) (stop bool, err error)

// scanRowsWithLSN streams a table's rows together with their durable LSNs. It
// is used by buffered transactions to capture a per-row read set for
// compare-and-swap validation at commit.
func (m *TableManager) scanRowsWithLSN(table string, fn rowWithLSNVisitFunc) error {
	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanWithLimit([]byte(m.dataPrefix(table)), scanPageSize)
		if err != nil {
			return err
		}
		defer func() {
			if err := cursor.Close(); retErr == nil {
				retErr = err
			}
		}()

		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, e := range entries {
				row, err := decodeRow(e.Value)
				if err != nil {
					return err
				}
				stop, err := fn(row, e.LSN)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
			if done {
				return nil
			}
		}
	})
}

// scanCountKeys counts the durable rows of table using a key-only scan so row
// values are never pulled across the wire. It is used for first-time COUNT(*)
// derivation.
func (m *TableManager) scanCountKeys(table string) (int, error) {
	count := 0
	err := m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanKeys([]byte(m.dataPrefix(table)))
		if err != nil {
			return err
		}
		defer func() {
			if err := cursor.Close(); retErr == nil {
				retErr = err
			}
		}()
		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			count += len(entries)
			if done {
				return nil
			}
		}
	})
	return count, err
}

// CountFast returns the exact number of rows in a table. The count is derived
// from durable rows on first use (recovering across restarts) and then
// maintained incrementally by the write paths, so repeated COUNT(*) queries
// avoid a full table scan. It intentionally does not persist a counter to KV:
// the KV layer has no atomic increment primitive, and a durable counter that
// could diverge from the rows on crash would be worse than a lazily-derived,
// always-exact value. The cost is one key-only scan the first time COUNT(*) is
// issued after startup.
func (m *TableManager) CountFast(table string) (int, error) {
	key := strings.ToLower(table)
	tableSchema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	// Cached read path: if the count is initialized for the current table
	// generation, return it without taking any table lock.
	m.cacheMu.Lock()
	if m.countGeneration[key].Equal(tableSchema.CreatedAt) {
		if m.countsInit[key] {
			n := m.counts[key]
			m.cacheMu.Unlock()
			return n, nil
		}
	} else {
		delete(m.counts, key)
		delete(m.countsInit, key)
		m.countGeneration[key] = tableSchema.CreatedAt
	}
	m.cacheMu.Unlock()

	// First derivation: hold the table write lock so the key-only scan is
	// exact against concurrent writes.
	tl := m.tableLock(key)
	tl.Lock()
	defer tl.Unlock()

	m.cacheMu.Lock()
	if !m.countGeneration[key].Equal(tableSchema.CreatedAt) {
		delete(m.counts, key)
		delete(m.countsInit, key)
		m.countGeneration[key] = tableSchema.CreatedAt
	}
	if m.countsInit[key] {
		n := m.counts[key]
		m.cacheMu.Unlock()
		return n, nil
	}
	m.cacheMu.Unlock()

	count, err := m.scanCountKeys(table)
	if err != nil {
		return 0, err
	}

	m.cacheMu.Lock()
	m.counts[key] = count
	m.countsInit[key] = true
	m.cacheMu.Unlock()

	return count, nil
}

// countInitialized reports whether the derived count cache is initialized for
// the table at this instant. Write paths capture it before their KV write so a
// concurrent first derivation does not double-count the just-written row.
func (m *TableManager) countInitialized(table string) bool {
	key := strings.ToLower(table)
	m.cacheMu.Lock()
	init := m.countsInit[key]
	m.cacheMu.Unlock()
	return init
}

// incrCount adjusts the derived per-table row count. wasInit reports whether
// the count was already initialized before the corresponding write, so a count
// that was not yet initialized is left to be re-derived from durable rows
// (which already reflect the write) on next use.
func (m *TableManager) incrCount(table string, generation time.Time, delta int, wasInit bool) {
	key := strings.ToLower(table)
	m.cacheMu.Lock()
	if !m.countGeneration[key].Equal(generation) {
		delete(m.counts, key)
		delete(m.countsInit, key)
		m.countGeneration[key] = generation
	}
	if wasInit && m.countsInit[key] {
		m.counts[key] += delta
	}
	if !wasInit {
		// The count was not initialized before the write, so a concurrent first
		// derivation may have missed the just-written row. Invalidate to force
		// an exact re-derivation on the next COUNT(*).
		delete(m.countsInit, key)
	}
	m.cacheMu.Unlock()
}

// dataKey returns the key for a row.
func (m *TableManager) dataKey(table, pk string) string {
	return fmt.Sprintf("%s:_data:%s:%s", m.database, strings.ToLower(table), pk)
}

// dataPrefix returns the prefix for all rows in a table.
func (m *TableManager) dataPrefix(table string) string {
	return fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(table))
}

// prepareInsert validates and normalizes an insert row, generating the ROWID
// when required. It returns the normalized row and the full data key without
// writing anything, so both the autocommit path (compare-and-swap) and the
// buffered transaction path (staging) can share it. The input row has its
// primary key populated as a side effect.
func (m *TableManager) prepareInsert(table string, row Row) (Row, string, error) {
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return nil, "", err
	}

	// Get primary key value
	pkValue, ok := row[schema.PrimaryKey]
	if !ok {
		for k, v := range row {
			if strings.EqualFold(k, schema.PrimaryKey) {
				pkValue = v
				ok = true
				break
			}
		}
	}

	pkCol, _ := schema.GetColumn(schema.PrimaryKey)
	isIntegerPK := pkCol != nil && isIntegerType(pkCol.Type)

	var rowid int64
	if !ok || pkValue == nil {
		if isIntegerPK || !ok {
			rowid, err = m.schema.GetNextRowID(table)
			if err != nil {
				return nil, "", err
			}
			pkValue = rowid
			row[schema.PrimaryKey] = rowid
			ok = true
		} else {
			return nil, "", fmt.Errorf("missing primary key: %s", schema.PrimaryKey)
		}
	} else if isIntegerPK {
		switch v := pkValue.(type) {
		case int64:
			rowid = v
		case float64:
			if math.Trunc(v) != v {
				return nil, "", fmt.Errorf("invalid integer primary key: %v", v)
			}
			rowid = int64(v)
		case int:
			rowid = int64(v)
		default:
			rowid = 0
		}
		if rowid > 0 {
			if err := m.schema.UpdateMaxRowID(table, rowid); err != nil {
				return nil, "", err
			}
		}
	}

	pk := fmt.Sprintf("%v", pkValue)

	for _, col := range schema.Columns {
		if !col.Nullable && col.Default == nil {
			val, hasVal := row[col.Name]
			if !hasVal {
				for k, v := range row {
					if strings.EqualFold(k, col.Name) {
						val = v
						hasVal = true
						break
					}
				}
			}
			if !hasVal || val == nil {
				return nil, "", fmt.Errorf("missing required column: %s", col.Name)
			}
		}
	}

	normalizedRow := make(Row)
	for _, col := range schema.Columns {
		for k, v := range row {
			if strings.EqualFold(k, col.Name) {
				normalizedRow[col.Name] = v
				break
			}
		}
	}
	for _, col := range schema.Columns {
		if _, ok := normalizedRow[col.Name]; !ok && col.Default != nil {
			normalizedRow[col.Name] = col.Default
		}
	}

	if rowid > 0 {
		normalizedRow["_rowid_"] = rowid
	} else {
		newRowID, err := m.schema.GetNextRowID(table)
		if err != nil {
			return nil, "", err
		}
		normalizedRow["_rowid_"] = newRowID
	}

	return normalizedRow, m.dataKey(table, pk), nil
}

// Insert inserts a new row. The duplicate check and write are one atomic
// compare-and-swap so concurrent inserts of the same primary key cannot both
// persist. The shared table gate is acquired before the key's striped lock so
// a queued scan writer cannot invert the lock order with point operations.
func (m *TableManager) Insert(table string, row Row) error {
	_, err := m.InsertWithRowID(table, row)
	return err
}

// InsertWithRowID returns the actual row identifier, never a concurrent counter.
func (m *TableManager) InsertWithRowID(table string, row Row) (int64, error) {
	nr, key, err := m.prepareInsert(table, row)
	if err != nil {
		return 0, err
	}
	rowid, _ := rowIDFromRow(nr)
	data, err := encodeRow(nr)
	if err != nil {
		return 0, fmt.Errorf("failed to serialize row: %w", err)
	}
	wasInit := m.countInitialized(table)

	// Point writers share this gate with each other. Transaction commits and
	// scan-based writes take it exclusively, so generation validation and cache
	// publication are ordered without serializing writes to different keys.
	// A UNIQUE index forces the exclusive gate so the validating scan below
	// cannot race another writer (or a concurrent CREATE UNIQUE INDEX).
	unlock, uniq, err := m.lockForWrite(table)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if uniq {
		if err := m.validateUniqueRows(table, []Row{nr}, nil); err != nil {
			return 0, err
		}
	}
	st := m.stripeKey(key)
	st.Lock()
	defer st.Unlock()
	committed, err := m.compareWritePoint(
		[]CompareCheck{{Key: []byte(key), LSN: 0}},
		[]BatchOp{{Op: batchPut, Key: []byte(key), Value: data}},
	)
	if err != nil {
		return 0, err
	}
	if !committed {
		return 0, fmt.Errorf("duplicate primary key: %v", row[schemaPrimaryKey(m.schema, table)])
	}

	if err := m.updateIndexesForRow(table, nr, true); err != nil {
		return 0, err
	}
	// Publish derived index state before its generations. A reader that races
	// with publication either sees the old generation and aborts or sees the
	// complete new state.
	if err := m.bumpIndexPredicates(table, nr); err != nil {
		return 0, err
	}
	m.bumpGeneration(table)
	if schema, serr := m.schema.GetSchema(table); serr == nil {
		m.incrCount(table, schema.CreatedAt, 1, wasInit)
	}
	return rowid, nil
}

func schemaPrimaryKey(s *SchemaManager, table string) string {
	schema, err := s.GetSchema(table)
	if err != nil {
		return "_rowid_"
	}
	return schema.PrimaryKey
}

// bulkBatchByteBudget bounds a single atomic BATCH_WRITE payload below the
// PKBFI frame limit so a bulk insert never emits a frame the server rejects.
// Each op contributes 12 header bytes plus its key and value.
const bulkBatchByteBudget = 60 * 1024 * 1024

// chunkBatchOps splits ops into atomic BATCH_WRITE chunks bounded by both the
// PKBFI operation-count limit and the frame-size limit. Each chunk is a slice
// of the backing array, valid until the next append to ops.
func chunkBatchOps(ops []BatchOp) [][]BatchOp {
	var chunks [][]BatchOp
	for i := 0; i < len(ops); {
		end := i + maxOperations
		if end > len(ops) {
			end = len(ops)
		}
		bytes := 0
		j := i
		for j < end {
			sz := 12 + len(ops[j].Key) + len(ops[j].Value)
			if j > i && bytes+sz > bulkBatchByteBudget {
				break
			}
			bytes += sz
			j++
		}
		if j == i {
			j = i + 1
		}
		chunks = append(chunks, ops[i:j])
		i = j
	}
	return chunks
}

// InsertBulk inserts multiple rows efficiently using atomic BATCH_WRITE chunks
// bounded by the PKBFI operation-count and frame-size limits. Skips per-row
// duplicate checks (caller must ensure uniqueness). Used by INSERT ... SELECT.
func (m *TableManager) InsertBulk(table string, rows []Row) (int, error) {
	count, _, err := m.InsertBulkWithLastRowID(table, rows)
	return count, err
}

// InsertBulkWithLastRowID is InsertBulk, additionally returning the ROWID of the
// last persisted row (0 when nothing persisted).
func (m *TableManager) InsertBulkWithLastRowID(table string, rows []Row) (int, int64, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	tl := m.tableLock(table)
	tl.Lock()
	defer tl.Unlock()

	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, 0, err
	}
	wasInit := m.countInitialized(table)

	pkCol, _ := schema.GetColumn(schema.PrimaryKey)
	isIntegerPK := pkCol != nil && isIntegerType(pkCol.Type)

	// Normalize rows and assign _rowid_.
	normalized := make([]Row, 0, len(rows))
	var maxRowID int64
	for _, row := range rows {
		nr := make(Row)
		for _, col := range schema.Columns {
			for k, v := range row {
				if strings.EqualFold(k, col.Name) {
					nr[col.Name] = v
					break
				}
			}
		}
		for _, col := range schema.Columns {
			if _, ok := nr[col.Name]; !ok && col.Default != nil {
				nr[col.Name] = col.Default
			}
		}
		var rowid int64
		var hasRowid bool
		if isIntegerPK {
			switch v := nr[schema.PrimaryKey].(type) {
			case float64:
				if math.Trunc(v) != v {
					return 0, 0, fmt.Errorf("invalid integer primary key: %v", v)
				}
				rowid = int64(v)
				hasRowid = true
			case int64:
				rowid = v
				hasRowid = true
			case int:
				rowid = int64(v)
				hasRowid = true
			}
		}
		if !hasRowid {
			// Auto-generate for an INTEGER primary key or the synthetic _rowid_,
			// mirroring prepareInsert so INSERT ... SELECT works on AUTOINCREMENT
			// tables.
			if isIntegerPK || schema.PrimaryKey == "_rowid_" {
				rowid, err = m.schema.GetNextRowID(table)
				if err != nil {
					return 0, 0, err
				}
				if isIntegerPK {
					nr[schema.PrimaryKey] = rowid
				} else {
					nr["_rowid_"] = rowid
				}
			} else {
				pk, ok := nr[schema.PrimaryKey]
				if !ok || pk == nil {
					return 0, 0, fmt.Errorf("missing primary key: %s", schema.PrimaryKey)
				}
				rowid, err = m.schema.GetNextRowID(table)
				if err != nil {
					return 0, 0, err
				}
			}
		}
		nr["_rowid_"] = rowid
		if rowid > maxRowID {
			maxRowID = rowid
		}
		normalized = append(normalized, nr)
	}

	if maxRowID > 0 {
		m.schema.UpdateMaxRowID(table, maxRowID)
	}

	// Serialize all rows into batch operations. A parallel slice keeps the
	// normalized Row for each op for index maintenance after the write.
	ops := make([]BatchOp, 0, len(normalized))
	encoded := make([]Row, 0, len(normalized))
	for _, nr := range normalized {
		pk := fmt.Sprintf("%v", nr[schema.PrimaryKey])
		data, err := encodeRow(nr)
		if err != nil {
			return 0, 0, err
		}
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(m.dataKey(table, pk)), Value: data})
		encoded = append(encoded, nr)
	}
	keys := make([][]byte, len(ops))
	seen := make(map[string]struct{}, len(ops))
	for i, op := range ops {
		key := string(op.Key)
		if _, duplicate := seen[key]; duplicate {
			return 0, 0, fmt.Errorf("duplicate primary key: %s", key)
		}
		seen[key] = struct{}{}
		keys[i] = op.Key
	}
	existing := make([]bool, len(keys))
	if err := m.pool.WithClient(func(client *KVClient) error {
		for start := 0; start < len(keys); start += maxOperations {
			end := start + maxOperations
			if end > len(keys) {
				end = len(keys)
			}
			found, err := client.ExistsMany(keys[start:end])
			if err != nil {
				return err
			}
			copy(existing[start:end], found)
		}
		return nil
	}); err != nil {
		return 0, 0, err
	}
	for i, found := range existing {
		if found {
			return 0, 0, fmt.Errorf("duplicate primary key: %s", ops[i].Key)
		}
	}

	if err := m.validateUniqueRows(table, encoded, nil); err != nil {
		return 0, 0, err
	}

	// Write rows in atomic BATCH_WRITE chunks. Maintain in-memory indexes only
	// for rows that actually persisted, so a partial failure cannot leave an
	// already-built index stale.
	var firstErr error
	numOK := 0
	for _, chunk := range chunkBatchOps(ops) {
		err := m.pool.WithClient(func(c *KVClient) error {
			_, err := c.BatchWrite(chunk, nil)
			return err
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		numOK += len(chunk)
	}
	// Maintain indexes for the rows that persisted (the first numOK ops).
	for i := 0; i < numOK; i++ {
		if err := m.updateIndexesForRow(table, encoded[i], true); err != nil {
			return numOK, 0, err
		}
	}
	if numOK > 0 {
		m.bumpGeneration(table)
		if err := m.bumpIndexPredicates(table, encoded[:numOK]...); err != nil {
			return numOK, 0, err
		}
	}
	m.incrCount(table, schema.CreatedAt, numOK, wasInit)

	var lastRowID int64
	if numOK > 0 {
		lastRowID, _ = rowIDFromRow(encoded[numOK-1])
	}
	return numOK, lastRowID, firstErr
}

// updateIndexesForRow adds or removes entries from already-built in-memory
// indexes. Index entries are rebuildable from durable row data, so this method
// intentionally does not write idx:* keys to KV.
func (m *TableManager) updateIndexesForRow(table string, row Row, add bool) error {
	indexes, err := m.schema.ListTableIndexes(table)
	if err != nil || len(indexes) == 0 {
		return err
	}

	rowid, ok := rowIDFromRow(row)
	if !ok {
		return nil
	}
	tableKey := strings.ToLower(table)
	tableSchema, schemaErr := m.schema.GetSchema(table)
	if schemaErr == nil {
		m.cacheMu.Lock()
		if keys, initialized := m.rowKeyCache[tableKey]; initialized {
			if add {
				keys[rowid] = fmt.Sprintf("%v", row[tableSchema.PrimaryKey])
			} else {
				delete(keys, rowid)
			}
		}
		m.cacheMu.Unlock()
	}

	for _, idx := range indexes {
		indexName := strings.ToLower(idx.Name)
		m.cacheMu.RLock()
		_, initialized := m.indexCache[indexName]
		m.cacheMu.RUnlock()
		if !initialized {
			continue
		}

		colValue, err := m.buildIndexValue(row, idx.Columns)
		if err != nil {
			return err
		}

		if add {
			m.AddIndexEntry(idx.Name, colValue, rowid)
		} else {
			m.RemoveIndexEntry(idx.Name, colValue, rowid)
		}
	}
	return nil
}

// Select retrieves rows from a table by scanning durable rows and collecting
// only matching rows.
func (m *TableManager) Select(table string, filter func(Row) bool) ([]Row, error) {
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()
	return m.selectRows(table, filter)
}

// selectRows scans a table while the caller holds its shared or exclusive gate.
func (m *TableManager) selectRows(table string, filter func(Row) bool) ([]Row, error) {
	if !m.schema.TableExists(table) {
		return nil, fmt.Errorf("table not found: %s", table)
	}

	rows := make([]Row, 0)
	err := m.scanRows(table, func(row Row) (bool, error) {
		if filter == nil || filter(row) {
			rows = append(rows, row)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func cloneRow(row Row) Row {
	if row == nil {
		return nil
	}
	cloned := make(Row, len(row))
	for k, v := range row {
		cloned[k] = v
	}
	return cloned
}

// SelectWithLimit retrieves rows with limit and offset, applying filter/offset
// while scanning and closing the scan early once the limit is reached.
func (m *TableManager) SelectWithLimit(table string, filter func(Row) bool, limit, offset int) ([]Row, error) {
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()

	if !m.schema.TableExists(table) {
		return nil, fmt.Errorf("table not found: %s", table)
	}

	rows := make([]Row, 0)
	skipped := 0
	pageSize := scanPageSize
	if limit > 0 {
		desired := limit
		if offset > 0 {
			if offset >= scanPageSize-desired {
				desired = scanPageSize
			} else {
				desired += offset
			}
		}
		if desired < 1 {
			desired = 1
		}
		if desired < pageSize {
			pageSize = desired
		}
	}
	err := m.scanRowsWithPageSize(table, uint32(pageSize), func(row Row) (bool, error) {
		if filter != nil && !filter(row) {
			return false, nil
		}
		if skipped < offset {
			skipped++
			return false, nil
		}
		rows = append(rows, row)
		return limit > 0 && len(rows) >= limit, nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Update updates rows matching the filter.
func (m *TableManager) Update(table string, updates Row, filter func(Row) bool) (int, error) {
	return m.updateLocked(table, func(Row) (Row, error) { return updates, nil }, filter)
}

// UpdateFunc updates rows matching the filter using a function to compute new values.
// The updateFn receives the current row and returns the updates to apply.
func (m *TableManager) UpdateFunc(table string, updateFn func(Row) (Row, error), filter func(Row) bool) (int, error) {
	return m.updateLocked(table, updateFn, filter)
}

// updateLocked is the shared scan-based update implementation. It applies
// per-row updates, revalidates unique indexes, and moves a row when its primary
// key changes (deleting the old key and refusing to overwrite an occupied new
// key) so an UPDATE of the primary key does not orphan or clobber rows.
func (m *TableManager) updateLocked(table string, updateFn func(Row) (Row, error), filter func(Row) bool) (int, error) {
	tl := m.tableLock(strings.ToLower(table))
	tl.Lock()
	defer tl.Unlock()
	rows, err := m.selectRows(table, filter)
	if err != nil {
		return 0, err
	}
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, row := range rows {
		oldRow := cloneRow(row)
		oldKey := m.dataKey(table, fmt.Sprintf("%v", oldRow[schema.PrimaryKey]))
		if err := m.updateIndexesForRow(table, row, false); err != nil {
			return count, err
		}

		updates, err := updateFn(row)
		if err != nil {
			_ = m.updateIndexesForRow(table, oldRow, true)
			return count, err
		}
		for k, v := range updates {
			for _, col := range schema.Columns {
				if strings.EqualFold(k, col.Name) {
					row[col.Name] = v
					break
				}
			}
		}

		newKey := m.dataKey(table, fmt.Sprintf("%v", row[schema.PrimaryKey]))
		data, err := encodeRow(row)
		if err != nil {
			_ = m.updateIndexesForRow(table, oldRow, true)
			continue
		}

		excluded := map[string]bool{oldKey: true}
		if err := m.validateUniqueRows(table, []Row{row}, excluded); err != nil {
			_ = m.updateIndexesForRow(table, oldRow, true)
			return count, err
		}

		if newKey != oldKey {
			// Refuse to move onto an existing primary key.
			if _, _, gerr := m.getByPKWithLSN(table, fmt.Sprintf("%v", row[schema.PrimaryKey])); gerr == nil {
				_ = m.updateIndexesForRow(table, oldRow, true)
				return count, fmt.Errorf("duplicate primary key: %v", row[schema.PrimaryKey])
			} else if gerr != ErrKeyNotFound {
				_ = m.updateIndexesForRow(table, oldRow, true)
				return count, gerr
			}
			if err := m.pool.WithClient(func(c *KVClient) error {
				_, derr := c.Del([]byte(oldKey))
				return derr
			}); err != nil {
				_ = m.updateIndexesForRow(table, oldRow, true)
				continue
			}
		}

		if err := m.pool.WithClient(func(c *KVClient) error {
			_, perr := c.Put([]byte(newKey), data)
			return perr
		}); err != nil {
			_ = m.updateIndexesForRow(table, oldRow, true)
			continue
		}
		if err := m.updateIndexesForRow(table, row, true); err != nil {
			return count, err
		}
		if err := m.bumpIndexPredicates(table, oldRow, row); err != nil {
			return count, err
		}
		count++
	}

	if count > 0 {
		m.bumpGeneration(table)
	}
	return count, nil
}

// UpdateByPK updates one row without scanning the table. It uses the key's
// striped lock and a compare-and-swap write so a concurrent modification of the
// same row fails with a serialization error instead of being silently lost.
func (m *TableManager) UpdateByPK(table, pk string, updateFn func(Row) (Row, error)) (Row, bool, error) {
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return nil, false, err
	}

	key := m.dataKey(table, pk)
	unlock, uniq, err := m.lockForWrite(table)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	st := m.stripeKey(key)
	st.Lock()
	defer st.Unlock()

	row, lsn, err := m.getByPKWithLSN(table, pk)
	if err == ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	oldRow := cloneRow(row)
	updates, err := updateFn(row)
	if err != nil {
		return nil, false, err
	}
	for name, value := range updates {
		for _, column := range schema.Columns {
			if strings.EqualFold(name, column.Name) {
				row[column.Name] = value
				break
			}
		}
	}

	data, err := encodeRow(row)
	if err != nil {
		return nil, false, err
	}
	if uniq {
		if err := m.validateUniqueRows(table, []Row{row}, excludedKey(key)); err != nil {
			return nil, false, err
		}
	}
	committed, err := m.compareWritePoint(
		[]CompareCheck{{Key: []byte(key), LSN: lsn}},
		[]BatchOp{{Op: batchPut, Key: []byte(key), Value: data}},
	)
	if err != nil {
		return nil, false, err
	}
	if !committed {
		return nil, false, ErrSerialization
	}

	if err := m.updateIndexesForRow(table, oldRow, false); err != nil {
		return nil, false, err
	}
	if err := m.updateIndexesForRow(table, row, true); err != nil {
		return nil, false, err
	}
	if err := m.bumpIndexPredicates(table, oldRow, row); err != nil {
		return nil, false, err
	}
	m.bumpGeneration(table)
	return oldRow, true, nil
}

// Delete deletes rows matching the filter.
func (m *TableManager) Delete(table string, filter func(Row) bool) (int, error) {
	tl := m.tableLock(strings.ToLower(table))
	tl.Lock()
	defer tl.Unlock()
	rows, err := m.selectRows(table, filter)
	if err != nil {
		return 0, err
	}
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}
	wasInit := m.countInitialized(table)

	count := 0
	for _, row := range rows {
		// Remove index entries before deleting row
		if err := m.updateIndexesForRow(table, row, false); err != nil {
			return count, err
		}

		pkValue := row[schema.PrimaryKey]
		pk := fmt.Sprintf("%v", pkValue)
		key := m.dataKey(table, pk)

		err = m.pool.WithClient(func(c *KVClient) error {
			_, err := c.Del([]byte(key))
			return err
		})
		if err == nil {
			if err := m.bumpIndexPredicates(table, row); err != nil {
				return count, err
			}
			count++
		} else {
			// Restore the index entries removed above.
			_ = m.updateIndexesForRow(table, row, true)
		}
	}

	if count > 0 {
		m.bumpGeneration(table)
	}
	m.incrCount(table, schema.CreatedAt, -count, wasInit)
	return count, nil
}

// DeleteByPK deletes one row without scanning the table, using the key's
// striped lock and a compare-and-swap delete.
func (m *TableManager) DeleteByPK(table, pk string) (Row, bool, error) {
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return nil, false, err
	}

	key := m.dataKey(table, pk)
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()
	st := m.stripeKey(key)
	st.Lock()
	defer st.Unlock()
	wasInit := m.countInitialized(table)

	row, lsn, err := m.getByPKWithLSN(table, pk)
	if err == ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	committed, err := m.compareWritePoint(
		[]CompareCheck{{Key: []byte(key), LSN: lsn}},
		[]BatchOp{{Op: batchDelete, Key: []byte(key)}},
	)
	if err != nil {
		return nil, false, err
	}
	if !committed {
		return nil, false, ErrSerialization
	}

	if err := m.updateIndexesForRow(table, row, false); err != nil {
		return nil, false, err
	}
	if err := m.bumpIndexPredicates(table, row); err != nil {
		return nil, false, err
	}
	m.bumpGeneration(table)
	m.incrCount(table, schema.CreatedAt, -1, wasInit)
	return row, true, nil
}

// GetByPK retrieves a row by primary key. Point reads take only the key's
// striped lock so reads of different keys progress concurrently.
func (m *TableManager) GetByPK(table string, pk string) (Row, error) {
	key := m.dataKey(table, pk)
	st := m.stripeKey(key)
	st.Lock()
	defer st.Unlock()

	if !m.schema.TableExists(table) {
		return nil, fmt.Errorf("table not found: %s", table)
	}
	row, _, err := m.getByPKWithLSN(table, pk)
	return row, err
}

// getByPKWithLSN reads a row by primary key and returns its KV LSN (0 when
// absent). It performs no locking; callers must hold the appropriate striped
// or table lock.
func (m *TableManager) getByPKWithLSN(table, pk string) (Row, uint64, error) {
	key := m.dataKey(table, pk)
	var value []byte
	var lsn uint64

	err := m.pool.WithClient(func(c *KVClient) error {
		res, err := c.Get([]byte(key))
		if err != nil {
			return err
		}
		value = res.Value
		lsn = res.LSN
		return nil
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, 0, ErrKeyNotFound
		}
		return nil, 0, err
	}

	row, err := decodeRow(value)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse row: %w", err)
	}

	return row, lsn, nil
}

// Count returns the number of rows in a table matching the filter.
func (m *TableManager) Count(table string, filter func(Row) bool) (int, error) {
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()

	if !m.schema.TableExists(table) {
		return 0, fmt.Errorf("table not found: %s", table)
	}

	count := 0
	err := m.scanRows(table, func(row Row) (bool, error) {
		if filter == nil || filter(row) {
			count++
		}
		return false, nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// Truncate removes all rows from a table.
func (m *TableManager) Truncate(table string) (int, error) {
	return m.Delete(table, nil)
}

// isIntegerType checks if a type name is an integer type.
func isIntegerType(typeName string) bool {
	t := strings.ToUpper(typeName)
	switch t {
	case "INTEGER", "INT", "SMALLINT", "BIGINT", "TINYINT", "MEDIUMINT":
		return true
	}
	return false
}

// IsRowIDColumn checks if a column name is a ROWID alias.
func IsRowIDColumn(name string) bool {
	n := strings.ToLower(name)
	return n == "rowid" || n == "oid" || n == "_rowid_"
}

// Index entry methods - leveraging radix trie for prefix-based lookups
// Format: {database}:idx:{index_name}:{column_value} → JSON array of rowids

// indexEntryKey returns the key for an index entry.
func (m *TableManager) indexEntryKey(indexName string, colValue interface{}) string {
	return fmt.Sprintf("%s:idx:%s:%s", m.database, strings.ToLower(indexName), formatIndexValue(colValue))
}

// indexPrefix returns the prefix for all entries of an index.
func (m *TableManager) indexPrefix(indexName string) string {
	return fmt.Sprintf("%s:idx:%s:", m.database, strings.ToLower(indexName))
}

func formatIndexValue(value interface{}) string {
	switch v := value.(type) {
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%f", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case int:
		return fmt.Sprintf("%d", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func rowIDFromRow(row Row) (int64, bool) {
	switch v := row["_rowid_"].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

func (m *TableManager) ensureIndex(index *Index) error {
	indexKey := strings.ToLower(index.Name)
	m.cacheMu.RLock()
	disabled := m.disabledIndexes[indexKey]
	_, initialized := m.indexCache[indexKey]
	m.cacheMu.RUnlock()
	if disabled {
		return nil
	}
	if initialized {
		return nil
	}

	// Serialize index build against writes to the same table so the derived
	// entries cannot miss a concurrently-inserted row.
	table := index.Table
	key := strings.ToLower(table)
	tl := m.tableLock(key)
	tl.Lock()
	defer tl.Unlock()

	m.cacheMu.RLock()
	disabled = m.disabledIndexes[indexKey]
	_, initialized = m.indexCache[indexKey]
	m.cacheMu.RUnlock()
	if disabled {
		return nil
	}
	if initialized {
		return nil
	}

	tableSchema, err := m.schema.GetSchema(table)
	if err != nil {
		return err
	}

	values := make(map[string][]int64)
	rowKeys := make(map[int64]string)
	if err := m.scanRows(table, func(row Row) (bool, error) {
		rowid, ok := rowIDFromRow(row)
		if !ok {
			return false, nil
		}
		colValue, err := m.buildIndexValue(row, index.Columns)
		if err != nil {
			return true, err
		}
		valueKey := formatIndexValue(colValue)
		values[valueKey] = append(values[valueKey], rowid)
		rowKeys[rowid] = fmt.Sprintf("%v", row[tableSchema.PrimaryKey])
		return false, nil
	}); err != nil {
		return err
	}

	m.cacheMu.Lock()
	if _, initialized := m.indexCache[indexKey]; !initialized {
		m.indexCache[indexKey] = values
		m.indexTable[indexKey] = key
		m.rowKeyCache[key] = rowKeys
	}
	m.cacheMu.Unlock()

	return nil
}

// AddIndexEntry adds a rowid to an in-memory index entry.
func (m *TableManager) AddIndexEntry(indexName string, colValue interface{}, rowid int64) error {
	indexKey := strings.ToLower(indexName)
	valueKey := formatIndexValue(colValue)

	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	values, ok := m.indexCache[indexKey]
	if !ok {
		return nil
	}
	rowids := values[valueKey]
	for _, r := range rowids {
		if r == rowid {
			return nil
		}
	}
	values[valueKey] = append(rowids, rowid)
	return nil
}

// RemoveIndexEntry removes a rowid from an in-memory index entry.
func (m *TableManager) RemoveIndexEntry(indexName string, colValue interface{}, rowid int64) error {
	indexKey := strings.ToLower(indexName)
	valueKey := formatIndexValue(colValue)

	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()

	values, ok := m.indexCache[indexKey]
	if !ok {
		return nil
	}
	rowids := values[valueKey]
	newRowids := make([]int64, 0, len(rowids))
	for _, r := range rowids {
		if r != rowid {
			newRowids = append(newRowids, r)
		}
	}

	if len(newRowids) == 0 {
		delete(values, valueKey)
		return nil
	}

	values[valueKey] = newRowids
	return nil
}

// LookupIndex returns rowids matching a column value using the index.
func (m *TableManager) LookupIndex(indexName string, colValue interface{}) ([]int64, error) {
	index, err := m.schema.GetIndex(indexName)
	if err != nil {
		return nil, err
	}
	if err := m.ensureIndex(index); err != nil {
		return nil, err
	}

	indexKey := strings.ToLower(indexName)
	valueKey := formatIndexValue(colValue)

	m.cacheMu.RLock()
	rowids := append([]int64(nil), m.indexCache[indexKey][valueKey]...)
	m.cacheMu.RUnlock()

	return rowids, nil
}

// ClearIndex removes an index's in-memory entries and marks it disabled so a
// concurrent lookup cannot rebuild it after DROP but before the schema entry
// is removed. Index entries are derived from durable rows, so there is nothing
// durable to delete here.
func (m *TableManager) ClearIndex(indexName, tableName string, columns []string) error {
	indexKey := strings.ToLower(indexName)
	tableKey := strings.ToLower(tableName)
	m.cacheMu.Lock()
	delete(m.indexCache, indexKey)
	delete(m.indexTable, indexKey)
	m.disabledIndexes[indexKey] = true
	rowKeysNeeded := false
	for _, indexedTable := range m.indexTable {
		if indexedTable == tableKey {
			rowKeysNeeded = true
			break
		}
	}
	if !rowKeysNeeded {
		delete(m.rowKeyCache, tableKey)
	}
	m.cacheMu.Unlock()
	return nil
}

// BuildIndex builds index entries for all existing rows in a table.
func (m *TableManager) BuildIndex(indexName, tableName string, columns []string) error {
	indexKey := strings.ToLower(indexName)
	m.cacheMu.Lock()
	delete(m.disabledIndexes, indexKey)
	delete(m.indexCache, indexKey)
	delete(m.indexTable, indexKey)
	m.cacheMu.Unlock()

	index, err := m.schema.GetIndex(indexName)
	if err == nil {
		return m.ensureIndex(index)
	}
	if !errors.Is(err, ErrIndexNotFound) {
		return err
	}

	tableSchema, schemaErr := m.schema.GetSchema(tableName)
	if schemaErr != nil {
		return schemaErr
	}
	indexCols := make([]IndexColumn, len(columns))
	for i, name := range columns {
		indexCols[i] = IndexColumn{Name: name}
	}
	values := make(map[string][]int64)
	rowKeys := make(map[int64]string)
	if err := m.scanRows(tableName, func(row Row) (bool, error) {
		rowid, ok := rowIDFromRow(row)
		if !ok {
			return false, nil
		}
		colValue, err := m.buildIndexValue(row, indexCols)
		if err != nil {
			return true, err
		}
		values[formatIndexValue(colValue)] = append(values[formatIndexValue(colValue)], rowid)
		rowKeys[rowid] = fmt.Sprintf("%v", row[tableSchema.PrimaryKey])
		return false, nil
	}); err != nil {
		return err
	}

	m.cacheMu.Lock()
	m.indexCache[indexKey] = values
	m.indexTable[indexKey] = strings.ToLower(tableName)
	m.rowKeyCache[strings.ToLower(tableName)] = rowKeys
	m.cacheMu.Unlock()

	return nil
}

// indexColumnValue resolves a single index column's value for a row. A plain
// column is looked up case-insensitively; an expression column is evaluated
// through the registered expression evaluator.
func (m *TableManager) indexColumnValue(row Row, col IndexColumn) (interface{}, error) {
	if col.Expression != "" {
		eval := m.expressionEvaluator()
		if eval == nil {
			return nil, fmt.Errorf("expression index column %q requires an expression evaluator", col.Expression)
		}
		return eval(col.Expression, row)
	}
	if v, ok := row[col.Name]; ok {
		return v, nil
	}
	for k, v := range row {
		if strings.EqualFold(k, col.Name) {
			return v, nil
		}
	}
	return nil, nil
}

// buildIndexValue creates the index key value from an index's columns. Single
// column values are formatted directly; composite values are joined with a NUL
// separator.
func (m *TableManager) buildIndexValue(row Row, columns []IndexColumn) (string, error) {
	if len(columns) == 1 {
		v, err := m.indexColumnValue(row, columns[0])
		if err != nil {
			return "", err
		}
		return formatIndexValue(v), nil
	}

	parts := make([]string, 0, len(columns))
	for _, col := range columns {
		v, err := m.indexColumnValue(row, col)
		if err != nil {
			return "", err
		}
		parts = append(parts, formatIndexValue(v))
	}
	return strings.Join(parts, "\x00"), nil
}

// ── UNIQUE index enforcement ────────────────────────────────────────────────
//
// Uniqueness is enforced by scanning durable rows rather than by persisting a
// separate claim key. There is therefore no storage migration and no new durable
// state: a table with a UNIQUE index takes its exclusive table gate for writes so
// a validating scan can never race a concurrent writer. NULL values are exempt
// (SQLite permits multiple NULLs in a unique index), and composite values are
// encoded with length prefixes so "a\x00b" in one column can never collide with
// "a", "b" across two columns.

// uniqueIndexes returns the UNIQUE indexes defined on a table.
func (m *TableManager) uniqueIndexes(table string) ([]*Index, error) {
	indexes, err := m.schema.ListTableIndexes(table)
	if err != nil {
		return nil, err
	}
	var uniq []*Index
	for _, idx := range indexes {
		if idx.Unique {
			uniq = append(uniq, idx)
		}
	}
	return uniq, nil
}

// hasUniqueIndex reports whether the table has any UNIQUE index. An IO error is
// surfaced so callers never silently skip uniqueness enforcement.
func (m *TableManager) hasUniqueIndex(table string) (bool, error) {
	indexes, err := m.uniqueIndexes(table)
	if err != nil {
		return false, err
	}
	return len(indexes) > 0, nil
}

// HasUniqueIndex reports whether the table has any UNIQUE index, surfacing IO
// errors. It is the exported form used by the executor to decide whether to run a
// DML statement inside an implicit transaction for statement-level atomicity.
func (m *TableManager) HasUniqueIndex(table string) (bool, error) {
	return m.hasUniqueIndex(table)
}

// encodeUniqueValue encodes the indexed value of a row with per-column type tags
// and length prefixes. It returns isNull=true when any indexed column is NULL, in
// which case the row is exempt from uniqueness. The length prefix makes composite
// encodings collision-free. Expression columns are evaluated through the
// registered evaluator.
func (m *TableManager) encodeUniqueValue(row Row, columns []IndexColumn) (string, bool, error) {
	var sb strings.Builder
	for _, col := range columns {
		v, err := m.indexColumnValue(row, col)
		if err != nil {
			return "", false, err
		}
		if v == nil {
			return "", true, nil
		}
		s := encodeUniqueScalar(v)
		sb.WriteString(strconv.Itoa(len(s)))
		sb.WriteByte(':')
		sb.WriteString(s)
	}
	return sb.String(), false, nil
}

// encodeUniqueScalar encodes a scalar for uniqueness comparison. Integral
// numerics are canonicalized across their integer/float/unsigned Go
// representations so a computed value such as 1.5-0.5 (float64) collides with
// the literal 1 (int64) the same way SQLite's numeric affinity does. Non-integral
// reals keep a full-precision tag so no distinct value is lost. TEXT and BLOB
// remain distinct from numbers.
func encodeUniqueScalar(v interface{}) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "b1"
		}
		return "b0"
	case int:
		return "i" + strconv.FormatInt(int64(t), 10)
	case int64:
		return "i" + strconv.FormatInt(t, 10)
	case uint:
		return "i" + strconv.FormatUint(uint64(t), 10)
	case uint8:
		return "i" + strconv.FormatUint(uint64(t), 10)
	case uint16:
		return "i" + strconv.FormatUint(uint64(t), 10)
	case uint32:
		return "i" + strconv.FormatUint(uint64(t), 10)
	case uint64:
		return "i" + strconv.FormatUint(t, 10)
	case uintptr:
		return "i" + strconv.FormatUint(uint64(t), 10)
	case float64:
		if s, ok := encodeIntegralFloat(t); ok {
			return s
		}
		return "r" + strconv.FormatFloat(t, 'g', -1, 64)
	case string:
		return "s" + t
	case []byte:
		return "x" + string(t)
	default:
		return "s" + fmt.Sprintf("%v", t)
	}
}

// encodeIntegralFloat canonicalizes a whole float64 to the same "i" encoding used
// by integer values, without precision loss, so integral reals and integers are
// not treated as distinct under a unique index. It reports false (and returns
// nothing) for non-integral values or values outside the int64 range.
func encodeIntegralFloat(v float64) (string, bool) {
	if v != math.Trunc(v) {
		return "", false
	}
	if v < -9223372036854775808.0 || v >= 9223372036854775808.0 {
		return "", false
	}
	return "i" + strconv.FormatInt(int64(v), 10), true
}

// validateUniqueRows checks that pending rows do not conflict with one another or
// with durable rows on any UNIQUE index. excludedKeys lists the actual durable
// data keys whose rows are being replaced in this operation (delete, or an update
// of a non-indexed column), so those rows' own unique values do not self-conflict
// and swapping two unique values remains possible. Matching is done against the
// raw scanned KV key rather than a re-stringified primary key, so numeric and
// composite keys are unambiguous. The caller must hold the table's exclusive gate.
func (m *TableManager) validateUniqueRows(table string, pending []Row, excludedKeys map[string]bool) error {
	indexes, err := m.uniqueIndexes(table)
	if err != nil {
		return err
	}
	if len(indexes) == 0 {
		return nil
	}

	// seen tracks, per unique index, the encoded values already claimed by the
	// pending rows. Keying by index name (not just the encoded value) is essential:
	// a single row can legitimately carry the same value in two different unique
	// indexes (e.g. name and lower_name both "alice"), which must not collide with
	// itself. Two rows only conflict when they share a value within the SAME index.
	seen := make(map[string]map[string]bool, len(indexes))
	for _, row := range pending {
		for _, idx := range indexes {
			encoded, isNull, err := m.encodeUniqueValue(row, idx.Columns)
			if err != nil {
				return err
			}
			if isNull {
				continue
			}
			perIndex := seen[idx.Name]
			if perIndex == nil {
				perIndex = make(map[string]bool)
				seen[idx.Name] = perIndex
			}
			if perIndex[encoded] {
				return fmt.Errorf("UNIQUE constraint failed: %s", idx.Name)
			}
			perIndex[encoded] = true
		}
	}

	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanWithLimit([]byte(m.dataPrefix(table)), scanPageSize)
		if err != nil {
			return err
		}
		defer func() {
			if err := cursor.Close(); retErr == nil {
				retErr = err
			}
		}()
		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, e := range entries {
				if excludedKeys != nil && excludedKeys[string(e.Key)] {
					continue
				}
				row, err := decodeRow(e.Value)
				if err != nil {
					return err
				}
				for _, idx := range indexes {
					encoded, isNull, encErr := m.encodeUniqueValue(row, idx.Columns)
					if encErr != nil {
						return encErr
					}
					if isNull {
						continue
					}
					if perIndex := seen[idx.Name]; perIndex != nil && perIndex[encoded] {
						return fmt.Errorf("UNIQUE constraint failed: %s", idx.Name)
					}
				}
			}
			if done {
				return nil
			}
		}
	})
}

// excludedKey returns the exclusion set that exempts a single row's durable value
// from the uniqueness scan, keyed by its actual durable data key.
func excludedKey(dataKey string) map[string]bool {
	return map[string]bool{dataKey: true}
}

// lockForWrite acquires the table gate appropriate for a point write: exclusive
// when the table has a UNIQUE index (so the validating scan cannot race another
// writer), shared otherwise. The uniqueness decision is re-checked under the lock
// so a concurrent CREATE UNIQUE INDEX cannot slip in between the unlocked probe
// and the lock acquisition; the returned unlock closes the gate.
func (m *TableManager) lockForWrite(table string) (unlock func(), unique bool, err error) {
	tl := m.tableLock(table)
	for {
		uniq, err := m.hasUniqueIndex(table)
		if err != nil {
			return nil, false, err
		}
		if uniq {
			tl.Lock()
			// Re-check under the exclusive gate (authoritative): CreateUniqueIndex
			// also takes the exclusive gate, so it cannot run while we hold it.
			uniq, err = m.hasUniqueIndex(table)
			if err != nil {
				tl.Unlock()
				return nil, false, err
			}
			if uniq {
				return tl.Unlock, true, nil
			}
			tl.Unlock()
			continue
		}
		tl.RLock()
		// A unique index could have appeared before we acquired the shared gate;
		// re-check and escalate if so.
		uniq, err = m.hasUniqueIndex(table)
		if err != nil {
			tl.RUnlock()
			return nil, false, err
		}
		if uniq {
			tl.RUnlock()
			continue
		}
		return tl.RUnlock, false, nil
	}
}

// ValidateUniqueIndex rejects a UNIQUE index definition if existing rows already
// contain duplicate non-NULL values for the indexed columns.
func (m *TableManager) ValidateUniqueIndex(index *Index) error {
	seen := make(map[string]bool)
	return m.scanRows(index.Table, func(row Row) (bool, error) {
		encoded, isNull, err := m.encodeUniqueValue(row, index.Columns)
		if err != nil {
			return true, err
		}
		if isNull {
			return false, nil
		}
		if seen[encoded] {
			return true, fmt.Errorf("UNIQUE constraint failed: %s", index.Name)
		}
		seen[encoded] = true
		return false, nil
	})
}

// CreateUniqueIndex validates existing rows and registers the index while
// holding the table's exclusive gate, so a concurrent writer cannot insert a
// conflicting value between validation and registration.
func (m *TableManager) CreateUniqueIndex(index *Index) error {
	tl := m.tableLock(index.Table)
	tl.Lock()
	defer tl.Unlock()
	if err := m.ValidateUniqueIndex(index); err != nil {
		return err
	}
	return m.schema.CreateIndex(index)
}

type indexedRowVersion struct {
	row Row
	key string
	lsn uint64
}

type indexPredicateSnapshot struct {
	valueKey    string
	valueGen    uint64
	wildcardKey string
	wildcardGen uint64
}

// selectByIndexWithLSN retrieves indexed rows and their durable versions. The
// transaction layer uses the versions for optimistic commit validation.
func (m *TableManager) selectByIndexWithLSN(table, indexName string, colValue interface{}) ([]indexedRowVersion, indexPredicateSnapshot, error) {
	return m.selectByIndexKeyWithLSN(table, indexName, formatIndexValue(colValue))
}

// IndexRowKey computes the formatted in-memory index key a row maps to for an
// index. Expression columns are evaluated through the same registered evaluator
// used by uniqueness enforcement, so the key matches both durable index entries
// and staged overlay rows.
func (m *TableManager) IndexRowKey(idx *Index, row Row) (string, error) {
	value, err := m.buildIndexValue(row, idx.Columns)
	if err != nil {
		return "", err
	}
	return formatIndexValue(value), nil
}

// IndexValueContainsNull reports whether any column contributing to an index
// key evaluates to NULL. UNIQUE indexes exempt such rows from conflicts under
// SQLite semantics.
func (m *TableManager) IndexValueContainsNull(idx *Index, row Row) (bool, error) {
	_, isNull, err := m.encodeUniqueValue(row, idx.Columns)
	return isNull, err
}

// selectByIndexKeyWithLSN is selectByIndexWithLSN with an already-computed
// formatted index key, so callers can look up composite and expression indexes
// without re-deriving the key from a single column value.
func (m *TableManager) selectByIndexKeyWithLSN(table, indexName, valueKey string) ([]indexedRowVersion, indexPredicateSnapshot, error) {
	index, err := m.schema.GetIndex(indexName)
	if err != nil {
		return nil, indexPredicateSnapshot{}, err
	}
	if err := m.ensureIndex(index); err != nil {
		return nil, indexPredicateSnapshot{}, err
	}

	tableKey := strings.ToLower(table)
	tl := m.tableLock(tableKey)
	tl.RLock()
	defer tl.RUnlock()
	if !m.schema.TableExists(table) {
		return nil, indexPredicateSnapshot{}, fmt.Errorf("table not found: %s", table)
	}

	indexKey := strings.ToLower(indexName)
	predicateKey, predicateGen, wildcardKey, wildcardGen := m.predicateSnapshot(table, indexName, valueKey)
	snapshot := indexPredicateSnapshot{
		valueKey: predicateKey, valueGen: predicateGen,
		wildcardKey: wildcardKey, wildcardGen: wildcardGen,
	}
	m.cacheMu.RLock()
	rowids := append([]int64(nil), m.indexCache[indexKey][valueKey]...)
	primaryKeys := make([]string, 0, len(rowids))
	missingRowID := int64(0)
	missingRowKey := false
	for _, rowid := range rowids {
		if primaryKey, ok := m.rowKeyCache[tableKey][rowid]; ok {
			primaryKeys = append(primaryKeys, primaryKey)
		} else {
			missingRowID = rowid
			missingRowKey = true
			break
		}
	}
	m.cacheMu.RUnlock()
	if missingRowKey {
		return nil, indexPredicateSnapshot{}, fmt.Errorf("index %s is missing rowid %d", indexName, missingRowID)
	}

	// If no rowids found, return empty result
	if len(rowids) == 0 {
		return []indexedRowVersion{}, snapshot, nil
	}

	rows := make([]indexedRowVersion, 0, len(primaryKeys))
	err = m.pool.WithClient(func(client *KVClient) error {
		keys := make([][]byte, len(primaryKeys))
		for i, primaryKey := range primaryKeys {
			keys[i] = []byte(m.dataKey(table, primaryKey))
		}
		results, err := client.MultiGet(keys)
		if err != nil {
			return err
		}
		for i, result := range results {
			if !result.Found {
				primaryKey := primaryKeys[i]
				return fmt.Errorf("index %s references missing primary key %s", indexName, primaryKey)
			}
			row, err := decodeRow(result.Value)
			if err != nil {
				return err
			}
			rows = append(rows, indexedRowVersion{
				row: row,
				key: m.dataKey(table, primaryKeys[i]),
				lsn: result.LSN,
			})
		}
		return nil
	})
	if err != nil {
		return nil, indexPredicateSnapshot{}, err
	}
	return rows, snapshot, nil
}

// SelectByIndex retrieves rows using an in-memory equality index followed by a
// single MultiGet for the matching primary keys.
func (m *TableManager) SelectByIndex(table, indexName string, colValue interface{}) ([]Row, error) {
	versions, _, err := m.selectByIndexWithLSN(table, indexName, colValue)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, len(versions))
	for i := range versions {
		rows[i] = versions[i].row
	}
	return rows, nil
}
