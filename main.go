package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/csvexport"
	"github.com/danfragoso/pizzasql-next/pkg/csvimport"
	"github.com/danfragoso/pizzasql-next/pkg/executor"
	"github.com/danfragoso/pizzasql-next/pkg/httpserver"
	"github.com/danfragoso/pizzasql-next/pkg/kvmanager"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgserver"
	pizzaruntime "github.com/danfragoso/pizzasql-next/pkg/runtime"
	"github.com/danfragoso/pizzasql-next/pkg/sqlexport"
	"github.com/danfragoso/pizzasql-next/pkg/sqlimport"
	"github.com/danfragoso/pizzasql-next/pkg/sqliteimport"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

var (
	kvAddr          = flag.String("kvaddr", "", "PizzaKV server address (default: auto-connect to managed instance)")
	kvLaunch        = flag.Bool("kv", false, "Launch PizzaKV automatically")
	kvFlags         = flag.String("kvflags", "", "Flags to pass to PizzaKV (e.g., \"-iwal\")")
	forceYes        = flag.Bool("y", false, "Auto-accept prompts (skip interactive confirmation)")
	database        = flag.String("db", "pizzasql", "Database name")
	poolSize        = flag.Int("pool", 100, "Connection pool size")
	timeout         = flag.Duration("timeout", 120*time.Second, "Query timeout")
	httpEnable      = flag.Bool("http", false, "Enable HTTP server")
	httpHost        = flag.String("http-host", "localhost", "HTTP server host")
	httpPort        = flag.Int("http-port", 8080, "HTTP server port")
	httpCORS        = flag.Bool("http-cors", true, "Enable CORS")
	httpAuth        = flag.Bool("http-auth", false, "Enable authentication")
	httpCompression = flag.Bool("http-compression", true, "Enable HTTP response compression")
	quiet           = flag.Bool("quiet", false, "Disable request/query logging")
	apiKeys         = flag.String("api-keys", "", "Comma-separated API keys")

	// PostgreSQL wire protocol server flags
	pgEnable = flag.Bool("pg", false, "Enable PostgreSQL wire protocol server")
	pgHost   = flag.String("pg-host", "localhost", "PostgreSQL server host")
	pgPort   = flag.Int("pg-port", 5432, "PostgreSQL server port")

	// Export/Import flags
	exportFile   = flag.String("o", "", "Output file for export")
	importFile   = flag.String("i", "", "Input file for import")
	exportTable  = flag.String("table", "", "Specific table to export (empty = all)")
	exportDrop   = flag.Bool("drop", false, "Include DROP TABLE statements in export")
	ignoreErrors = flag.Bool("ignore-errors", false, "Continue import on errors")
	exportFormat = flag.String("format", "", "Export/import format: sql, csv (auto-detect from extension)")
	createTable  = flag.Bool("create-table", false, "Create table if not exists (CSV import)")
	showVersion  = flag.Bool("version", false, "Print version and exit")
)

var kvManager *kvmanager.Manager
var startPprofServerHook func() *http.Server

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	// Warn if other pizzasql instances are running; prompt to continue.
	if err := pizzaruntime.CheckExistingInstances(*forceYes); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Register this process in its own runtime directory.
	pizzaruntime.WritePizzaSQL(os.Getpid(), 0, 0)
	defer pizzaruntime.Cleanup()

	// Set up signal handling for graceful shutdown.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		stopPizzaKV()
		pizzaruntime.Cleanup()
		os.Exit(0)
	}()

	// If -kv flag is set, always launch a dedicated PizzaKV for this instance.
	if *kvLaunch {
		if err := launchPizzaKV(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to launch PizzaKV: %v\n", err)
			os.Exit(1)
		}
		defer stopPizzaKV()
	} else if *kvAddr == "" {
		*kvAddr = "localhost:8085"
	}

	// Start whichever servers are enabled, then block until signal.
	if *httpEnable || *pgEnable {
		httpRuntimePort := 0
		pgRuntimePort := 0
		if *httpEnable {
			httpRuntimePort = *httpPort
		}
		if *pgEnable {
			pgRuntimePort = *pgPort
		}
		if err := pizzaruntime.WritePizzaSQL(os.Getpid(), httpRuntimePort, pgRuntimePort); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write runtime info: %v\n", err)
		}
		runServers()
		return
	}

	// Check for export command
	if *exportFile != "" {
		runExport()
		return
	}

	// Check for import command
	if *importFile != "" {
		runImport()
		return
	}

	// Check for command-line SQL
	args := flag.Args()
	if len(args) > 0 {
		// Execute single SQL statement
		sql := strings.Join(args, " ")
		executeSingle(sql)
		return
	}

	// Check for piped input
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		// Input is from pipe
		executePipe()
		return
	}

	// Interactive REPL mode
	runREPL()
}

func executeSingle(sql string) {
	// Try to connect to PizzaKV
	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		// Fall back to expression-only mode
		executeExpressionOnly(sql)
		return
	}
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, *database)
	table := storage.NewTableManager(pool, schema, *database)
	exec := executor.New(schema, table)
	exec.SyncCatalog()

	result, err := executeSQL(exec, sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(result.String())
}

func executePipe() {
	// Try to connect to PizzaKV
	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		// Fall back to expression-only mode
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			sql := strings.TrimSpace(scanner.Text())
			if sql == "" || strings.HasPrefix(sql, "--") {
				continue
			}
			executeExpressionOnly(sql)
		}
		return
	}
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, *database)
	table := storage.NewTableManager(pool, schema, *database)
	exec := executor.New(schema, table)
	exec.SyncCatalog()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		sql := strings.TrimSpace(scanner.Text())
		if sql == "" || strings.HasPrefix(sql, "--") {
			continue
		}
		result, err := executeSQL(exec, sql)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			continue
		}
		fmt.Print(result.String())
	}
}

func runREPL() {
	fmt.Println("PizzaSQL - SQL-92 compatible database")
	fmt.Printf("Build: %s\n", version.String())
	fmt.Println("Type 'help' for usage, 'quit' to exit")
	fmt.Println()

	// Try to connect to PizzaKV
	var pool *storage.KVPool
	var schema *storage.SchemaManager
	var table *storage.TableManager
	var exec *executor.Executor

	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		fmt.Printf("Warning: Cannot connect to PizzaKV at %s\n", *kvAddr)
		fmt.Println("Running in expression-only mode (no table storage)")
		fmt.Println()
	} else {
		schema = storage.NewSchemaManager(pool, *database)
		table = storage.NewTableManager(pool, schema, *database)
		exec = executor.New(schema, table)
		exec.SyncCatalog()
		fmt.Printf("Connected to PizzaKV at %s (database: %s)\n\n", *kvAddr, *database)
	}

	historyFile := replHistoryFile()
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "pizzasql> ",
		HistoryFile:     historyFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
		AutoComplete:    newREPLCompleter(schema),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize interactive input: %v\n", err)
		return
	}
	defer rl.Close()

	var sqlBuffer strings.Builder

	for {
		if sqlBuffer.Len() == 0 {
			rl.SetPrompt("pizzasql> ")
		} else {
			rl.SetPrompt("       -> ")
		}

		line, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt {
				if sqlBuffer.Len() > 0 {
					sqlBuffer.Reset()
					fmt.Println("Buffer cleared")
					continue
				}
				fmt.Println("^C")
				continue
			}
			if err == io.EOF {
				fmt.Println()
				break
			}
			fmt.Fprintf(os.Stderr, "Input error: %v\n", err)
			continue
		}

		line = strings.TrimSpace(line)

		// Handle special commands
		switch strings.ToLower(line) {
		case "quit", "exit", "\\q":
			fmt.Println("Goodbye!")
			if pool != nil {
				pool.Close()
			}
			return
		case "help", "\\h":
			printHelp()
			continue
		case "tables", "\\dt":
			if schema != nil {
				listTables(schema)
			} else {
				fmt.Println("Not connected to database")
			}
			continue
		case "clear", "\\c":
			sqlBuffer.Reset()
			fmt.Println("Buffer cleared")
			continue
		case "status", "\\s":
			printStatus(exec != nil)
			continue
		case "functions", "\\df":
			printFunctions()
			continue
		}

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}

		// Accumulate SQL
		if sqlBuffer.Len() > 0 {
			sqlBuffer.WriteString(" ")
		}
		sqlBuffer.WriteString(line)

		// Check if statement is complete (ends with semicolon)
		sql := sqlBuffer.String()
		if !strings.HasSuffix(sql, ";") {
			continue
		}

		// Remove semicolon and execute
		sql = strings.TrimSuffix(sql, ";")
		sqlBuffer.Reset()

		if exec != nil {
			result, err := executeSQL(exec, sql)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				continue
			}
			fmt.Print(result.String())
		} else {
			executeExpressionOnly(sql)
		}
	}
}

type replCompleter struct {
	getTables func() []string
}

func newREPLCompleter(schema *storage.SchemaManager) readline.AutoCompleter {
	return &replCompleter{
		getTables: func() []string {
			if schema == nil {
				return nil
			}
			tables, err := schema.ListTables()
			if err != nil {
				return nil
			}
			return tables
		},
	}
}

func (c *replCompleter) Do(line []rune, pos int) ([][]rune, int) {
	if pos > len(line) {
		pos = len(line)
	}
	fragment := string(line[:pos])
	start := pos
	for start > 0 {
		r := line[start-1]
		if !(r == '_' || r == '\\' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			break
		}
		start--
	}
	prefix := fragment[start:pos]
	prefixUpper := strings.ToUpper(prefix)

	candidates := append(replCommands(), sqlKeywords()...)
	candidates = append(candidates, c.getTables()...)

	seen := make(map[string]struct{}, len(candidates))
	var out [][]rune
	for _, cand := range candidates {
		cand = strings.TrimSpace(cand)
		if cand == "" {
			continue
		}
		upper := strings.ToUpper(cand)
		if _, ok := seen[upper]; ok {
			continue
		}
		seen[upper] = struct{}{}
		if prefixUpper == "" || strings.HasPrefix(upper, prefixUpper) {
			suffix := cand
			if len(prefix) > 0 && len(cand) >= len(prefix) && strings.EqualFold(cand[:len(prefix)], prefix) {
				suffix = cand[len(prefix):]
			}
			suffix = matchSuffixCase(prefix, suffix)
			out = append(out, []rune(suffix))
		}
	}

	return out, len(prefix)
}

func replHistoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".pizzasql_history"
	}
	return filepath.Join(home, ".pizzasql_history")
}

func replCommands() []string {
	return []string{"help", "quit", "exit", "tables", "clear", "status", "functions", "\\h", "\\q", "\\dt", "\\c", "\\s", "\\df"}
}

func sqlKeywords() []string {
	return []string{
		"SELECT", "FROM", "WHERE", "INSERT", "INTO", "VALUES", "UPDATE", "SET", "DELETE",
		"CREATE", "TABLE", "DROP", "ALTER", "INDEX", "VIEW", "JOIN", "LEFT", "RIGHT", "INNER",
		"ON", "GROUP", "BY", "ORDER", "LIMIT", "OFFSET", "HAVING", "DISTINCT", "AS", "AND", "OR",
		"NOT", "NULL", "TRUE", "FALSE", "PRAGMA", "BEGIN", "COMMIT", "ROLLBACK", "PIZZASQL_VERSION",
	}
}

func matchSuffixCase(prefix, suffix string) string {
	if prefix == "" || suffix == "" {
		return suffix
	}
	hasLetter := false
	allUpper := true
	allLower := true
	for _, r := range prefix {
		if r >= 'A' && r <= 'Z' {
			hasLetter = true
			allLower = false
			continue
		}
		if r >= 'a' && r <= 'z' {
			hasLetter = true
			allUpper = false
			continue
		}
	}
	if !hasLetter {
		return suffix
	}
	if allUpper {
		return strings.ToUpper(suffix)
	}
	if allLower {
		return strings.ToLower(suffix)
	}
	return suffix
}

func executeSQL(exec *executor.Executor, sql string) (*executor.Result, error) {
	l := lexer.New(sql)
	p := parser.New(l)
	stmt, err := p.Parse()
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	return exec.Execute(stmt)
}

func executeExpressionOnly(sql string) {
	l := lexer.New(sql)
	p := parser.New(l)
	stmt, err := p.Parse()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Parse error: %v\n", err)
		return
	}

	// For SELECT statements without FROM, we can evaluate expressions
	if sel, ok := stmt.(*parser.SelectStmt); ok && len(sel.From) == 0 {
		exec := &executor.Executor{}
		result, err := executeSelectExpr(exec, sel)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		fmt.Print(result.String())
		return
	}

	// For other statements, just print what was parsed
	switch s := stmt.(type) {
	case *parser.SelectStmt:
		fmt.Printf("SELECT statement with %d columns\n", len(s.Columns))
		if len(s.From) > 0 {
			fmt.Printf("  FROM: %s\n", s.From[0].Name)
		}
		if s.Where != nil {
			fmt.Println("  WHERE: <condition>")
		}
		fmt.Println("(Not connected to database - cannot execute)")
	case *parser.InsertStmt:
		fmt.Printf("INSERT into %s (%d rows)\n", s.Table.Name, len(s.Values))
		fmt.Println("(Not connected to database - cannot execute)")
	case *parser.UpdateStmt:
		fmt.Printf("UPDATE %s (%d assignments)\n", s.Table.Name, len(s.Set))
		fmt.Println("(Not connected to database - cannot execute)")
	case *parser.DeleteStmt:
		fmt.Printf("DELETE from %s\n", s.Table.Name)
		fmt.Println("(Not connected to database - cannot execute)")
	case *parser.CreateTableStmt:
		fmt.Printf("CREATE TABLE %s (%d columns)\n", s.Table.Name, len(s.Columns))
		fmt.Println("(Not connected to database - cannot execute)")
	case *parser.DropTableStmt:
		fmt.Printf("DROP TABLE %s\n", s.Tables[0].Name)
		fmt.Println("(Not connected to database - cannot execute)")
	default:
		fmt.Printf("Parsed: %T\n", stmt)
	}
}

// executeSelectExpr handles SELECT without FROM (expression evaluation)
func executeSelectExpr(exec *executor.Executor, stmt *parser.SelectStmt) (*executor.Result, error) {
	result := executor.NewResult("SELECT")

	// Determine columns
	for i, col := range stmt.Columns {
		if col.Alias != "" {
			result.AddColumn(col.Alias)
		} else {
			result.AddColumn(fmt.Sprintf("column%d", i+1))
		}
	}

	// Evaluate expressions using reflection to access private method
	// For simplicity, we'll use a minimal evaluator here
	values := make([]interface{}, len(stmt.Columns))
	for i, col := range stmt.Columns {
		val, err := evalExprSimple(col.Expr)
		if err != nil {
			return nil, err
		}
		values[i] = val
	}
	result.AddRow(values...)

	return result, nil
}

// evalExprSimple is a simplified expression evaluator for standalone expressions
func evalExprSimple(expr parser.Expr) (interface{}, error) {
	switch e := expr.(type) {
	case *parser.LiteralExpr:
		switch e.Type {
		case lexer.TokenNumber:
			if strings.Contains(e.Value, ".") {
				var f float64
				fmt.Sscanf(e.Value, "%f", &f)
				return f, nil
			}
			var i int64
			fmt.Sscanf(e.Value, "%d", &i)
			return i, nil
		case lexer.TokenString:
			return e.Value, nil
		case lexer.TokenNULL:
			return nil, nil
		case lexer.TokenTRUE:
			return true, nil
		case lexer.TokenFALSE:
			return false, nil
		}
	case *parser.BinaryExpr:
		left, err := evalExprSimple(e.Left)
		if err != nil {
			return nil, err
		}
		right, err := evalExprSimple(e.Right)
		if err != nil {
			return nil, err
		}
		return evalBinarySimple(e.Op, left, right)
	case *parser.UnaryExpr:
		val, err := evalExprSimple(e.Operand)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case lexer.TokenMinus:
			return -toFloatSimple(val), nil
		case lexer.TokenNOT:
			return !toBoolSimple(val), nil
		}
		return val, nil
	case *parser.ParenExpr:
		return evalExprSimple(e.Expr)
	case *parser.FunctionCall:
		switch strings.ToUpper(e.Name) {
		case "PIZZASQL_VERSION", "SQLITE_VERSION":
			return version.String(), nil
		default:
			return nil, fmt.Errorf("unsupported function in expression mode: %s", e.Name)
		}
	}
	return nil, fmt.Errorf("unsupported expression type: %T", expr)
}

func evalBinarySimple(op lexer.TokenType, left, right interface{}) (interface{}, error) {
	switch op {
	case lexer.TokenPlus:
		return toFloatSimple(left) + toFloatSimple(right), nil
	case lexer.TokenMinus:
		return toFloatSimple(left) - toFloatSimple(right), nil
	case lexer.TokenStar:
		return toFloatSimple(left) * toFloatSimple(right), nil
	case lexer.TokenSlash:
		r := toFloatSimple(right)
		if r == 0 {
			return nil, nil
		}
		return toFloatSimple(left) / r, nil
	case lexer.TokenEq:
		return compareSimple(left, right) == 0, nil
	case lexer.TokenNeq:
		return compareSimple(left, right) != 0, nil
	case lexer.TokenLt:
		return compareSimple(left, right) < 0, nil
	case lexer.TokenGt:
		return compareSimple(left, right) > 0, nil
	case lexer.TokenLte:
		return compareSimple(left, right) <= 0, nil
	case lexer.TokenGte:
		return compareSimple(left, right) >= 0, nil
	case lexer.TokenAND:
		return toBoolSimple(left) && toBoolSimple(right), nil
	case lexer.TokenOR:
		return toBoolSimple(left) || toBoolSimple(right), nil
	}
	return nil, fmt.Errorf("unsupported operator: %v", op)
}

func toFloatSimple(v interface{}) float64 {
	switch val := v.(type) {
	case int64:
		return float64(val)
	case float64:
		return val
	case bool:
		if val {
			return 1
		}
		return 0
	}
	return 0
}

func toBoolSimple(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case int64:
		return val != 0
	case float64:
		return val != 0
	}
	return false
}

func compareSimple(a, b interface{}) int {
	fa := toFloatSimple(a)
	fb := toFloatSimple(b)
	if fa < fb {
		return -1
	}
	if fa > fb {
		return 1
	}
	return 0
}

func printHelp() {
	fmt.Println("PizzaSQL Commands:")
	fmt.Println("  help, \\h     Show this help")
	fmt.Println("  quit, \\q     Exit the program")
	fmt.Println("  tables, \\dt  List all tables")
	fmt.Println("  clear, \\c    Clear the input buffer")
	fmt.Println("  status, \\s   Show build and connection status")
	fmt.Println("  functions, \\df List built-in SQL functions")
	fmt.Println()
	fmt.Println("SQL Statements (end with semicolon):")
	fmt.Println("  SELECT ... FROM ... WHERE ...")
	fmt.Println("  INSERT INTO table (cols) VALUES (...)")
	fmt.Println("  UPDATE table SET col = val WHERE ...")
	fmt.Println("  DELETE FROM table WHERE ...")
	fmt.Println("  CREATE TABLE table (col TYPE, ...)")
	fmt.Println("  DROP TABLE table")
	fmt.Println()
	fmt.Println("Expression Mode (SELECT without FROM):")
	fmt.Println("  SELECT 1 + 2 * 3;")
	fmt.Println("  SELECT UPPER('hello');")
	fmt.Println()
	fmt.Println("Export/Import:")
	fmt.Println("  pizzasql -db mydb -o backup.sql           Export database to SQL file")
	fmt.Println("  pizzasql -db mydb -table users -o t.sql   Export single table")
	fmt.Println("  pizzasql -db mydb -o backup.sql -drop     Include DROP TABLE statements")
	fmt.Println("  pizzasql -db mydb -i backup.sql           Import SQL file")
	fmt.Println("  pizzasql -db mydb -i source.db            Import SQLite .db file (auto-detected)")
	fmt.Println("  pizzasql -db mydb -i source.db -ignore-errors  Import, skip errors")
	fmt.Println()
	fmt.Println("CSV Format:")
	fmt.Println("  pizzasql -db mydb -table users -o users.csv         Export table to CSV")
	fmt.Println("  pizzasql -db mydb -table users -i users.csv         Import CSV to table")
	fmt.Println("  pizzasql -db mydb -table new -i data.csv -create-table  Create table from CSV")
}

func listTables(schema *storage.SchemaManager) {
	tables, err := schema.ListTables()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}

	if len(tables) == 0 {
		fmt.Println("No tables found")
		return
	}

	fmt.Println("Tables:")
	for _, t := range tables {
		fmt.Printf("  %s\n", t)
	}
}

func printStatus(connected bool) {
	fmt.Printf("version: %s\n", version.String())
	if connected {
		fmt.Println("storage: connected")
		return
	}
	fmt.Println("storage: expression-only mode")
}

func printFunctions() {
	fns := analyzer.BuiltinFunctions()
	fmt.Println("Built-in SQL functions:")
	for _, fn := range fns {
		kind := "scalar"
		if fn.IsAggregate {
			kind = "aggregate"
		}
		if fn.MaxArgs < 0 {
			fmt.Printf("  %-18s %s (args: %d+)\n", fn.Name, kind, fn.MinArgs)
			continue
		}
		if fn.MinArgs == fn.MaxArgs {
			fmt.Printf("  %-18s %s (args: %d)\n", fn.Name, kind, fn.MinArgs)
			continue
		}
		fmt.Printf("  %-18s %s (args: %d..%d)\n", fn.Name, kind, fn.MinArgs, fn.MaxArgs)
	}
}

func runExport() {
	// Connect to PizzaKV
	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to PizzaKV at %s: %v\n", *kvAddr, err)
		os.Exit(1)
	}
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, *database)
	table := storage.NewTableManager(pool, schema, *database)

	// Determine format from flag or file extension
	format := strings.ToLower(*exportFormat)
	if format == "" {
		format = detectFileFormat(*exportFile)
	}

	switch format {
	case "csv":
		// CSV export requires a table name
		if *exportTable == "" {
			fmt.Fprintf(os.Stderr, "CSV export requires -table flag\n")
			os.Exit(1)
		}

		csvOpts := csvexport.DefaultExportOptions()
		csvOpts.Table = *exportTable

		data, err := csvexport.ExportTableToBytes(schema, table, csvOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Export failed: %v\n", err)
			os.Exit(1)
		}

		err = os.WriteFile(*exportFile, data, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write file: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Exported table '%s' to %s (CSV)\n", *exportTable, *exportFile)

	default: // sql, sqlite
		// Configure export options
		opts := sqlexport.ExportOptions{
			IncludeData: true,
			DropTables:  *exportDrop,
		}

		if *exportTable != "" {
			opts.Tables = []string{*exportTable}
		}

		// Export database
		sql, err := sqlexport.ExportDatabase(schema, table, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Export failed: %v\n", err)
			os.Exit(1)
		}

		// Write to file
		err = os.WriteFile(*exportFile, []byte(sql), 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write file: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Exported database '%s' to %s\n", *database, *exportFile)
	}
}

func runImport() {
	// Connect to PizzaKV
	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to PizzaKV at %s: %v\n", *kvAddr, err)
		os.Exit(1)
	}
	defer pool.Close()

	schema := storage.NewSchemaManager(pool, *database)
	table := storage.NewTableManager(pool, schema, *database)
	exec := executor.New(schema, table)
	exec.SyncCatalog()

	// Read file
	data, err := os.ReadFile(*importFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
		os.Exit(1)
	}

	// Determine format from flag or file extension
	format := strings.ToLower(*exportFormat)
	if format == "" {
		format = detectFileFormat(*importFile)
	}

	switch format {
	case "csv":
		// CSV import requires a table name
		if *exportTable == "" {
			fmt.Fprintf(os.Stderr, "CSV import requires -table flag\n")
			os.Exit(1)
		}

		csvOpts := csvimport.DefaultImportOptions()
		csvOpts.TableName = *exportTable
		csvOpts.IgnoreErrors = *ignoreErrors
		csvOpts.CreateTable = *createTable

		result, err := csvimport.ImportCSV(strings.NewReader(string(data)), schema, table, csvOpts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Import failed: %v\n", err)
			if len(result.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "Errors:\n")
				for _, e := range result.Errors {
					fmt.Fprintf(os.Stderr, "  - %s\n", e)
				}
			}
			os.Exit(1)
		}

		fmt.Printf("CSV import completed successfully\n")
		fmt.Printf("  Rows imported: %d\n", result.RowsImported)
		if result.RowsSkipped > 0 {
			fmt.Printf("  Rows skipped: %d\n", result.RowsSkipped)
		}
		if result.TableCreated {
			fmt.Printf("  Table created: %s\n", *exportTable)
		}
		if len(result.Errors) > 0 {
			fmt.Printf("  Warnings/Errors: %d\n", len(result.Errors))
			for _, e := range result.Errors {
				fmt.Printf("    - %s\n", e)
			}
		}

	case "sqlite":
		// Binary SQLite .db import
		opts := sqliteimport.DefaultImportOptions()
		opts.IgnoreErrors = *ignoreErrors

		result, err := sqliteimport.ImportSQLiteFile(*importFile, exec, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Import failed: %v\n", err)
			if len(result.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "Errors:\n")
				for _, e := range result.Errors {
					fmt.Fprintf(os.Stderr, "  - %s\n", e)
				}
			}
			os.Exit(1)
		}

		fmt.Printf("SQLite import completed successfully\n")
		if len(result.TablesCreated) > 0 {
			fmt.Printf("  Tables created: %s\n", strings.Join(result.TablesCreated, ", "))
		}
		if len(result.TablesImported) > 0 {
			fmt.Printf("  Tables imported: %s\n", strings.Join(result.TablesImported, ", "))
		}
		fmt.Printf("  Rows inserted: %d\n", result.RowsInserted)
		if result.IndexesCreated > 0 {
			fmt.Printf("  Indexes created: %d\n", result.IndexesCreated)
		}
		if len(result.Errors) > 0 {
			fmt.Printf("  Warnings/Errors: %d\n", len(result.Errors))
			for _, e := range result.Errors {
				fmt.Printf("    - %s\n", e)
			}
		}

	default: // sql
		// Configure import options
		opts := sqlimport.ImportOptions{
			IgnoreErrors: *ignoreErrors,
		}

		// Import SQL
		result, err := sqlimport.ImportSQL(exec, string(data), opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Import failed: %v\n", err)
			if len(result.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "Errors:\n")
				for _, e := range result.Errors {
					fmt.Fprintf(os.Stderr, "  - %s\n", e)
				}
			}
			os.Exit(1)
		}

		fmt.Printf("Import completed successfully\n")
		fmt.Printf("  Statements executed: %d\n", result.StatementsExecuted)
		if len(result.TablesCreated) > 0 {
			fmt.Printf("  Tables created: %s\n", strings.Join(result.TablesCreated, ", "))
		}
		if len(result.TablesDropped) > 0 {
			fmt.Printf("  Tables dropped: %s\n", strings.Join(result.TablesDropped, ", "))
		}
		fmt.Printf("  Rows inserted: %d\n", result.RowsInserted)

		if len(result.Errors) > 0 {
			fmt.Printf("  Warnings/Errors: %d\n", len(result.Errors))
			for _, e := range result.Errors {
				fmt.Printf("    - %s\n", e)
			}
		}
	}
}

func detectFileFormat(filename string) string {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".csv") {
		return "csv"
	}
	if strings.HasSuffix(lower, ".db") || strings.HasSuffix(lower, ".sqlite") || strings.HasSuffix(lower, ".sqlite3") {
		return "sqlite"
	}
	return "sql"
}

func runServers() {
	pool, err := storage.NewKVPool(*kvAddr, *poolSize, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to PizzaKV at %s: %v\n", *kvAddr, err)
		os.Exit(1)
	}
	defer pool.Close()

	dbManagerConfig := &storage.DatabaseManagerConfig{
		DefaultDatabase: *database,
		AutoCreate:      true,
	}
	dbManager := storage.NewDatabaseManager(pool, dbManagerConfig)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	var httpSrv *httpserver.Server
	var pprofSrv *http.Server
	var pgSrv *pgserver.Server

	if *httpEnable {
		config := httpserver.DefaultConfig()
		config.Host = *httpHost
		config.Port = *httpPort
		config.EnableCORS = *httpCORS
		config.EnableAuth = *httpAuth
		config.EnableCompression = *httpCompression
		config.EnableLogging = !*quiet
		if *apiKeys != "" {
			config.APIKeys = strings.Split(*apiKeys, ",")
		}
		httpSrv = httpserver.NewWithDatabaseManager(config, dbManager)
		if startPprofServerHook != nil {
			pprofSrv = startPprofServerHook()
		}
		go func() {
			if err := httpSrv.Start(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
				os.Exit(1)
			}
		}()
		fmt.Printf("HTTP  http://%s:%d\n", *httpHost, *httpPort)
	}

	if *pgEnable {
		config := pgserver.DefaultConfig()
		config.Host = *pgHost
		config.Port = *pgPort
		config.DefaultDatabase = *database
		config.Quiet = *quiet
		pgSrv = pgserver.New(config, dbManager)
		go func() {
			if err := pgSrv.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "PostgreSQL server error: %v\n", err)
				os.Exit(1)
			}
		}()
		fmt.Printf("PG    postgresql://%s:%d/%s\n", *pgHost, *pgPort, *database)
	}

	fmt.Printf("KV    %s\n", *kvAddr)
	fmt.Printf("DB    %s\n", *database)
	fmt.Println("Press Ctrl+C to stop")

	<-stop
	fmt.Println("\nShutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if httpSrv != nil {
		if err := httpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "HTTP shutdown error: %v\n", err)
		}
	}
	if pprofSrv != nil {
		pprofSrv.Shutdown(ctx)
	}
	if pgSrv != nil {
		if err := pgSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "PG shutdown error: %v\n", err)
		}
	}
}

// launchPizzaKV starts a dedicated PizzaKV instance for this pizzasql process.
func launchPizzaKV() error {
	if _, err := os.Stat(".db"); err == nil {
		if !*forceYes {
			if live := pizzaruntime.LiveInstances(); len(live) > 0 {
				inst := live[0]
				kvAddr := "<addr>"
				if inst.PizzaKV != nil {
					kvAddr = inst.PizzaKV.Addr
				}
				return fmt.Errorf(".db file already exists and another pizzasql instance is running (PID %d)\n"+
					"  To connect to its pizzakv:      pizzasql -kvaddr=%s\n"+
					"  To start fresh (removes data):  rm .db && pizzasql -kv\n"+
					"  To run a separate instance:     cd /other/dir && pizzasql -kv",
					inst.PizzaSQL.PID, kvAddr)
			}
		}
	}

	kvManager = kvmanager.NewManager()

	fmt.Println("Starting PizzaKV...")
	info, err := kvManager.Start(*kvFlags)
	if err != nil {
		return err
	}

	fmt.Printf("PizzaKV started on %s (PID: %d)\n", info.Addr, info.PID)
	fmt.Printf("Runtime: %s\n", pizzaruntime.File)
	fmt.Println("PizzaKV is ready!")

	*kvAddr = info.Addr
	return nil
}

// stopPizzaKV stops the managed PizzaKV instance
func stopPizzaKV() {
	if kvManager != nil {
		fmt.Println("Stopping PizzaKV...")
		if err := kvManager.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "Error stopping PizzaKV: %v\n", err)
		} else {
			fmt.Println("PizzaKV stopped")
		}
	}
}
