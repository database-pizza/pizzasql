package storage

import "strings"

// UNIQUE caches use the constraint encoder, not the coarser SELECT candidate
// encoder. Owners are actual durable keys, preserving NULL and legacy-key rules.
// Validation and publication hold the table's exclusive gate. cacheMu protects
// cache membership and removals from shared-gate point deletes.
type uniqueIndexCache struct {
	table  string
	values map[string]map[string]bool
	keys   map[string]string
}

type uniqueCacheChange struct {
	name   string
	cache  *uniqueIndexCache
	values map[string]*string // nil removes a key or represents a NULL index value
}

type uniqueCachePlan []uniqueCacheChange

func (m *TableManager) prepareUniqueRows(table string, entries map[string]*overlayEntry) (uniqueCachePlan, error) {
	indexes, err := m.uniqueIndexes(table)
	if err != nil {
		return nil, err
	}
	if len(indexes) == 0 {
		return nil, nil
	}
	caches, err := m.ensureUniqueCaches(table, indexes)
	if err != nil {
		return nil, err
	}
	plan := make(uniqueCachePlan, 0, len(indexes))
	for _, idx := range indexes {
		cache := caches[idx.Name]
		change := uniqueCacheChange{name: strings.ToLower(idx.Name), cache: cache, values: make(map[string]*string, len(entries))}
		seen := make(map[string]bool)
		for key, entry := range entries {
			change.values[key] = nil
			if entry.absent {
				continue
			}
			value, isNull, err := m.encodeUniqueValue(entry.row, idx.Columns)
			if err != nil {
				return nil, err
			}
			if isNull {
				continue
			}
			if seen[value] {
				return nil, &UniqueViolationError{Constraint: idx.Name}
			}
			seen[value] = true
			for owner := range cache.values[value] {
				if _, replaced := entries[owner]; !replaced {
					return nil, &UniqueViolationError{Constraint: idx.Name}
				}
			}
			change.values[key] = &value
		}
		plan = append(plan, change)
	}
	return plan, nil
}

func (m *TableManager) ensureUniqueCaches(table string, indexes []*Index) (map[string]*uniqueIndexCache, error) {
	caches := make(map[string]*uniqueIndexCache, len(indexes))
	var missing []*Index
	m.cacheMu.RLock()
	for _, idx := range indexes {
		if cache := m.uniqueCache[strings.ToLower(idx.Name)]; cache != nil && cacheableColumnIndex(idx) {
			caches[idx.Name] = cache
		} else {
			missing = append(missing, idx)
		}
	}
	m.cacheMu.RUnlock()
	if len(missing) == 0 {
		return caches, nil
	}
	for _, idx := range missing {
		caches[idx.Name] = &uniqueIndexCache{table: strings.ToLower(table), values: make(map[string]map[string]bool), keys: make(map[string]string)}
	}
	err := m.pool.WithClient(func(client *KVClient) (retErr error) {
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
			for _, entry := range entries {
				row, err := decodeRow(entry.Value)
				if err != nil {
					return err
				}
				for _, idx := range missing {
					value, isNull, err := m.encodeUniqueValue(row, idx.Columns)
					if err != nil {
						return err
					}
					if !isNull {
						caches[idx.Name].add(string(entry.Key), value)
					}
				}
			}
			if done {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	m.cacheMu.Lock()
	for _, idx := range missing {
		if cacheableColumnIndex(idx) {
			m.uniqueCache[strings.ToLower(idx.Name)] = caches[idx.Name]
		}
	}
	m.cacheMu.Unlock()
	return caches, nil
}

// Expression evaluators have no determinism contract. Rebuild these constraints
// for each validation rather than freezing time/random/function-dependent values.
func cacheableColumnIndex(index *Index) bool {
	for _, column := range index.Columns {
		if column.Expression != "" {
			return false
		}
	}
	return true
}

func (c *uniqueIndexCache) remove(key string) {
	value, ok := c.keys[key]
	if !ok {
		return
	}
	delete(c.keys, key)
	delete(c.values[value], key)
	if len(c.values[value]) == 0 {
		delete(c.values, value)
	}
}

func (c *uniqueIndexCache) add(key, value string) {
	if c.values[value] == nil {
		c.values[value] = make(map[string]bool)
	}
	c.values[value][key] = true
	c.keys[key] = value
}

// Apply only after durable success. accepted is used by the storage bulk API,
// which can commit several batches; SQL statements use one atomic transaction.
func (m *TableManager) applyUniquePlan(plan uniqueCachePlan, accepted map[string]bool) {
	if len(plan) == 0 {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	for _, change := range plan {
		if m.uniqueCache[change.name] != change.cache {
			continue
		}
		for key := range change.values {
			if accepted == nil || accepted[key] {
				change.cache.remove(key)
			}
		}
		for key, value := range change.values {
			if value != nil && (accepted == nil || accepted[key]) {
				change.cache.add(key, *value)
			}
		}
	}
}

// A transport failure may hide a durable write. Drop derived state and make
// earlier predicate/scan snapshots retry rather than trusting an unknown outcome.
// Callers retain their normal table publication gate.
func (m *TableManager) invalidateWriteFailure(table string) {
	m.InvalidateCache(table)
	m.bumpIndexPredicateWildcard(table)
	m.bumpGeneration(table)
}

func (m *TableManager) removeUniqueKey(table, key string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	for _, cache := range m.uniqueCache {
		if cache.table == strings.ToLower(table) {
			cache.remove(key)
		}
	}
}
