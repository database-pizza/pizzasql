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

	// schemaTx is the transaction-scoped catalog overlay. It exists only while a
	// transaction is open: DDL stages into it instead of publishing, and commit
	// merges it into the same batch write as the staged rows. A nil overlay means
	// every catalog read goes straight to the shared SchemaManager.
	schemaTx *SchemaTx

	// fkCatalog caches the foreign-key metadata of the schemaTx view, keyed by
	// the catalog version that produced it.
	fkCatalog *foreignKeyCatalog
	fkVersion uint64
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
	s.schemaTx = newSchemaTx(s)
	s.fkCatalog = nil
	s.fkVersion = 0
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
	s.rollbackToLocked(pos)
	s.aborted = false
}

// SessionSavepoint captures the complete session state at a SQL SAVEPOINT: the
// staged rows with their mutation log and read set, and the staged DDL catalog.
// A rollback restores all of it, so DDL issued after a savepoint is undone with
// the rows that accompanied it.
type SessionSavepoint struct {
	datalog       int
	overlay       map[string]map[string]*overlayEntry
	mutationLog   []txMutation
	reads         map[string]uint64
	scanGens      map[string]uint64
	predicateGens map[string]predicateRead
	schema        *schemaTxSnapshot
}

// Savepoint captures the session state a later RollbackToSavepoint restores.
func (s *Session) Savepoint() *SessionSavepoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savepointLocked()
}

// savepointLocked captures the session state (caller holds the session lock).
func (s *Session) savepointLocked() *SessionSavepoint {
	sp := &SessionSavepoint{
		datalog:       len(s.log),
		overlay:       make(map[string]map[string]*overlayEntry, len(s.overlay)),
		mutationLog:   append([]txMutation(nil), s.log...),
		reads:         copyLSNMap(s.reads),
		scanGens:      copyLSNMap(s.scanGens),
		predicateGens: make(map[string]predicateRead, len(s.predicateGens)),
	}
	for table, entries := range s.overlay {
		staged := make(map[string]*overlayEntry, len(entries))
		for key, entry := range entries {
			staged[key] = entry
		}
		sp.overlay[table] = staged
	}
	for key, predicate := range s.predicateGens {
		sp.predicateGens[key] = predicate
	}
	if s.schemaTx != nil {
		snapshot := s.schemaTx.snapshot()
		sp.schema = &snapshot
	}
	return sp
}

// RollbackToSavepoint restores the state captured by Savepoint.
func (s *Session) RollbackToSavepoint(sp *SessionSavepoint) {
	if sp == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollbackToSavepointLocked(sp)
	s.aborted = false
}

// rollbackToSavepointLocked restores a savepoint (caller holds the session
// lock).
func (s *Session) rollbackToSavepointLocked(sp *SessionSavepoint) {
	s.overlay = sp.overlay
	s.log = sp.mutationLog
	s.reads = sp.reads
	s.scanGens = sp.scanGens
	s.predicateGens = sp.predicateGens
	if s.schemaTx != nil && sp.schema != nil {
		s.schemaTx.restore(*sp.schema)
	}
	s.fkCatalog = nil
	s.fkVersion = 0
}

func copyLSNMap(source map[string]uint64) map[string]uint64 {
	cloned := make(map[string]uint64, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
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
// compare-and-swap batch write. It discards the commit LSN; callers that need a
// ordering cursor use CommitWithLSN.
func (s *Session) Commit() error {
	_, err := s.CommitWithLSN()
	return err
}

// CommitWithLSN behaves like Commit and additionally returns the LSN assigned
// to the durable batch. A read-only or empty transaction returns zero because
// no new durable state was published.
func (s *Session) CommitWithLSN() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return 0, fmt.Errorf("cannot commit: no transaction in progress")
	}
	if s.aborted {
		s.resetLocked()
		return 0, fmt.Errorf("current transaction is aborted")
	}

	// Staged DDL is published by this same batch write, so the commit is atomic
	// across catalog changes and rows. hasDDL also forces the exclusive table
	// gates for the affected tables below, because the commit rewrites or
	// removes their durable rows.
	hasDDL := s.schemaTx != nil && s.schemaTx.hasDDL()

	// Lock order for every path that touches both a table and the catalog is
	// session lock -> foreign-key write gate -> table gates (sorted by name) ->
	// schema catalog mutex. Table gates are plain sync.RWMutex values and are
	// NOT reentrant, so nothing that re-acquires a table gate may run while the
	// commit still holds its validation gates; index cache maintenance is
	// therefore deferred until after releaseGates below.
	//
	// Referential integrity spans tables: a commit must validate the final
	// child rows against the final parent rows, including rows another table
	// staged in this same transaction. Take the foreign-key write gate before
	// any table gate (the canonical lock order) and register the constrained
	// component as scan reads so acquireCommitLocks holds it exclusively while
	// the validation scans run. A concurrent write on either side would bump one
	// of those generations and fail the commit with 40001 instead of committing
	// a dangling reference.
	closure, err := s.foreignKeyClosure()
	if err != nil {
		s.resetLocked()
		return 0, err
	}
	if len(closure) > 0 {
		s.table.foreignKeyMu.Lock()
		defer s.table.foreignKeyMu.Unlock()
	}
	for table := range closure {
		if _, ok := s.scanGens[table]; !ok {
			s.scanGens[table] = s.table.generation(table)
		}
	}

	// Acquire the per-table gates, escalating any table whose UNIQUE index
	// appeared after the initial unlocked probe. See acquireCommitLocks.
	_, _, unlockAll, uniqueTables, err := s.acquireCommitLocks()
	if err != nil {
		s.resetLocked()
		return 0, err
	}
	gatesHeld := true
	releaseGates := func() {
		if gatesHeld {
			unlockAll()
			gatesHeld = false
		}
	}
	defer releaseGates()

	// Another session may have published DDL while this transaction was open.
	// Re-read the committed catalog under the gates so the final row validation
	// below sees the catalog that is actually in effect.
	if s.schemaTx != nil {
		s.schemaTx.refreshCommitted()
		s.fkCatalog = nil
		s.fkVersion = 0
	}

	// A staged row was normalized against the schema as of its INSERT or UPDATE,
	// which a concurrent ADD COLUMN can predate. Re-apply the schema that is
	// current under the held gates before any derived state is computed: the CAS
	// check cannot reject such a row (a new key is only checked for absence), so
	// a NOT NULL column published after the row image was built would otherwise
	// be violated durably, and a UNIQUE index would not see the default the row
	// is about to receive. Run it before uniqueness and cache planning so both
	// observe the final values.
	if err := s.applyOverlaySchemaDefaults(); err != nil {
		s.resetLocked()
		return 0, err
	}

	// Validate scan generations for phantom detection.
	for t, gen := range s.scanGens {
		if s.table.generation(t) != gen {
			s.resetLocked()
			return 0, ErrSerialization
		}
	}
	for key, predicate := range s.predicateGens {
		if s.table.predicateGeneration(key) != predicate.gen {
			s.resetLocked()
			return 0, ErrSerialization
		}
	}

	// Compute the durable effects of staged DDL before derived state is planned:
	// the rows an ALTER TABLE rewrites and a RENAME TABLE moves are part of the
	// transaction's final row state, so uniqueness and cache planning must see
	// them. The catalog writes travel in the same batch as everything else.
	var ddlOps []BatchOp
	var ddlChecks []CompareCheck
	var ddlEntries map[string]map[string]*overlayEntry
	var touchedTables, touchedIndexes []string
	if hasDDL {
		catalogOps, catalogChecks, err := s.schemaTx.commitCatalogOps()
		if err != nil {
			s.resetLocked()
			return 0, err
		}
		dataOps, dataEntries, err := s.schemaTx.commitDataOps()
		if err != nil {
			s.resetLocked()
			return 0, err
		}
		ddlOps = append(catalogOps, dataOps...)
		ddlChecks = catalogChecks
		ddlEntries = dataEntries
		touchedTables = s.schemaTx.touchedTables()
		touchedIndexes = s.schemaTx.touchedIndexes()
	}

	// Validate the final overlay against every foreign key in the constrained
	// component. The per-table gates acquired above exclude concurrent writers,
	// so this sees a stable state; checking the final state (rather than each
	// statement) is what lets a transaction delete a parent and its children in
	// either order as long as the committed state stays consistent.
	if err := s.validateForeignKeyFinalState(closure); err != nil {
		s.resetLocked()
		return 0, err
	}

	// Validate the final overlay of every unique-indexed written table against
	// durable rows. This runs under the exclusive gate acquired above, so it
	// cannot race a concurrent writer. Duplicate unique values fail the commit
	// with a "UNIQUE constraint failed" error rather than ErrSerialization.
	uniquePlans := make(map[string]uniqueCachePlan, len(uniqueTables))
	for t := range uniqueTables {
		entries := s.finalRowEntries(t, ddlEntries)
		plan, err := s.table.prepareUniqueRowsWith(t, entries, s.uniqueIndexesLocked(t))
		if err != nil {
			s.resetLocked()
			return 0, err
		}
		uniquePlans[t] = plan
	}

	exclusiveTables := make(map[string]bool)
	for table := range uniqueTables {
		exclusiveTables[table] = true
	}
	for table := range s.scanGens {
		exclusiveTables[table] = true
	}
	for _, predicate := range s.predicateGens {
		exclusiveTables[predicate.table] = true
	}
	cachePlans := make(map[string]*tableCachePlan)
	for table := range s.overlay {
		if !exclusiveTables[table] {
			continue
		}
		entries := s.finalRowEntries(table, ddlEntries)
		plan, err := s.table.prepareTableCache(table, entries, s.reads)
		if err != nil {
			s.resetLocked()
			return 0, err
		}
		cachePlans[table] = plan
	}

	// Build the compare set from every observed key.
	checks := make([]CompareCheck, 0, len(s.reads))
	for key, lsn := range s.reads {
		checks = append(checks, CompareCheck{Key: []byte(key), LSN: lsn})
	}

	// Build the batch ops from the staged overlay.
	ops := make([]BatchOp, 0)
	for _, entries := range s.overlay {
		for key, e := range entries {
			if e.absent {
				ops = append(ops, BatchOp{Op: batchDelete, Key: []byte(key)})
			} else {
				data, err := encodeRow(e.row)
				if err != nil {
					s.resetLocked()
					return 0, err
				}
				ops = append(ops, BatchOp{Op: batchPut, Key: []byte(key), Value: data})
			}
		}
	}

	if hasDDL {
		ops = append(ops, ddlOps...)
		checks = append(checks, ddlChecks...)
	}

	sort.Slice(checks, func(i, j int) bool { return string(checks[i].Key) < string(checks[j].Key) })
	sort.Slice(ops, func(i, j int) bool { return string(ops[i].Key) < string(ops[j].Key) })

	// A read-only transaction has nothing to write; commit trivially.
	if len(ops) == 0 {
		s.resetLocked()
		return 0, nil
	}

	var committed bool
	var commitLSN uint64
	err = s.table.pool.WithClient(func(c *KVClient) error {
		lsn, ok, err := c.CompareBatchWrite(checks, ops, nil)
		commitLSN = lsn
		committed = ok
		return err
	})
	if err != nil || !committed {
		// A transport error can hide a durable commit; a compare conflict can
		// also expose an out-of-band writer. Rebuild derived state next time.
		for table := range s.overlay {
			s.table.invalidateWriteFailure(table)
		}
	}
	if err != nil {
		s.resetLocked()
		return 0, err
	}
	if !committed {
		s.resetLocked()
		return 0, ErrSerialization
	}

	// Publish derived state before generations while retaining the table gates.
	for t := range s.overlay {
		if plan := cachePlans[t]; plan != nil {
			s.table.applyTableCache(plan)
		} else {
			s.table.invalidateCache(t)
		}
		s.table.applyUniquePlan(uniquePlans[t], nil)
		s.table.bumpIndexPredicateWildcard(t)
		s.table.bumpGeneration(t)
	}
	if hasDDL {
		s.publishCommittedCatalog(touchedTables)
	}

	// Index cache maintenance needs the table gates itself (BuildIndex locks the
	// table gate to rebuild entries), and that gate is not reentrant, so it runs
	// only after this commit drops the gates it held for validation.
	releaseGates()
	if hasDDL {
		s.publishCommittedIndexes(touchedIndexes)
	}

	s.resetLocked()
	return commitLSN, nil
}

// publishCommittedCatalog publishes a committed catalog change to the rest of
// the process. The durable batch is already written, so this only drops the
// derived state that described the previous catalog: the shared schema catalog
// caches and the row-derived caches of every touched table. Other sessions
// reload lazily from the committed records. The caller still holds the table
// gates, which is the documented lock order (table gate, then catalog mutex).
func (s *Session) publishCommittedCatalog(tables []string) {
	s.schema.invalidateCommittedDDL()
	for _, table := range tables {
		s.table.InvalidateCache(table)
		s.table.bumpIndexPredicateWildcard(table)
		s.table.bumpGeneration(table)
	}
}

// publishCommittedIndexes resets the derived entries of every index the
// transaction changed. A dropped index loses its cache and is marked disabled
// for indexing until it is created again; a committed index (including a
// re-created name a previous session dropped) is rebuilt so its entries and
// disabled marker match the catalog. The caller must not hold the table gates:
// BuildIndex acquires the table gate itself.
func (s *Session) publishCommittedIndexes(indexes []string) {
	committed, err := s.schema.ListIndexes()
	if err != nil {
		committed = nil
	}
	known := make(map[string]bool, len(committed))
	for _, name := range committed {
		known[strings.ToLower(name)] = true
	}
	for _, index := range indexes {
		if !known[index] {
			s.table.ClearIndex(index, "", nil) //nolint:errcheck
			continue
		}
		// A committed index may have reused a name this process dropped
		// earlier; rebuild it so its derived entries and disabled marker match
		// the catalog again.
		if definition, err := s.schema.GetIndex(index); err == nil {
			s.table.BuildIndex(definition.Name, definition.Table, nil) //nolint:errcheck
		}
	}
}

// acquireCommitLocks acquires the per-table gates needed to commit the staged
// transaction, returning the held locks, whether each is exclusive, an unlock
// function, and the set of written tables that carry a UNIQUE index. A written
// table with a UNIQUE index is locked exclusively so its final overlay can be
// validated against durable rows. The uniqueness probe is re-checked under the
// held gates so a concurrent CREATE UNIQUE INDEX cannot slip in after the probe
// and leave a duplicate unvalidated.
func (s *Session) acquireCommitLocks() (locks []*sync.RWMutex, exclusive []bool, unlockAll func(), uniqueTables map[string]bool, err error) {
	unlock := func(ls []*sync.RWMutex, ex []bool) {
		for i := len(ls) - 1; i >= 0; i-- {
			if ex[i] {
				ls[i].Unlock()
			} else {
				ls[i].RUnlock()
			}
		}
	}
	for {
		// Tables read through scans or predicates need an exclusive validation
		// gate. Tables that are only written use the shared publication gate,
		// allowing disjoint optimistic commits to proceed concurrently while
		// still excluding scanner commits.
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
		// Staged DDL rewrites or removes durable rows and publishes catalog
		// records, so the commit needs the exclusive gate of every touched table.
		if s.schemaTx != nil && s.schemaTx.hasDDL() {
			for _, t := range s.schemaTx.touchedTables() {
				affected[t] = true
			}
		}

		// Tables written in this transaction that carry a UNIQUE index need the
		// exclusive gate so their final overlay can be validated against durable
		// rows without racing another writer.
		uniqueTables = make(map[string]bool)
		for t := range s.overlay {
			uniq, err := s.hasUniqueIndexLocked(t)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			if uniq {
				uniqueTables[t] = true
				affected[t] = true
			}
		}

		tables := make([]string, 0, len(affected))
		for t := range affected {
			tables = append(tables, t)
		}
		sort.Strings(tables)

		locks = make([]*sync.RWMutex, len(tables))
		exclusive = make([]bool, len(tables))
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

		// Re-check under the held gates. A concurrent CREATE UNIQUE INDEX can
		// only have completed before we acquired the gate (it needs the exclusive
		// gate), so this probe is authoritative; escalate and retry if a written
		// table gained a unique index.
		retry := false
		for i, t := range tables {
			if exclusive[i] {
				continue
			}
			if _, written := s.overlay[t]; !written {
				continue
			}
			uniq, err := s.hasUniqueIndexLocked(t)
			if err != nil {
				unlock(locks, exclusive)
				return nil, nil, nil, nil, err
			}
			if uniq {
				retry = true
				break
			}
		}
		if retry {
			unlock(locks, exclusive)
			continue
		}

		unlockAll = func() { unlock(locks, exclusive) }
		return locks, exclusive, unlockAll, uniqueTables, nil
	}
}

// applyOverlaySchemaDefaults re-applies the current schema to every staged row
// (the caller holds the per-table gates). Columns published after a row image
// was built receive their catalog default, and required columns are validated,
// so a commit cannot persist a row that violates the schema now in effect.
func (s *Session) applyOverlaySchemaDefaults() error {
	for table, entries := range s.overlay {
		schema, err := s.getSchemaLocked(table)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.absent {
				continue
			}
			if err := s.applyStagedRewritesLocked(table, entry.row); err != nil {
				return err
			}
			if err := s.schema.applySchemaDefaults(schema, entry.row); err != nil {
				return err
			}
			if err := s.schema.validateChecks(schema, entry.row); err != nil {
				return err
			}
		}
	}
	return nil
}

// finalRowEntries merges the rows a staged DDL rewrite or rename produces into
// the table's staged entries, so uniqueness and cache planning validate the row
// image the commit is about to publish (caller holds the session lock).
func (s *Session) finalRowEntries(table string, ddlEntries map[string]map[string]*overlayEntry) map[string]*overlayEntry {
	staged := s.overlay[table]
	rewritten := ddlEntries[table]
	if len(rewritten) == 0 {
		return staged
	}
	merged := make(map[string]*overlayEntry, len(staged)+len(rewritten))
	for key, entry := range staged {
		merged[key] = entry
	}
	for key, entry := range rewritten {
		merged[key] = entry
	}
	return merged
}

// uniqueIndexesLocked returns the UNIQUE indexes of a table as this session
// observes them (caller holds the session lock).
func (s *Session) uniqueIndexesLocked(table string) []*Index {
	var indexes []*Index
	if s.schemaTx != nil {
		indexes, _ = s.schemaTx.listTableIndexes(table)
	} else {
		indexes, _ = s.schema.ListTableIndexes(table)
	}
	unique := indexes[:0]
	for _, idx := range indexes {
		if idx.Unique {
			unique = append(unique, idx)
		}
	}
	return unique
}

// hasUniqueIndexLocked reports whether the session's view of a table carries a
// UNIQUE index, so a transaction validates rows against an index it staged.
func (s *Session) hasUniqueIndexLocked(table string) (bool, error) {
	if s.schemaTx == nil {
		return s.table.hasUniqueIndex(table)
	}
	indexes, err := s.schemaTx.listTableIndexes(table)
	if err != nil {
		return false, err
	}
	for _, idx := range indexes {
		if idx.Unique {
			return true, nil
		}
	}
	return false, nil
}

func (s *Session) resetLocked() {
	s.inTx = false
	s.aborted = false
	s.overlay = make(map[string]map[string]*overlayEntry)
	s.log = nil
	s.reads = make(map[string]uint64)
	s.scanGens = make(map[string]uint64)
	s.predicateGens = make(map[string]predicateRead)
	s.schemaTx = nil
	s.fkCatalog = nil
	s.fkVersion = 0
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

func (s *Session) stageDelete(table, key string, row Row) {
	tl := strings.ToLower(table)
	if s.overlay[tl] == nil {
		s.overlay[tl] = make(map[string]*overlayEntry)
	}
	s.log = append(s.log, txMutation{table: tl, key: key, prev: s.overlay[tl][key]})
	// Keep the deleted row so COMMIT can exempt its rowid from the unique-index
	// scan and release its unique value.
	s.overlay[tl][key] = &overlayEntry{row: cloneRow(row), absent: true}
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
	stagedKey := s.table.dataKey(table, pk)
	tl := strings.ToLower(table)
	if e, ok := s.overlay[tl][stagedKey]; ok {
		if e.absent {
			return nil, ErrKeyNotFound
		}
		return cloneRow(e.row), nil
	}
	durableKey := s.durableDataKeyLocked(table, pk)
	row, lsn, err := s.table.getKeyWithLSN(durableKey)
	if err != nil {
		if err == ErrKeyNotFound {
			s.reads[durableKey] = 0
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	s.reads[durableKey] = lsn
	if err := s.applyStagedRewritesLocked(table, row); err != nil {
		return nil, err
	}
	return row, nil
}

// durableSourceLocked returns the committed table name whose data prefix holds
// the rows of a table this transaction sees. RENAME TABLE moves the definition
// immediately but leaves rows under the committed prefix until the commit, so
// reads must follow the source (caller holds the session lock).
func (s *Session) durableSourceLocked(table string) string {
	if s.schemaTx == nil {
		return table
	}
	lower := strings.ToLower(table)
	source, ok := s.schemaTx.dataSources[lower]
	if !ok || source == "" {
		return table
	}
	return source
}

// hasStagedSourceLocked reports whether the table's durable rows live under a
// different (older) name because RENAME TABLE is staged.
func (s *Session) hasStagedSourceLocked(table string) bool {
	if s.schemaTx == nil {
		return false
	}
	lower := strings.ToLower(table)
	source, ok := s.schemaTx.dataSources[lower]
	return ok && source != "" && source != lower
}

// durableDataKeyLocked returns the key a row is durably stored under, following
// a staged rename (caller holds the session lock).
func (s *Session) durableDataKeyLocked(table, pk string) string {
	return s.table.dataKey(s.durableSourceLocked(table), pk)
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

// SelectLimited bounds an autocommit scan. Explicit transactions retain the
// complete overlay/read-set behavior until bounded transactional scans have a
// separate phantom/overlay contract; their caller still applies LIMIT later.
func (s *Session) SelectLimited(table string, filter func(Row) bool, maxRows int) ([]Row, error) {
	s.mu.Lock()
	if s.inTx {
		defer s.mu.Unlock()
		return s.selectLocked(table, filter)
	}
	s.mu.Unlock()
	if maxRows == 0 {
		return []Row{}, nil
	}
	return s.table.SelectWithLimit(table, filter, maxRows, 0)
}

func (s *Session) selectLocked(table string, filter func(Row) bool) ([]Row, error) {
	schema, err := s.getSchemaLocked(table)
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
	source := s.durableSourceLocked(table)
	sourceLower := strings.ToLower(source)
	if sourceLower != tl {
		// A concurrent write to the committed name must invalidate the commit
		// just like a write to the staged name.
		if _, ok := s.scanGens[sourceLower]; !ok {
			s.scanGens[sourceLower] = s.table.generation(sourceLower)
		}
	}

	var rows []Row
	err = s.table.scanRowsAtPrefix(s.table.dataPrefix(source), func(durableKey string, row Row, lsn uint64) (bool, error) {
		pk, err := schema.PrimaryKeyForRow(row)
		if err != nil {
			return true, err
		}
		if _, ok := overlay[s.table.dataKey(table, pk)]; ok {
			return false, nil
		}
		s.reads[durableKey] = lsn
		if err := s.applyStagedRewritesLocked(table, row); err != nil {
			return true, err
		}
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
		row := cloneRow(e.row)
		if err := s.applyStagedRewritesLocked(table, row); err != nil {
			return nil, err
		}
		if filter == nil || filter(row) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// applyStagedRewritesLocked applies the transaction's pending ALTER TABLE row
// transformations, so the session reads its own DDL before the commit writes
// the transformed images (caller holds the session lock).
func (s *Session) applyStagedRewritesLocked(table string, row Row) error {
	if s.schemaTx == nil {
		return nil
	}
	_, err := s.schemaTx.rewriteRow(table, row)
	return err
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
	index, err := s.getIndexLocked(indexName)
	if err != nil {
		return nil, err
	}
	col := index.Columns[0].Name
	want := formatIndexValue(colValue)
	if s.hasStagedSourceLocked(table) || !s.indexIsCommittedLocked(index.Name) {
		// The derived index caches describe the committed catalog: an index this
		// transaction created has no entries yet, and a renamed table's entries
		// are keyed under the old storage name. Either way, scan the rows the
		// session observes instead.
		return s.filterByIndexLocked(table, index, want)
	}
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
		if err := s.applyStagedRewritesLocked(table, version.row); err != nil {
			return nil, err
		}
		rows = append(rows, version.row)
	}
	for _, entry := range overlay {
		if !entry.absent && formatIndexValue(entry.row[col]) == want {
			rows = append(rows, cloneRow(entry.row))
		}
	}
	return rows, nil
}

// SelectByIndexKey retrieves the rows whose index key equals valueKey, merging
// the staged overlay so a transaction sees its own writes. valueKey is the
// formatted key from TableManager.IndexRowKey, which supports plain, composite,
// and expression indexes uniformly.
func (s *Session) SelectByIndexKey(table string, idx *Index, valueKey string) ([]Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		versions, _, err := s.table.selectByIndexKeyWithLSN(table, idx.Name, valueKey)
		if err != nil {
			return nil, err
		}
		rows := make([]Row, len(versions))
		for i := range versions {
			rows[i] = versions[i].row
		}
		return rows, nil
	}

	tableKey := strings.ToLower(table)
	if s.hasStagedSourceLocked(table) || !s.indexIsCommittedLocked(idx.Name) {
		return s.filterByIndexLocked(table, idx, valueKey)
	}
	versions, predicate, err := s.table.selectByIndexKeyWithLSN(table, idx.Name, valueKey)
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
		if err := s.applyStagedRewritesLocked(table, version.row); err != nil {
			return nil, err
		}
		rows = append(rows, version.row)
	}
	for _, entry := range overlay {
		if entry.absent {
			continue
		}
		key, kerr := s.table.IndexRowKey(idx, entry.row)
		if kerr != nil {
			return nil, kerr
		}
		if key == valueKey {
			rows = append(rows, cloneRow(entry.row))
		}
	}
	return rows, nil
}

// filterByIndexLocked returns the rows this session observes whose index key
// equals valueKey, without consulting the derived index caches. It is the
// fallback for a table renamed in this transaction, whose committed index
// entries still name the old storage prefix (caller holds the session lock).
func (s *Session) filterByIndexLocked(table string, idx *Index, valueKey string) ([]Row, error) {
	rows, err := s.selectLocked(table, nil)
	if err != nil {
		return nil, err
	}
	matched := make([]Row, 0, len(rows))
	for _, row := range rows {
		key, err := s.table.IndexRowKey(idx, row)
		if err != nil {
			return nil, err
		}
		if key == valueKey {
			matched = append(matched, row)
		}
	}
	return matched, nil
}

// indexIsCommittedLocked reports whether the shared table manager can resolve an
// index, and therefore whether its derived index caches cover it (caller holds
// the session lock).
func (s *Session) indexIsCommittedLocked(name string) bool {
	_, err := s.schema.GetIndex(name)
	return err == nil
}

// getIndexLocked resolves an index definition against the session catalog
// (caller holds the session lock).
func (s *Session) getIndexLocked(name string) (*Index, error) {
	if s.schemaTx != nil {
		return s.schemaTx.getIndex(name)
	}
	return s.schema.GetIndex(name)
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
	_, err := s.InsertWithRowID(table, row)
	return err
}

// InsertWithRowID stages an insert in a transaction, or performs a durable
// autocommit insert otherwise, returning the actual generated ROWID.
func (s *Session) InsertWithRowID(table string, row Row) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.InsertWithRowID(table, row)
	}
	return s.insertLocked(table, row)
}

// InsertBulk stages or durably bulk-inserts multiple rows.
func (s *Session) InsertBulk(table string, rows []Row) (int, error) {
	count, _, err := s.InsertBulkWithLastRowID(table, rows)
	return count, err
}

// InsertBulkWithLastRowID is InsertBulk, additionally returning the ROWID of the
// last staged/persisted row (0 when nothing was inserted).
func (s *Session) InsertBulkWithLastRowID(table string, rows []Row) (int, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inTx {
		return s.table.InsertBulkWithLastRowID(table, rows)
	}
	var lastRowID int64
	count := 0
	for _, row := range rows {
		rid, err := s.insertLocked(table, row)
		if err != nil {
			return count, lastRowID, err
		}
		lastRowID = rid
		count++
	}
	return count, lastRowID, nil
}

// insertLocked is the transaction insert helper (caller holds s.mu).
func (s *Session) insertLocked(table string, row Row) (int64, error) {
	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return 0, err
	}
	nr, key, err := s.table.prepareInsertWithSchema(table, schema, row)
	if err != nil {
		return 0, err
	}
	rowid, _ := rowIDFromRow(nr)
	tl := strings.ToLower(table)
	// prepareInsert returned the durable key; duplicate reporting shows the same
	// encoded primary-key component the key carries.
	pk := strings.TrimPrefix(key, s.table.dataPrefix(table))

	if e, ok := s.overlay[tl][key]; ok {
		if !e.absent {
			return 0, &PrimaryKeyViolationError{Table: table, Key: pk}
		}
	} else {
		durableKey := s.durableDataKeyLocked(table, pk)
		_, lsn, err := s.table.getKeyWithLSN(durableKey)
		if err == nil {
			s.reads[durableKey] = lsn
			return 0, &PrimaryKeyViolationError{Table: table, Key: pk}
		}
		if err != nil && err != ErrKeyNotFound {
			return 0, err
		}
		s.reads[durableKey] = 0
	}

	s.stagePut(table, key, nr)
	return rowid, nil
}

// UpdateByPK stages or durably applies a single-row update. A table that is the
// parent of a cascading constraint runs in an implicit transaction so the parent
// rewrite and every cascaded row publish in one commit.
func (s *Session) UpdateByPK(table, pk string, updateFn func(Row) (Row, error)) (Row, bool, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		cascading, unlockFK, err := s.beginCascadingPointWrite(table, false)
		if err != nil {
			return nil, false, err
		}
		if !cascading {
			return s.table.UpdateByPK(table, pk, updateFn)
		}
		oldRow, updated, err := s.updateByPKStaged(table, pk, updateFn)
		unlockFK()
		if err != nil {
			_ = s.Rollback()
			return nil, false, err
		}
		if err := s.Commit(); err != nil {
			return nil, false, err
		}
		return oldRow, updated, nil
	}
	defer s.mu.Unlock()
	return s.updateByPKLocked(table, pk, updateFn)
}

func (s *Session) updateByPKStaged(table, pk string, updateFn func(Row) (Row, error)) (Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateByPKLocked(table, pk, updateFn)
}

// updateByPKLocked stages a point update. The caller holds the session lock and
// a transaction.
func (s *Session) updateByPKLocked(table, pk string, updateFn func(Row) (Row, error)) (Row, bool, error) {
	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return nil, false, err
	}

	row, err := s.getByPKLocked(table, pk)
	if err == ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	oldRow := cloneRow(row)
	updates, err := s.runUpdateFn(updateFn, row)
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

	if err := s.schema.validateRowConstraints(schema, row); err != nil {
		return nil, false, err
	}
	pos := len(s.log)
	if err := s.stageCascadeUpdate(table, oldRow, row, newCascadeExpansion(s.schema)); err != nil {
		s.rollbackToLocked(pos)
		return nil, false, err
	}
	return oldRow, true, nil
}

// runUpdateFn releases the session lock while invoking the update callback so the
// callback can run nested reads (e.g. a scalar subquery in SET) through the same
// session without deadlocking on s.mu. A connection executes one statement at a
// time, so the staged overlay cannot change while the callback runs. The lock is
// re-acquired before returning.
func (s *Session) runUpdateFn(updateFn func(Row) (Row, error), row Row) (Row, error) {
	s.mu.Unlock()
	defer s.mu.Lock()
	return updateFn(row)
}

// DeleteByPK stages or durably applies a single-row delete. A table that is the
// parent of a cascading constraint runs in an implicit transaction so the
// parent delete and every cascaded row publish in one commit.
func (s *Session) DeleteByPK(table, pk string) (Row, bool, error) {
	s.mu.Lock()
	if !s.inTx {
		s.mu.Unlock()
		cascading, unlockFK, err := s.beginCascadingPointWrite(table, true)
		if err != nil {
			return nil, false, err
		}
		if !cascading {
			return s.table.DeleteByPK(table, pk)
		}
		row, deleted, err := s.deleteByPKStaged(table, pk)
		unlockFK()
		if err != nil {
			_ = s.Rollback()
			return nil, false, err
		}
		if err := s.Commit(); err != nil {
			return nil, false, err
		}
		return row, deleted, nil
	}
	defer s.mu.Unlock()
	return s.deleteByPKLocked(table, pk)
}

func (s *Session) deleteByPKStaged(table, pk string) (Row, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteByPKLocked(table, pk)
}

// deleteByPKLocked stages a point delete. The caller holds the session lock and
// a transaction.
func (s *Session) deleteByPKLocked(table, pk string) (Row, bool, error) {
	key := s.table.dataKey(table, pk)
	row, err := s.getByPKLocked(table, pk)
	if err == ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	pos := len(s.log)
	if err := s.stageCascadeDelete(table, key, row, newCascadeExpansion(s.schema)); err != nil {
		s.rollbackToLocked(pos)
		return nil, false, err
	}
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
	if err := s.applyStagedRewritesLocked(table, row); err != nil {
		return nil, err
	}
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

	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return 0, err
	}
	rows, err := s.selectLocked(table, filter)
	if err != nil {
		return 0, err
	}
	count := 0
	exp := newCascadeExpansion(s.schema)
	pos := len(s.log)
	for _, row := range rows {
		oldRow := cloneRow(row)
		updates, err := s.runUpdateFn(updateFn, row)
		if err != nil {
			s.rollbackToLocked(pos)
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
		if err := s.schema.validateRowConstraints(schema, row); err != nil {
			s.rollbackToLocked(pos)
			return count, err
		}
		if err := s.stageCascadeUpdate(table, oldRow, row, exp); err != nil {
			s.rollbackToLocked(pos)
			return count, err
		}
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

	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return 0, err
	}
	rows, err := s.selectLocked(table, filter)
	if err != nil {
		return 0, err
	}
	count := 0
	exp := newCascadeExpansion(s.schema)
	pos := len(s.log)
	for _, row := range rows {
		pk, err := schema.PrimaryKeyForRow(row)
		if err != nil {
			s.rollbackToLocked(pos)
			return count, err
		}
		if err := s.stageCascadeDelete(table, s.table.dataKey(table, pk), row, exp); err != nil {
			s.rollbackToLocked(pos)
			return count, err
		}
		count++
	}
	return count, nil
}

// ClearIndex drops the in-memory entries of an index. Inside a transaction the
// drop is staged in the catalog overlay instead: the committed index (and its
// cache) stays intact for every other session, and the commit clears it once the
// catalog change is durable. This keeps the rollback invariant that a
// transaction never mutates shared derived state, so ROLLBACK has nothing to
// restore beyond the session's own schema snapshot and row overlay.
func (s *Session) ClearIndex(indexName, tableName string, columns []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return nil
	}
	return s.table.ClearIndex(indexName, tableName, columns)
}

// InvalidateCache drops the derived caches of a table. Inside a transaction the
// durable rows and committed catalog are still current, so nothing is
// invalidated until the commit publishes the catalog change; a rollback then
// leaves every shared cache exactly as it was.
func (s *Session) InvalidateCache(table string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return
	}
	s.table.InvalidateCache(table)
}

// BuildIndex builds index entries for every existing row. Inside a transaction
// the index is only staged; its entries are derived lazily from durable rows
// once the commit publishes it and the session is merged in afterwards. Staging
// instead of building keeps a rolled-back CREATE INDEX from leaving derived
// entries or a cleared disabled marker behind.
func (s *Session) BuildIndex(indexName, tableName string, columns []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return nil
	}
	return s.table.BuildIndex(indexName, tableName, columns)
}
