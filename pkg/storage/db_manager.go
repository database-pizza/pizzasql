package storage

import (
	"fmt"
	"sync"
)

// DatabaseInstance represents a single database with its own schema and table managers.
type DatabaseInstance struct {
	Name   string
	Schema *SchemaManager
	Table  *TableManager
	// Hub is the shared LISTEN/NOTIFY channel registry for this database. All
	// connections to the same database instance publish and subscribe through
	// the same hub; separate instances (tenants) are fully isolated.
	Hub *Hub
}

// DatabaseManager manages multiple database instances.
type DatabaseManager struct {
	pool            *KVPool
	defaultDatabase string
	databases       map[string]*DatabaseInstance
	autoCreate      bool
	mu              sync.RWMutex
}

// DatabaseManagerConfig holds configuration for the database manager.
type DatabaseManagerConfig struct {
	DefaultDatabase string
	AutoCreate      bool // Auto-create databases on first access
}

// NewDatabaseManager creates a new database manager.
func NewDatabaseManager(pool *KVPool, config *DatabaseManagerConfig) *DatabaseManager {
	if config == nil {
		config = &DatabaseManagerConfig{
			DefaultDatabase: "pizzasql",
			AutoCreate:      true,
		}
	}

	dm := &DatabaseManager{
		pool:            pool,
		defaultDatabase: config.DefaultDatabase,
		databases:       make(map[string]*DatabaseInstance),
		autoCreate:      config.AutoCreate,
	}

	// Pre-create the default database instance
	dm.getOrCreateDatabase(config.DefaultDatabase)

	return dm
}

// GetDatabase returns the database instance for the given name.
// If name is empty, returns the default database.
// If autoCreate is enabled and the database doesn't exist, it will be created.
func (dm *DatabaseManager) GetDatabase(name string) (*DatabaseInstance, error) {
	if name == "" {
		name = dm.defaultDatabase
	}

	dm.mu.RLock()
	db, exists := dm.databases[name]
	dm.mu.RUnlock()

	if exists {
		return db, nil
	}

	if !dm.autoCreate {
		return nil, fmt.Errorf("database not found: %s", name)
	}

	return dm.getOrCreateDatabase(name), nil
}

// getOrCreateDatabase creates a new database instance if it doesn't exist.
func (dm *DatabaseManager) getOrCreateDatabase(name string) *DatabaseInstance {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	// Double-check after acquiring write lock
	if db, exists := dm.databases[name]; exists {
		return db
	}

	schema := NewSchemaManager(dm.pool, name)
	table := NewTableManager(dm.pool, schema, name)

	db := &DatabaseInstance{
		Name:   name,
		Schema: schema,
		Table:  table,
		Hub:    NewHub(),
	}

	dm.databases[name] = db
	return db
}

// DefaultDatabase returns the default database name.
func (dm *DatabaseManager) DefaultDatabase() string {
	return dm.defaultDatabase
}

// ListDatabases returns a list of all active database names.
func (dm *DatabaseManager) ListDatabases() []string {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	names := make([]string, 0, len(dm.databases))
	for name := range dm.databases {
		names = append(names, name)
	}
	return names
}

// DatabaseExists checks if a database instance exists (is loaded).
func (dm *DatabaseManager) DatabaseExists(name string) bool {
	dm.mu.RLock()
	defer dm.mu.RUnlock()
	_, exists := dm.databases[name]
	return exists
}

// GetPool returns the underlying KV pool.
func (dm *DatabaseManager) GetPool() *KVPool {
	return dm.pool
}

// SetAutoCreate enables or disables auto-creation of databases.
func (dm *DatabaseManager) SetAutoCreate(autoCreate bool) {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	dm.autoCreate = autoCreate
}
