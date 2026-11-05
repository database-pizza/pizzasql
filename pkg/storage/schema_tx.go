package storage

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// rowRewrite is one pending ALTER TABLE row transformation. It reports whether
// it changed the row and must be idempotent: it runs again on every read of the
// table while the transaction is open and once more on each durable row when
// the transaction commits.
type rowRewrite func(row Row) (bool, error)

// SchemaTx is the transaction-scoped catalog overlay of one Session. DDL inside
// an explicit transaction records its catalog and row effects here instead of
// publishing them: the owning session reads through the overlay, every other
// session keeps reading the committed catalog, ROLLBACK discards it, and COMMIT
// merges the overlay into the session's single compare-and-swap batch write, so
// a multi-statement migration becomes durable atomically or not at all.
//
// The overlay starts out empty and reads through to the durable catalog while
// recording the LSN each touched key was observed at. Those LSNs become the
// commit's compare checks, so a concurrent DDL that committed after the
// transaction derived its catalog makes the commit fail with ErrSerialization
// instead of overwriting the concurrent change.
//
// Every method assumes the owning session lock is held: a Session executes one
// statement at a time, so the overlay needs no lock of its own and can share the
// session's row overlay directly.
type SchemaTx struct {
	base *SchemaManager
	sess *Session

	// staged holds the transaction's definition of every touched table, keyed
	// by lowercased name. A nil entry means the name is not visible to this
	// transaction: it was dropped, or moved by RENAME TABLE.
	staged map[string]*Schema
	// stagedIndexes holds the transaction's definition of every touched index,
	// keyed by lowercased name; nil means dropped.
	stagedIndexes map[string]*Index
	// committedSchemas and committedIndexes cache the definitions read from
	// durable storage so the executor's repeated catalog lookups inside one
	// statement do not re-read them. An entry is dropped when the transaction
	// stages a replacement for the same name.
	committedSchemas map[string]*Schema
	committedIndexes map[string]*Index
	// tables and indexList are the staged catalog lists. They are loaded from
	// the committed catalog on first use and rewritten in place afterwards.
	tables    []string
	indexList []string
	// catalogDirty and indexListDirty report that the corresponding list is
	// part of the commit.
	catalogDirty   bool
	indexListDirty bool

	// reads records the durable LSN every staged catalog key was derived from
	// (0 for a key that was absent). commitChecks turns them into compare
	// checks.
	reads map[string]uint64

	// rewrites holds pending row transformations keyed by the lowercased table
	// name whose rows they apply to, in statement order.
	rewrites map[string][]rowRewrite
	// dataSources maps a staged table name to the durable table name whose data
	// prefix still holds its rows: RENAME TABLE moves the definition without
	// touching rows, so a later ALTER or DROP must look under the old prefix.
	// An entry with an empty source is a table created by this transaction.
	dataSources map[string]string
	// drops holds durable table names whose data prefix is deleted at commit.
	drops map[string]bool
	// renames maps a durable table name to the staged name its rows move to at
	// commit.
	renames map[string]string

	// rekeys records the overlay re-keyings this transaction performed, in
	// order, so a savepoint can undo them.
	rekeys []rekeyPair

	version uint64
}

type rekeyPair struct {
	from string
	to   string
}

// schemaTxSnapshot is the transaction catalog state at a savepoint.
type schemaTxSnapshot struct {
	version        uint64
	staged         map[string]*Schema
	stagedIndexes  map[string]*Index
	tables         []string
	indexList      []string
	catalogDirty   bool
	indexListDirty bool
	rewrites       map[string][]rowRewrite
	dataSources    map[string]string
	drops          map[string]bool
	renames        map[string]string
	rekeys         int
}

func newSchemaTx(s *Session) *SchemaTx {
	return &SchemaTx{
		base:             s.schema,
		sess:             s,
		staged:           make(map[string]*Schema),
		stagedIndexes:    make(map[string]*Index),
		committedSchemas: make(map[string]*Schema),
		committedIndexes: make(map[string]*Index),
		reads:            make(map[string]uint64),
		rewrites:         make(map[string][]rowRewrite),
		dataSources:      make(map[string]string),
		drops:            make(map[string]bool),
		renames:          make(map[string]string),
		version:          s.schema.Version(),
	}
}

// snapshot captures the catalog overlay. The maps are copied shallowly: staged
// definitions are cloned before they are mutated, so the captured pointers stay
// immutable.
func (tx *SchemaTx) snapshot() schemaTxSnapshot {
	snap := schemaTxSnapshot{
		version:        tx.version,
		staged:         make(map[string]*Schema, len(tx.staged)),
		stagedIndexes:  make(map[string]*Index, len(tx.stagedIndexes)),
		tables:         append([]string(nil), tx.tables...),
		indexList:      append([]string(nil), tx.indexList...),
		catalogDirty:   tx.catalogDirty,
		indexListDirty: tx.indexListDirty,
		rewrites:       make(map[string][]rowRewrite, len(tx.rewrites)),
		dataSources:    make(map[string]string, len(tx.dataSources)),
		drops:          make(map[string]bool, len(tx.drops)),
		renames:        make(map[string]string, len(tx.renames)),
		rekeys:         len(tx.rekeys),
	}
	for k, v := range tx.staged {
		snap.staged[k] = v
	}
	for k, v := range tx.stagedIndexes {
		snap.stagedIndexes[k] = v
	}
	for k, v := range tx.rewrites {
		snap.rewrites[k] = append([]rowRewrite(nil), v...)
	}
	for k, v := range tx.dataSources {
		snap.dataSources[k] = v
	}
	for k, v := range tx.drops {
		snap.drops[k] = v
	}
	for k, v := range tx.renames {
		snap.renames[k] = v
	}
	return snap
}

// restore returns the overlay to a snapshot and undoes the overlay re-keyings
// performed after it. Every collection is copied rather than aliased: the
// snapshot is retained for a later ROLLBACK TO SAVEPOINT, and DDL staged after
// this restore must not mutate the retained state. Staged schema and index
// definitions are immutable once stored, so a shallow copy of the maps is
// enough. The caller holds the session lock.
func (tx *SchemaTx) restore(snap schemaTxSnapshot) {
	for i := len(tx.rekeys) - 1; i >= snap.rekeys; i-- {
		tx.sess.rekeyOverlayLocked(tx.rekeys[i].to, tx.rekeys[i].from)
	}
	tx.rekeys = tx.rekeys[:snap.rekeys]
	tx.version = snap.version
	tx.staged = copySchemaMap(snap.staged)
	tx.stagedIndexes = copyIndexMap(snap.stagedIndexes)
	tx.tables = append([]string(nil), snap.tables...)
	tx.indexList = append([]string(nil), snap.indexList...)
	tx.catalogDirty = snap.catalogDirty
	tx.indexListDirty = snap.indexListDirty
	tx.rewrites = copyRewriteMap(snap.rewrites)
	tx.dataSources = copyStringMap(snap.dataSources)
	tx.drops = copyBoolMap(snap.drops)
	tx.renames = copyStringMap(snap.renames)
}

func copySchemaMap(source map[string]*Schema) map[string]*Schema {
	cloned := make(map[string]*Schema, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func copyIndexMap(source map[string]*Index) map[string]*Index {
	cloned := make(map[string]*Index, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func copyRewriteMap(source map[string][]rowRewrite) map[string][]rowRewrite {
	cloned := make(map[string][]rowRewrite, len(source))
	for key, value := range source {
		cloned[key] = append([]rowRewrite(nil), value...)
	}
	return cloned
}

func copyStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func copyBoolMap(source map[string]bool) map[string]bool {
	cloned := make(map[string]bool, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// bump advances the transaction's catalog version so the executor's analyzer
// catalog resynchronizes after every staged change.
func (tx *SchemaTx) bump() {
	tx.version++
}

func (tx *SchemaTx) recordRead(key string, lsn uint64) {
	if _, ok := tx.reads[key]; !ok {
		tx.reads[key] = lsn
	}
}

// refreshCommitted drops the cached committed definitions before the commit's
// final validation. Another session's DDL may have committed while this
// transaction was open, so the rows it stages must be validated against the
// catalog that is current under the commit gates; the staged overlay itself is
// kept. The catalog lists keep their recorded LSN: a staged list write must
// still fail its compare check rather than overwrite the concurrent change.
func (tx *SchemaTx) refreshCommitted() {
	tx.committedSchemas = make(map[string]*Schema)
	tx.committedIndexes = make(map[string]*Index)
	if !tx.catalogDirty {
		tx.tables = nil
	}
	if !tx.indexListDirty {
		tx.indexList = nil
	}
}

// getSchema returns the transaction's view of a table.
func (tx *SchemaTx) getSchema(name string) (*Schema, error) {
	lower := strings.ToLower(name)
	if schema, ok := tx.staged[lower]; ok {
		if schema == nil {
			return nil, fmt.Errorf("table not found: %s", name)
		}
		return cloneSchema(schema), nil
	}
	return tx.loadSchema(name)
}

// loadSchema reads the committed definition and records the LSN it observed.
func (tx *SchemaTx) loadSchema(name string) (*Schema, error) {
	lower := strings.ToLower(name)
	if schema, ok := tx.committedSchemas[lower]; ok {
		return cloneSchema(schema), nil
	}
	key := tx.base.schemaKey(name)
	data, lsn, found, err := tx.base.readDurable(key)
	if err != nil {
		return nil, err
	}
	tx.recordRead(key, lsn)
	if !found {
		return nil, fmt.Errorf("table not found: %s", name)
	}
	var schema Schema
	if err := json.Unmarshal([]byte(data), &schema); err != nil {
		return nil, fmt.Errorf("failed to parse schema: %w", err)
	}
	schema.normalizePrimaryKey()
	tx.committedSchemas[lower] = &schema
	return cloneSchema(&schema), nil
}

func (tx *SchemaTx) tableExists(name string) bool {
	_, err := tx.getSchema(name)
	return err == nil
}

// listTables returns the staged table catalog in creation order.
func (tx *SchemaTx) listTables() ([]string, error) {
	if tx.tables == nil {
		tables, lsn, err := tx.base.catalogTablesDurable()
		if err != nil {
			return nil, err
		}
		tx.recordRead(tx.base.catalogKey(), lsn)
		tx.tables = tables
	}
	return append([]string(nil), tx.tables...), nil
}

func (tx *SchemaTx) stageTableList(tables []string) {
	tx.tables = tables
	tx.catalogDirty = true
}

// getIndex returns the transaction's view of an index definition.
func (tx *SchemaTx) getIndex(name string) (*Index, error) {
	lower := strings.ToLower(name)
	if index, ok := tx.stagedIndexes[lower]; ok {
		if index == nil {
			return nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
		}
		return cloneIndex(index), nil
	}
	if index, ok := tx.committedIndexes[lower]; ok {
		return cloneIndex(index), nil
	}
	key := tx.base.indexKey(name)
	data, lsn, found, err := tx.base.readDurable(key)
	if err != nil {
		return nil, err
	}
	tx.recordRead(key, lsn)
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
	}
	var index Index
	if err := json.Unmarshal([]byte(data), &index); err != nil {
		return nil, fmt.Errorf("failed to parse index: %w", err)
	}
	tx.committedIndexes[lower] = &index
	return cloneIndex(&index), nil
}

func (tx *SchemaTx) indexExists(name string) bool {
	_, err := tx.getIndex(name)
	return err == nil
}

func (tx *SchemaTx) listIndexes() ([]string, error) {
	if tx.indexList == nil {
		indexes, lsn, err := tx.base.indexListDurable()
		if err != nil {
			return nil, err
		}
		tx.recordRead(tx.base.indexListKey(), lsn)
		tx.indexList = indexes
	}
	return append([]string(nil), tx.indexList...), nil
}

func (tx *SchemaTx) stageIndexList(indexes []string) {
	tx.indexList = indexes
	tx.indexListDirty = true
}

func (tx *SchemaTx) listTableIndexes(table string) ([]*Index, error) {
	names, err := tx.listIndexes()
	if err != nil {
		return nil, err
	}
	var result []*Index
	for _, name := range names {
		idx, err := tx.getIndex(name)
		if err != nil {
			if err == ErrIndexNotFound || strings.Contains(err.Error(), ErrIndexNotFound.Error()) {
				continue
			}
			return nil, err
		}
		if strings.EqualFold(idx.Table, table) {
			result = append(result, idx)
		}
	}
	return result, nil
}

// stageSchema records a replacement table definition.
func (tx *SchemaTx) stageSchema(schema *Schema) {
	lower := strings.ToLower(schema.Name)
	delete(tx.committedSchemas, lower)
	tx.staged[lower] = cloneSchema(schema)
	if _, ok := tx.dataSources[lower]; !ok {
		tx.dataSources[lower] = lower
	}
	tx.recordRead(tx.base.schemaKey(schema.Name), 0)
	tx.bump()
}

// stageDroppedTable records that a name stopped being addressable.
func (tx *SchemaTx) stageDroppedTable(lower string) {
	delete(tx.committedSchemas, lower)
	tx.staged[lower] = nil
	tx.recordRead(tx.base.schemaKey(lower), 0)
	tx.bump()
}

// createTable stages CREATE TABLE.
func (tx *SchemaTx) createTable(schema *Schema) error {
	if tx.tableExists(schema.Name) {
		return fmt.Errorf("table already exists: %s", schema.Name)
	}
	lower := strings.ToLower(schema.Name)
	schema = cloneSchema(schema)
	schema.CreatedAt = time.Now()
	schema.normalizePrimaryKey()
	tx.staged[lower] = schema
	tx.dataSources[lower] = ""
	tables, err := tx.listTables()
	if err != nil {
		return err
	}
	for _, name := range tables {
		if strings.EqualFold(name, schema.Name) {
			return fmt.Errorf("table already exists: %s", schema.Name)
		}
	}
	tx.stageTableList(append(tables, schema.Name))
	tx.bump()
	return nil
}

// createIndex stages CREATE INDEX.
func (tx *SchemaTx) createIndex(index *Index) error {
	if tx.indexExists(index.Name) {
		return fmt.Errorf("index already exists: %s", index.Name)
	}
	if !tx.tableExists(index.Table) {
		return fmt.Errorf("table not found: %s", index.Table)
	}
	index = cloneIndex(index)
	index.CreatedAt = time.Now()
	delete(tx.committedIndexes, strings.ToLower(index.Name))
	tx.stagedIndexes[strings.ToLower(index.Name)] = index
	names, err := tx.listIndexes()
	if err != nil {
		return err
	}
	tx.stageIndexList(append(names, index.Name))
	tx.recordRead(tx.base.indexKey(index.Name), 0)
	tx.bump()
	return nil
}

// dropIndex stages DROP INDEX.
func (tx *SchemaTx) dropIndex(name string) error {
	index, err := tx.getIndex(name)
	if err != nil {
		return fmt.Errorf("index not found: %s", name)
	}
	lower := strings.ToLower(name)
	delete(tx.committedIndexes, lower)
	tx.stagedIndexes[lower] = nil
	names, err := tx.listIndexes()
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(names))
	for _, existing := range names {
		if !strings.EqualFold(existing, name) {
			filtered = append(filtered, existing)
		}
	}
	tx.stageIndexList(filtered)
	tx.recordRead(tx.base.indexKey(index.Name), 0)
	tx.bump()
	return nil
}

// dropTable stages DROP TABLE. The durable rows are removed by the commit; the
// session's staged rows for the table are discarded immediately because the
// table is no longer visible to this transaction.
func (tx *SchemaTx) dropTable(name string) error {
	lower := strings.ToLower(name)
	if _, err := tx.getSchema(name); err != nil {
		return fmt.Errorf("table not found: %s", name)
	}
	source := tx.durableSource(lower)
	if source != "" {
		tx.drops[source] = true
	}
	delete(tx.rewrites, lower)
	delete(tx.dataSources, lower)
	// A name that was renamed inside this transaction has no durable record of
	// its own; the rename already deleted the original key at commit time.
	if _, renamed := tx.renames[lower]; !renamed {
		tx.recordRead(tx.base.schemaKey(lower), 0)
	}
	tx.stageDroppedTable(lower)

	for _, idx := range tx.tableIndexes(lower) {
		if err := tx.dropIndex(idx.Name); err != nil {
			return err
		}
	}
	tables, err := tx.listTables()
	if err != nil {
		return err
	}
	filtered := make([]string, 0, len(tables))
	for _, existing := range tables {
		if !strings.EqualFold(existing, name) {
			filtered = append(filtered, existing)
		}
	}
	tx.stageTableList(filtered)
	tx.sess.dropOverlayTableLocked(lower)
	tx.sess.markTableScanned(lower)
	return nil
}

// durableSource returns the committed table name whose data prefix holds the
// rows of a staged table, or "" when the table was created by this transaction.
func (tx *SchemaTx) durableSource(lower string) string {
	if source, ok := tx.dataSources[lower]; ok {
		return source
	}
	return lower
}

// tableIndexes returns the staged index definitions of a table.
func (tx *SchemaTx) tableIndexes(lower string) []*Index {
	names, err := tx.listIndexes()
	if err != nil {
		return nil
	}
	var result []*Index
	for _, name := range names {
		idx, err := tx.getIndex(name)
		if err != nil || !strings.EqualFold(idx.Table, lower) {
			continue
		}
		result = append(result, idx)
	}
	return result
}

// addColumn stages ALTER TABLE ADD COLUMN. The new column, its CHECK
// predicates, its UNIQUE index, and the default backfill of existing rows are
// validated against the rows this session sees (durable rows plus its own
// staged writes) before anything is staged, so a rejected DDL leaves the
// transaction's catalog untouched.
func (tx *SchemaTx) addColumn(table string, column Column, checks []CheckConstraint, unique *Index) error {
	schema, err := tx.getSchema(table)
	if err != nil {
		return err
	}
	if err := checkColumnAbsent(schema, column.Name); err != nil {
		return err
	}
	if unique != nil {
		if !strings.EqualFold(unique.Table, schema.Name) {
			return fmt.Errorf("index %s belongs to table %s, not %s", unique.Name, unique.Table, schema.Name)
		}
		if tx.indexExists(unique.Name) {
			return fmt.Errorf("index already exists: %s", unique.Name)
		}
	}

	now := tx.base.defaultNow()
	// Pin the table generation before scanning: a concurrent commit that lands
	// between the scan and the generation capture would otherwise be invisible,
	// so a durable row the scan did not validate could violate the new
	// constraint. Recording first turns that interleaving into 40001 at COMMIT.
	tx.sess.markTableScanned(strings.ToLower(table))
	// Validation covers the committed rows, mirroring the autocommit DDL path:
	// rows this transaction staged are re-validated against the staged schema at
	// commit (applyOverlaySchemaDefaults), because a staged row may still be
	// rewritten or removed before then.
	rows, err := tx.durableRows(table)
	if err != nil {
		return err
	}
	if !column.Nullable && !columnHasDefault(column) && len(rows) > 0 {
		return &NotNullViolationError{Table: schema.Name, Column: column.Name}
	}
	eval := tx.base.checkEvaluator()
	if len(checks) > 0 && eval == nil {
		return fmt.Errorf("cannot validate CHECK constraints on %s: no CHECK evaluator installed", table)
	}
	seen := make(map[string]struct{})
	for _, row := range rows {
		value, present := rowColumnValue(row, column.Name)
		if !present {
			if fallback, has, defErr := tx.base.columnDefaultValue(column, now); defErr != nil {
				return defErr
			} else if has {
				value = fallback
				present = true
			}
		}
		if len(checks) > 0 {
			probe := cloneRow(row)
			if present {
				probe[column.Name] = value
			}
			for _, check := range checks {
				holds, evalErr := eval(check.Expression, probe)
				if evalErr != nil {
					return fmt.Errorf("evaluate CHECK constraint %s on %s: %w", check.Label(), table, evalErr)
				}
				if !holds {
					return &CheckViolationError{Table: table, Constraint: check.Label()}
				}
			}
		}
		if unique != nil && value != nil {
			encoded := encodeUniqueScalar(value)
			if _, duplicate := seen[encoded]; duplicate {
				return &UniqueViolationError{Constraint: unique.Name}
			}
			seen[encoded] = struct{}{}
		}
	}

	schema = cloneSchema(schema)
	schema.Columns = append(schema.Columns, column)
	schema.Checks = append(schema.Checks, checks...)
	tx.stageSchema(schema)
	if unique != nil {
		tx.stagedIndexes[strings.ToLower(unique.Name)] = cloneIndex(unique)
		names, err := tx.listIndexes()
		if err != nil {
			return err
		}
		tx.stageIndexList(append(names, unique.Name))
		tx.recordRead(tx.base.indexKey(unique.Name), 0)
	}
	if columnHasDefault(column) {
		value, has, err := tx.base.columnDefaultValue(column, now)
		if err != nil {
			return err
		}
		if has {
			name := column.Name
			tx.addRewrite(table, func(row Row) (bool, error) {
				if _, ok := rowColumnValue(row, name); ok {
					return false, nil
				}
				row[name] = value
				return true, nil
			})
		}
	}
	return nil
}

// durableRows returns the committed rows of a table, ignoring this
// transaction's staged rows and pending rewrites.
func (tx *SchemaTx) durableRows(table string) ([]Row, error) {
	var rows []Row
	prefix := tx.sess.table.dataPrefix(tx.durableSource(strings.ToLower(table)))
	err := tx.sess.table.scanRowsAtPrefix(prefix, func(_ string, row Row, _ uint64) (bool, error) {
		rows = append(rows, row)
		return false, nil
	})
	return rows, err
}

// dropColumn stages ALTER TABLE DROP COLUMN.
func (tx *SchemaTx) dropColumn(table, columnName string) error {
	schema, err := tx.getSchema(table)
	if err != nil {
		return err
	}
	if schema.IsPrimaryKeyColumn(columnName) {
		return fmt.Errorf("cannot drop primary key column: %s", columnName)
	}
	columns := make([]Column, 0, len(schema.Columns)-1)
	found := false
	for _, column := range schema.Columns {
		if strings.EqualFold(column.Name, columnName) {
			found = true
			continue
		}
		columns = append(columns, column)
	}
	if !found {
		return fmt.Errorf("column not found: %s", columnName)
	}
	schema = cloneSchema(schema)
	schema.Columns = columns
	tx.stageSchema(schema)
	tx.addRewrite(table, func(row Row) (bool, error) {
		for key := range row {
			if strings.EqualFold(key, columnName) {
				delete(row, key)
				return true, nil
			}
		}
		return false, nil
	})
	tx.sess.markTableScanned(strings.ToLower(table))
	return nil
}

// renameColumn stages ALTER TABLE RENAME COLUMN, including the column
// references of the table's index definitions.
func (tx *SchemaTx) renameColumn(table, oldName, newName string) error {
	schema, err := tx.getSchema(table)
	if err != nil {
		return err
	}
	for _, column := range schema.Columns {
		if strings.EqualFold(column.Name, newName) {
			return fmt.Errorf("column already exists: %s", newName)
		}
	}
	schema = cloneSchema(schema)
	found := false
	for i := range schema.Columns {
		if !strings.EqualFold(schema.Columns[i].Name, oldName) {
			continue
		}
		schema.Columns[i].Name = newName
		found = true
		for j, pk := range schema.PrimaryKeyColumns {
			if strings.EqualFold(pk, oldName) {
				schema.PrimaryKeyColumns[j] = newName
			}
		}
		if strings.EqualFold(schema.PrimaryKey, oldName) {
			schema.PrimaryKey = newName
		}
		break
	}
	if !found {
		return fmt.Errorf("column not found: %s", oldName)
	}
	// CHECK predicates persist SQL text and are not rewritten; the executor
	// rejects a rename that one depends on, matching the autocommit path.
	tx.stageSchema(schema)
	for _, idx := range tx.tableIndexes(strings.ToLower(table)) {
		changed := false
		updated := cloneIndex(idx)
		for i := range updated.Columns {
			if updated.Columns[i].Expression == "" && strings.EqualFold(updated.Columns[i].Name, oldName) {
				updated.Columns[i].Name = newName
				changed = true
			}
		}
		if !changed {
			continue
		}
		tx.stagedIndexes[strings.ToLower(updated.Name)] = updated
		tx.recordRead(tx.base.indexKey(updated.Name), 0)
	}
	tx.addRewrite(table, func(row Row) (bool, error) {
		for key, value := range row {
			if strings.EqualFold(key, oldName) {
				delete(row, key)
				row[newName] = value
				return true, nil
			}
		}
		return false, nil
	})
	tx.sess.markTableScanned(strings.ToLower(table))
	return nil
}

// renameTable stages ALTER TABLE RENAME TABLE. Rows stay under their committed
// data prefix until commit; the session overlay is re-keyed immediately so the
// transaction reads its own writes under the new name.
func (tx *SchemaTx) renameTable(oldName, newName string) error {
	schema, err := tx.getSchema(oldName)
	if err != nil {
		return err
	}
	if tx.tableExists(newName) {
		return fmt.Errorf("table already exists: %s", newName)
	}
	oldLower := strings.ToLower(oldName)
	newLower := strings.ToLower(newName)
	source := tx.durableSource(oldLower)

	schema = cloneSchema(schema)
	schema.Name = newName
	delete(tx.committedSchemas, oldLower)
	delete(tx.committedSchemas, newLower)
	tx.staged[oldLower] = nil
	tx.staged[newLower] = schema
	if source == "" {
		tx.dataSources[newLower] = ""
	} else {
		tx.dataSources[newLower] = source
		if strings.EqualFold(source, newLower) {
			// Renaming back to the committed name cancels the pending move: the
			// rows already live under the destination prefix, so emitting a
			// put+delete for the same key would be order-dependent.
			delete(tx.renames, source)
		} else {
			tx.renames[source] = newName
		}
	}
	delete(tx.dataSources, oldLower)
	// A drop staged before the rename keeps its durable source; the rename
	// moves that source instead, so the drop no longer applies to it.
	delete(tx.drops, newLower)
	if rewrites, ok := tx.rewrites[oldLower]; ok {
		delete(tx.rewrites, oldLower)
		tx.rewrites[newLower] = rewrites
	}

	for _, idx := range tx.tableIndexes(oldLower) {
		updated := cloneIndex(idx)
		updated.Table = newName
		tx.stagedIndexes[strings.ToLower(updated.Name)] = updated
		tx.recordRead(tx.base.indexKey(updated.Name), 0)
	}

	tables, err := tx.listTables()
	if err != nil {
		return err
	}
	found := false
	for i, name := range tables {
		if strings.EqualFold(name, oldName) {
			tables[i] = newName
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("table not found in catalog: %s", oldName)
	}
	tx.stageTableList(tables)

	tx.rekeys = append(tx.rekeys, rekeyPair{from: oldLower, to: newLower})
	tx.sess.rekeyOverlayLocked(oldLower, newLower)
	source = tx.durableSource(newLower)
	if source != "" {
		tx.sess.markTableScanned(source)
	}
	tx.bump()
	return nil
}

// addRewrite appends a row transformation for a table.
func (tx *SchemaTx) addRewrite(table string, rewrite rowRewrite) {
	lower := strings.ToLower(table)
	tx.rewrites[lower] = append(tx.rewrites[lower], rewrite)
}

// rewriteRow applies every pending transformation of a table to a row.
func (tx *SchemaTx) rewriteRow(table string, row Row) (bool, error) {
	rewrites, ok := tx.rewrites[strings.ToLower(table)]
	if !ok {
		return false, nil
	}
	changed := false
	for _, rewrite := range rewrites {
		applied, err := rewrite(row)
		if err != nil {
			return changed, err
		}
		changed = changed || applied
	}
	return changed, nil
}

// commitCatalogOps returns the durable catalog writes of the transaction and
// the compare checks that keep them from overwriting a concurrent commit. The
// caller holds the session lock and the affected table gates.
func (tx *SchemaTx) commitCatalogOps() ([]BatchOp, []CompareCheck, error) {
	var ops []BatchOp
	keys := make([]string, 0, len(tx.staged)+len(tx.stagedIndexes)+2)

	for lower, schema := range tx.staged {
		if schema == nil {
			if _, renamed := tx.renames[lower]; renamed {
				// RENAME TABLE names the source through renames; the delete of
				// the old schema record is emitted below.
				continue
			}
			ops = append(ops,
				BatchOp{Op: batchDelete, Key: []byte(tx.base.schemaKey(lower))},
				BatchOp{Op: batchDelete, Key: []byte(tx.base.rowIDKey(lower))},
			)
			// The ROWID key is an in-memory allocator input, not catalog state a
			// concurrent statement can change, so its delete needs no compare.
			keys = append(keys, tx.base.schemaKey(lower))
			continue
		}
		data, err := json.Marshal(schema)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize schema: %w", err)
		}
		key := tx.base.schemaKey(schema.Name)
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(key), Value: data})
		keys = append(keys, key)
	}
	// A staged table definition whose name replaced an older one deletes the
	// old record in the same batch. A rename whose destination is the committed
	// name (renamed away and back) has no move to publish.
	for source, newName := range tx.renames {
		if strings.EqualFold(source, newName) {
			continue
		}
		ops = append(ops, BatchOp{Op: batchDelete, Key: []byte(tx.base.schemaKey(source))})
		keys = append(keys, tx.base.schemaKey(source))
		ops = append(ops, BatchOp{Op: batchDelete, Key: []byte(tx.base.rowIDKey(source))})
	}

	for lower, index := range tx.stagedIndexes {
		if index == nil {
			ops = append(ops, BatchOp{Op: batchDelete, Key: []byte(tx.base.indexKey(lower))})
			keys = append(keys, tx.base.indexKey(lower))
			continue
		}
		data, err := json.Marshal(index)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize index: %w", err)
		}
		key := tx.base.indexKey(index.Name)
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(key), Value: data})
		keys = append(keys, key)
	}

	if tx.catalogDirty {
		data, err := json.Marshal(tx.tables)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize catalog: %w", err)
		}
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(tx.base.catalogKey()), Value: data})
		keys = append(keys, tx.base.catalogKey())
	}
	if tx.indexListDirty {
		data, err := json.Marshal(tx.indexList)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to serialize index list: %w", err)
		}
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(tx.base.indexListKey()), Value: data})
		keys = append(keys, tx.base.indexListKey())
	}

	checks := make([]CompareCheck, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		checks = append(checks, CompareCheck{Key: []byte(key), LSN: tx.reads[key]})
	}
	return ops, checks, nil
}

// commitDataOps returns the durable row writes of the transaction: dropped
// tables lose every row, renamed tables move to their new prefix, and ALTER
// TABLE transformations are applied to the committed row images. Rows this
// session staged in its overlay are published from the overlay instead, so they
// are skipped here. The caller holds the session lock and the exclusive table
// gates of every affected table.
func (tx *SchemaTx) commitDataOps(budget *ddlBatchBudget) ([]BatchOp, map[string]map[string]*overlayEntry, error) {
	entries := make(map[string]map[string]*overlayEntry)
	var ops []BatchOp
	if len(tx.drops) == 0 && len(tx.renames) == 0 && len(tx.rewrites) == 0 {
		return nil, nil, nil
	}

	affected := make(map[string]bool)
	for table := range tx.drops {
		affected[table] = true
	}
	for table := range tx.renames {
		affected[table] = true
	}
	for table := range tx.rewrites {
		if source := tx.durableSource(table); source != "" && !tx.drops[source] {
			affected[source] = true
		}
	}
	names := make([]string, 0, len(affected))
	for table := range affected {
		names = append(names, table)
	}
	sort.Strings(names)

	for _, source := range names {
		if tx.drops[source] {
			dropped, err := tx.scanRowsForDDL(source, budget, func(key string, row Row) (bool, Row, error) {
				if tx.sess.overlayHolds(strings.ToLower(source), key) {
					return false, nil, nil
				}
				return true, nil, nil
			})
			if err != nil {
				return nil, nil, err
			}
			ops = append(ops, dropped...)
			continue
		}

		newName, renamed := tx.renames[source]
		if renamed && strings.EqualFold(source, newName) {
			// A rename that returned to the committed name has no row move; the
			// table still owes its pending ALTER rewrites.
			renamed = false
		}
		if renamed {
			moved, movedEntries, err := tx.renameRowOps(source, newName, budget)
			if err != nil {
				return nil, nil, err
			}
			lower := strings.ToLower(newName)
			if entries[lower] == nil {
				entries[lower] = make(map[string]*overlayEntry)
			}
			for key, entry := range movedEntries {
				entries[lower][key] = entry
			}
			ops = append(ops, moved...)
			continue
		}

		rewrites := tx.rewrites[source]
		if len(rewrites) == 0 {
			continue
		}
		updated, err := tx.scanRowsForDDL(source, budget, func(key string, row Row) (bool, Row, error) {
			if tx.sess.overlayHolds(strings.ToLower(source), key) {
				return false, nil, nil
			}
			changed, err := tx.rewriteRow(source, row)
			if err != nil {
				return false, nil, err
			}
			return changed, row, nil
		})
		if err != nil {
			return nil, nil, err
		}
		lower := strings.ToLower(source)
		if entries[lower] == nil {
			entries[lower] = make(map[string]*overlayEntry)
		}
		for _, op := range updated {
			row, err := decodeRow(op.Value)
			if err != nil {
				return nil, nil, err
			}
			entries[lower][string(op.Key)] = &overlayEntry{row: row}
			ops = append(ops, op)
		}
	}
	return ops, entries, nil
}

// renameRowOps builds the row move of one RENAME TABLE: every committed row of
// source is written under the destination prefix and its old key deleted. A row
// the transaction already re-staged under the destination (an UPDATE, DELETE,
// or re-INSERT after the rename) is owned by the session overlay, so only the
// stale committed key is deleted; without that delete the rename would leave an
// unreachable row under the dropped table name.
func (tx *SchemaTx) renameRowOps(source, newName string, budget *ddlBatchBudget) ([]BatchOp, map[string]*overlayEntry, error) {
	entries := make(map[string]*overlayEntry)
	var ops []BatchOp
	oldPrefix := tx.sess.table.dataPrefix(source)
	newPrefix := tx.sess.table.dataPrefix(newName)
	lowerNew := strings.ToLower(newName)
	err := tx.sess.table.scanRowsAtPrefix(oldPrefix, func(oldKey string, row Row, _ uint64) (bool, error) {
		if _, err := tx.rewriteRow(newName, row); err != nil {
			return true, err
		}
		suffix := strings.TrimPrefix(oldKey, oldPrefix)
		newKey := newPrefix + suffix
		if tx.sess.overlayHolds(lowerNew, newKey) {
			deleted := BatchOp{Op: batchDelete, Key: []byte(oldKey)}
			if err := budget.reserve(source, 1, batchOpBytes(deleted)); err != nil {
				return true, err
			}
			ops = append(ops, deleted)
			return false, nil
		}
		value, err := encodeRow(row)
		if err != nil {
			return true, err
		}
		put := BatchOp{Op: batchPut, Key: []byte(newKey), Value: value}
		del := BatchOp{Op: batchDelete, Key: []byte(oldKey)}
		if err := budget.reserve(source, 2, batchOpBytes(put)+batchOpBytes(del)); err != nil {
			return true, err
		}
		entries[newKey] = &overlayEntry{row: row}
		ops = append(ops, put, del)
		return false, nil
	})
	return ops, entries, err
}

// ddlBatchBudget bounds the row operations one transactional DDL commit may add
// to its single compare-batch. The whole transaction is published atomically as
// one storage transaction, so it cannot page its writes the way autocommit DDL
// does: a rewrite that would exceed the operation or byte ceiling fails with an
// explicit limit error while the table is still being scanned, instead of
// buffering every row and failing inside CompareBatchWrite.
type ddlBatchBudget struct {
	ops   int
	bytes int
}

// reserve counts one table's pending operations and fails as soon as the batch
// ceiling is crossed.
func (b *ddlBatchBudget) reserve(table string, ops, bytes int) error {
	b.ops += ops
	b.bytes += bytes
	if b.ops > maxOperations || b.bytes > maxTransactionSize {
		return &ddlBatchLimitError{Table: table, Operations: b.ops, Bytes: b.bytes}
	}
	return nil
}

// ddlBatchLimitError reports that a transactional DDL row rewrite cannot fit in
// the single storage transaction the commit publishes.
type ddlBatchLimitError struct {
	Table      string
	Operations int
	Bytes      int
}

func (e *ddlBatchLimitError) Error() string {
	return fmt.Sprintf(
		"transactional DDL on %s cannot be published atomically: the rewrite needs more than the storage batch limit (%d operations / %d bytes, reached %d operations / %d bytes); run the statement outside a transaction so it can page its writes, or split the migration",
		e.Table, maxOperations, maxTransactionSize, e.Operations, e.Bytes)
}

// batchOpBytes is the wire size of one operation (header, key, and value), used
// to bound a commit batch before it is encoded.
func batchOpBytes(op BatchOp) int {
	return 12 + len(op.Key) + len(op.Value)
}

// scanRowsForDDL streams a durable table's rows through transform. When
// transform reports a change the row is re-encoded and returned as a put under
// its existing key; when it reports removal the key is returned as a delete.
func (tx *SchemaTx) scanRowsForDDL(table string, budget *ddlBatchBudget, transform func(key string, row Row) (write bool, replacement Row, err error)) ([]BatchOp, error) {
	var ops []BatchOp
	prefix := []byte(tx.sess.table.dataPrefix(table))
	err := tx.base.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.Scan(prefix)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := cursor.Close(); retErr == nil {
				retErr = cerr
			}
		}()
		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, entry := range entries {
				row, err := decodeRow(entry.Value)
				if err != nil {
					return err
				}
				write, replacement, err := transform(string(entry.Key), row)
				if err != nil {
					return err
				}
				if !write {
					continue
				}
				if replacement == nil {
					op := BatchOp{Op: batchDelete, Key: append([]byte(nil), entry.Key...)}
					if err := budget.reserve(table, 1, batchOpBytes(op)); err != nil {
						return err
					}
					ops = append(ops, op)
					continue
				}
				value, err := encodeRow(replacement)
				if err != nil {
					return err
				}
				op := BatchOp{Op: batchPut, Key: append([]byte(nil), entry.Key...), Value: value}
				if err := budget.reserve(table, 1, batchOpBytes(op)); err != nil {
					return err
				}
				ops = append(ops, op)
			}
			if done {
				return nil
			}
		}
	})
	return ops, err
}

// touchedTables lists the lowercased table names whose caches the commit must
// invalidate, including the source names rows moved away from.
func (tx *SchemaTx) touchedTables() []string {
	set := make(map[string]bool)
	for lower, schema := range tx.staged {
		set[lower] = true
		if schema != nil {
			set[strings.ToLower(schema.Name)] = true
			if source, ok := tx.dataSources[lower]; ok && source != "" {
				set[source] = true
			}
		}
	}
	for table := range tx.drops {
		set[table] = true
	}
	for table := range tx.renames {
		set[table] = true
	}
	for table := range tx.rewrites {
		set[table] = true
		if source := tx.durableSource(table); source != "" {
			set[source] = true
		}
	}
	names := make([]string, 0, len(set))
	for table := range set {
		names = append(names, table)
	}
	sort.Strings(names)
	return names
}

// touchedIndexes lists the lowercased index names whose caches the commit must
// reset.
func (tx *SchemaTx) touchedIndexes() []string {
	names := make([]string, 0, len(tx.stagedIndexes))
	for lower := range tx.stagedIndexes {
		names = append(names, lower)
	}
	sort.Strings(names)
	return names
}

// hasDataEffects reports whether the commit must rewrite or remove durable rows.
func (tx *SchemaTx) hasDataEffects() bool {
	return len(tx.drops) > 0 || len(tx.renames) > 0 || len(tx.rewrites) > 0
}

// hasDDL reports whether the transaction staged a catalog change that the
// commit has to publish.
func (tx *SchemaTx) hasDDL() bool {
	return len(tx.staged) > 0 || len(tx.stagedIndexes) > 0 || tx.catalogDirty || tx.indexListDirty || tx.hasDataEffects()
}

// Session catalog access
//
// A Session is the catalog view of one SQL connection. Outside an explicit
// transaction every accessor delegates to the shared SchemaManager; while a
// transaction is open the accessors resolve against its SchemaTx overlay, so
// the session reads its own DDL without publishing it.

// GetSchema returns the table definition this session observes.
func (s *Session) GetSchema(name string) (*Schema, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getSchemaLocked(name)
}

// getSchemaLocked resolves a table definition (caller holds the session lock).
func (s *Session) getSchemaLocked(name string) (*Schema, error) {
	if s.schemaTx != nil {
		return s.schemaTx.getSchema(name)
	}
	return s.schema.GetSchema(name)
}

// TableExists reports whether the session can resolve a table.
func (s *Session) TableExists(name string) bool {
	_, err := s.GetSchema(name)
	return err == nil
}

// ListTables returns the table catalog this session observes.
func (s *Session) ListTables() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return s.schemaTx.listTables()
	}
	return s.schema.ListTables()
}

// GetIndex returns an index definition this session observes.
func (s *Session) GetIndex(name string) (*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return s.schemaTx.getIndex(name)
	}
	return s.schema.GetIndex(name)
}

// IndexExists reports whether the session can resolve an index.
func (s *Session) IndexExists(name string) bool {
	_, err := s.GetIndex(name)
	return err == nil
}

// ListIndexes returns the index catalog this session observes.
func (s *Session) ListIndexes() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return s.schemaTx.listIndexes()
	}
	return s.schema.ListIndexes()
}

// ListTableIndexes returns the index definitions of a table as this session
// observes them.
func (s *Session) ListTableIndexes(table string) ([]*Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx != nil {
		return s.schemaTx.listTableIndexes(table)
	}
	return s.schema.ListTableIndexes(table)
}

// Version identifies the catalog generation this session observes, including
// staged DDL, so the executor resynchronizes its analyzer catalog after a
// transactional DDL statement and again after the overlay is discarded.
func (s *Session) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versionLocked()
}

func (s *Session) versionLocked() uint64 {
	if s.schemaTx != nil {
		return s.schemaTx.version
	}
	return s.schema.Version()
}

// GetNextRowID allocates from the shared per-table ROWID counter. The counter is
// derived from durable rows and is not part of the catalog, so a transaction
// uses the same allocator as autocommit.
func (s *Session) GetNextRowID(table string) (int64, error) {
	return s.schema.GetNextRowID(table)
}

// UpdateMaxRowID raises the shared per-table ROWID counter.
func (s *Session) UpdateMaxRowID(table string, rowid int64) error {
	return s.schema.UpdateMaxRowID(table, rowid)
}

// CreateTable stages CREATE TABLE inside a transaction and publishes it
// immediately otherwise.
func (s *Session) CreateTable(schema *Schema) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.CreateTable(schema)
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.createTable(schema) })
}

// DropTable stages DROP TABLE inside a transaction and publishes it otherwise.
func (s *Session) DropTable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(name, func() error { return s.schema.DropTable(name) })
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.dropTable(name) })
}

// CreateIndex stages CREATE INDEX inside a transaction and publishes it
// otherwise. A UNIQUE index is validated against the rows the session observes,
// so a transaction that staged duplicate rows fails the statement, not the
// commit.
func (s *Session) CreateIndex(index *Index) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(index.Table, func() error {
			if index.Unique {
				return s.table.CreateUniqueIndex(index)
			}
			return s.schema.CreateIndex(index)
		})
	}
	return s.stageDDL(func(tx *SchemaTx) error {
		if err := tx.createIndex(index); err != nil {
			return err
		}
		if !index.Unique {
			return nil
		}
		return s.validateUniqueIndexLocked(index)
	})
}

// validateUniqueIndexLocked rejects a staged UNIQUE index whose indexed values
// already collide in the rows this session observes (caller holds the session
// lock).
func (s *Session) validateUniqueIndexLocked(index *Index) error {
	rows, err := s.selectLocked(index.Table, nil)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		encoded, isNull, err := s.table.encodeUniqueValue(row, index.Columns)
		if err != nil {
			return err
		}
		if isNull {
			continue
		}
		if _, duplicate := seen[encoded]; duplicate {
			return &UniqueViolationError{Constraint: index.Name}
		}
		seen[encoded] = struct{}{}
	}
	return nil
}

// DropIndex stages DROP INDEX inside a transaction and publishes it otherwise.
func (s *Session) DropIndex(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.DropIndex(name)
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.dropIndex(name) })
}

// AddColumn stages ALTER TABLE ADD COLUMN inside a transaction and publishes it
// otherwise.
func (s *Session) AddColumn(table string, column Column, checks []CheckConstraint, unique *Index) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(table, func() error { return s.schema.AddColumn(table, column, checks, unique) })
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.addColumn(table, column, checks, unique) })
}

// DropColumn stages ALTER TABLE DROP COLUMN inside a transaction and publishes
// it otherwise.
func (s *Session) DropColumn(table, columnName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(table, func() error { return s.schema.DropColumn(table, columnName) })
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.dropColumn(table, columnName) })
}

// RenameTable stages ALTER TABLE RENAME TABLE inside a transaction and
// publishes it otherwise.
func (s *Session) RenameTable(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(oldName, func() error { return s.schema.RenameTable(oldName, newName) })
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.renameTable(oldName, newName) })
}

// RenameColumn stages ALTER TABLE RENAME COLUMN inside a transaction and
// publishes it otherwise.
func (s *Session) RenameColumn(table, oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.guardTable(table, func() error { return s.schema.RenameColumn(table, oldName, newName) })
	}
	return s.stageDDL(func(tx *SchemaTx) error { return tx.renameColumn(table, oldName, newName) })
}

// stageDDL runs one DDL statement against the transaction overlay and restores
// the catalog, overlay, and read set if it fails, so a rejected statement
// leaves the transaction exactly as it was (caller holds the session lock).
func (s *Session) stageDDL(apply func(tx *SchemaTx) error) error {
	savepoint := s.savepointLocked()
	if err := apply(s.schemaTx); err != nil {
		s.rollbackToSavepointLocked(savepoint)
		return err
	}
	return nil
}

// overlayHolds reports whether the session staged the given durable key of a
// table.
func (s *Session) overlayHolds(lowerTable, key string) bool {
	_, ok := s.overlay[lowerTable][key]
	return ok
}

// markTableScanned pins the table's current generation as a read so the commit
// fails with a serialization error if another session writes the table between
// the DDL statement and the commit.
func (s *Session) markTableScanned(table string) {
	lower := strings.ToLower(table)
	if _, ok := s.scanGens[lower]; !ok {
		s.scanGens[lower] = s.table.generation(lower)
	}
}

// dropOverlayTableLocked discards the staged rows of a table the transaction
// dropped, including their mutation-log entries (caller holds the session lock).
func (s *Session) dropOverlayTableLocked(lower string) {
	delete(s.overlay, lower)
	log := s.log[:0]
	for _, mutation := range s.log {
		if mutation.table != lower {
			log = append(log, mutation)
		}
	}
	s.log = log
	delete(s.scanGens, lower)
	for key, predicate := range s.predicateGens {
		if predicate.table == lower {
			delete(s.predicateGens, key)
		}
	}
	prefix := s.table.dataPrefix(lower)
	for key := range s.reads {
		if strings.HasPrefix(key, prefix) {
			delete(s.reads, key)
		}
	}
}

// rekeyOverlayLocked moves the staged rows of a renamed table to the new data
// prefix so the transaction reads its own rows back under the new name
// (caller holds the session lock).
func (s *Session) rekeyOverlayLocked(oldTable, newTable string) {
	oldLower := strings.ToLower(oldTable)
	newLower := strings.ToLower(newTable)
	if oldLower == newLower {
		return
	}
	oldPrefix := s.table.dataPrefix(oldLower)
	newPrefix := s.table.dataPrefix(newLower)

	if entries, ok := s.overlay[oldLower]; ok {
		delete(s.overlay, oldLower)
		moved := make(map[string]*overlayEntry, len(entries))
		for key, entry := range entries {
			moved[newPrefix+strings.TrimPrefix(key, oldPrefix)] = entry
		}
		s.overlay[newLower] = moved
	}
	for i := range s.log {
		if s.log[i].table != oldLower {
			continue
		}
		s.log[i].table = newLower
		s.log[i].key = newPrefix + strings.TrimPrefix(s.log[i].key, oldPrefix)
	}
	if gen, ok := s.scanGens[oldLower]; ok {
		delete(s.scanGens, oldLower)
		s.scanGens[newLower] = gen
	}
	reads := make(map[string]uint64, len(s.reads))
	for key, lsn := range s.reads {
		if strings.HasPrefix(key, oldPrefix) {
			reads[newPrefix+strings.TrimPrefix(key, oldPrefix)] = lsn
			continue
		}
		reads[key] = lsn
	}
	s.reads = reads
	predicates := make(map[string]predicateRead, len(s.predicateGens))
	for key, predicate := range s.predicateGens {
		if predicate.table == oldLower {
			predicate.table = newLower
		}
		if strings.HasPrefix(key, oldPrefix) {
			key = newPrefix + strings.TrimPrefix(key, oldPrefix)
		}
		predicates[key] = predicate
	}
	s.predicateGens = predicates
}

// ForeignKeyParticipants reports whether a write to table can affect referential
// integrity as this session observes the catalog.
func (s *Session) ForeignKeyParticipants(table string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.ForeignKeyParticipants(table)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return false, err
	}
	return catalog.participants[strings.ToLower(table)], nil
}

// ForeignKeyColumnDependency returns the constraints that keep a column in
// place, as this session observes the catalog.
func (s *Session) ForeignKeyColumnDependency(table, column string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.ForeignKeyColumnDependency(table, column)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return nil, err
	}
	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var labels []string
	add := func(child string, fk ForeignKeyConstraint) {
		label := foreignKeyReferenceName(child, fk)
		if seen[label] {
			return
		}
		seen[label] = true
		labels = append(labels, label)
	}
	for _, fk := range schema.ForeignKeys {
		for _, name := range fk.Columns {
			if strings.EqualFold(name, column) {
				add(schema.Name, fk)
				break
			}
		}
	}
	for _, ref := range catalog.referencing[strings.ToLower(table)] {
		for _, name := range ref.FK.RefColumns {
			if strings.EqualFold(name, column) {
				add(ref.Table, ref.FK)
				break
			}
		}
	}
	sort.Strings(labels)
	return labels, nil
}

// ForeignKeyTableDependency returns the constraints that name table as their
// referenced parent, as this session observes the catalog.
func (s *Session) ForeignKeyTableDependency(table string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.ForeignKeyTableDependency(table)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var labels []string
	for _, ref := range catalog.referencing[strings.ToLower(table)] {
		label := foreignKeyReferenceName(ref.Table, ref.FK)
		if seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels, nil
}

// ForeignKeyClosure returns the constrained component of the given tables as
// this session observes the catalog.
func (s *Session) ForeignKeyClosure(tables []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.schemaTx == nil {
		return s.schema.ForeignKeyClosure(tables)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return nil, err
	}
	return catalog.closure(tables), nil
}

// foreignKeyCatalogLocked builds the foreign-key metadata of the session's
// catalog view (caller holds the session lock).
func (s *Session) foreignKeyCatalogLocked() (*foreignKeyCatalog, error) {
	version := s.versionLocked()
	if s.fkCatalog != nil && s.fkVersion == version {
		return s.fkCatalog, nil
	}
	tables, err := s.schemaTx.listTables()
	if err != nil {
		return nil, err
	}
	catalog, err := buildForeignKeyCatalog(version, tables, func(name string) (*Schema, error) {
		return s.getSchemaLocked(name)
	})
	if err != nil {
		return nil, err
	}
	s.fkCatalog = catalog
	s.fkVersion = version
	return catalog, nil
}
