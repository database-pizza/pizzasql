package storage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrSerialization is returned when a transaction's optimistic validation fails
// because a concurrent transaction committed a conflicting change.
var ErrSerialization = fmt.Errorf("serialization failure: concurrent transaction modified the database")

// Session is a buffered SQL transaction/session wrapper around a TableManager.
// While in a transaction, writes are staged in memory, reads observe that
// staged overlay, and COMMIT issues a single atomic compare-and-swap batch
// write. Rollback simply discards the staged changes (no compensating durable
// writes). A Session is used for both autocommit statements (where operations
// delegate straight to the durable TableManager) and buffered transactions.
type Session struct {
	schema *SchemaManager
	table  *TableManager

	mu sync.Mutex

	inTx    bool
	aborted bool

	// overlay holds staged writes keyed by lowercased table then data key.
	overlay map[string]map[string]*overlayEntry

	// log records mutations in order so savepoints can roll back.
	log []txMutation

	// reads records the durable LSN of every key the transaction observed
	// (0 means the key was observed absent). It becomes the compare set at
	// commit and also captures the base LSN of every written key.
	reads map[string]uint64

	// scanGens records the per-table generation captured by the first scan of
	// each table, validated at commit to detect phantoms. Indexed equality reads
	// use predicateGens so writes to other index values do not cause conflicts.
	scanGens      map[string]uint64
	predicateGens map[string]predicateRead
}

type predicateRead struct {
	table string
	gen   uint64
}

type overlayEntry struct {
	row    Row
	absent bool
}

type txMutation struct {
	table string
	key   string
	prev  *overlayEntry
}

// NewSession creates a session wrapping the given schema and table managers.
func NewSession(schema *SchemaManager, table *TableManager) *Session {
	return &Session{
		schema:        schema,
		table:         table,
		overlay:       make(map[string]map[string]*overlayEntry),
		reads:         make(map[string]uint64),
		scanGens:      make(map[string]uint64),
		predicateGens: make(map[string]predicateRead),
	}
}

// Begin starts a buffered transaction.
func (s *Session) Begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inTx {
		return fmt.Errorf("cannot start a transaction within a transaction")
	}
	s.inTx = true
	s.aborted = false
	s.overlay = make(map[string]map[string]*overlayEntry)
	s.log = nil
	s.reads = make(map[string]uint64)
	s.scanGens = make(map[string]uint64)
	s.predicateGens = make(map[string]predicateRead)
	return nil
}

// InTx reports whether a transaction is in progress.
func (s *Session) InTx() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inTx
}

// Abort marks the transaction as aborted without discarding state.
func (s *Session) Abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inTx {
		s.aborted = true
	}
}

// Snapshot returns the current mutation-log position for a savepoint.
func (s *Session) Snapshot() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.log)
}

// RollbackTo discards mutations after the given savepoint position.
func (s *Session) RollbackTo(pos int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.log) - 1; i >= pos && i >= 0; i-- {
		m := s.log[i]
		if m.prev == nil {
			delete(s.overlay[m.table], m.key)
			if len(s.overlay[m.table]) == 0 {
				delete(s.overlay, m.table)
			}
		} else {
			if s.overlay[m.table] == nil {
				s.overlay[m.table] = make(map[string]*overlayEntry)
			}
			s.overlay[m.table][m.key] = m.prev
		}
	}
	if pos < len(s.log) {
		s.log = s.log[:pos]
	}
	s.aborted = false
}

// Rollback discards the transaction without writing anything durable.
func (s *Session) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return fmt.Errorf("cannot rollback: no transaction in progress")
	}
	s.resetLocked()
	return nil
}

// Commit validates and durably applies the staged transaction in one atomic
// compare-and-swap batch write.
func (s *Session) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return fmt.Errorf("cannot commit: no transaction in progress")
	}
	if s.aborted {
		s.resetLocked()
		return fmt.Errorf("current transaction is aborted")
	}

	// Collect affected tables and lock them in sorted order. Tables read through
	// scans or predicates need an exclusive validation gate. Tables that are
	// only written use the shared publication gate, allowing disjoint optimistic
	// commits to proceed concurrently while still excluding scanner commits.
	affected := make(map[string]bool)
	for t := range s.overlay {
		affected[t] = false
	}
	for t := range s.scanGens {
		affected[t] = true
	}
	for _, predicate := range s.predicateGens {
		affected[predicate.table] = true
	}
	tables := make([]string, 0, len(affected))
	for t := range affected {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	locks := make([]*sync.RWMutex, len(tables))
	exclusive := make([]bool, len(tables))
	for i, t := range tables {
		locks[i] = s.table.tableLock(t)
		exclusive[i] = affected[t]
	}
	for i, lock := range locks {
		if exclusive[i] {
			lock.Lock()
		} else {
			lock.RLock()
		}
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			if exclusive[i] {
				locks[i].Unlock()
			} else {
				locks[i].RUnlock()
			}
		}
	}()

	// Validate scan generations for phantom detection.
	for t, gen := range s.scanGens {
		if s.table.generation(t) != gen {
			s.resetLocked()
			return ErrSerialization
		}
	}
	for key, predicate := range s.predicateGens {
		if s.table.predicateGeneration(key) != predicate.gen {
			s.resetLocked()
			return ErrSerialization
		}
	}

	// Build the compare set from every observed key.
	checks := make([]CompareCheck, 0, len(s.reads))
	for key, lsn := range s.reads {
		checks = append(checks, CompareCheck{Key: []byte(key), LSN: lsn})
	}
	sort.Slice(checks, func(i, j int) bool { return string(checks[i].Key) < string(checks[j].Key) })

	// Build the batch ops from the staged overlay.
	ops := make([]BatchOp, 0)
	for _, entries := range s.overlay {
		for key, e := range entries {
			if e.absent {
				ops = append(ops, BatchOp{Op: batchDelete, Key: []byte(key)})
			} else {
				data, err := encodeRow(e.row)
				if err != nil {
					return err
				}
				ops = append(ops, BatchOp{Op: batchPut, Key: []byte(key), Value: data})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return string(ops[i].Key) < string(ops[j].Key) })

	// A read-only transaction has nothing to write; commit trivially.
	if len(ops) == 0 {
		s.resetLocked()
		return nil
	}

	var committed bool
	err := s.table.pool.WithClient(func(c *KVClient) error {
		_, ok, err := c.CompareBatchWrite(checks, ops, nil)
		committed = ok
		return err
	})
	if err != nil {
		return err
	}
	if !committed {
		s.resetLocked()
		return ErrSerialization
	}

	// Advance generations and invalidate derived caches for written tables.
	for t := range s.overlay {
		s.table.InvalidateCache(t)
		s.table.bumpIndexPredicateWildcard(t)
		s.table.bumpGeneration(t)
	}

	s.resetLocked()
	return nil
}

func (s *Session) resetLocked() {
	s.inTx = false
	s.aborted = false
	s.overlay = make(map[string]map[string]*overlayEntry)
	s.log = nil
	s.reads = make(map[string]uint64)
	s.scanGens = make(map[string]uint64)
	s.predicateGens = make(map[string]predicateRead)
}

func (s *Session) stagePut(table, key string, row Row) {
	tl := strings.ToLower(table)
	if s.overlay[tl] == nil {
		s.overlay[tl] = make(map[string]*overlayEntry)
	}
	s.log = append(s.log, txMutation{table: tl, key: key, prev: s.overlay[tl][key]})
	s.overlay[tl][key] = &overlayEntry{row: cloneRow(row)}
	if _, ok := s.reads[key]; !ok {
		s.reads[key] = 0
	}
}

func (s *Session) stageDelete(table, key string) {
	tl := strings.ToLower(table)
	if s.overlay[tl] == nil {
		s.overlay[tl] = make(map[string]*overlayEntry)
	}
	s.log = append(s.log, txMutation{table: tl, key: key, prev: s.overlay[tl][key]})
	s.overlay[tl][key] = &overlayEntry{absent: true}
	if _, ok := s.reads[key]; !ok {
		s.reads[key] = 0
	}
}

// GetByPK retrieves a row by primary key, observing the staged overlay in a
// transaction and recording the observed LSN for validation.
func (s *Session) GetByPK(table, pk string) (Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.GetByPK(table, pk)
	}
	key := s.table.dataKey(table, pk)
	tl := strings.ToLower(table)
	if e, ok := s.overlay[tl][key]; ok {
		if e.absent {
			return nil, ErrKeyNotFound
		}
		return cloneRow(e.row), nil
	}
	row, lsn, err := s.table.getByPKWithLSN(table, pk)
	if err != nil {
		if err == ErrKeyNotFound {
			s.reads[key] = 0
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	s.reads[key] = lsn
	return row, nil
}

// Select scans a table, merging the staged overlay so a transaction sees its
// own writes, and records per-row LSNs plus the table generation.
func (s *Session) Select(table string, filter func(Row) bool) ([]Row, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		return s.table.Select(table, filter)
	}
	defer s.mu.Unlock()
	return s.selectLocked(table, filter)
}

func (s *Session) selectLocked(table string, filter func(Row) bool) ([]Row, error) {
	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return nil, err
	}
	tl := strings.ToLower(table)
	tableLock := s.table.tableLock(tl)
	tableLock.RLock()
	defer tableLock.RUnlock()
	if _, ok := s.scanGens[tl]; !ok {
		s.scanGens[tl] = s.table.generation(tl)
	}
	overlay := s.overlay[tl]

	var rows []Row
	err = s.table.scanRowsWithLSN(table, func(row Row, lsn uint64) (bool, error) {
		key := s.table.dataKey(table, fmt.Sprintf("%v", row[schema.PrimaryKey]))
		if _, ok := overlay[key]; ok {
			return false, nil
		}
		s.reads[key] = lsn
		if filter == nil || filter(row) {
			rows = append(rows, row)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	for _, e := range overlay {
		if e.absent {
			continue
		}
		if filter == nil || filter(e.row) {
			rows = append(rows, cloneRow(e.row))
		}
	}
	return rows, nil
}

// SelectByIndex reads only matching durable rows while capturing their LSNs,
// then merges the transaction overlay. The table generation protects against
// matching rows being inserted or removed after the lookup.
func (s *Session) SelectByIndex(table, indexName string, colValue interface{}) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.SelectByIndex(table, indexName, colValue)
	}
	tableKey := strings.ToLower(table)
	index, err := s.schema.GetIndex(indexName)
	if err != nil {
		return nil, err
	}
	col := index.Columns[0].Name
	want := formatIndexValue(colValue)
	versions, predicate, err := s.table.selectByIndexWithLSN(table, indexName, colValue)
	if err != nil {
		return nil, err
	}
	if _, ok := s.predicateGens[predicate.valueKey]; !ok {
		s.predicateGens[predicate.valueKey] = predicateRead{table: tableKey, gen: predicate.valueGen}
	}
	if _, ok := s.predicateGens[predicate.wildcardKey]; !ok {
		s.predicateGens[predicate.wildcardKey] = predicateRead{table: tableKey, gen: predicate.wildcardGen}
	}
	overlay := s.overlay[tableKey]
	rows := make([]Row, 0, len(versions)+len(overlay))
	for _, version := range versions {
		if _, staged := overlay[version.key]; staged {
			continue
		}
		s.reads[version.key] = version.lsn
		rows = append(rows, version.row)
	}
	for _, entry := range overlay {
		if !entry.absent && formatIndexValue(entry.row[col]) == want {
			rows = append(rows, cloneRow(entry.row))
		}
	}
	return rows, nil
}

// CountFast returns the exact row count, observing the staged overlay in a
// transaction.
func (s *Session) CountFast(table string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.CountFast(table)
	}
	rows, err := s.selectLocked(table, nil)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// Insert stages an insert in a transaction, or performs a durable autocommit
// insert otherwise.
func (s *Session) Insert(table string, row Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.Insert(table, row)
	}

	nr, key, err := s.table.prepareInsert(table, row)
	if err != nil {
		return err
	}
	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return err
	}
	pk := fmt.Sprintf("%v", nr[schema.PrimaryKey])
	tl := strings.ToLower(table)

	if e, ok := s.overlay[tl][key]; ok {
		if !e.absent {
			return fmt.Errorf("duplicate primary key: %s", pk)
		}
	} else {
		_, lsn, err := s.table.getByPKWithLSN(table, pk)
		if err == nil {
			s.reads[key] = lsn
			return fmt.Errorf("duplicate primary key: %s", pk)
		}
		if err != ErrKeyNotFound {
			return err
		}
		s.reads[key] = 0
	}

	s.stagePut(table, key, nr)
	return nil
}

// InsertBulk stages or durably bulk-inserts multiple rows.
func (s *Session) InsertBulk(table string, rows []Row) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.InsertBulk(table, rows)
	}
	count := 0
	for _, row := range rows {
		if err := s.insertLocked(table, row); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// insertLocked is the transaction insert helper (caller holds s.mu).
func (s *Session) insertLocked(table string, row Row) error {
	nr, key, err := s.table.prepareInsert(table, row)
	if err != nil {
		return err
	}
	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return err
	}
	pk := fmt.Sprintf("%v", nr[schema.PrimaryKey])
	tl := strings.ToLower(table)

	if e, ok := s.overlay[tl][key]; ok {
		if !e.absent {
			return fmt.Errorf("duplicate primary key: %s", pk)
		}
	} else {
		_, lsn, err := s.table.getByPKWithLSN(table, pk)
		if err == nil {
			s.reads[key] = lsn
			return fmt.Errorf("duplicate primary key: %s", pk)
		}
		if err != ErrKeyNotFound {
			return err
		}
		s.reads[key] = 0
	}

	s.stagePut(table, key, nr)
	return nil
}

// UpdateByPK stages or durably applies a single-row update.
func (s *Session) UpdateByPK(table, pk string, updateFn func(Row) (Row, error)) (Row, bool, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		return s.table.UpdateByPK(table, pk, updateFn)
	}
	defer s.mu.Unlock()

	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return nil, false, err
	}
	key := s.table.dataKey(table, pk)

	row, err := s.getByPKLocked(table, pk)
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

	s.stagePut(table, key, row)
	return oldRow, true, nil
}

// DeleteByPK stages or durably applies a single-row delete.
func (s *Session) DeleteByPK(table, pk string) (Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.DeleteByPK(table, pk)
	}

	key := s.table.dataKey(table, pk)
	row, err := s.getByPKLocked(table, pk)
	if err == ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	s.stageDelete(table, key)
	return row, true, nil
}

// getByPKLocked reads a row observing the overlay (caller holds s.mu).
func (s *Session) getByPKLocked(table, pk string) (Row, error) {
	key := s.table.dataKey(table, pk)
	tl := strings.ToLower(table)
	if e, ok := s.overlay[tl][key]; ok {
		if e.absent {
			return nil, ErrKeyNotFound
		}
		return cloneRow(e.row), nil
	}
	row, lsn, err := s.table.getByPKWithLSN(table, pk)
	if err != nil {
		if err == ErrKeyNotFound {
			s.reads[key] = 0
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	s.reads[key] = lsn
	return row, nil
}

// UpdateFunc stages or durably applies a scan-based update.
func (s *Session) UpdateFunc(table string, updateFn func(Row) (Row, error), filter func(Row) bool) (int, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		return s.table.UpdateFunc(table, updateFn, filter)
	}
	defer s.mu.Unlock()

	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}
	rows, err := s.selectLocked(table, filter)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		updates, err := updateFn(row)
		if err != nil {
			return count, err
		}
		for name, value := range updates {
			for _, column := range schema.Columns {
				if strings.EqualFold(name, column.Name) {
					row[column.Name] = value
					break
				}
			}
		}
		key := s.table.dataKey(table, fmt.Sprintf("%v", row[schema.PrimaryKey]))
		s.stagePut(table, key, row)
		count++
	}
	return count, nil
}

// Delete stages or durably applies a scan-based delete.
func (s *Session) Delete(table string, filter func(Row) bool) (int, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		return s.table.Delete(table, filter)
	}
	defer s.mu.Unlock()

	schema, err := s.schema.GetSchema(table)
	if err != nil {
		return 0, err
	}
	rows, err := s.selectLocked(table, filter)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		key := s.table.dataKey(table, fmt.Sprintf("%v", row[schema.PrimaryKey]))
		s.stageDelete(table, key)
		count++
	}
	return count, nil
}

// ClearIndex passes through to the durable table manager.
func (s *Session) ClearIndex(indexName, tableName string, columns []string) error {
	return s.table.ClearIndex(indexName, tableName, columns)
}

// InvalidateCache passes through to the durable table manager.
func (s *Session) InvalidateCache(table string) {
	s.table.InvalidateCache(table)
}

// BuildIndex passes through to the durable table manager.
func (s *Session) BuildIndex(indexName, tableName string, columns []string) error {
	return s.table.BuildIndex(indexName, tableName, columns)
}
