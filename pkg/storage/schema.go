package storage

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
)

// ErrIndexNotFound is returned by GetIndex when an index is absent. It is
// distinct from a storage/IO error so callers like ListTableIndexes can skip a
// concurrently-dropped index without swallowing real read failures.
var ErrIndexNotFound = errors.New("index not found")

// Schema represents a table schema.
type Schema struct {
	Name          string    `json:"name"`
	Columns       []Column  `json:"columns"`
	PrimaryKey    string    `json:"primary_key"`
	CreatedAt     time.Time `json:"created_at"`
	NextRowID     int64     `json:"next_rowid"`
	AutoIncrement bool      `json:"autoincrement"`
}

// Column represents a column definition.
type Column struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Nullable   bool        `json:"nullable"`
	Default    interface{} `json:"default,omitempty"`
	PrimaryKey bool        `json:"primary_key"`
	// GeneratedExpr holds the SQL text of a GENERATED ALWAYS AS (expr) column.
	// GeneratedStored reports whether the value is materialized on write
	// (STORED, the only form this engine persists) versus computed on read.
	GeneratedExpr   string `json:"generated_expr,omitempty"`
	GeneratedStored bool   `json:"generated_stored,omitempty"`
}

// Index represents an index definition.
type Index struct {
	Name      string        `json:"name"`
	Table     string        `json:"table"`
	Columns   []IndexColumn `json:"columns"`
	Unique    bool          `json:"unique"`
	CreatedAt time.Time     `json:"created_at"`
	// OnConflict is the default conflict resolution declared for this index via
	// a UNIQUE(...) ON CONFLICT clause. Empty means the SQLite default (ABORT).
	OnConflict string `json:"on_conflict,omitempty"`
}

// IndexColumn represents a column in an index. Expression is set for expression
// indexes (e.g. lower(email)); a plain column index leaves it empty and uses
// Name.
type IndexColumn struct {
	Name       string `json:"name"`
	Desc       bool   `json:"desc"`
	Expression string `json:"expression,omitempty"`
}

// rowIDAllocator owns the next-ROWID state for a single table. It is a separate
// mutex per table so allocating a ROWID on one table never serializes against
// another table, and never contends with the SchemaManager catalog lock.
type rowIDAllocator struct {
	mu   sync.Mutex
	next int64
	init bool
}

// SchemaManager manages table schemas.
type SchemaManager struct {
	pool            *KVPool
	database        string
	cache           map[string]*Schema
	indexCache      map[string]*Index
	indexListCache  []string
	indexListCached bool
	version         uint64
	mu              sync.RWMutex
	tableLocksMu    sync.Mutex
	tableLocks      map[string]*sync.RWMutex

	rowIDMu    sync.Mutex
	rowIDAlloc map[string]*rowIDAllocator
}

// BeginTransaction is retained for API compatibility. Buffered per-session
// transactions no longer take a database-wide transaction lock; staged writes
// are validated and committed atomically with CompareBatchWrite instead.
func (m *SchemaManager) BeginTransaction() {}

// EndTransaction is retained for API compatibility.
func (m *SchemaManager) EndTransaction() {}

// LockStatement is retained for API compatibility. Statement execution is now
// serialized through per-table locks and optimistic validation, so no global
// statement lock is required.
func (m *SchemaManager) LockStatement() {}

// UnlockStatement is retained for API compatibility.
func (m *SchemaManager) UnlockStatement() {}

// NewSchemaManager creates a new schema manager.
func NewSchemaManager(pool *KVPool, database string) *SchemaManager {
	return &SchemaManager{
		pool:       pool,
		database:   database,
		cache:      make(map[string]*Schema),
		indexCache: make(map[string]*Index),
		tableLocks: make(map[string]*sync.RWMutex),
		rowIDAlloc: make(map[string]*rowIDAllocator),
	}
}

// rowIDAllocatorFor returns (creating if needed) the per-table ROWID allocator.
func (m *SchemaManager) rowIDAllocatorFor(table string) *rowIDAllocator {
	key := strings.ToLower(table)
	m.rowIDMu.Lock()
	a, ok := m.rowIDAlloc[key]
	if !ok {
		a = &rowIDAllocator{}
		m.rowIDAlloc[key] = a
	}
	m.rowIDMu.Unlock()
	return a
}

func (m *SchemaManager) tableLock(table string) *sync.RWMutex {
	key := strings.ToLower(table)
	m.tableLocksMu.Lock()
	lock, ok := m.tableLocks[key]
	if !ok {
		lock = &sync.RWMutex{}
		m.tableLocks[key] = lock
	}
	m.tableLocksMu.Unlock()
	return lock
}

// GetDatabaseName returns the database name.
func (m *SchemaManager) GetDatabaseName() string {
	return m.database
}

// GetPool returns the KV pool.
func (m *SchemaManager) GetPool() *KVPool {
	return m.pool
}

// Version returns the in-process schema catalog version. It is incremented for
// schema/index definition changes so cached executors can resync their analyzer
// catalogs without scanning storage on every query.
func (m *SchemaManager) Version() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.version
}

func (m *SchemaManager) bumpVersionLocked() {
	m.version++
}

// schemaKey returns the key for a table schema.
func (m *SchemaManager) schemaKey(table string) string {
	return fmt.Sprintf("%s:_schema:%s", m.database, strings.ToLower(table))
}

// catalogKey returns the key for the table catalog.
func (m *SchemaManager) catalogKey() string {
	return fmt.Sprintf("%s:_sys:tables", m.database)
}

// rowIDKey returns the key for a table's next ROWID counter.
func (m *SchemaManager) rowIDKey(table string) string {
	return fmt.Sprintf("%s:_sys:rowid:%s", m.database, strings.ToLower(table))
}

// CreateTable creates a new table.
func (m *SchemaManager) CreateTable(schema *Schema) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if table already exists
	key := m.schemaKey(schema.Name)
	err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.Read(key)
		return err
	})
	if err == nil {
		return fmt.Errorf("table already exists: %s", schema.Name)
	}

	// Keep the cached schema private so callers cannot mutate a published
	// catalog snapshot after this operation returns.
	schema = cloneSchema(schema)
	schema.CreatedAt = time.Now()

	// Determine primary key if not set
	if schema.PrimaryKey == "" {
		for _, col := range schema.Columns {
			if col.PrimaryKey {
				schema.PrimaryKey = col.Name
				break
			}
		}
		// No explicit primary key declared — use synthetic _rowid_ so user
		// columns remain unconstrained and can hold duplicate or NULL values.
		if schema.PrimaryKey == "" {
			schema.PrimaryKey = "_rowid_"
		}
	}

	// Serialize schema
	data, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("failed to serialize schema: %w", err)
	}

	// Write schema
	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(data))
	})
	if err != nil {
		return fmt.Errorf("failed to write schema: %w", err)
	}

	// Update catalog
	if err := m.addToCatalog(schema.Name); err != nil {
		// Rollback schema write
		m.pool.WithClient(func(c *KVClient) error {
			return c.Delete(key)
		})
		return err
	}

	// Update cache
	m.cache[strings.ToLower(schema.Name)] = schema
	m.bumpVersionLocked()

	return nil
}

// DropTable drops a table.
func (m *SchemaManager) DropTable(name string) error {
	tableLock := m.tableLock(name)
	tableLock.Lock()
	defer tableLock.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.schemaKey(name)

	// Check if table exists
	err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.Read(key)
		return err
	})
	if err != nil {
		return fmt.Errorf("table not found: %s", name)
	}

	// Delete all rows by scanning their actual keys and batch-deleting them, so
	// a table drop no longer leaks durable rows.
	if err := m.deleteKeysWithPrefix([]byte(fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(name)))); err != nil {
		return err
	}

	// Delete schema
	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Delete(key)
	})
	if err != nil {
		return fmt.Errorf("failed to delete schema: %w", err)
	}

	// Delete ROWID state.
	m.pool.WithClient(func(c *KVClient) error {
		return c.Delete(m.rowIDKey(name))
	})

	// Update catalog
	if err := m.removeFromCatalog(name); err != nil {
		return err
	}

	// Update cache
	tableLower := strings.ToLower(name)
	delete(m.cache, tableLower)
	m.rowIDMu.Lock()
	delete(m.rowIDAlloc, tableLower)
	m.rowIDMu.Unlock()
	m.bumpVersionLocked()

	return nil
}

// GetSchema retrieves a table schema.
func (m *SchemaManager) GetSchema(name string) (*Schema, error) {
	m.mu.RLock()
	if schema, ok := m.cache[strings.ToLower(name)]; ok {
		// Clone while still holding the read lock: the cached schema's
		// NextRowID field is mutated under the write lock, so cloning outside
		// the lock races with that mutation.
		cloned := cloneSchema(schema)
		m.mu.RUnlock()
		return cloned, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if schema, ok := m.cache[strings.ToLower(name)]; ok {
		return cloneSchema(schema), nil
	}

	key := m.schemaKey(name)
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, fmt.Errorf("table not found: %s", name)
		}
		return nil, err
	}

	var schema Schema
	if err := json.Unmarshal([]byte(data), &schema); err != nil {
		return nil, fmt.Errorf("failed to parse schema: %w", err)
	}

	m.cache[strings.ToLower(name)] = &schema
	return cloneSchema(&schema), nil
}

func cloneSchema(schema *Schema) *Schema {
	if schema == nil {
		return nil
	}
	cloned := *schema
	cloned.Columns = append([]Column(nil), schema.Columns...)
	return &cloned
}

func cloneIndex(index *Index) *Index {
	if index == nil {
		return nil
	}
	cloned := *index
	cloned.Columns = append([]IndexColumn(nil), index.Columns...)
	return &cloned
}

// TableExists checks if a table exists.
func (m *SchemaManager) TableExists(name string) bool {
	_, err := m.GetSchema(name)
	return err == nil
}

// ListTables returns all table names.
func (m *SchemaManager) ListTables() ([]string, error) {
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(m.catalogKey())
		return err
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, nil
		}
		return nil, err
	}

	var tables []string
	if err := json.Unmarshal([]byte(data), &tables); err != nil {
		return nil, fmt.Errorf("failed to parse catalog: %w", err)
	}

	return tables, nil
}

// addToCatalog adds a table to the catalog.
func (m *SchemaManager) addToCatalog(name string) error {
	tables, err := m.ListTables()
	if err != nil && err != ErrKeyNotFound {
		return err
	}

	// Check if already exists
	lowerName := strings.ToLower(name)
	for _, t := range tables {
		if strings.ToLower(t) == lowerName {
			return nil
		}
	}

	tables = append(tables, name)
	data, err := json.Marshal(tables)
	if err != nil {
		return err
	}

	return m.pool.WithClient(func(c *KVClient) error {
		return c.Write(m.catalogKey(), string(data))
	})
}

// removeFromCatalog removes a table from the catalog.
func (m *SchemaManager) removeFromCatalog(name string) error {
	tables, err := m.ListTables()
	if err != nil {
		return err
	}

	lowerName := strings.ToLower(name)
	newTables := make([]string, 0, len(tables))
	for _, t := range tables {
		if strings.ToLower(t) != lowerName {
			newTables = append(newTables, t)
		}
	}

	data, err := json.Marshal(newTables)
	if err != nil {
		return err
	}

	return m.pool.WithClient(func(c *KVClient) error {
		return c.Write(m.catalogKey(), string(data))
	})
}

// InvalidateCache clears the cache for a table.
func (m *SchemaManager) InvalidateCache(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tableLower := strings.ToLower(name)
	delete(m.cache, tableLower)
	m.rowIDMu.Lock()
	delete(m.rowIDAlloc, tableLower)
	m.rowIDMu.Unlock()
}

// ToAnalyzerTableInfo converts a Schema to analyzer.TableInfo.
func (s *Schema) ToAnalyzerTableInfo() *analyzer.TableInfo {
	info := &analyzer.TableInfo{
		Name: s.Name,
	}

	for _, col := range s.Columns {
		info.Columns = append(info.Columns, analyzer.ColumnInfo{
			Name:       col.Name,
			Type:       analyzer.TypeFromName(col.Type),
			Nullable:   col.Nullable,
			PrimaryKey: col.PrimaryKey,
			TableName:  s.Name,
			Generated:  col.GeneratedExpr != "",
		})
	}

	return info
}

// GetColumn returns a column by name.
func (s *Schema) GetColumn(name string) (*Column, bool) {
	lowerName := strings.ToLower(name)
	for i := range s.Columns {
		if strings.ToLower(s.Columns[i].Name) == lowerName {
			return &s.Columns[i], true
		}
	}
	return nil, false
}

// GetNextRowID gets and increments the next ROWID for a table. Allocation is
// serialized per table via the table's own allocator so inserts on different
// tables never contend, and no global SchemaManager lock is held across the
// durable derivation scan.
func (m *SchemaManager) GetNextRowID(table string) (int64, error) {
	alloc := m.rowIDAllocatorFor(table)
	alloc.mu.Lock()
	defer alloc.mu.Unlock()
	next, err := m.nextRowIDLocked(alloc, table)
	if err != nil {
		return 0, err
	}
	alloc.next = next + 1
	return next, nil
}

// UpdateMaxRowID updates the next ROWID if the provided value is higher.
func (m *SchemaManager) UpdateMaxRowID(table string, rowid int64) error {
	alloc := m.rowIDAllocatorFor(table)
	alloc.mu.Lock()
	defer alloc.mu.Unlock()
	next, err := m.nextRowIDLocked(alloc, table)
	if err != nil {
		return err
	}
	if rowid >= next {
		alloc.next = rowid + 1
	}
	return nil
}

// nextRowIDLocked returns the current next ROWID, deriving it from durable rows
// on first use (must hold the per-table allocator lock).
func (m *SchemaManager) nextRowIDLocked(alloc *rowIDAllocator, table string) (int64, error) {
	if alloc.init {
		return alloc.next, nil
	}
	next, err := m.deriveNextRowID(table)
	if err != nil {
		return 0, err
	}
	if next < 1 {
		next = 1
	}
	alloc.next = next
	alloc.init = true
	return alloc.next, nil
}

// deriveNextRowID scans durable row keys to recover max(rowid)+1, streaming
// each page through decodeRow instead of materializing every value.
func (m *SchemaManager) deriveNextRowID(table string) (int64, error) {
	prefix := []byte(fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(table)))
	var maxRowID int64
	err := m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.Scan(prefix)
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
					return fmt.Errorf("failed to parse row while deriving ROWID: %w", err)
				}
				if rowid, ok := valueAsInt64(row["_rowid_"]); ok && rowid > maxRowID {
					maxRowID = rowid
				}
			}
			if done {
				return nil
			}
		}
	})
	if err != nil {
		return 0, err
	}
	return maxRowID + 1, nil
}

// deleteKeysWithPrefix streams the keys with the given prefix one page at a
// time and atomically batch-deletes them, so a bulk operation never leaves
// durable rows behind.
func (m *SchemaManager) deleteKeysWithPrefix(prefix []byte) error {
	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanKeys(prefix)
		if err != nil {
			return err
		}
		defer func() {
			if err := cursor.Close(); retErr == nil {
				retErr = err
			}
		}()

		ops := make([]BatchOp, 0, scanPageSize)
		batchBytes := 8
		flush := func() error {
			if len(ops) == 0 {
				return nil
			}
			if _, err := client.BatchWrite(ops, nil); err != nil {
				return err
			}
			ops = ops[:0]
			batchBytes = 8
			return nil
		}

		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, e := range entries {
				opBytes := 12 + len(e.Key)
				if len(ops) == maxOperations || batchBytes+opBytes > bulkBatchByteBudget {
					if err := flush(); err != nil {
						return err
					}
				}
				ops = append(ops, BatchOp{Op: batchDelete, Key: append([]byte(nil), e.Key...)})
				batchBytes += opBytes
			}
			if done {
				return flush()
			}
		}
	})
}

func valueAsInt64(value interface{}) (int64, bool) {
	switch v := value.(type) {
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

// getSchemaLocked retrieves schema (must hold lock).
func (m *SchemaManager) getSchemaLocked(name string) (*Schema, error) {
	if schema, ok := m.cache[strings.ToLower(name)]; ok {
		return schema, nil
	}

	key := m.schemaKey(name)
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, fmt.Errorf("table not found: %s", name)
		}
		return nil, err
	}

	var schema Schema
	if err := json.Unmarshal([]byte(data), &schema); err != nil {
		return nil, fmt.Errorf("failed to parse schema: %w", err)
	}

	m.cache[strings.ToLower(name)] = &schema
	return &schema, nil
}

// saveSchemaLocked saves schema (must hold lock).
func (m *SchemaManager) saveSchemaLocked(schema *Schema) error {
	data, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("failed to serialize schema: %w", err)
	}

	key := m.schemaKey(schema.Name)
	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(data))
	})
	if err != nil {
		return fmt.Errorf("failed to write schema: %w", err)
	}

	m.cache[strings.ToLower(schema.Name)] = schema
	return nil
}

// Index management methods

// indexKey returns the key for an index.
func (m *SchemaManager) indexKey(name string) string {
	return fmt.Sprintf("%s:index:%s", m.database, strings.ToLower(name))
}

// indexListKey returns the key for the index list.
func (m *SchemaManager) indexListKey() string {
	return fmt.Sprintf("%s:indexes", m.database)
}

// CreateIndex creates a new index.
func (m *SchemaManager) CreateIndex(index *Index) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if index already exists
	key := m.indexKey(index.Name)
	err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.Read(key)
		return err
	})
	if err == nil {
		return fmt.Errorf("index already exists: %s", index.Name)
	}

	// Verify table exists
	if _, err := m.getSchemaLocked(index.Table); err != nil {
		return fmt.Errorf("table not found: %s", index.Table)
	}

	// Save index
	index.CreatedAt = time.Now()
	data, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("failed to serialize index: %w", err)
	}

	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(data))
	})
	if err != nil {
		return fmt.Errorf("failed to write index: %w", err)
	}

	// Add to index list
	if err := m.addToIndexList(index.Name); err != nil {
		return err
	}
	m.indexCache[strings.ToLower(index.Name)] = cloneIndex(index)
	m.bumpVersionLocked()
	return nil
}

// DropIndex drops an index.
func (m *SchemaManager) DropIndex(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := m.indexKey(name)
	err := m.pool.WithClient(func(c *KVClient) error {
		return c.Delete(key)
	})
	if err != nil {
		return fmt.Errorf("failed to delete index: %w", err)
	}

	if err := m.removeFromIndexList(name); err != nil {
		return err
	}
	delete(m.indexCache, strings.ToLower(name))
	m.bumpVersionLocked()
	return nil
}

// IndexExists checks if an index exists.
func (m *SchemaManager) IndexExists(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.indexExistsUnsafe(name)
}

// indexExistsUnsafe reports whether an index definition is durable (m.mu must be
// held).
func (m *SchemaManager) indexExistsUnsafe(name string) bool {
	if _, ok := m.indexCache[strings.ToLower(name)]; ok {
		return true
	}

	key := m.indexKey(name)
	err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.Read(key)
		return err
	})
	return err == nil
}

// GetIndex retrieves an index by name.
func (m *SchemaManager) GetIndex(name string) (*Index, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cacheKey := strings.ToLower(name)
	if index, ok := m.indexCache[cacheKey]; ok {
		return cloneIndex(index), nil
	}

	key := m.indexKey(name)
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err != nil {
		if err == ErrKeyNotFound {
			return nil, fmt.Errorf("%w: %s", ErrIndexNotFound, name)
		}
		return nil, err
	}

	var index Index
	if err := json.Unmarshal([]byte(data), &index); err != nil {
		return nil, fmt.Errorf("failed to parse index: %w", err)
	}

	m.indexCache[cacheKey] = &index
	return cloneIndex(&index), nil
}

// ListIndexes returns all index names.
func (m *SchemaManager) ListIndexes() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.indexListUnsafe()
}

// indexListUnsafe returns all index names, loading them from storage on first
// use (m.mu must be held).
func (m *SchemaManager) indexListUnsafe() ([]string, error) {
	if m.indexListCached {
		return append([]string(nil), m.indexListCache...), nil
	}

	key := m.indexListKey()
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err == ErrKeyNotFound {
		m.indexListCache = nil
		m.indexListCached = true
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}

	var indexes []string
	if err := json.Unmarshal([]byte(data), &indexes); err != nil {
		return nil, fmt.Errorf("failed to parse index list: %w", err)
	}

	m.indexListCache = append([]string(nil), indexes...)
	m.indexListCached = true
	return indexes, nil
}

// ListTableIndexes returns all indexes for a table.
func (m *SchemaManager) ListTableIndexes(table string) ([]*Index, error) {
	indexes, err := m.ListIndexes()
	if err != nil {
		return nil, err
	}

	var result []*Index
	for _, name := range indexes {
		idx, err := m.GetIndex(name)
		if err != nil {
			// An index dropped concurrently is simply absent; any other error
			// (storage/IO or corrupt data) must not be silently swallowed.
			if errors.Is(err, ErrIndexNotFound) {
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

// addToIndexList adds an index name to the list.
func (m *SchemaManager) addToIndexList(name string) error {
	key := m.indexListKey()
	var indexes []string

	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err == nil {
		json.Unmarshal([]byte(data), &indexes)
	}

	indexes = append(indexes, name)
	newData, _ := json.Marshal(indexes)

	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(newData))
	})
	if err == nil {
		m.indexListCache = append([]string(nil), indexes...)
		m.indexListCached = true
	}
	return err
}

// removeFromIndexList removes an index name from the list.
func (m *SchemaManager) removeFromIndexList(name string) error {
	key := m.indexListKey()
	var indexes []string

	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err != nil {
		return nil
	}
	json.Unmarshal([]byte(data), &indexes)

	var newIndexes []string
	for _, idx := range indexes {
		if !strings.EqualFold(idx, name) {
			newIndexes = append(newIndexes, idx)
		}
	}

	newData, _ := json.Marshal(newIndexes)
	err = m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(newData))
	})
	if err == nil {
		m.indexListCache = append([]string(nil), newIndexes...)
		m.indexListCached = true
	}
	return err
}

// AddColumn adds a new column to a table. When unique is non-nil the column's
// UNIQUE index is registered as part of the same statement.
//
// A constant DEFAULT is materialized into every existing row before the schema
// is published. NOT NULL, UNIQUE, and index validation all read persisted row
// values, so a default that only existed in the catalog would be invisible to
// them: a UNIQUE index over the new column could not see the duplicate values
// every existing row shares, and an UPDATE to NULL would look non-null. The
// rewrite is the same shape as DropColumn: the table's exclusive gate keeps
// point writes and CAS commits out while rows are scanned, validated, and
// rewritten, the transform is idempotent, and the schema is saved last so an
// interrupted operation resumes against the old schema.
//
// The gate also makes the validation authoritative: NOT NULL without a default
// and duplicate UNIQUE values are rejected before any row is touched, and the
// column and its index are published together in one batch write, so a crash
// cannot leave the column visible without the constraint that validates it.
// A nullable column without a default needs no rewrite because a missing row
// field already reads as NULL. Like DROP COLUMN, the statement holds the catalog
// lock for the whole scan, so the cost is one table scan (two for UNIQUE) and a
// table-wide stall per ADD COLUMN that has a default or an index.
func (m *SchemaManager) AddColumn(table string, column Column, unique *Index) error {
	tableLock := m.tableLock(table)
	tableLock.Lock()
	defer tableLock.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	schema, err := m.getSchemaUnsafe(table)
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
		if m.indexExistsUnsafe(unique.Name) {
			return fmt.Errorf("index already exists: %s", unique.Name)
		}
		if err := m.validateAddColumnUnique(table, column, unique); err != nil {
			return err
		}
	}

	// A required column with nothing to write into rows that already exist
	// cannot be declared on a populated table.
	if !column.Nullable && column.Default == nil {
		hasRows, err := m.tableHasRows(table)
		if err != nil {
			return err
		}
		if hasRows {
			return &NotNullViolationError{Table: schema.Name, Column: column.Name}
		}
	}

	if column.Default != nil {
		if err := m.rewriteRows(table, func(row Row) bool {
			if _, ok := rowColumnValue(row, column.Name); ok {
				return false
			}
			row[column.Name] = column.Default
			return true
		}); err != nil {
			return err
		}
	}

	return m.publishAddedColumn(schema, column, unique)
}

// publishAddedColumn publishes a column and, when present, its unique index in
// one atomic batch write (m.mu must be held): the schema snapshot, the index
// definition, and the index list always become durable together or not at all.
func (m *SchemaManager) publishAddedColumn(schema *Schema, column Column, unique *Index) error {
	// Publish a fresh snapshot instead of mutating readers' shared pointer.
	schema = cloneSchema(schema)
	schema.Columns = append(schema.Columns, column)
	schemaData, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("failed to serialize schema: %w", err)
	}
	ops := []BatchOp{{Op: batchPut, Key: []byte(m.schemaKey(schema.Name)), Value: schemaData}}

	var indexList []string
	if unique != nil {
		unique.CreatedAt = time.Now()
		indexData, err := json.Marshal(unique)
		if err != nil {
			return fmt.Errorf("failed to serialize index: %w", err)
		}
		indexList, err = m.indexListUnsafe()
		if err != nil {
			return err
		}
		indexList = append(indexList, unique.Name)
		listData, err := json.Marshal(indexList)
		if err != nil {
			return fmt.Errorf("failed to serialize index list: %w", err)
		}
		ops = append(ops,
			BatchOp{Op: batchPut, Key: []byte(m.indexKey(unique.Name)), Value: indexData},
			BatchOp{Op: batchPut, Key: []byte(m.indexListKey()), Value: listData},
		)
	}

	if err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.BatchWrite(ops, nil)
		return err
	}); err != nil {
		return fmt.Errorf("failed to publish column %s: %w", column.Name, err)
	}

	// Publish in-memory state only after the durable write.
	m.cache[strings.ToLower(schema.Name)] = schema
	if unique != nil {
		m.indexCache[strings.ToLower(unique.Name)] = cloneIndex(unique)
		m.indexListCache = append([]string(nil), indexList...)
		m.indexListCached = true
	}
	m.bumpVersionLocked()
	return nil
}

func checkColumnAbsent(schema *Schema, name string) error {
	for _, col := range schema.Columns {
		if strings.EqualFold(col.Name, name) {
			return fmt.Errorf("column already exists: %s", name)
		}
	}
	return nil
}

// DropColumn removes a column from a table.
func (m *SchemaManager) DropColumn(table, columnName string) error {
	tableLock := m.tableLock(table)
	tableLock.Lock()
	defer tableLock.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	schema, err := m.getSchemaUnsafe(table)
	if err != nil {
		return err
	}

	// Cannot drop primary key column
	if strings.EqualFold(schema.PrimaryKey, columnName) {
		return fmt.Errorf("cannot drop primary key column: %s", columnName)
	}

	// Find and remove column
	newColumns := make([]Column, 0, len(schema.Columns)-1)
	found := false
	for _, col := range schema.Columns {
		if strings.EqualFold(col.Name, columnName) {
			found = true
			continue
		}
		newColumns = append(newColumns, col)
	}

	if !found {
		return fmt.Errorf("column not found: %s", columnName)
	}

	schema = cloneSchema(schema)
	schema.Columns = newColumns
	if err := m.rewriteRows(table, func(row Row) bool {
		for key := range row {
			if strings.EqualFold(key, columnName) {
				delete(row, key)
				return true
			}
		}
		return false
	}); err != nil {
		return err
	}

	// Update schema
	return m.updateSchemaUnsafe(schema)
}

// RenameTable renames a table.
func (m *SchemaManager) RenameTable(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if old table exists
	schema, err := m.getSchemaUnsafe(oldName)
	if err != nil {
		return err
	}

	// Check if new table name already exists
	_, err = m.getSchemaUnsafe(newName)
	if err == nil {
		return fmt.Errorf("table already exists: %s", newName)
	}

	schema = cloneSchema(schema)
	// Update schema name
	schema.Name = newName

	// Move durable rows from the old table's data prefix to the new one. Without
	// this, ALTER TABLE ... RENAME leaves every row under the old key and the
	// renamed table appears empty.
	if err := m.renameDataKeys(oldName, newName); err != nil {
		return err
	}

	// Repoint indexes that belonged to the old table.
	if err := m.repointIndexesLocked(oldName, newName); err != nil {
		return err
	}

	// Build the replacement catalog before switching schema names. The schema,
	// catalog, and stale ROWID state change in one batch, so a crash cannot
	// leave neither table name addressable after all row chunks have moved.
	tables, err := m.ListTables()
	if err != nil {
		return err
	}
	foundOld := false
	for i, name := range tables {
		if strings.EqualFold(name, oldName) {
			tables[i] = newName
			foundOld = true
			break
		}
	}
	if !foundOld {
		return fmt.Errorf("table not found in catalog: %s", oldName)
	}
	catalogData, err := json.Marshal(tables)
	if err != nil {
		return err
	}
	schemaData, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	oldKey := m.schemaKey(oldName)
	newKey := m.schemaKey(newName)
	err = m.pool.WithClient(func(c *KVClient) error {
		_, err := c.BatchWrite([]BatchOp{
			{Op: batchPut, Key: []byte(newKey), Value: schemaData},
			{Op: batchDelete, Key: []byte(oldKey)},
			{Op: batchDelete, Key: []byte(m.rowIDKey(oldName))},
			{Op: batchPut, Key: []byte(m.catalogKey()), Value: catalogData},
		}, nil)
		return err
	})
	if err != nil {
		return err
	}

	// Update cache
	oldLower := strings.ToLower(oldName)
	newLower := strings.ToLower(newName)
	delete(m.cache, oldLower)
	m.rowIDMu.Lock()
	delete(m.rowIDAlloc, oldLower)
	m.rowIDMu.Unlock()

	// Update cache
	m.cache[newLower] = schema
	m.bumpVersionLocked()

	return nil
}

// renameDataKeys moves every durable row of a table from the old name's data
// prefix to the new name's prefix in a single atomic batch per page.
func (m *SchemaManager) renameDataKeys(oldName, newName string) error {
	oldPrefix := []byte(fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(oldName)))
	newPrefix := fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(newName))
	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.Scan(oldPrefix)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := cursor.Close(); retErr == nil {
				retErr = cerr
			}
		}()

		ops := make([]BatchOp, 0, scanPageSize*2)
		batchBytes := 8
		flush := func() error {
			if len(ops) == 0 {
				return nil
			}
			if _, err := client.BatchWrite(ops, nil); err != nil {
				return err
			}
			ops = ops[:0]
			batchBytes = 8
			return nil
		}
		for {
			entries, done, err := cursor.Next()
			if err != nil {
				return err
			}
			for _, e := range entries {
				suffix := string(e.Key[len(oldPrefix):])
				newKey := []byte(newPrefix + suffix)
				opBytes := 24 + len(newKey) + len(e.Value) + len(e.Key)
				if len(ops)+2 > maxOperations || batchBytes+opBytes > bulkBatchByteBudget {
					if err := flush(); err != nil {
						return err
					}
				}
				ops = append(ops,
					BatchOp{Op: batchPut, Key: newKey, Value: e.Value},
					BatchOp{Op: batchDelete, Key: append([]byte(nil), e.Key...)},
				)
				batchBytes += opBytes
			}
			if done {
				return flush()
			}
		}
	})
}

// updateIndexesOfTableLocked applies mutate to the durable definition of every
// index on table and persists the definitions it changed. mutate reports
// whether it modified the definition; unmodified indexes are left untouched.
// The changed definitions are written in one batch, so a table's indexes never
// split between the old and new shape. The caller holds m.mu; it reads the
// durable index list directly rather than calling the self-locking ListIndexes
// helper.
func (m *SchemaManager) updateIndexesOfTableLocked(table string, mutate func(*Index) bool) error {
	var names []string
	if m.indexListCached {
		names = append([]string(nil), m.indexListCache...)
	} else {
		var data string
		err := m.pool.WithClient(func(c *KVClient) error {
			var rerr error
			data, rerr = c.Read(m.indexListKey())
			return rerr
		})
		if err == ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(data), &names); err != nil {
			return err
		}
	}

	type indexUpdate struct {
		index Index
		key   string
		value []byte
	}
	var updates []indexUpdate
	for _, name := range names {
		key := m.indexKey(name)
		var data string
		err := m.pool.WithClient(func(c *KVClient) error {
			var rerr error
			data, rerr = c.Read(key)
			return rerr
		})
		if err != nil {
			if err == ErrKeyNotFound {
				continue
			}
			return err
		}
		var idx Index
		if err := json.Unmarshal([]byte(data), &idx); err != nil {
			return err
		}
		if !strings.EqualFold(idx.Table, table) {
			continue
		}
		if !mutate(&idx) {
			continue
		}
		updated, err := json.Marshal(&idx)
		if err != nil {
			return err
		}
		updates = append(updates, indexUpdate{index: idx, key: key, value: updated})
	}
	if len(updates) == 0 {
		return nil
	}

	ops := make([]BatchOp, 0, len(updates))
	for _, update := range updates {
		ops = append(ops, BatchOp{Op: batchPut, Key: []byte(update.key), Value: update.value})
	}
	if err := m.pool.WithClient(func(c *KVClient) error {
		_, err := c.BatchWrite(ops, nil)
		return err
	}); err != nil {
		return fmt.Errorf("update index definitions for table %s: %w", table, err)
	}

	for _, update := range updates {
		if cached, ok := m.indexCache[strings.ToLower(update.index.Name)]; ok {
			*cached = update.index
		}
	}
	return nil
}

// repointIndexesLocked updates indexes whose table was renamed.
func (m *SchemaManager) repointIndexesLocked(oldName, newName string) error {
	return m.updateIndexesOfTableLocked(oldName, func(idx *Index) bool {
		idx.Table = newName
		return true
	})
}

// renameIndexColumnsLocked rewrites the stored definition of every index on
// table so a reference to oldName becomes newName. Index names stay unchanged:
// they are independent identifiers that SQLite does not derive from column
// names, and callers may have chosen them explicitly. A rename to an existing
// column name is rejected before this runs. Expression columns keep their SQL
// text; callers must reject a rename that such an expression depends on,
// because the text is not rewritten.
func (m *SchemaManager) renameIndexColumnsLocked(table, oldName, newName string) error {
	return m.updateIndexesOfTableLocked(table, func(idx *Index) bool {
		changed := false
		for i := range idx.Columns {
			// Expression columns persist SQL text, not a column reference; their
			// text is left for the caller to drop and recreate the index.
			if idx.Columns[i].Expression == "" && strings.EqualFold(idx.Columns[i].Name, oldName) {
				idx.Columns[i].Name = newName
				changed = true
			}
		}
		return changed
	})
}

// RenameColumn renames a column in a table. Column references in index
// definitions are rewritten; expression-index text is not, so a SQL caller must
// reject a rename that an expression depends on.
func (m *SchemaManager) RenameColumn(table, oldName, newName string) error {
	tableLock := m.tableLock(table)
	tableLock.Lock()
	defer tableLock.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	schema, err := m.getSchemaUnsafe(table)
	if err != nil {
		return err
	}

	// Check if new column name already exists
	for _, col := range schema.Columns {
		if strings.EqualFold(col.Name, newName) {
			return fmt.Errorf("column already exists: %s", newName)
		}
	}

	schema = cloneSchema(schema)
	// Find and rename column
	found := false
	for i, col := range schema.Columns {
		if strings.EqualFold(col.Name, oldName) {
			schema.Columns[i].Name = newName
			found = true

			// Update primary key reference if needed
			if strings.EqualFold(schema.PrimaryKey, oldName) {
				schema.PrimaryKey = newName
			}
			break
		}
	}

	if !found {
		return fmt.Errorf("column not found: %s", oldName)
	}
	if err := m.rewriteRows(table, func(row Row) bool {
		for key, value := range row {
			if strings.EqualFold(key, oldName) {
				delete(row, key)
				row[newName] = value
				return true
			}
		}
		return false
	}); err != nil {
		return err
	}

	// Publish the renamed schema before the index definitions: reads of the new
	// column name resolve against rewritten rows as soon as the schema lands.
	// Index DDL is not transactional (as elsewhere in this engine), so a failure
	// here leaves stale index metadata for the caller to drop and recreate.
	if err := m.updateSchemaUnsafe(schema); err != nil {
		return err
	}

	// Index metadata drives secondary-index values, UNIQUE enforcement,
	// ON CONFLICT target resolution, and catalog introspection, so it must name
	// the renamed column before any later statement uses the table.
	return m.renameIndexColumnsLocked(table, oldName, newName)
}

// rewriteRows applies an idempotent name-keyed row transformation in bounded
// batches. Column DDL updates the schema only after all rows are rewritten, so
// an interrupted operation can safely resume against the old schema.
func (m *SchemaManager) rewriteRows(table string, transform func(Row) bool) error {
	return m.applyRows(table, func(row Row) (bool, error) {
		return transform(row), nil
	})
}

// applyRows scans a table once and applies transform to every row. Rows for
// which transform reports a change are re-encoded and written in bounded
// batches; an error from transform aborts the scan. Callers hold the table's
// exclusive gate, so no writer can interleave between the scan and the schema
// update that follows it. Aborting after a batch was flushed is safe only
// because the DDL transforms are idempotent.
func (m *SchemaManager) applyRows(table string, transform func(Row) (bool, error)) error {
	prefix := []byte(fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(table)))
	return m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.Scan(prefix)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := cursor.Close(); retErr == nil {
				retErr = cerr
			}
		}()

		ops := make([]BatchOp, 0, scanPageSize)
		batchBytes := 8
		flush := func() error {
			if len(ops) == 0 {
				return nil
			}
			if _, err := client.BatchWrite(ops, nil); err != nil {
				return err
			}
			ops = ops[:0]
			batchBytes = 8
			return nil
		}

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
				changed, err := transform(row)
				if err != nil {
					return err
				}
				if !changed {
					continue
				}
				value, err := encodeRow(row)
				if err != nil {
					return err
				}
				opBytes := 12 + len(entry.Key) + len(value)
				if len(ops) == maxOperations || batchBytes+opBytes > bulkBatchByteBudget {
					if err := flush(); err != nil {
						return err
					}
				}
				ops = append(ops, BatchOp{Op: batchPut, Key: append([]byte(nil), entry.Key...), Value: value})
				batchBytes += opBytes
			}
			if done {
				return flush()
			}
		}
	})
}

// validateAddColumnUnique rejects an ADD COLUMN ... UNIQUE whose backfilled
// values would duplicate across existing rows. It reads rows as they will look
// after the rewrite (a row that already carries the field from an interrupted
// earlier attempt keeps its value) and mirrors ValidateUniqueIndex for a single
// plain column: NULLs do not collide. The caller holds the table's exclusive
// gate, so no writer can insert a conflicting row between this check and the
// publication of the index.
func (m *SchemaManager) validateAddColumnUnique(table string, column Column, index *Index) error {
	seen := make(map[string]struct{})
	return m.applyRows(table, func(row Row) (bool, error) {
		value, ok := rowColumnValue(row, column.Name)
		if !ok {
			value = column.Default
		}
		if value == nil {
			return false, nil
		}
		encoded := encodeUniqueScalar(value)
		if _, duplicate := seen[encoded]; duplicate {
			return false, &UniqueViolationError{Constraint: index.Name}
		}
		seen[encoded] = struct{}{}
		return false, nil
	})
}

// tableHasRows reports whether a table holds at least one durable row.
func (m *SchemaManager) tableHasRows(table string) (bool, error) {
	prefix := []byte(fmt.Sprintf("%s:_data:%s:", m.database, strings.ToLower(table)))
	found := false
	err := m.pool.WithClient(func(client *KVClient) (retErr error) {
		cursor, err := client.ScanWithLimit(prefix, 1)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := cursor.Close(); retErr == nil {
				retErr = cerr
			}
		}()
		entries, _, err := cursor.Next()
		if err != nil {
			return err
		}
		found = len(entries) > 0
		return nil
	})
	return found, err
}

// getSchemaUnsafe gets a schema without locking (internal use).
func (m *SchemaManager) getSchemaUnsafe(table string) (*Schema, error) {
	tableLower := strings.ToLower(table)

	// Check cache
	if schema, ok := m.cache[tableLower]; ok {
		return schema, nil
	}

	// Read from storage
	key := m.schemaKey(table)
	var data string
	err := m.pool.WithClient(func(c *KVClient) error {
		var err error
		data, err = c.Read(key)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("table not found: %s", table)
	}

	var schema Schema
	if err := json.Unmarshal([]byte(data), &schema); err != nil {
		return nil, err
	}

	m.cache[tableLower] = &schema
	return &schema, nil
}

// updateSchemaUnsafe updates a schema without locking (internal use).
func (m *SchemaManager) updateSchemaUnsafe(schema *Schema) error {
	key := m.schemaKey(schema.Name)
	data, _ := json.Marshal(schema)

	err := m.pool.WithClient(func(c *KVClient) error {
		return c.Write(key, string(data))
	})
	if err != nil {
		return err
	}

	// Update cache
	m.cache[strings.ToLower(schema.Name)] = schema
	m.bumpVersionLocked()
	return nil
}
