package storage

import (
	"fmt"
	"strings"
	"time"
)

// ReferentialAction is one persisted FOREIGN KEY ON DELETE/ON UPDATE action.
// The catalog stores the SQL spelling so sqlite_master can reproduce the
// declaration; an omitted clause and NO ACTION behave identically.
type ReferentialAction string

const (
	ReferentialActionNoAction   ReferentialAction = "NO ACTION"
	ReferentialActionRestrict   ReferentialAction = "RESTRICT"
	ReferentialActionCascade    ReferentialAction = "CASCADE"
	ReferentialActionSetNull    ReferentialAction = "SET NULL"
	ReferentialActionSetDefault ReferentialAction = "SET DEFAULT"
)

// DeleteAction returns the action applied when the parent row is deleted.
func (fk ForeignKeyConstraint) DeleteAction() ReferentialAction {
	return parseReferentialAction(fk.OnDelete)
}

// UpdateAction returns the action applied when a referenced parent key changes.
func (fk ForeignKeyConstraint) UpdateAction() ReferentialAction {
	return parseReferentialAction(fk.OnUpdate)
}

// parseReferentialAction maps the persisted spelling to its action. Anything
// unrecognized is NO ACTION, the default the catalog uses when the clause was
// omitted, so a hand-edited catalog cannot silently disable enforcement.
func parseReferentialAction(text string) ReferentialAction {
	switch strings.ToUpper(strings.TrimSpace(text)) {
	case "CASCADE":
		return ReferentialActionCascade
	case "SET NULL":
		return ReferentialActionSetNull
	case "SET DEFAULT":
		return ReferentialActionSetDefault
	case "RESTRICT":
		return ReferentialActionRestrict
	default:
		return ReferentialActionNoAction
	}
}

// rewritesReferencingRows reports whether the action changes or removes the
// child rows instead of rejecting the parent change.
func (a ReferentialAction) rewritesReferencingRows() bool {
	switch a {
	case ReferentialActionCascade, ReferentialActionSetNull, ReferentialActionSetDefault:
		return true
	default:
		return false
	}
}

// HasCascadingReference reports whether table is the referenced parent of a
// constraint whose DELETE action (forDelete) or UPDATE action rewrites the
// referencing rows. Callers use it to choose the transactional write path, so a
// statement whose only actions are NO ACTION/RESTRICT keeps the optimized
// point-write path.
func (m *SchemaManager) HasCascadingReference(table string, forDelete bool) (bool, error) {
	refs, err := m.referencingForeignKeys(table)
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		action := ref.FK.UpdateAction()
		if forDelete {
			action = ref.FK.DeleteAction()
		}
		if action.rewritesReferencingRows() {
			return true, nil
		}
	}
	return false, nil
}

// beginCascadingPointWrite decides whether an autocommit point write must be
// staged so its cascades publish with the parent change, and starts that
// implicit transaction when it is needed.
//
// A table outside the foreign-key graph keeps the point-write fast path and
// never touches the gate. For a participating table the database-wide
// foreign-key write gate is taken before the cascade classification and held
// across Begin and the caller's staging. CREATE TABLE ... REFERENCES takes the
// same unconditional gate, so a constraint cannot be published between deciding
// that the write is not cascading and opening the transaction that would have
// applied its actions. The returned release function must be called before
// Commit, which acquires the same non-reentrant gate.
//
// A table gains its first constraint only together with a new child table,
// which is created empty under the gate and whose rows take the gate on insert.
// If the participant probe above goes stale, the non-cascading point path
// re-reads the catalog under the gate and rejects the write with 23503 instead
// of ignoring the new constraint.
//
// The gate is held while Begin and the caller's staging take the session lock.
// That order cannot invert with Commit: a Session serves one connection and one
// statement at a time, so Commit on this Session runs after the staging path
// releases the gate, while other sessions hold their own lock and contend only
// on the shared gate.
func (s *Session) beginCascadingPointWrite(table string, forDelete bool) (bool, func(), error) {
	participates, err := s.schema.ForeignKeyParticipants(table)
	if err != nil {
		return false, nil, err
	}
	if !participates {
		return false, func() {}, nil
	}
	unlockFK := s.table.LockForeignKeyWriteGate()
	cascades, err := s.schema.HasCascadingReference(table, forDelete)
	if err != nil {
		unlockFK()
		return false, nil, err
	}
	if !cascades {
		unlockFK()
		return false, func() {}, nil
	}
	if err := s.Begin(); err != nil {
		unlockFK()
		return false, nil, err
	}
	return true, unlockFK, nil
}

// foreignKeyRawValues returns the unformatted values a row holds for the given
// columns, and reports false when a column is missing. A NULL component is
// returned as nil: on a parent-side update the cascading action copies the new
// value, including a NULL that clears the reference.
func foreignKeyRawValues(row Row, columns []string) ([]interface{}, bool) {
	values := make([]interface{}, len(columns))
	for i, name := range columns {
		value, ok := rowColumnValue(row, name)
		if !ok {
			return nil, false
		}
		values[i] = value
	}
	return values, true
}

// foreignKeyActionKey formats a row's values for the cascade event key, keeping
// a NULL component distinguishable so a key that becomes NULL still changes the
// event identity.
func foreignKeyActionKey(row Row, columns []string) ([]string, bool) {
	values := make([]string, len(columns))
	for i, name := range columns {
		value, ok := rowColumnValue(row, name)
		if !ok {
			return nil, false
		}
		if value == nil {
			values[i] = "\x00NULL"
			continue
		}
		values[i] = formatIndexValue(value)
	}
	return values, true
}

// cascadeExpansion tracks the referential actions one statement already applied.
// It is the termination guard for self-references and constraint cycles: a row
// delete is applied at most once, and one (row, constraint, old key, new key)
// child update at most once. Because the applied event includes the key values
// involved, a cycle that revisits a row with a different key still terminates:
// the set of values a cascade can copy is bounded by the rows it starts from.
type cascadeExpansion struct {
	applied map[string]bool
	// now is the statement instant used to evaluate a SET DEFAULT expression
	// default, so every cascaded row of one statement sees the same value.
	now time.Time
}

func newCascadeExpansion(schema *SchemaManager) *cascadeExpansion {
	return &cascadeExpansion{applied: make(map[string]bool), now: schema.defaultNow()}
}

func (e *cascadeExpansion) markDelete(table, key string) bool {
	id := "D\x00" + strings.ToLower(table) + "\x00" + key
	if e.applied[id] {
		return false
	}
	e.applied[id] = true
	return true
}

func (e *cascadeExpansion) markUpdate(table, key string, fk ForeignKeyConstraint, oldKey, newKey []string) bool {
	id := "U\x00" + strings.ToLower(table) + "\x00" + key + "\x00" + strings.ToLower(fk.RefTable) + "\x00" + joinForeignKeyValues(fk.Columns) + "\x00" + joinForeignKeyValues(oldKey) + "\x00" + joinForeignKeyValues(newKey)
	if e.applied[id] {
		return false
	}
	e.applied[id] = true
	return true
}

// referencingRows returns the effective child rows (durable rows with the
// transaction overlay applied) that reference key through ref. The caller holds
// the session lock, and selectLocked registers the read so a concurrent write on
// the child side invalidates the commit instead of interleaving.
func (s *Session) referencingRows(ref referencingForeignKey, key []string) ([]Row, error) {
	return s.selectLocked(ref.Table, func(row Row) bool {
		return foreignKeyMatches(row, ref.FK.Columns, key)
	})
}

// stageCascadeDelete stages a row delete plus the ON DELETE actions of every
// constraint that names its table as the parent. The row is staged before the
// child scan, so a self-referential row is not visited as its own child.
func (s *Session) stageCascadeDelete(table, key string, row Row, exp *cascadeExpansion) error {
	if !exp.markDelete(table, key) {
		return nil
	}
	if entry, ok := s.overlay[strings.ToLower(table)][key]; ok && entry.absent {
		return nil
	}
	s.stageDelete(table, key, row)
	return s.cascadeAfterDelete(table, row, exp)
}

// cascadeAfterDelete applies the ON DELETE actions triggered by removing row
// from table.
func (s *Session) cascadeAfterDelete(table string, row Row, exp *cascadeExpansion) error {
	refs, err := s.referencingForeignKeysLocked(table)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		action := ref.FK.DeleteAction()
		if !action.rewritesReferencingRows() {
			continue
		}
		key, ok := foreignKeyValues(row, ref.FK.RefColumns)
		if !ok {
			continue
		}
		children, err := s.referencingRows(ref, key)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := s.applyDeleteAction(ref, child, action, exp); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDeleteAction performs one row's ON DELETE action. CASCADE removes the
// child and recurses through its own constraints; SET NULL/SET DEFAULT rewrite
// the child's foreign-key columns and then propagate as an update, because those
// columns can themselves be a key another table references.
func (s *Session) applyDeleteAction(ref referencingForeignKey, child Row, action ReferentialAction, exp *cascadeExpansion) error {
	schema, err := s.getSchemaLocked(ref.Table)
	if err != nil {
		return err
	}
	pk, err := schema.PrimaryKeyForRow(child)
	if err != nil {
		return err
	}
	key := s.table.dataKey(ref.Table, pk)
	if action == ReferentialActionCascade {
		return s.stageCascadeDelete(ref.Table, key, child, exp)
	}
	updated, err := s.applySetAction(schema, ref.FK, child, action, exp)
	if err != nil {
		return err
	}
	return s.stageCascadeUpdate(ref.Table, child, updated, exp)
}

// applySetAction returns the child row with its foreign-key columns replaced by
// NULL or by the column defaults. A column without a default receives NULL, the
// SQL default, which then fails NOT NULL if the column is required.
func (s *Session) applySetAction(schema *Schema, fk ForeignKeyConstraint, child Row, action ReferentialAction, exp *cascadeExpansion) (Row, error) {
	updated := cloneRow(child)
	for _, name := range fk.Columns {
		if action == ReferentialActionSetNull {
			updated[name] = nil
			continue
		}
		column, ok := schema.GetColumn(name)
		if !ok {
			return nil, fmt.Errorf("foreign key %s: column %s is missing from table %s", fk.Label(), name, schema.Name)
		}
		value, hasDefault, err := s.schema.columnDefaultValue(*column, exp.now)
		if err != nil {
			return nil, err
		}
		if !hasDefault {
			value = nil
		}
		updated[name] = value
	}
	return updated, nil
}

// cascadeAfterUpdate applies the ON UPDATE actions triggered by changing a
// parent row whose referenced key moved from oldRow to newRow.
func (s *Session) cascadeAfterUpdate(table string, oldRow, newRow Row, exp *cascadeExpansion) error {
	refs, err := s.referencingForeignKeysLocked(table)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		action := ref.FK.UpdateAction()
		if !action.rewritesReferencingRows() {
			continue
		}
		oldKey, ok := foreignKeyValues(oldRow, ref.FK.RefColumns)
		if !ok {
			continue
		}
		newKey, newOK := foreignKeyActionKey(newRow, ref.FK.RefColumns)
		if newOK && foreignKeyValuesEqual(oldKey, newKey) {
			continue
		}
		parentValues, ok := foreignKeyRawValues(newRow, ref.FK.RefColumns)
		if !ok {
			continue
		}
		children, err := s.referencingRows(ref, oldKey)
		if err != nil {
			return err
		}
		childSchema, err := s.getSchemaLocked(ref.Table)
		if err != nil {
			return err
		}
		for _, child := range children {
			childPK, err := childSchema.PrimaryKeyForRow(child)
			if err != nil {
				return err
			}
			childKey := s.table.dataKey(ref.Table, childPK)
			if !exp.markUpdate(ref.Table, childKey, ref.FK, oldKey, newKey) {
				continue
			}
			var updated Row
			switch action {
			case ReferentialActionCascade:
				updated = cloneRow(child)
				for i, column := range ref.FK.Columns {
					updated[column] = parentValues[i]
				}
			case ReferentialActionSetNull, ReferentialActionSetDefault:
				updated, err = s.applySetAction(childSchema, ref.FK, child, action, exp)
				if err != nil {
					return err
				}
			}
			if err := s.stageCascadeUpdate(ref.Table, child, updated, exp); err != nil {
				return err
			}
		}
	}
	return nil
}

// stageCascadeUpdate stages one cascaded row rewrite. The row keeps its key
// unless a cascaded foreign-key column is part of it, in which case the staged
// image moves and a collision with another row fails like any primary-key move.
// Required columns and CHECK predicates are validated immediately so a SET NULL
// on a NOT NULL column fails at the statement with 23502 instead of surfacing at
// commit; FK and UNIQUE validity are validated against the transaction's final
// state at commit.
func (s *Session) stageCascadeUpdate(table string, oldRow, newRow Row, exp *cascadeExpansion) error {
	schema, err := s.getSchemaLocked(table)
	if err != nil {
		return err
	}
	if err := s.schema.validateRowConstraints(schema, newRow); err != nil {
		return err
	}
	oldPK, err := schema.PrimaryKeyForRow(oldRow)
	if err != nil {
		return err
	}
	newPK, err := schema.PrimaryKeyForRow(newRow)
	if err != nil {
		return err
	}
	oldKey := s.table.dataKey(table, oldPK)
	newKey := s.table.dataKey(table, newPK)
	if oldKey != newKey {
		if _, gerr := s.getByPKLocked(table, newPK); gerr == nil {
			return &PrimaryKeyViolationError{Table: table, Key: newPK}
		} else if gerr != ErrKeyNotFound {
			return gerr
		}
		s.stageDelete(table, oldKey, oldRow)
	}
	s.stagePut(table, newKey, newRow)
	return s.cascadeAfterUpdate(table, oldRow, newRow, exp)
}

// rollbackToLocked discards mutations after a savepoint position. The caller
// holds the session lock.
func (s *Session) rollbackToLocked(pos int) {
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
}
