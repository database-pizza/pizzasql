package storage

import (
	"fmt"
	"math"
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
}

// NewTableManager creates a new table manager.
func NewTableManager(pool *KVPool, schema *SchemaManager, database string) *TableManager {
	return &TableManager{
		pool:            pool,
		schema:          schema,
		database:        database,
		indexCache:      make(map[string]map[string][]int64),
		indexTable:      make(map[string]string),
		rowKeyCache:     make(map[string]map[int64]string),
		disabledIndexes: make(map[string]bool),
		counts:          make(map[string]int),
		countsInit:      make(map[string]bool),
		countGeneration: make(map[string]time.Time),
	}
}

func (m *TableManager) tableLock(key string) *sync.RWMutex {
	return m.schema.tableLock(key)
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
	tl := m.tableLock(key)
	tl.Lock()
	defer tl.Unlock()
	tableSchema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	m.cacheMu.Lock()
	if !m.countGeneration[key].Equal(tableSchema.CreatedAt) {
		delete(m.counts, key)
		delete(m.countsInit, key)
		m.countGeneration[key] = tableSchema.CreatedAt
	}
	init := m.countsInit[key]
	n := m.counts[key]
	m.cacheMu.Unlock()
	if init {
		return n, nil
	}

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

// incrCount adjusts the derived per-table row count. It is a no-op until the
// count has been initialized, since an uninitialized count is re-derived from
// durable rows (which already reflect the write) on next use.
func (m *TableManager) incrCount(table string, generation time.Time, delta int) {
	key := strings.ToLower(table)
	m.cacheMu.Lock()
	if !m.countGeneration[key].Equal(generation) {
		delete(m.counts, key)
		delete(m.countsInit, key)
		m.countGeneration[key] = generation
	}
	if m.countsInit[key] {
		m.counts[key] += delta
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

// Insert inserts a new row.
func (m *TableManager) Insert(table string, row Row) error {
	tl := m.tableLock(table)
	tl.Lock()
	defer tl.Unlock()

	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return err
	}

	// Get primary key value
	pkValue, ok := row[schema.PrimaryKey]
	if !ok {
		// Try case-insensitive lookup
		for k, v := range row {
			if strings.EqualFold(k, schema.PrimaryKey) {
				pkValue = v
				ok = true
				break
			}
		}
	}

	// Check if PK is INTEGER PRIMARY KEY (implicit ROWID alias)
	pkCol, _ := schema.GetColumn(schema.PrimaryKey)
	isIntegerPK := pkCol != nil && isIntegerType(pkCol.Type)

	// Auto-generate ROWID if no primary key provided or if it's INTEGER PRIMARY KEY
	var rowid int64
	if !ok || pkValue == nil {
		if isIntegerPK || !ok {
			// Generate ROWID
			rowid, err = m.schema.GetNextRowID(table)
			if err != nil {
				return err
			}
			pkValue = rowid
			row[schema.PrimaryKey] = rowid
			ok = true
		} else {
			return fmt.Errorf("missing primary key: %s", schema.PrimaryKey)
		}
	} else if isIntegerPK {
		// User provided INTEGER PRIMARY KEY value - track it
		switch v := pkValue.(type) {
		case int64:
			rowid = v
		case float64:
			if math.Trunc(v) != v {
				return fmt.Errorf("invalid integer primary key: %v", v)
			}
			rowid = int64(v)
		case int:
			rowid = int64(v)
		default:
			rowid = 0
		}
		if rowid > 0 {
			m.schema.UpdateMaxRowID(table, rowid)
		}
	}

	pk := fmt.Sprintf("%v", pkValue)

	// Keep the duplicate check and write in one per-table critical section so
	// concurrent inserts of the same primary key cannot both persist a single
	// durable row and double-count it.
	key := m.dataKey(table, pk)
	var exists bool
	err = m.pool.WithClient(func(c *KVClient) error {
		var e error
		exists, e = c.Exists([]byte(key))
		return e
	})
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("duplicate primary key: %s", pk)
	}

	// Validate required columns
	for _, col := range schema.Columns {
		if !col.Nullable && col.Default == nil {
			val, hasVal := row[col.Name]
			if !hasVal {
				// Try case-insensitive lookup
				for k, v := range row {
					if strings.EqualFold(k, col.Name) {
						val = v
						hasVal = true
						break
					}
				}
			}
			if !hasVal || val == nil {
				return fmt.Errorf("missing required column: %s", col.Name)
			}
		}
	}

	// Normalize column names to match schema
	normalizedRow := make(Row)
	for _, col := range schema.Columns {
		for k, v := range row {
			if strings.EqualFold(k, col.Name) {
				normalizedRow[col.Name] = v
				break
			}
		}
	}

	// Apply defaults
	for _, col := range schema.Columns {
		if _, ok := normalizedRow[col.Name]; !ok && col.Default != nil {
			normalizedRow[col.Name] = col.Default
		}
	}

	// Store ROWID (use PK value for INTEGER PRIMARY KEY, otherwise generate)
	if rowid > 0 {
		normalizedRow["_rowid_"] = rowid
	} else {
		// Generate ROWID for non-integer primary keys
		newRowID, _ := m.schema.GetNextRowID(table)
		normalizedRow["_rowid_"] = newRowID
	}

	// Serialize row
	data, err := encodeRow(normalizedRow)
	if err != nil {
		return fmt.Errorf("failed to serialize row: %w", err)
	}

	err = m.pool.WithClient(func(c *KVClient) error {
		_, err := c.Put([]byte(key), data)
		return err
	})
	if err != nil {
		return err
	}

	// Update in-memory indexes only. Durable index entries are derived from rows.
	m.updateIndexesForRow(table, normalizedRow, true)

	m.incrCount(table, schema.CreatedAt, 1)
	return nil
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
	if len(rows) == 0 {
		return 0, nil
	}
	tl := m.tableLock(table)
	tl.Lock()
	defer tl.Unlock()

	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

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
					return 0, fmt.Errorf("invalid integer primary key: %v", v)
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
			if schema.PrimaryKey != "_rowid_" {
				pk, ok := nr[schema.PrimaryKey]
				if !ok || pk == nil {
					return 0, fmt.Errorf("missing primary key: %s", schema.PrimaryKey)
				}
			}
			rowid, err = m.schema.GetNextRowID(table)
			if err != nil {
				return 0, err
			}
			if schema.PrimaryKey == "_rowid_" {
				nr[schema.PrimaryKey] = rowid
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
			return 0, err
		}
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(m.dataKey(table, pk)), Value: data})
		encoded = append(encoded, nr)
	}
	keys := make([][]byte, len(ops))
	seen := make(map[string]struct{}, len(ops))
	for i, op := range ops {
		key := string(op.Key)
		if _, duplicate := seen[key]; duplicate {
			return 0, fmt.Errorf("duplicate primary key: %s", key)
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
		return 0, err
	}
	for i, found := range existing {
		if found {
			return 0, fmt.Errorf("duplicate primary key: %s", ops[i].Key)
		}
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
		m.updateIndexesForRow(table, encoded[i], true)
	}
	m.incrCount(table, schema.CreatedAt, numOK)

	return numOK, firstErr
}

// updateIndexesForRow adds or removes entries from already-built in-memory
// indexes. Index entries are rebuildable from durable row data, so this method
// intentionally does not write idx:* keys to KV.
func (m *TableManager) updateIndexesForRow(table string, row Row, add bool) {
	indexes, err := m.schema.ListTableIndexes(table)
	if err != nil || len(indexes) == 0 {
		return
	}

	rowid, ok := rowIDFromRow(row)
	if !ok {
		return
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

		columns := make([]string, len(idx.Columns))
		for i, col := range idx.Columns {
			columns[i] = col.Name
		}
		colValue := m.buildIndexValue(row, columns)

		if add {
			m.AddIndexEntry(idx.Name, colValue, rowid)
		} else {
			m.RemoveIndexEntry(idx.Name, colValue, rowid)
		}
	}
}

// Select retrieves rows from a table by scanning durable rows and collecting
// only matching rows.
func (m *TableManager) Select(table string, filter func(Row) bool) ([]Row, error) {
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()

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
	// Get all rows
	rows, err := m.Select(table, filter)
	if err != nil {
		return 0, err
	}

	tl := m.tableLock(strings.ToLower(table))
	tl.Lock()
	defer tl.Unlock()
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, row := range rows {
		// Snapshot the pre-update row so removed index entries can be restored
		// if persistence fails.
		oldRow := cloneRow(row)
		m.updateIndexesForRow(table, row, false)

		// Apply updates
		for k, v := range updates {
			// Normalize column name
			for _, col := range schema.Columns {
				if strings.EqualFold(k, col.Name) {
					row[col.Name] = v
					break
				}
			}
		}

		// Get primary key
		pkValue := row[schema.PrimaryKey]
		pk := fmt.Sprintf("%v", pkValue)

		// Serialize row
		data, err := encodeRow(row)
		if err != nil {
			m.updateIndexesForRow(table, oldRow, true)
			continue
		}

		// Write back
		key := m.dataKey(table, pk)
		err = m.pool.WithClient(func(c *KVClient) error {
			_, err := c.Put([]byte(key), data)
			return err
		})
		if err == nil {
			// Add new index entries after update
			m.updateIndexesForRow(table, row, true)
			count++
		} else {
			m.updateIndexesForRow(table, oldRow, true)
		}
	}

	return count, nil
}

// UpdateFunc updates rows matching the filter using a function to compute new values.
// The updateFn receives the current row and returns the updates to apply.
func (m *TableManager) UpdateFunc(table string, updateFn func(Row) (Row, error), filter func(Row) bool) (int, error) {
	// Get all rows
	rows, err := m.Select(table, filter)
	if err != nil {
		return 0, err
	}

	tl := m.tableLock(strings.ToLower(table))
	tl.Lock()
	defer tl.Unlock()
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, row := range rows {
		oldRow := cloneRow(row)
		m.updateIndexesForRow(table, row, false)

		// Compute updates using the provided function
		updates, err := updateFn(row)
		if err != nil {
			m.updateIndexesForRow(table, oldRow, true)
			return count, err
		}

		// Apply updates
		for k, v := range updates {
			// Normalize column name
			for _, col := range schema.Columns {
				if strings.EqualFold(k, col.Name) {
					row[col.Name] = v
					break
				}
			}
		}

		// Get primary key
		pkValue := row[schema.PrimaryKey]
		pk := fmt.Sprintf("%v", pkValue)

		// Serialize row
		data, err := encodeRow(row)
		if err != nil {
			m.updateIndexesForRow(table, oldRow, true)
			continue
		}

		// Write back
		key := m.dataKey(table, pk)
		err = m.pool.WithClient(func(c *KVClient) error {
			_, err := c.Put([]byte(key), data)
			return err
		})
		if err == nil {
			// Add new index entries after update
			m.updateIndexesForRow(table, row, true)
			count++
		} else {
			m.updateIndexesForRow(table, oldRow, true)
		}
	}

	return count, nil
}

// Delete deletes rows matching the filter.
func (m *TableManager) Delete(table string, filter func(Row) bool) (int, error) {
	// Get all rows
	rows, err := m.Select(table, filter)
	if err != nil {
		return 0, err
	}

	tl := m.tableLock(strings.ToLower(table))
	tl.Lock()
	defer tl.Unlock()
	schema, err := m.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, row := range rows {
		// Remove index entries before deleting row
		m.updateIndexesForRow(table, row, false)

		pkValue := row[schema.PrimaryKey]
		pk := fmt.Sprintf("%v", pkValue)
		key := m.dataKey(table, pk)

		err = m.pool.WithClient(func(c *KVClient) error {
			_, err := c.Del([]byte(key))
			return err
		})
		if err == nil {
			count++
		} else {
			// Restore the index entries removed above.
			m.updateIndexesForRow(table, row, true)
		}
	}

	m.incrCount(table, schema.CreatedAt, -count)
	return count, nil
}

// GetByPK retrieves a row by primary key.
func (m *TableManager) GetByPK(table string, pk string) (Row, error) {
	tl := m.tableLock(table)
	tl.RLock()
	defer tl.RUnlock()

	if !m.schema.TableExists(table) {
		return nil, fmt.Errorf("table not found: %s", table)
	}

	key := m.dataKey(table, pk)
	var value []byte

	err := m.pool.WithClient(func(c *KVClient) error {
		res, err := c.Get([]byte(key))
		if err != nil {
			return err
		}
		value = res.Value
		return nil
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, fmt.Errorf("row not found: %s", pk)
		}
		return nil, err
	}

	row, err := decodeRow(value)
	if err != nil {
		return nil, fmt.Errorf("failed to parse row: %w", err)
	}

	return row, nil
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

	columns := make([]string, len(index.Columns))
	for i, col := range index.Columns {
		columns[i] = col.Name
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
		colValue := m.buildIndexValue(row, columns)
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

	tableSchema, schemaErr := m.schema.GetSchema(tableName)
	if schemaErr != nil {
		return schemaErr
	}
	values := make(map[string][]int64)
	rowKeys := make(map[int64]string)
	if err := m.scanRows(tableName, func(row Row) (bool, error) {
		rowid, ok := rowIDFromRow(row)
		if !ok {
			return false, nil
		}
		colValue := m.buildIndexValue(row, columns)
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

// buildIndexValue creates the index key value from row columns.
func (m *TableManager) buildIndexValue(row Row, columns []string) string {
	formatValue := func(v interface{}) string {
		switch val := v.(type) {
		case float64:
			// Check if it's actually an integer value
			if val == float64(int64(val)) {
				return fmt.Sprintf("%d", int64(val))
			}
			return fmt.Sprintf("%f", val)
		case int64:
			return fmt.Sprintf("%d", val)
		case int:
			return fmt.Sprintf("%d", val)
		default:
			return fmt.Sprintf("%v", val)
		}
	}

	if len(columns) == 1 {
		return formatValue(row[columns[0]])
	}

	// Multi-column index: concatenate values with separator
	var parts []string
	for _, col := range columns {
		parts = append(parts, formatValue(row[col]))
	}
	return strings.Join(parts, "\x00")
}

// SelectByIndex retrieves rows using an index lookup. It obtains the matching
// rowids from the in-memory index, then streams the table's rows and returns
// only those whose rowid is indexed, without retaining a permanent row map.
func (m *TableManager) SelectByIndex(table, indexName string, colValue interface{}) ([]Row, error) {
	index, err := m.schema.GetIndex(indexName)
	if err != nil {
		return nil, err
	}
	if err := m.ensureIndex(index); err != nil {
		return nil, err
	}

	tableKey := strings.ToLower(table)
	tl := m.tableLock(tableKey)
	tl.RLock()
	defer tl.RUnlock()
	if !m.schema.TableExists(table) {
		return nil, fmt.Errorf("table not found: %s", table)
	}

	indexKey := strings.ToLower(indexName)
	valueKey := formatIndexValue(colValue)
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
		return nil, fmt.Errorf("index %s is missing rowid %d", indexName, missingRowID)
	}

	// If no rowids found, return empty result
	if len(rowids) == 0 {
		return []Row{}, nil
	}

	rows := make([]Row, 0, len(primaryKeys))
	err = m.pool.WithClient(func(client *KVClient) error {
		for _, primaryKey := range primaryKeys {
			result, err := client.Get([]byte(m.dataKey(table, primaryKey)))
			if err == ErrKeyNotFound {
				return fmt.Errorf("index %s references missing primary key %s", indexName, primaryKey)
			}
			if err != nil {
				return err
			}
			row, err := decodeRow(result.Value)
			if err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
