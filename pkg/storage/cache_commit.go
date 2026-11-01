package storage

import "strings"

type cachedRowChange struct {
	pk     string
	rowid  int64
	absent bool
	values map[string]string
}

type tableCachePlan struct {
	table      string
	rows       []cachedRowChange
	indexes    []string
	drop       []string
	countDelta int
}

// Prepare under an exclusive table gate, before durable publication. Shared-gate
// optimistic commits still invalidate: otherwise same-key commits could publish
// derived state in the opposite order from their durable CAS operations.
func (m *TableManager) prepareTableCache(table string, entries map[string]*overlayEntry, reads map[string]uint64) (*tableCachePlan, error) {
	indexes, err := m.schema.ListTableIndexes(table)
	if err != nil {
		return nil, err
	}
	plan := &tableCachePlan{table: strings.ToLower(table)}
	var warm []*Index
	m.cacheMu.RLock()
	for _, idx := range indexes {
		name := strings.ToLower(idx.Name)
		if _, ok := m.indexCache[name]; !ok {
			continue
		}
		if !cacheableColumnIndex(idx) {
			plan.drop = append(plan.drop, name)
			continue
		}
		warm = append(warm, idx)
		plan.indexes = append(plan.indexes, name)
	}
	m.cacheMu.RUnlock()
	for key, entry := range entries {
		if entry.absent {
			if reads[key] != 0 {
				plan.countDelta--
			}
		} else if reads[key] == 0 {
			plan.countDelta++
		}
		if len(warm) == 0 {
			continue
		}
		row := cachedRowChange{pk: strings.TrimPrefix(key, m.dataPrefix(table)), absent: entry.absent, values: make(map[string]string, len(warm))}
		if !entry.absent {
			row.rowid, _ = rowIDFromRow(entry.row)
			for _, idx := range warm {
				value, err := m.buildIndexValue(entry.row, idx.Columns)
				if err != nil {
					return nil, err
				}
				row.values[strings.ToLower(idx.Name)] = formatIndexValue(value)
			}
		}
		plan.rows = append(plan.rows, row)
	}
	return plan, nil
}

func (m *TableManager) applyTableCache(plan *tableCachePlan) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	for _, name := range plan.drop {
		delete(m.indexCache, name)
		delete(m.indexRowValues, name)
		delete(m.indexTable, name)
	}
	keys := m.rowKeyCache[plan.table]
	reverse := m.keyRowCache[plan.table]
	// Remove every previous owner before adding any replacement (PK moves, rowid
	// replacement and swaps must not depend on Go map iteration order).
	for _, row := range plan.rows {
		id, ok := reverse[row.pk]
		if !ok {
			continue
		}
		for _, name := range plan.indexes {
			if value, ok := m.indexRowValues[name][id]; ok {
				m.removeIndexEntryLocked(name, value, id)
			}
		}
		delete(keys, id)
		delete(reverse, row.pk)
	}
	for _, row := range plan.rows {
		if row.absent {
			continue
		}
		keys[row.rowid] = row.pk
		reverse[row.pk] = row.rowid
		for name, value := range row.values {
			m.indexCache[name][value] = append(m.indexCache[name][value], row.rowid)
			m.indexRowValues[name][row.rowid] = value
		}
	}
	if m.countsInit[plan.table] {
		m.counts[plan.table] += plan.countDelta
	}
}

// Call with cacheMu held when publishing a complete durable index snapshot.
func (m *TableManager) installIndexCacheLocked(name, table string, values map[string][]int64, keys map[int64]string) {
	m.indexCache[name] = values
	m.indexTable[name] = table
	m.rowKeyCache[table] = keys
	reverse := make(map[string]int64, len(keys))
	for id, key := range keys {
		reverse[key] = id
	}
	m.keyRowCache[table] = reverse
	indexed := make(map[int64]string, len(keys))
	for value, ids := range values {
		for _, id := range ids {
			indexed[id] = value
		}
	}
	m.indexRowValues[name] = indexed
}

func (m *TableManager) removeIndexEntryLocked(name, value string, id int64) {
	values := m.indexCache[name]
	if values == nil {
		return
	}
	ids := values[value]
	remaining := ids[:0]
	for _, candidate := range ids {
		if candidate != id {
			remaining = append(remaining, candidate)
		}
	}
	if len(remaining) == 0 {
		delete(values, value)
	} else {
		values[value] = remaining
	}
	if indexed, ok := m.indexRowValues[name][id]; ok && indexed == value {
		delete(m.indexRowValues[name], id)
	}
}
