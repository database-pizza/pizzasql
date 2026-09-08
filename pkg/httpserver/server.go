package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
)

const httpTransactionTTL = 30 * time.Minute

// Config holds HTTP server configuration.
type Config struct {
	Host              string
	Port              int
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	MaxConnections    int
	EnableCORS        bool
	EnableAuth        bool
	EnableCompression bool
	EnableLogging     bool
	APIKeys           []string
	TLSCertFile       string
	TLSKeyFile        string
}

// DefaultConfig returns default server configuration.
func DefaultConfig() *Config {
	return &Config{
		Host:              "localhost",
		Port:              8080,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxConnections:    1000,
		EnableCORS:        true,
		EnableAuth:        false,
		EnableCompression: true,
		EnableLogging:     true,
		APIKeys:           []string{},
	}
}

// Server represents the HTTP API server.
type Server struct {
	config    *Config
	executor  *executor.Executor // Default executor (for backward compatibility)
	schema    *storage.SchemaManager
	dbManager *storage.DatabaseManager // Multi-database support
	server    *http.Server
	stats     *Stats

	// transactionExecutors holds a per-session executor for the HTTP
	// transaction endpoints (BEGIN/COMMIT/ROLLBACK), keyed by transaction ID.
	// This prevents transaction state and caches from being shared across
	// concurrent HTTP requests.
	transactionExecutorsMu sync.RWMutex
	transactionExecutors   map[string]*transactionExecutor
}

type transactionExecutor struct {
	mu        sync.Mutex
	exec      *executor.Executor
	database  string
	expiresAt time.Time
}

// Stats tracks server statistics.
type Stats struct {
	QueriesExecuted int64
	QueriesSuccess  int64
	QueriesError    int64
	StartTime       time.Time
}

// New creates a new HTTP server.
// Deprecated: Use NewWithDatabaseManager for multi-database support.
func New(config *Config, exec *executor.Executor, schema *storage.SchemaManager) *Server {
	if config == nil {
		config = DefaultConfig()
	}

	s := &Server{
		config:               config,
		executor:             exec,
		schema:               schema,
		transactionExecutors: make(map[string]*transactionExecutor),
		stats: &Stats{
			StartTime: time.Now(),
		},
	}

	return s.init()
}

// NewWithDatabaseManager creates a new HTTP server with multi-database support.
func NewWithDatabaseManager(config *Config, dbManager *storage.DatabaseManager) *Server {
	if config == nil {
		config = DefaultConfig()
	}

	defaultDB, _ := dbManager.GetDatabase("")
	var defaultExec *executor.Executor
	var defaultSchema *storage.SchemaManager
	if defaultDB != nil {
		defaultExec = executor.New(defaultDB.Schema, defaultDB.Table)
		defaultExec.SyncCatalog()
		defaultSchema = defaultDB.Schema
	}

	s := &Server{
		config:               config,
		executor:             defaultExec,
		schema:               defaultSchema,
		dbManager:            dbManager,
		transactionExecutors: make(map[string]*transactionExecutor),
		stats: &Stats{
			StartTime: time.Now(),
		},
	}

	return s.init()
}

// init initializes the server routes and middleware.
func (s *Server) init() *Server {
	mux := http.NewServeMux()

	// Apply middleware (order matters: logging -> auth -> cors -> compression -> handler)
	var handler http.Handler = mux

	if s.config.EnableCompression {
		handler = s.compressionMiddleware(handler)
	}

	if s.config.EnableCORS {
		handler = s.corsMiddleware(handler)
	}

	if s.config.EnableAuth {
		handler = s.authMiddleware(handler)
	}

	if s.config.EnableLogging {
		handler = s.loggingMiddleware(handler)
	}

	// Register routes
	mux.HandleFunc("/query", s.handleQuery)
	mux.HandleFunc("/execute", s.handleExecute)
	mux.HandleFunc("/schema/tables", s.handleSchemaTables)
	mux.HandleFunc("/schema/tables/", s.handleSchemaTable)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/transaction/begin", s.handleTransactionBegin)
	mux.HandleFunc("/transaction/commit", s.handleTransactionCommit)
	mux.HandleFunc("/transaction/rollback", s.handleTransactionRollback)
	mux.HandleFunc("/export", s.handleExport)
	mux.HandleFunc("/import", s.handleImport)

	s.server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Host, s.config.Port),
		Handler:      handler,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
	}

	return s
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	addr := s.server.Addr
	log.Printf("Starting HTTP server on http://%s", addr)

	if s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		return s.server.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
	}

	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("Shutting down HTTP server...")
	return s.server.Shutdown(ctx)
}

// Addr returns the server address.
func (s *Server) Addr() string {
	return s.server.Addr
}

// getExecutorForDatabase returns a fresh executor for the specified database.
// A new executor is created per call so transaction state, subquery caches, and
// other mutable per-executor fields are never shared across concurrent HTTP
// requests. If dbName is empty, the default database is used.
func (s *Server) getExecutorForDatabase(dbName string) (*executor.Executor, *storage.SchemaManager, error) {
	// If no database manager, create a fresh executor from the default managers.
	if s.dbManager == nil {
		return s.executor.NewSessionExecutor(), s.schema, nil
	}

	dbInstance, err := s.dbManager.GetDatabase(dbName)
	if err != nil {
		return nil, nil, err
	}

	exec := executor.New(dbInstance.Schema, dbInstance.Table)
	exec.SyncCatalog()
	return exec, dbInstance.Schema, nil
}

// beginTransaction starts a transaction bound to a new session executor and
// registers it under the returned transaction ID.
func (s *Server) beginTransaction(dbName string) (string, *executor.Executor, error) {
	dbName = strings.TrimSpace(dbName)
	exec, _, err := s.getExecutorForDatabase(dbName)
	if err != nil {
		return "", nil, err
	}

	l := lexer.New("BEGIN")
	p := parser.New(l)
	stmt, _ := p.Parse()
	if _, err := exec.Execute(stmt); err != nil {
		return "", nil, err
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, fmt.Errorf("generate transaction ID: %w", err)
	}
	txID := "tx-" + hex.EncodeToString(idBytes)
	now := time.Now()
	s.transactionExecutorsMu.Lock()
	for id, tx := range s.transactionExecutors {
		if !tx.expiresAt.After(now) {
			delete(s.transactionExecutors, id)
		}
	}
	s.transactionExecutors[txID] = &transactionExecutor{
		exec:      exec,
		database:  dbName,
		expiresAt: now.Add(httpTransactionTTL),
	}
	s.transactionExecutorsMu.Unlock()
	return txID, exec, nil
}

func (s *Server) getTransactionExecutor(txID, dbName string) (*transactionExecutor, bool) {
	dbName = strings.TrimSpace(dbName)
	s.transactionExecutorsMu.Lock()
	tx, ok := s.transactionExecutors[txID]
	if ok && !tx.expiresAt.After(time.Now()) {
		delete(s.transactionExecutors, txID)
		ok = false
	}
	if ok && tx.database == dbName {
		tx.mu.Lock()
	} else {
		ok = false
	}
	s.transactionExecutorsMu.Unlock()
	return tx, ok
}

// takeTransactionExecutor removes a session before COMMIT or ROLLBACK so it
// cannot receive another request while its terminal command is running.
func (s *Server) takeTransactionExecutor(txID, dbName string) (*transactionExecutor, bool) {
	dbName = strings.TrimSpace(dbName)
	s.transactionExecutorsMu.Lock()
	tx, ok := s.transactionExecutors[txID]
	if ok && !tx.expiresAt.After(time.Now()) {
		delete(s.transactionExecutors, txID)
		ok = false
	}
	if ok && tx.database == dbName {
		delete(s.transactionExecutors, txID)
	} else {
		ok = false
	}
	s.transactionExecutorsMu.Unlock()
	if ok {
		tx.mu.Lock()
	}
	return tx, ok
}
