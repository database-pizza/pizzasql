package storage

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ForeignKeyViolationError reports a row that would leave a foreign key
// dangling. It is a distinct type so the protocol layers can report SQLSTATE
// 23503 instead of a generic execution error.
type ForeignKeyViolationError struct {
	// Table is the child table whose row cannot be written, or whose rows would
	// be orphaned by the parent change.
	Table string
	// Constraint is the label of the violated FOREIGN KEY declaration.
	Constraint string
}

func (e *ForeignKeyViolationError) Error() string {
	if e.Constraint != "" {
		return fmt.Sprintf("FOREIGN KEY constraint failed: %s", e.Constraint)
	}
	return "FOREIGN KEY constraint failed: " + e.Table
}

// foreignKeyValues returns the formatted values a row holds for the given
// columns, and reports false when any component is NULL or missing. MATCH
// SIMPLE semantics: a partially NULL key is never matched against the parent, so
// the constraint does not apply to that row.
func foreignKeyValues(row Row, columns []string) ([]string, bool) {
	values := make([]string, len(columns))
	for i, name := range columns {
		value, ok := rowColumnValue(row, name)
		if !ok || value == nil {
			return nil, false
		}
		values[i] = formatIndexValue(value)
	}
	return values, true
}

// foreignKeyMatches reports whether row's columns equal want.
func foreignKeyMatches(row Row, columns, want []string) bool {
	if len(columns) != len(want) {
		return false
	}
	got, ok := foreignKeyValues(row, columns)
	if !ok {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func foreignKeyValuesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// parentRowExists reports whether table holds a row whose columns match want.
// extra supplies rows that are not durable yet (the rows of the current
// statement), so a self-referential row can reference itself or another row of
// the same multi-row insert. The scan deliberately takes no table gate: callers
// hold the table's write gate already, and re-entering it would deadlock on a
// self-referential constraint.
func (m *TableManager) parentRowExists(table string, extra []Row, columns, want []string) (bool, error) {
	for _, row := range extra {
		if foreignKeyMatches(row, columns, want) {
			return true, nil
		}
	}
	found := false
	err := m.scanRows(table, func(row Row) (bool, error) {
		if foreignKeyMatches(row, columns, want) {
			found = true
			return true, nil
		}
		return false, nil
	})
	return found, err
}

// validateInsertForeignKeys enforces every FOREIGN KEY declared on schema for
// rows that are about to be written. rows holds all rows of the statement so a
// self-referential insert can reference a sibling row in the same batch.
func (m *TableManager) validateInsertForeignKeys(schema *Schema, rows []Row) error {
	if len(schema.ForeignKeys) == 0 {
		return nil
	}
	for _, fk := range schema.ForeignKeys {
		for _, row := range rows {
			want, ok := foreignKeyValues(row, fk.Columns)
			if !ok {
				continue
			}
			var extra []Row
			if strings.EqualFold(fk.RefTable, schema.Name) {
				extra = rows
			}
			found, err := m.parentRowExists(fk.RefTable, extra, fk.RefColumns, want)
			if err != nil {
				return fmt.Errorf("validate foreign key %s: %w", fk.Label(), err)
			}
			if !found {
				return &ForeignKeyViolationError{Table: schema.Name, Constraint: fk.Label()}
			}
		}
	}
	return nil
}

// validateParentKeyRemoval rejects a parent change that would orphan an existing
// child row. oldRow is the durable parent image; newRow is the replacement image
// for an UPDATE (nil for a DELETE). A change that leaves every referenced value
// untouched cannot orphan anything and is not checked.
//
// A self-referential table needs no special exemption for a row that references
// itself: the row being deleted (or re-keyed) is skipped in the child scan and
// its replacement image is evaluated instead, so deleting a self-referencing row
// works while re-keying it into a dangling reference still fails.
func (m *TableManager) validateParentKeyRemoval(table string, oldRow, newRow Row) error {
	referencing, err := m.schema.referencingForeignKeys(table)
	if err != nil {
		return err
	}
	selfPK := ""
	var selfSchema *Schema
	if len(referencing) > 0 {
		schema, err := m.schema.GetSchema(table)
		if err != nil {
			return err
		}
		selfSchema = schema
		if pk, err := schema.PrimaryKeyForRow(oldRow); err == nil {
			selfPK = pk
		}
	}
	for _, ref := range referencing {
		oldKey, ok := foreignKeyValues(oldRow, ref.FK.RefColumns)
		if !ok {
			continue
		}
		if newRow != nil {
			if newKey, ok := foreignKeyValues(newRow, ref.FK.RefColumns); ok && foreignKeyValuesEqual(oldKey, newKey) {
				continue
			}
		}
		selfTable := strings.EqualFold(ref.Table, table)
		found := false
		err := m.scanRows(ref.Table, func(row Row) (bool, error) {
			if selfTable && selfPK != "" {
				if pk, err := selfSchema.PrimaryKeyForRow(row); err == nil && pk == selfPK {
					return false, nil
				}
			}
			if foreignKeyMatches(row, ref.FK.Columns, oldKey) {
				found = true
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("validate foreign key %s: %w", ref.FK.Label(), err)
		}
		if !found && selfTable && newRow != nil && foreignKeyMatches(newRow, ref.FK.Columns, oldKey) {
			found = true
		}
		if found {
			return &ForeignKeyViolationError{Table: ref.Table, Constraint: ref.FK.Label()}
		}
	}
	return nil
}

// referencingForeignKey is one FOREIGN KEY declaration that targets a table.
type referencingForeignKey struct {
	Table string
	FK    ForeignKeyConstraint
}

// foreignKeyCatalog is the cached foreign-key metadata of the database. It is
// keyed by the schema manager version so a DDL change rebuilds it.
type foreignKeyCatalog struct {
	version      uint64
	participants map[string]bool
	referencing  map[string][]referencingForeignKey
	adjacency    map[string]map[string]bool
}

// foreignKeyCatalogSnapshot returns the current foreign-key metadata, rebuilding
// it when the schema version changed.
func (m *SchemaManager) foreignKeyCatalogSnapshot() (*foreignKeyCatalog, error) {
	version := m.Version()
	m.fkCatalogMu.Lock()
	cached := m.fkCatalog
	m.fkCatalogMu.Unlock()
	if cached != nil && cached.version == version {
		return cached, nil
	}

	tables, err := m.ListTables()
	if err != nil {
		return nil, err
	}
	catalog, err := buildForeignKeyCatalog(version, tables, m.GetSchema)
	if err != nil {
		return nil, err
	}

	m.fkCatalogMu.Lock()
	if m.Version() == version {
		m.fkCatalog = catalog
	}
	m.fkCatalogMu.Unlock()
	return catalog, nil
}

// buildForeignKeyCatalog derives the foreign-key metadata of a catalog view from
// its table list and definitions. Both the shared SchemaManager cache and a
// transaction's staged catalog build it, so a session validates against the
// constraints it can see.
func buildForeignKeyCatalog(version uint64, tables []string, getSchema func(string) (*Schema, error)) (*foreignKeyCatalog, error) {
	catalog := &foreignKeyCatalog{
		version:      version,
		participants: make(map[string]bool),
		referencing:  make(map[string][]referencingForeignKey),
		adjacency:    make(map[string]map[string]bool),
	}
	link := func(a, b string) {
		if catalog.adjacency[a] == nil {
			catalog.adjacency[a] = make(map[string]bool)
		}
		catalog.adjacency[a][b] = true
	}
	for _, name := range tables {
		schema, err := getSchema(name)
		if err != nil {
			return nil, err
		}
		child := strings.ToLower(schema.Name)
		for _, fk := range schema.ForeignKeys {
			parent := strings.ToLower(fk.RefTable)
			catalog.participants[child] = true
			catalog.participants[parent] = true
			catalog.referencing[parent] = append(catalog.referencing[parent], referencingForeignKey{Table: schema.Name, FK: fk})
			link(child, parent)
			link(parent, child)
		}
	}
	return catalog, nil
}

// closure returns the foreign-key-connected component of the given tables.
func (c *foreignKeyCatalog) closure(tables []string) map[string]bool {
	closure := make(map[string]bool)
	queue := make([]string, 0, len(tables))
	for _, table := range tables {
		if c.participants[strings.ToLower(table)] {
			queue = append(queue, strings.ToLower(table))
		}
	}
	for len(queue) > 0 {
		table := queue[0]
		queue = queue[1:]
		if closure[table] {
			continue
		}
		closure[table] = true
		neighbors := make([]string, 0, len(c.adjacency[table]))
		for neighbor := range c.adjacency[table] {
			neighbors = append(neighbors, neighbor)
		}
		sort.Strings(neighbors)
		for _, neighbor := range neighbors {
			if !closure[neighbor] {
				queue = append(queue, neighbor)
			}
		}
	}
	return closure
}

// referencingForeignKeys returns every FOREIGN KEY declaration whose parent is
// table.
func (m *SchemaManager) referencingForeignKeys(table string) ([]referencingForeignKey, error) {
	catalog, err := m.foreignKeyCatalogSnapshot()
	if err != nil {
		return nil, err
	}
	return catalog.referencing[strings.ToLower(table)], nil
}

// foreignKeyReferenceName formats a constraint for a DDL error message. The
// child table is included because an unnamed constraint's label is only unique
// within its table.
func foreignKeyReferenceName(child string, fk ForeignKeyConstraint) string {
	return fmt.Sprintf("%s on %s", fk.Label(), child)
}

// ForeignKeyColumnDependency returns the constraints that keep a column in
// place: a FOREIGN KEY this table declares on it, or a FOREIGN KEY (from this
// table or another one) that references it as a parent column. DROP/RENAME
// COLUMN cannot rewrite persisted constraint text, so a non-empty result must
// reject the change instead of silently disabling the constraint.
func (m *SchemaManager) ForeignKeyColumnDependency(table, column string) ([]string, error) {
	catalog, err := m.foreignKeyCatalogSnapshot()
	if err != nil {
		return nil, err
	}
	schema, err := m.GetSchema(table)
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
// referenced parent, including a self-reference. RENAME TABLE cannot rewrite
// them, and DROP TABLE would leave them dangling.
func (m *SchemaManager) ForeignKeyTableDependency(table string) ([]string, error) {
	catalog, err := m.foreignKeyCatalogSnapshot()
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

// ForeignKeyParticipants reports whether a write to table can affect referential
// integrity. A table participates when it declares a FOREIGN KEY or when another
// table references it.
func (m *SchemaManager) ForeignKeyParticipants(table string) (bool, error) {
	catalog, err := m.foreignKeyCatalogSnapshot()
	if err != nil {
		return false, err
	}
	return catalog.participants[strings.ToLower(table)], nil
}

// ForeignKeyClosure returns the tables whose rows must be re-validated when the
// given tables change: the foreign-key-connected component of every changed
// table that participates in a constraint. The result is empty when no changed
// table participates.
func (m *SchemaManager) ForeignKeyClosure(tables []string) (map[string]bool, error) {
	catalog, err := m.foreignKeyCatalogSnapshot()
	if err != nil {
		return nil, err
	}
	return catalog.closure(tables), nil
}

// lockForeignKeyWrites serializes writes that can affect referential integrity,
// so a validation scan cannot interleave with a concurrent insert, update, or
// delete on the other side of a constraint. It must be acquired before any
// table gate: commits validate several tables at once, and the canonical order
// (foreign-key gate, then table gates) is what keeps that deadlock-free.
func (m *TableManager) lockForeignKeyWrites(tables ...string) (func(), error) {
	for _, table := range tables {
		participates, err := m.schema.ForeignKeyParticipants(table)
		if err != nil {
			return nil, err
		}
		if participates {
			m.foreignKeyMu.Lock()
			return m.foreignKeyMu.Unlock, nil
		}
	}
	return func() {}, nil
}

// LockForeignKeyWriteGate takes the foreign-key write gate unconditionally.
// Schema DDL uses it around CREATE TABLE and DROP TABLE: the table that gains or
// loses a constraint is not part of the foreign-key graph yet (or has just been
// removed from it), so the participant check would skip the gate exactly when
// the graph is changing. Callers must take it before any table gate.
func (m *TableManager) LockForeignKeyWriteGate() func() {
	m.foreignKeyMu.Lock()
	return m.foreignKeyMu.Unlock
}

// foreignKeyStateValidator validates the final row state of a set of tables
// against every FOREIGN KEY declaration among them.
type foreignKeyStateValidator struct {
	tables  map[string][]Row
	keySets map[string]map[string]bool
	schemas map[string]*Schema
}

// validateForeignKeyState checks every child row of the given tables against its
// parent rows. tables maps a lowercased table name to its final row set (durable
// rows with staged deletes applied plus staged writes).
func (v *foreignKeyStateValidator) validateForeignKeyState() error {
	if v.keySets == nil {
		v.keySets = make(map[string]map[string]bool)
	}
	for table, rows := range v.tables {
		schema := v.schemas[table]
		if schema == nil {
			continue
		}
		for _, fk := range schema.ForeignKeys {
			parent := strings.ToLower(fk.RefTable)
			parentRows, ok := v.tables[parent]
			if !ok {
				return fmt.Errorf("foreign key %s: parent table %s is not available for validation", fk.Label(), fk.RefTable)
			}
			key := parent + "\x00" + joinForeignKeyValues(fk.RefColumns)
			set, ok := v.keySets[key]
			if !ok {
				set = make(map[string]bool, len(parentRows))
				for _, row := range parentRows {
					values, ok := foreignKeyValues(row, fk.RefColumns)
					if !ok {
						continue
					}
					set[joinForeignKeyValues(values)] = true
				}
				v.keySets[key] = set
			}
			for _, row := range rows {
				values, ok := foreignKeyValues(row, fk.Columns)
				if !ok {
					continue
				}
				if !set[joinForeignKeyValues(values)] {
					return &ForeignKeyViolationError{Table: schema.Name, Constraint: fk.Label()}
				}
			}
		}
	}
	return nil
}

func joinForeignKeyValues(values []string) string {
	var b strings.Builder
	for _, value := range values {
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteByte(':')
		b.WriteString(value)
	}
	return b.String()
}

// foreignKeyClosure returns the tables a commit must re-validate, or nil when no
// staged table participates in a foreign key.
func (s *Session) foreignKeyClosure() (map[string]bool, error) {
	tables := make([]string, 0, len(s.overlay))
	seen := make(map[string]bool, len(s.overlay))
	for table := range s.overlay {
		if seen[table] {
			continue
		}
		seen[table] = true
		tables = append(tables, table)
	}
	// DDL staged by this transaction can add or remove a constraint without
	// staging a row, so its tables join the validation component too.
	if s.schemaTx != nil {
		for table, schema := range s.schemaTx.staged {
			if schema == nil || seen[table] {
				continue
			}
			seen[table] = true
			tables = append(tables, table)
		}
	}
	if len(tables) == 0 {
		return nil, nil
	}
	return s.foreignKeyClosureLocked(tables)
}

// foreignKeyClosureLocked computes the constrained component of tables against
// the catalog this session observes (caller holds the session lock).
func (s *Session) foreignKeyClosureLocked(tables []string) (map[string]bool, error) {
	if s.schemaTx == nil {
		return s.schema.ForeignKeyClosure(tables)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return nil, err
	}
	return catalog.closure(tables), nil
}

// referencingForeignKeysLocked returns the FOREIGN KEY declarations whose parent
// is table, as this session observes the catalog.
func (s *Session) referencingForeignKeysLocked(table string) ([]referencingForeignKey, error) {
	if s.schemaTx == nil {
		return s.schema.referencingForeignKeys(table)
	}
	catalog, err := s.foreignKeyCatalogLocked()
	if err != nil {
		return nil, err
	}
	return catalog.referencing[strings.ToLower(table)], nil
}

// validateForeignKeyFinalState checks the committed row image of every table in
// the constrained component. The caller holds the session lock and the table
// gates of those tables.
func (s *Session) validateForeignKeyFinalState(closure map[string]bool) error {
	if len(closure) == 0 {
		return nil
	}
	schemas := make(map[string]*Schema, len(closure))
	tables := make(map[string][]Row, len(closure))
	for table := range closure {
		schema, err := s.getSchemaLocked(table)
		if err != nil {
			return err
		}
		rows, err := s.finalRows(table, schema)
		if err != nil {
			return err
		}
		schemas[table] = schema
		tables[table] = rows
	}
	validator := foreignKeyStateValidator{tables: tables, schemas: schemas}
	return validator.validateForeignKeyState()
}

// finalRows returns the table's durable rows with the transaction overlay
// applied: deleted keys are removed, staged rows are added, and a key that was
// both deleted and re-staged appears once with its staged image. It takes no
// table gate because the caller holds the table's exclusive gate.
func (s *Session) finalRows(table string, schema *Schema) ([]Row, error) {
	overlay := s.overlay[strings.ToLower(table)]
	source := s.durableSourceLocked(table)
	rows := make([]Row, 0)
	err := s.table.scanRowsAtPrefix(s.table.dataPrefix(source), func(_ string, row Row, _ uint64) (bool, error) {
		pk, err := schema.PrimaryKeyForRow(row)
		if err != nil {
			return true, err
		}
		if _, staged := overlay[s.table.dataKey(table, pk)]; staged {
			return false, nil
		}
		if err := s.applyStagedRewritesLocked(table, row); err != nil {
			return true, err
		}
		rows = append(rows, row)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	for _, entry := range overlay {
		if entry.absent {
			continue
		}
		row := cloneRow(entry.row)
		if err := s.applyStagedRewritesLocked(table, row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}
