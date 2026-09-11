package executor

import (
	"container/heap"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/analyzer"
	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/storage"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

// Executor executes SQL statements.
type Executor struct {
	schema   *storage.SchemaManager
	table    *storage.TableManager
	session  *storage.Session
	analyzer *analyzer.Analyzer
	catalog  *analyzer.Catalog
	// Last SchemaManager version reflected in catalog.
	catalogVersion uint64

	// Multi-database support
	attachedDatabases map[string]*DatabaseConnection // alias -> connection
	currentDatabase   string                         // current database alias (default is "main")

	// Transaction state
	inTransaction      bool
	savepoints         []string // stack of savepoint names
	savepointPositions []int    // session mutation-log positions for each savepoint

	// Subquery context for correlated subqueries
	outerRow storage.Row

	// Per-query cache for non-correlated IN (SELECT ...) subquery results.
	// Keyed by subquery AST pointer; valid for one top-level Execute call.
	subqueryCache map[*parser.SelectStmt]*Result

	// Per-query cache for decorrelated scalar aggregate subqueries.
	// Keyed by subquery AST pointer; valid for one top-level Execute call.
	correlatedAggCache map[*parser.SelectStmt]*correlatedAggCache

	// In-memory view registry: view name (lowercase) → SELECT AST.
	views map[string]*parser.SelectStmt

	// Materialized common table expressions for the current query, keyed by
	// lowercased CTE name. Recursive CTEs populate this during fixpoint
	// iteration so their own legs can read the working set.
	cteTables map[string]*cteTable

	// Session-local SQLite compatibility state, tracked per connection so
	// last_insert_rowid()/changes()/total_changes() reflect this session only.
	lastInsertRowID int64 // rowid of the most recent successful INSERT
	changes         int64 // rows changed by the most recent INSERT/UPDATE/DELETE
	totalChanges    int64 // rows changed since this connection opened (monotonic)
}

type correlatedAggCache struct {
	values       map[string]interface{}
	defaultValue interface{}
}

type correlatedAggSpec struct {
	innerKey parser.Expr
	outerKey *parser.ColumnRef
	aggExpr  parser.Expr
}

// DatabaseConnection represents an attached database.
type DatabaseConnection struct {
	Alias  string
	Path   string // Database path or identifier
	Schema *storage.SchemaManager
	Table  *storage.TableManager
}

// New creates a new executor.
func New(schema *storage.SchemaManager, table *storage.TableManager) *Executor {
	catalog := analyzer.NewCatalog()
	executor := &Executor{
		schema:            schema,
		table:             table,
		session:           storage.NewSession(schema, table),
		analyzer:          analyzer.New(catalog),
		catalog:           catalog,
		attachedDatabases: make(map[string]*DatabaseConnection),
		currentDatabase:   "main",
		views:             make(map[string]*parser.SelectStmt),
	}

	// Register the main database
	executor.attachedDatabases["main"] = &DatabaseConnection{
		Alias:  "main",
		Path:   schema.GetDatabaseName(),
		Schema: schema,
		Table:  table,
	}

	return executor
}

// NewSessionExecutor returns a new executor sharing the same schema and table
// managers but with fresh per-executor state (transaction, subquery caches,
// views). It is used to isolate concurrent requests that must not share mutable
// executor state.
func (e *Executor) NewSessionExecutor() *Executor {
	exec := New(e.schema, e.table)
	exec.SyncCatalog()
	return exec
}

// SyncCatalog synchronizes the analyzer catalog with the storage schema.
func (e *Executor) SyncCatalog() error {
	tables, err := e.schema.ListTables()
	if err != nil {
		return err
	}

	storageTables := make(map[string]struct{}, len(tables))
	for _, tableName := range tables {
		storageTables[strings.ToUpper(tableName)] = struct{}{}
		schema, err := e.schema.GetSchema(tableName)
		if err != nil {
			continue
		}
		// Drop table from catalog if it exists, then recreate with updated schema
		e.catalog.DropTable(tableName)
		e.catalog.CreateTable(schema.ToAnalyzerTableInfo())
	}
	for _, table := range e.catalog.GetTables() {
		if table.IsView {
			continue
		}
		if _, exists := storageTables[strings.ToUpper(table.Name)]; !exists {
			e.catalog.DropTable(table.Name)
		}
	}

	e.catalogVersion = e.schema.Version()
	return nil
}

// Execute executes a SQL statement.
func (e *Executor) Execute(stmt parser.Statement) (*Result, error) {
	if _, beginning := stmt.(*parser.BeginStmt); !beginning && !e.inTransaction {
		e.schema.LockStatement()
		defer e.schema.UnlockStatement()
	}
	e.subqueryCache = make(map[*parser.SelectStmt]*Result)
	e.correlatedAggCache = make(map[*parser.SelectStmt]*correlatedAggCache)
	defer func() {
		e.subqueryCache = nil
		e.correlatedAggCache = nil
	}()

	// PRAGMA doesn't need analysis. SQLite catalog-introspection pragmas
	// (index_list, index_info, table_xinfo) are answered by the catalog agent
	// first; everything else falls through to the built-in handler.
	if pragma, ok := stmt.(*parser.PragmaStmt); ok {
		if res, handled, err := e.sqliteCatalogPragma(pragma); handled {
			return res, err
		}
		return e.executePragma(pragma)
	}

	// EXPLAIN doesn't need analysis
	if explain, ok := stmt.(*parser.ExplainStmt); ok {
		return e.executeExplain(explain)
	}

	// Transaction statements don't need analysis
	switch s := stmt.(type) {
	case *parser.BeginStmt:
		return e.executeBegin(s)
	case *parser.CommitStmt:
		return e.executeCommit(s)
	case *parser.RollbackStmt:
		return e.executeRollback(s)
	case *parser.SavepointStmt:
		return e.executeSavepoint(s)
	case *parser.ReleaseStmt:
		return e.executeRelease(s)
	case *parser.CreateIndexStmt:
		return e.executeCreateIndex(s)
	case *parser.DropIndexStmt:
		return e.executeDropIndex(s)
	case *parser.CreateViewStmt:
		return e.executeCreateView(s)
	case *parser.DropViewStmt:
		return e.executeDropView(s)
	case *parser.AttachStmt:
		return e.executeAttach(s)
	case *parser.DetachStmt:
		return e.executeDetach(s)
	}

	// SQLite catalog/metadata dispatch. SELECTs against sqlite_master /
	// sqlite_schema must be answered before the analyzer, which would otherwise
	// reject those virtual tables as unknown. Falls through when unhandled.
	if sel, ok := stmt.(*parser.SelectStmt); ok {
		if res, handled, err := e.sqliteCatalogSelect(sel); handled {
			return res, err
		}
	}

	// Analyze first. If the cached analyzer catalog is stale because schema was
	// changed through another executor/API path, resync from storage and retry
	// once before returning table/column-not-found errors.
	if err := e.analyzeWithCatalogRetry(stmt); err != nil {
		return nil, err
	}

	switch s := stmt.(type) {
	case *parser.SelectStmt:
		return e.executeSelect(s)
	case *parser.InsertStmt:
		return e.executeInsert(s)
	case *parser.UpdateStmt:
		return e.executeUpdate(s)
	case *parser.DeleteStmt:
		return e.executeDelete(s)
	case *parser.CreateTableStmt:
		return e.executeCreateTable(s)
	case *parser.DropTableStmt:
		return e.executeDropTable(s)
	case *parser.CreateIndexStmt:
		return e.executeCreateIndex(s)
	case *parser.DropIndexStmt:
		return e.executeDropIndex(s)
	case *parser.AlterTableStmt:
		return e.executeAlterTable(s)
	default:
		return nil, fmt.Errorf("unsupported statement type: %T", stmt)
	}
}

func (e *Executor) analyzeWithCatalogRetry(stmt parser.Statement) error {
	if e.catalogVersion != e.schema.Version() {
		if err := e.SyncCatalog(); err != nil {
			return err
		}
	}

	a := analyzer.New(e.catalog)
	err := a.Analyze(stmt)
	if err == nil {
		return nil
	}
	if !isCatalogMiss(err) {
		return err
	}

	if syncErr := e.SyncCatalog(); syncErr != nil {
		return err
	}

	a = analyzer.New(e.catalog)
	return a.Analyze(stmt)
}

func isCatalogMiss(err error) bool {
	var analysisErr *analyzer.AnalysisError
	if !errors.As(err, &analysisErr) {
		return false
	}
	return analysisErr.Type == analyzer.ErrTableNotFound ||
		analysisErr.Type == analyzer.ErrColumnNotFound
}

// isCountStarSingleTable reports whether the statement is the safe COUNT(*)
// shape eligible for the metadata fast path: a single-table SELECT with exactly
// one COUNT(*) column and no filters, grouping, DISTINCT, JOIN, subquery, or
// LIMIT/OFFSET. Anything else returns false so unsupported shapes use the
// normal scan path.
func isCountStarSingleTable(stmt *parser.SelectStmt) bool {
	if stmt.Compound != nil || stmt.Distinct {
		return false
	}
	if stmt.Where != nil || stmt.Having != nil {
		return false
	}
	if len(stmt.GroupBy) > 0 || len(stmt.OrderBy) > 0 {
		return false
	}
	if stmt.Limit != nil || stmt.Offset != nil {
		return false
	}
	if len(stmt.From) != 1 {
		return false
	}
	ref := stmt.From[0]
	if ref.Subquery != nil || ref.Join != nil {
		return false
	}
	if len(stmt.Columns) != 1 || stmt.Columns[0].Star {
		return false
	}
	fn, ok := stmt.Columns[0].Expr.(*parser.FunctionCall)
	if !ok {
		return false
	}
	if !strings.EqualFold(fn.Name, "count") || !fn.Star || len(fn.Args) > 0 {
		return false
	}
	return true
}

// executeSelect executes a SELECT statement (or compound SELECT).
func (e *Executor) executeSelect(stmt *parser.SelectStmt) (*Result, error) {
	if len(stmt.With) > 0 {
		return e.executeWith(stmt)
	}
	if stmt.Compound != nil {
		return e.executeCompound(stmt.Compound)
	}
	if len(stmt.From) == 0 {
		// SELECT without FROM (e.g., SELECT 1+1)
		return e.executeSelectExpr(stmt)
	}

	// Check if FROM clause is a subquery (derived table)
	if stmt.From[0].Subquery != nil {
		return e.executeSelectFromSubquery(stmt)
	}

	tableName := stmt.From[0].Name

	// A materialized CTE is served from memory, not from storage.
	if cte, ok := e.cteTableFor(tableName); ok {
		return e.executeSelectOnMaterialized(stmt, cte.columns, cteRowsToValues(cte))
	}

	// Transparently expand view references as derived-table subqueries.
	if viewDef, ok := e.views[strings.ToLower(tableName)]; ok {
		alias := stmt.From[0].Alias
		if alias == "" {
			alias = tableName
		}
		modifiedStmt := *stmt
		modifiedFrom := make([]parser.TableRef, len(stmt.From))
		copy(modifiedFrom, stmt.From)
		modifiedFrom[0] = parser.TableRef{Subquery: viewDef, Alias: alias}
		modifiedStmt.From = modifiedFrom
		return e.executeSelectFromSubquery(&modifiedStmt)
	}

	schema, err := e.schema.GetSchema(tableName)
	if err != nil {
		return nil, err
	}

	// COUNT(*) fast path: exact metadata-based count for the safe single-table
	// shape with no filters/grouping/distinct/join. Any unsupported shape falls
	// through to the normal scan path.
	if isCountStarSingleTable(stmt) {
		count, err := e.session.CountFast(tableName)
		if err != nil {
			return nil, err
		}
		result := NewResult("SELECT")
		if stmt.Columns[0].Alias != "" {
			result.AddColumn(stmt.Columns[0].Alias)
		} else {
			result.AddColumn("column1")
		}
		result.AddRow(int64(count))
		return result, nil
	}

	// Multi-table FROM (comma-separated implicit cross join): collect and cross join all tables,
	// then apply WHERE after. Don't push WHERE down here — conditions reference multiple tables.
	isMultiTable := len(stmt.From) > 1 && stmt.From[0].Join == nil

	// Optimize constant WHERE clauses
	var constantWhereResult *bool
	if stmt.Where != nil && !isMultiTable {
		// Check if WHERE clause is a constant expression (doesn't reference any columns)
		refs := collectColumnRefs(stmt.Where)
		if len(refs) == 0 {
			// Evaluate the constant expression
			val, err := e.evalExpr(stmt.Where, nil)
			if err == nil {
				result := toBool(val)
				constantWhereResult = &result
			}
		}
	}

	// If WHERE is constant false, check if we have aggregates first
	if constantWhereResult != nil && !*constantWhereResult {
		// If query has GROUP BY, return empty result (no groups match)
		// If query has aggregates but no GROUP BY, evaluate them on empty row set
		if e.hasAggregates(stmt.Columns) {
			if len(stmt.GroupBy) > 0 {
				// GROUP BY with no matching rows: return empty result (no groups)
				// Fall through to the non-aggregate case below
			} else {
				// Aggregate without GROUP BY: return single row with aggregate results on empty set
				return e.executeAggregateSelect(stmt, []storage.Row{}, schema)
			}
		}
		// Non-aggregate query with WHERE false: return empty result
		result := NewResult("SELECT")
		for i, col := range stmt.Columns {
			if col.Alias != "" {
				result.AddColumn(col.Alias)
			} else if ref, ok := col.Expr.(*parser.ColumnRef); ok {
				result.AddColumn(ref.Column)
			} else if col.Star {
				for _, c := range schema.Columns {
					result.AddColumn(c.Name)
				}
			} else {
				result.AddColumn(fmt.Sprintf("column%d", i+1))
			}
		}
		return result, nil
	}

	// Try to use index for WHERE clause (single-table only)
	var rows []storage.Row
	usedIndex := false

	// If WHERE is constant true, skip it during table scan
	effectiveWhere := stmt.Where
	if constantWhereResult != nil && *constantWhereResult {
		effectiveWhere = nil
	}

	// Pre-evaluate non-correlated subqueries in the WHERE before the scan takes
	// the session lock.
	e.primeSubqueries(effectiveWhere)

	// A WHERE that contains a subquery is evaluated after the scan. Evaluating a
	// correlated subquery inside the locked scan re-enters the session lock and
	// deadlocks, so only a subquery-free predicate is pushed into the scan.
	whereHasSubquery := false
	for _, ref := range collectColumnRefs(effectiveWhere) {
		if ref == "__subquery__" {
			whereHasSubquery = true
			break
		}
	}
	appliedInScan := effectiveWhere != nil && !whereHasSubquery && stmt.From[0].Alias == "" && !isMultiTable && stmt.From[0].Join == nil

	if effectiveWhere != nil && !isMultiTable {
		// Check if we can use an index
		colName, colValue, isEquality := e.extractIndexableCondition(stmt.Where)
		if isEquality {
			if strings.EqualFold(schema.PrimaryKey, colName) {
				row, getErr := e.session.GetByPK(tableName, fmt.Sprintf("%v", colValue))
				if getErr == nil {
					normalizeRowBySchema(row, schema)
					rows = []storage.Row{row}
					usedIndex = true
				} else if getErr == storage.ErrKeyNotFound {
					rows = []storage.Row{}
					usedIndex = true
				} else {
					return nil, getErr
				}
			} else {
				// Look for an index on this column
				indexes, _ := e.schema.ListTableIndexes(tableName)
				for _, idx := range indexes {
					if len(idx.Columns) == 1 && strings.EqualFold(idx.Columns[0].Name, colName) {
						rows, err = e.session.SelectByIndex(tableName, idx.Name, colValue)
						if err == nil {
							usedIndex = true
							for i := range rows {
								normalizeRowBySchema(rows[i], schema)
							}
						}
						break
					}
				}
			}
		}
	}

	// Fall back to full table scan if no index used
	if !usedIndex {
		var filterErr error
		var filter func(storage.Row) bool
		if appliedInScan {
			filter = func(row storage.Row) bool {
				val, ferr := e.evalExpr(effectiveWhere, row)
				if ferr != nil {
					filterErr = ferr
					return false
				}
				return toBool(val)
			}
		}
		rows, err = e.session.Select(tableName, filter)
		if filterErr != nil {
			return nil, filterErr
		}
		for _, row := range rows {
			normalizeRowBySchema(row, schema)
		}
	}
	if err != nil {
		return nil, err
	}

	// Add table alias to rows if there's an explicit alias
	if stmt.From[0].Alias != "" {
		for i := range rows {
			rows[i] = e.addTableAlias(rows[i], stmt.From[0].Alias)
		}
	} else if isMultiTable {
		// For multi-table cross joins without alias, prefix columns with table name
		// so WHERE can distinguish t3.a3 from t7.a7.
		for i := range rows {
			rows[i] = e.addTableAlias(rows[i], tableName)
		}
	}

	// Apply WHERE for a single-table scan that did not push the predicate into
	// the locked scan (aliased tables, or a WHERE containing a subquery). A
	// query with a JOIN defers WHERE until after the join, because the WHERE may
	// reference columns from the joined table.
	if effectiveWhere != nil && !isMultiTable && stmt.From[0].Join == nil && !appliedInScan {
		var filterErr error
		var filtered []storage.Row
		for _, row := range rows {
			val, ferr := e.evalExpr(effectiveWhere, row)
			if ferr != nil {
				filterErr = ferr
				break
			}
			if toBool(val) {
				filtered = append(filtered, row)
			}
		}
		if filterErr != nil {
			return nil, filterErr
		}
		rows = filtered
	}

	// Handle explicit JOINs from the first FROM entry (single-table+JOIN path)
	if !isMultiTable && len(stmt.From) > 0 && stmt.From[0].Join != nil {
		rows, err = e.executeJoins(stmt.From[0], rows)
		if err != nil {
			return nil, err
		}
		// Cross-join with any remaining comma-separated FROM entries (mixed JOIN+comma syntax)
		for _, tref := range stmt.From[1:] {
			rightRows, rerr := e.session.Select(tref.Name, nil)
			if rerr != nil {
				return nil, rerr
			}
			rightAlias := tref.Alias
			if rightAlias == "" {
				rightAlias = tref.Name
			}
			for i := range rightRows {
				rightRows[i] = e.addTableAlias(rightRows[i], rightAlias)
			}
			var joined []storage.Row
			for _, l := range rows {
				for _, r := range rightRows {
					m := make(storage.Row, len(l)+len(r))
					for k, v := range l {
						m[k] = v
					}
					for k, v := range r {
						m[k] = v
					}
					joined = append(joined, m)
				}
			}
			rows = joined
			// Handle JOINs within this tref too
			if tref.Join != nil {
				rows, err = e.executeJoins(tref, rows)
				if err != nil {
					return nil, err
				}
			}
		}
		// Apply WHERE after all joins. It is intentionally deferred past the base
		// scan for JOIN queries because the WHERE may reference joined-table columns.
		if effectiveWhere != nil {
			var filtered []storage.Row
			for _, row := range rows {
				val, _ := e.evalExpr(effectiveWhere, row)
				if toBool(val) {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}
	}

	// Handle implicit cross joins (comma-separated FROM tables)
	if isMultiTable {
		// Build the column-set for each table so we can push WHERE conditions down.
		type tableInfo struct {
			alias   string
			name    string
			colsSet map[string]bool // lower-case column names for this table
		}

		allTableInfos := make([]tableInfo, len(stmt.From))
		for i, tref := range stmt.From {
			alias := tref.Alias
			if alias == "" {
				alias = tref.Name
			}
			sch, _ := e.schema.GetSchema(tref.Name)
			cols := map[string]bool{}
			if sch != nil {
				for _, c := range sch.Columns {
					cols[strings.ToLower(c.Name)] = true
				}
			}
			allTableInfos[i] = tableInfo{alias: alias, name: tref.Name, colsSet: cols}
		}

		// Split WHERE into AND-clauses and determine which tables each clause touches.
		var andClauses []parser.Expr
		if stmt.Where != nil {
			andClauses = splitANDClauses(stmt.Where)
		}

		// For each table, collect conditions that reference only its own columns.
		tableFilters := make([][]parser.Expr, len(stmt.From))
		var crossFilters []parser.Expr
		for _, clause := range andClauses {
			refs := collectColumnRefs(clause)
			ownerIdx := -1
			cross := false
			for _, ref := range refs {
				colLower := strings.ToLower(ref)
				found := -1
				for i, ti := range allTableInfos {
					if ti.colsSet[colLower] {
						if found == -1 {
							found = i
						} else if found != i {
							cross = true
							break
						}
					}
				}
				if cross {
					break
				}
				if found != -1 {
					if ownerIdx == -1 {
						ownerIdx = found
					} else if ownerIdx != found {
						cross = true
						break
					}
				}
			}
			if cross || ownerIdx == -1 {
				crossFilters = append(crossFilters, clause)
			} else {
				tableFilters[ownerIdx] = append(tableFilters[ownerIdx], clause)
			}
		}

		// Build cross-condition adjacency: for each cross filter, record which table indices it touches.
		type crossEdge struct{ a, b int }
		var crossEdges []crossEdge
		for _, clause := range crossFilters {
			refs := collectColumnRefs(clause)
			touched := map[int]bool{}
			for _, ref := range refs {
				cl := strings.ToLower(ref)
				for j, ti := range allTableInfos {
					if ti.colsSet[cl] {
						touched[j] = true
					}
				}
			}
			idxs := make([]int, 0, len(touched))
			for j := range touched {
				idxs = append(idxs, j)
			}
			if len(idxs) == 2 {
				crossEdges = append(crossEdges, crossEdge{idxs[0], idxs[1]})
			}
		}

		// Build alias-to-index map for applyWhenSeen.
		aliasToIdx := make(map[string]int, len(allTableInfos))
		for i, ti := range allTableInfos {
			aliasToIdx[strings.ToLower(ti.alias)] = i
		}

		// Helper: find cross-conditions applicable when seenSet is fully present.
		// A condition is applicable only when ALL tables it references are in seenSet.
		// For table-qualified refs (e.g. cor0.col2), we check the qualifying alias is seen.
		applyWhenSeen := func(seenSet map[int]bool, pending []parser.Expr) (applicable, still []parser.Expr) {
			for _, clause := range pending {
				tableRefs := collectTableColumnRefs(clause)
				ok := true
				for _, tr := range tableRefs {
					col := strings.ToLower(tr.col)
					tbl := strings.ToLower(tr.tbl)
					found := false
					if tbl != "" {
						// Explicit table qualifier — check that qualifying alias is seen.
						if idx, exists := aliasToIdx[tbl]; exists && seenSet[idx] {
							found = true
						}
					} else {
						// Unqualified — any seen table with this column satisfies it.
						for j, tti := range allTableInfos {
							if tti.colsSet[col] && seenSet[j] {
								found = true
								break
							}
						}
					}
					if !found {
						ok = false
						break
					}
				}
				if ok {
					applicable = append(applicable, clause)
				} else {
					still = append(still, clause)
				}
			}
			return
		}

		// Helper: inline cross-join two row-sets, applying a predicate.
		inlineJoin := func(left, right []storage.Row, pred parser.Expr) []storage.Row {
			out := make([]storage.Row, 0, len(left))
			for _, l := range left {
				for _, r := range right {
					m := make(storage.Row, len(l)+len(r))
					for k, v := range l {
						m[k] = v
					}
					for k, v := range r {
						m[k] = v
					}
					if pred != nil {
						val, _ := e.evalExpr(pred, m)
						if !toBool(val) {
							continue
						}
					}
					out = append(out, m)
				}
			}
			return out
		}

		// Pre-join connected components of "cross-only" tables (0 single-table filters,
		// connected via cross conditions to other cross-only tables).
		// This prevents n^k explosions when bare tables are joined last.
		crossOnlySet := map[int]bool{}
		for i := range stmt.From {
			if len(tableFilters[i]) > 0 {
				continue
			}
			for _, ce := range crossEdges {
				if ce.a == i || ce.b == i {
					crossOnlySet[i] = true
					break
				}
			}
		}

		// BFS: find connected components among cross-only tables.
		compOf := make([]int, len(stmt.From))
		for i := range compOf {
			compOf[i] = -1
		}
		nComps := 0
		for start := range stmt.From {
			if !crossOnlySet[start] || compOf[start] != -1 {
				continue
			}
			queue := []int{start}
			compOf[start] = nComps
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				for _, ce := range crossEdges {
					var nb int = -1
					if ce.a == cur && crossOnlySet[ce.b] {
						nb = ce.b
					} else if ce.b == cur && crossOnlySet[ce.a] {
						nb = ce.a
					}
					if nb >= 0 && compOf[nb] == -1 {
						compOf[nb] = nComps
						queue = append(queue, nb)
					}
				}
			}
			nComps++
		}

		// Group cross-only tables by component.
		compTbls := make([][]int, nComps)
		for i, c := range compOf {
			if c >= 0 {
				compTbls[c] = append(compTbls[c], i)
			}
		}

		// Pre-join each component with ≥2 tables; collect results as virtual units.
		type virtualUnit struct {
			tableIdxs map[int]bool
			rows      []storage.Row
		}
		var virtuals []virtualUnit
		preJoined := map[int]bool{} // original table indices consumed into virtuals
		remaining := make([]parser.Expr, len(crossFilters))
		copy(remaining, crossFilters)

		for _, comp := range compTbls {
			if len(comp) < 2 {
				continue
			}
			// Pick seed: table with most cross-edges within component.
			seed := comp[0]
			for _, idx := range comp[1:] {
				degIdx, degSeed := 0, 0
				for _, ce := range crossEdges {
					if ce.a == idx || ce.b == idx {
						degIdx++
					}
					if ce.a == seed || ce.b == seed {
						degSeed++
					}
				}
				if degIdx > degSeed {
					seed = idx
				}
			}

			// Load seed.
			seedRows, rerr := e.session.Select(stmt.From[seed].Name, nil)
			if rerr != nil {
				return nil, rerr
			}
			seedSchema, _ := e.schema.GetSchema(stmt.From[seed].Name)
			for j := range seedRows {
				normalizeRowBySchema(seedRows[j], seedSchema)
				seedRows[j] = e.addTableAlias(seedRows[j], allTableInfos[seed].alias)
			}
			vSeen := map[int]bool{seed: true}

			// Greedy within-component join.
			compSet := map[int]bool{}
			for _, idx := range comp {
				compSet[idx] = true
			}
			for len(vSeen) < len(comp) {
				// Pick next table in component with cross-edge to vSeen.
				nextC := -1
				for _, idx := range comp {
					if vSeen[idx] {
						continue
					}
					for _, ce := range crossEdges {
						if (ce.a == idx && vSeen[ce.b]) || (ce.b == idx && vSeen[ce.a]) {
							nextC = idx
							break
						}
					}
					if nextC >= 0 {
						break
					}
				}
				if nextC < 0 {
					for _, idx := range comp {
						if !vSeen[idx] {
							nextC = idx
							break
						}
					}
				}

				nextRows, rerr := e.session.Select(stmt.From[nextC].Name, nil)
				if rerr != nil {
					return nil, rerr
				}
				nextSchema, _ := e.schema.GetSchema(stmt.From[nextC].Name)
				for j := range nextRows {
					normalizeRowBySchema(nextRows[j], nextSchema)
					nextRows[j] = e.addTableAlias(nextRows[j], allTableInfos[nextC].alias)
				}
				vSeen[nextC] = true

				appl, still := applyWhenSeen(vSeen, remaining)
				remaining = still
				var pred parser.Expr
				if len(appl) > 0 {
					pred = combineAND(appl)
				}
				seedRows = inlineJoin(seedRows, nextRows, pred)
			}

			virtuals = append(virtuals, virtualUnit{tableIdxs: vSeen, rows: seedRows})
			for idx := range vSeen {
				preJoined[idx] = true
			}
		}

		// Build greedy order for non-pre-joined tables.
		// Score: single-table filter count + 1000 × cross-edges to already-joined.
		orderNonPJ := make([]int, 0, len(stmt.From)-len(preJoined))
		inOrderNPJ := make([]bool, len(stmt.From))

		best := -1
		for j := range stmt.From {
			if preJoined[j] {
				continue
			}
			if best < 0 || len(tableFilters[j]) > len(tableFilters[best]) {
				best = j
			}
		}
		if best >= 0 {
			orderNonPJ = append(orderNonPJ, best)
			inOrderNPJ[best] = true
		}
		for len(orderNonPJ)+len(preJoined) < len(stmt.From) {
			joined := map[int]bool{}
			for _, idx := range orderNonPJ {
				joined[idx] = true
			}
			nextIdx := -1
			nextScore := -1
			for j := range stmt.From {
				if inOrderNPJ[j] || preJoined[j] {
					continue
				}
				score := len(tableFilters[j])
				for _, ce := range crossEdges {
					if (ce.a == j && joined[ce.b]) || (ce.b == j && joined[ce.a]) {
						score += 1000
					}
				}
				if score > nextScore {
					nextScore = score
					nextIdx = j
				}
			}
			if nextIdx < 0 {
				for j := range stmt.From {
					if !inOrderNPJ[j] && !preJoined[j] {
						nextIdx = j
						break
					}
				}
			}
			if nextIdx >= 0 {
				orderNonPJ = append(orderNonPJ, nextIdx)
				inOrderNPJ[nextIdx] = true
			}
		}

		// Load the initial rows for the first non-pre-joined table (or use the already-loaded rows).
		seenTables := map[int]bool{}
		if len(orderNonPJ) > 0 {
			first := orderNonPJ[0]
			if first != 0 {
				rows, err = e.session.Select(stmt.From[first].Name, nil)
				if err != nil {
					return nil, err
				}
				firstSchema, _ := e.schema.GetSchema(stmt.From[first].Name)
				for i := range rows {
					normalizeRowBySchema(rows[i], firstSchema)
					rows[i] = e.addTableAlias(rows[i], allTableInfos[first].alias)
				}
			}
			if len(tableFilters[first]) > 0 {
				pred := combineAND(tableFilters[first])
				var filtered []storage.Row
				for _, row := range rows {
					val, _ := e.evalExpr(pred, row)
					if toBool(val) {
						filtered = append(filtered, row)
					}
				}
				rows = filtered
			}
			seenTables[first] = true

			// Join remaining non-pre-joined tables.
			for _, idx := range orderNonPJ[1:] {
				ti := allTableInfos[idx]
				rightRows, rerr := e.session.Select(stmt.From[idx].Name, nil)
				if rerr != nil {
					return nil, rerr
				}
				rightSchema, _ := e.schema.GetSchema(stmt.From[idx].Name)
				for j := range rightRows {
					normalizeRowBySchema(rightRows[j], rightSchema)
					rightRows[j] = e.addTableAlias(rightRows[j], ti.alias)
				}
				if len(tableFilters[idx]) > 0 {
					pred := combineAND(tableFilters[idx])
					var filtered []storage.Row
					for _, row := range rightRows {
						val, _ := e.evalExpr(pred, row)
						if toBool(val) {
							filtered = append(filtered, row)
						}
					}
					rightRows = filtered
				}
				seenTables[idx] = true
				appl, still := applyWhenSeen(seenTables, remaining)
				remaining = still
				var pred parser.Expr
				if len(appl) > 0 {
					pred = combineAND(appl)
				}
				rows = inlineJoin(rows, rightRows, pred)
			}
		} else {
			// All tables were pre-joined; start with empty placeholder.
			rows = []storage.Row{{}}
		}

		// Integrate virtual (pre-joined) units into the result.
		for _, vu := range virtuals {
			for idx := range vu.tableIdxs {
				seenTables[idx] = true
			}
			appl, still := applyWhenSeen(seenTables, remaining)
			remaining = still
			var pred parser.Expr
			if len(appl) > 0 {
				pred = combineAND(appl)
			}
			rows = inlineJoin(rows, vu.rows, pred)
		}

		// Apply any remaining conditions (shouldn't normally happen).
		if len(remaining) > 0 {
			pred := combineAND(remaining)
			var filtered []storage.Row
			for _, row := range rows {
				val, _ := e.evalExpr(pred, row)
				if toBool(val) {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
		}

		// Also cross-join with any JOIN chains within FROM entries (mixed comma+JOIN syntax).
		// We cannot use executeJoins here because the left rows already have qualified keys
		// from the isMultiTable cross-join; re-aliasing the left side would corrupt them.
		for _, tref := range stmt.From {
			join := tref.Join
			for join != nil && join.Table != nil {
				rightRef := join.Table
				rightRows, rerr := e.session.Select(rightRef.Name, nil)
				if rerr != nil {
					return nil, rerr
				}
				rightAlias := rightRef.Alias
				if rightAlias == "" {
					rightAlias = rightRef.Name
				}
				rightSchema, _ := e.schema.GetSchema(rightRef.Name)
				for j := range rightRows {
					normalizeRowBySchema(rightRows[j], rightSchema)
					rightRows[j] = e.addTableAlias(rightRows[j], rightAlias)
				}
				var joined []storage.Row
				for _, l := range rows {
					for _, r := range rightRows {
						m := make(storage.Row, len(l)+len(r))
						for k, v := range l {
							m[k] = v
						}
						for k, v := range r {
							if _, exists := m[k]; !exists {
								m[k] = v
							} else if strings.Contains(k, ".") {
								m[k] = v // qualified keys from right always win
							}
						}
						if join.Condition != nil {
							val, _ := e.evalExpr(join.Condition, m)
							if !toBool(val) {
								continue
							}
						}
						joined = append(joined, m)
					}
				}
				rows = joined
				join = rightRef.Join
			}
		}
	}

	// Handle GROUP BY
	if len(stmt.GroupBy) > 0 {
		return e.executeGroupBy(stmt, rows, schema)
	}

	// Check for aggregate functions without GROUP BY
	hasAggregate := e.hasAggregates(stmt.Columns)
	if hasAggregate {
		return e.executeAggregateSelect(stmt, rows, schema)
	}

	// Apply ORDER BY, LIMIT, and OFFSET.
	rows = e.orderAndLimitRows(rows, stmt.OrderBy, stmt.Limit, stmt.Offset, stmt.Columns)

	// Build result
	result := NewResult("SELECT")

	// For multi-table or JOIN queries, collect all table refs for SELECT * expansion.
	hasJoin := len(stmt.From) > 0 && stmt.From[0].Join != nil
	allTableRefs := collectAllTableRefs(stmt.From)

	// Resolve qualified wildcard (table.*) projections once, keyed by qualifier.
	type starProjection struct {
		cols   []storage.Column
		prefix string
	}
	tableStars := make(map[string]starProjection)
	for _, col := range stmt.Columns {
		if col.TableStar == "" {
			continue
		}
		key := strings.ToUpper(col.TableStar)
		if _, ok := tableStars[key]; ok {
			continue
		}
		cols, prefix, err := e.resolveTableStar(stmt.From, col.TableStar)
		if err != nil {
			return nil, err
		}
		tableStars[key] = starProjection{cols: cols, prefix: prefix}
	}

	// Determine columns
	for i, col := range stmt.Columns {
		if col.Alias != "" {
			result.AddColumn(col.Alias)
		} else if ref, ok := col.Expr.(*parser.ColumnRef); ok {
			result.AddColumn(ref.Column)
		} else if col.Star {
			if isMultiTable || hasJoin {
				// Add columns from ALL joined tables in order
				for _, tref := range allTableRefs {
					sch, _ := e.schema.GetSchema(tref.Name)
					if sch != nil {
						for _, c := range sch.Columns {
							result.AddColumn(c.Name)
						}
					}
				}
			} else {
				// Handle SELECT * - add all columns from schema
				for _, c := range schema.Columns {
					result.AddColumn(c.Name)
				}
			}
		} else if col.TableStar != "" {
			proj := tableStars[strings.ToUpper(col.TableStar)]
			for _, c := range proj.cols {
				result.AddColumn(c.Name)
			}
		} else {
			result.AddColumn(fmt.Sprintf("column%d", i+1))
		}
	}

	// Populate column types from schema metadata for single-table projections so
	// protocol consumers can decode typed values (e.g. timestamps) even when the
	// result set is empty. Join and multi-table projections defer type metadata.
	if !isMultiTable && !hasJoin {
		result.ColumnTypes = selectColumnTypes(stmt, schema)
	}

	// Window functions in the projection are evaluated over the scanned rows
	// before their per-row values are read.
	windowValues, werr := e.computeWindowValues(stmt, rows)
	if werr != nil {
		return nil, werr
	}

	// Add rows - evaluate each select expression
	for rowIdx, row := range rows {
		values := make([]interface{}, 0)
		for _, col := range stmt.Columns {
			if col.Star {
				if isMultiTable || hasJoin {
					// For multi-table SELECT *, extract columns using qualified names
					for _, tref := range allTableRefs {
						sch, _ := e.schema.GetSchema(tref.Name)
						if sch != nil {
							for _, c := range sch.Columns {
								qualKey := tref.Alias + "." + c.Name
								val, ok := row[qualKey]
								if !ok {
									val = row[c.Name]
								}
								values = append(values, val)
							}
						}
					}
				} else {
					// For SELECT *, add all columns in order. A real column named
					// oid/rowid/_rowid_ is an ordinary column here, not the hidden
					// rowid alias, so use its own value key.
					for _, c := range schema.Columns {
						values = append(values, row[c.Name])
					}
				}
			} else if col.TableStar != "" {
				// Qualified wildcard (table.*): emit only that table's columns,
				// resolving values via the effective alias, falling back to the
				// unqualified key for single-table queries without an alias.
				proj := tableStars[strings.ToUpper(col.TableStar)]
				for _, c := range proj.cols {
					val, ok := row[proj.prefix+"."+c.Name]
					if !ok {
						val = row[c.Name]
					}
					values = append(values, val)
				}
			} else if we, ok := col.Expr.(*parser.WindowExpr); ok {
				values = append(values, windowValues[we][rowIdx])
			} else {
				// Evaluate the expression
				val, err := e.evalExpr(col.Expr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, val)
			}
		}
		result.AddRow(values...)
	}

	// Apply DISTINCT if specified
	if stmt.Distinct {
		result.Rows = e.applyDistinct(result.Rows)
		result.RowCount = len(result.Rows)
	}

	return result, nil
}

// executeCompound executes a compound SELECT (UNION / UNION ALL / INTERSECT / EXCEPT).
func (e *Executor) executeCompound(c *parser.CompoundSelect) (*Result, error) {
	left, err := e.executeSelect(c.Left)
	if err != nil {
		return nil, err
	}
	right, err := e.executeSelect(c.Right)
	if err != nil {
		return nil, err
	}

	rowKey := func(row []interface{}) string {
		parts := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				parts[i] = "\x00NULL"
			} else {
				parts[i] = fmt.Sprintf("%v", v)
			}
		}
		return strings.Join(parts, "\x01")
	}

	result := NewResult("SELECT")
	for _, col := range left.Columns {
		result.AddColumn(col)
	}

	switch c.Op {
	case parser.SetOpUnion:
		seen := map[string]bool{}
		for _, row := range left.Rows {
			k := rowKey(row)
			if !seen[k] {
				seen[k] = true
				result.AddRow(row...)
			}
		}
		for _, row := range right.Rows {
			k := rowKey(row)
			if !seen[k] {
				seen[k] = true
				result.AddRow(row...)
			}
		}
	case parser.SetOpUnionAll:
		for _, row := range left.Rows {
			result.AddRow(row...)
		}
		for _, row := range right.Rows {
			result.AddRow(row...)
		}
	case parser.SetOpIntersect:
		rightSet := map[string]bool{}
		for _, row := range right.Rows {
			rightSet[rowKey(row)] = true
		}
		seen := map[string]bool{}
		for _, row := range left.Rows {
			k := rowKey(row)
			if rightSet[k] && !seen[k] {
				seen[k] = true
				result.AddRow(row...)
			}
		}
	case parser.SetOpExcept:
		rightSet := map[string]bool{}
		for _, row := range right.Rows {
			rightSet[rowKey(row)] = true
		}
		seen := map[string]bool{}
		for _, row := range left.Rows {
			k := rowKey(row)
			if !rightSet[k] && !seen[k] {
				seen[k] = true
				result.AddRow(row...)
			}
		}
	}

	// Apply compound-level ORDER BY / LIMIT / OFFSET if present.
	if len(c.OrderBy) > 0 {
		e.sortResultRows(result, c.OrderBy, nil, nil)
	}
	if c.Limit != nil {
		limitVal, err := e.evalExpr(c.Limit, nil)
		if err == nil {
			limit := int(toFloat(limitVal))
			if limit < len(result.Rows) {
				result.Rows = result.Rows[:limit]
			}
		}
	}
	if c.Offset != nil {
		offsetVal, err := e.evalExpr(c.Offset, nil)
		if err == nil {
			offset := int(toFloat(offsetVal))
			if offset >= len(result.Rows) {
				result.Rows = nil
			} else if offset > 0 {
				result.Rows = result.Rows[offset:]
			}
		}
	}

	return result, nil
}

// executeSelectExpr executes a SELECT without FROM.
func (e *Executor) executeSelectExpr(stmt *parser.SelectStmt) (*Result, error) {
	// If any column contains an aggregate, treat as single-group aggregate over one implicit row.
	if e.hasAggregates(stmt.Columns) {
		return e.executeAggregateSelect(stmt, []storage.Row{{}}, nil)
	}

	result := NewResult("SELECT")

	// Determine columns
	for i, col := range stmt.Columns {
		if col.Alias != "" {
			result.AddColumn(col.Alias)
		} else {
			result.AddColumn(fmt.Sprintf("column%d", i+1))
		}
	}

	// Evaluate expressions
	values := make([]interface{}, len(stmt.Columns))
	for i, col := range stmt.Columns {
		val, err := e.evalExpr(col.Expr, nil)
		if err != nil {
			return nil, err
		}
		values[i] = val
	}
	result.AddRow(values...)

	return result, nil
}

// executeSelectFromSubquery executes a SELECT with a subquery in FROM clause.
func (e *Executor) executeSelectFromSubquery(stmt *parser.SelectStmt) (*Result, error) {
	// Execute the subquery to get the derived table
	subqueryResult, err := e.executeSelect(stmt.From[0].Subquery)
	if err != nil {
		return nil, fmt.Errorf("subquery error: %w", err)
	}

	return e.executeSelectOnMaterialized(stmt, subqueryResult.Columns, subqueryResult.Rows)
}

// executeSelectOnMaterialized runs a SELECT whose FROM[0] is already
// materialized as (columns, rowValues). It is shared by derived tables and
// materialized CTEs.
func (e *Executor) executeSelectOnMaterialized(stmt *parser.SelectStmt, columns []string, rowValues [][]interface{}) (*Result, error) {
	derivedRows := make([]storage.Row, 0, len(rowValues))
	for _, values := range rowValues {
		row := make(storage.Row, len(columns))
		for i, col := range columns {
			if i < len(values) {
				row[col] = values[i]
			}
		}
		derivedRows = append(derivedRows, row)
	}

	// Handle JOINs if present
	if stmt.From[0].Join != nil {
		var err error
		derivedRows, err = e.executeJoin(stmt.From[0], derivedRows)
		if err != nil {
			return nil, err
		}
	}

	// Apply WHERE clause on derived table
	if stmt.Where != nil {
		filteredRows := make([]storage.Row, 0)
		for _, row := range derivedRows {
			val, err := e.evalExpr(stmt.Where, row)
			if err != nil {
				continue
			}
			if toBool(val) {
				filteredRows = append(filteredRows, row)
			}
		}
		derivedRows = filteredRows
	}

	// Handle GROUP BY
	if len(stmt.GroupBy) > 0 {
		return e.executeGroupBy(stmt, derivedRows, schemaFromColumns(columns))
	}

	// Check for aggregate functions without GROUP BY
	if e.hasAggregates(stmt.Columns) {
		return e.executeAggregateSelect(stmt, derivedRows, schemaFromColumns(columns))
	}

	// Apply ORDER BY, LIMIT, and OFFSET.
	derivedRows = e.orderAndLimitRows(derivedRows, stmt.OrderBy, stmt.Limit, stmt.Offset, stmt.Columns)

	// Build result. A projection may mix * and expressions, so each column is
	// expanded independently.
	result := NewResult("SELECT")
	for _, col := range stmt.Columns {
		switch {
		case col.Star:
			for _, c := range columns {
				result.AddColumn(c)
			}
		case col.Alias != "":
			result.AddColumn(col.Alias)
		case col.Expr != nil:
			if colRef, ok := col.Expr.(*parser.ColumnRef); ok {
				result.AddColumn(colRef.Column)
			} else {
				result.AddColumn("column")
			}
		default:
			result.AddColumn("column")
		}
	}

	// Window functions in the projection are evaluated over the materialized
	// rows before the values are read.
	windowValues, err := e.computeWindowValues(stmt, derivedRows)
	if err != nil {
		return nil, err
	}

	for rowIdx, row := range derivedRows {
		values := make([]interface{}, 0, len(stmt.Columns))
		for _, col := range stmt.Columns {
			switch {
			case col.Star:
				for _, c := range columns {
					values = append(values, row[c])
				}
			case col.Expr != nil:
				if we, ok := col.Expr.(*parser.WindowExpr); ok {
					values = append(values, windowValues[we][rowIdx])
					continue
				}
				val, err := e.evalExpr(col.Expr, row)
				if err != nil {
					return nil, err
				}
				values = append(values, val)
			}
		}
		result.AddRow(values...)
	}

	return result, nil
}

// schemaFromColumns builds a schema with ANY-typed columns for materialized
// derived tables and CTEs.
func schemaFromColumns(columns []string) *storage.Schema {
	schema := &storage.Schema{Name: "derived", Columns: make([]storage.Column, len(columns))}
	for i, col := range columns {
		schema.Columns[i] = storage.Column{Name: col, Type: "ANY"}
	}
	return schema
}

// executeAggregateSelect executes a SELECT with aggregate functions.
func (e *Executor) executeAggregateSelect(stmt *parser.SelectStmt, rows []storage.Row, schema *storage.Schema) (*Result, error) {
	result := NewResult("SELECT")

	// Determine columns and evaluate aggregates
	for i, col := range stmt.Columns {
		if col.Alias != "" {
			result.AddColumn(col.Alias)
		} else if col.Star {
			result.AddColumn("*")
		} else {
			result.AddColumn(fmt.Sprintf("column%d", i+1))
		}
	}

	// Calculate values
	values := make([]interface{}, len(stmt.Columns))
	for i, col := range stmt.Columns {
		val, err := e.evalAggregateExpr(col.Expr, rows)
		if err != nil {
			return nil, err
		}
		values[i] = val
	}
	result.AddRow(values...)

	return result, nil
}

// executeGroupBy executes a GROUP BY query.
func (e *Executor) executeGroupBy(stmt *parser.SelectStmt, rows []storage.Row, schema *storage.Schema) (*Result, error) {
	result := NewResult("SELECT")

	// Expand SELECT * if present
	expandedColumns := make([]parser.SelectColumn, 0, len(stmt.Columns))
	for _, col := range stmt.Columns {
		if col.Star {
			for _, c := range schema.Columns {
				expandedColumns = append(expandedColumns, parser.SelectColumn{
					Expr: &parser.ColumnRef{Column: c.Name},
				})
			}
		} else if col.TableStar != "" {
			// Qualified wildcard: expand only the named table's columns.
			cols, prefix, err := e.resolveTableStar(stmt.From, col.TableStar)
			if err != nil {
				return nil, err
			}
			for _, c := range cols {
				expandedColumns = append(expandedColumns, parser.SelectColumn{
					Expr: &parser.ColumnRef{Table: prefix, Column: c.Name},
				})
			}
		} else {
			expandedColumns = append(expandedColumns, col)
		}
	}

	// Determine column names
	columnNames := make([]string, len(expandedColumns))
	for i, col := range expandedColumns {
		if col.Alias != "" {
			columnNames[i] = col.Alias
			result.AddColumn(col.Alias)
		} else if ref, ok := col.Expr.(*parser.ColumnRef); ok {
			columnNames[i] = ref.Column
			result.AddColumn(ref.Column)
		} else {
			columnNames[i] = fmt.Sprintf("column%d", i+1)
			result.AddColumn(columnNames[i])
		}
	}

	// Fast path: use running accumulators instead of collecting rows per group.
	// Applicable when there is no HAVING clause and all aggregate SELECT columns
	// are direct FunctionCalls (COUNT/SUM/AVG/MIN/MAX).
	if e.canUseGroupAccum(stmt, expandedColumns) {
		return e.executeGroupByAccum(stmt, rows, result, expandedColumns, columnNames)
	}

	// Slow path: collect full rows per group then evaluate aggregates over them.
	groups := make(map[string][]storage.Row)
	for _, row := range rows {
		key := e.buildGroupKey(stmt.GroupBy, row)
		groups[key] = append(groups[key], row)
	}

	for _, groupRows := range groups {
		if stmt.Having != nil {
			val, err := e.evalAggregateExpr(stmt.Having, groupRows)
			if err != nil || val == nil || !toBool(val) {
				continue
			}
		}
		values := make([]interface{}, len(expandedColumns))
		for i, col := range expandedColumns {
			if e.isAggregate(col.Expr) {
				val, err := e.evalAggregateExpr(col.Expr, groupRows)
				if err != nil {
					return nil, err
				}
				values[i] = val
			} else {
				val, err := e.evalExpr(col.Expr, groupRows[0])
				if err != nil {
					return nil, err
				}
				values[i] = val
			}
		}
		result.AddRow(values...)
	}

	return e.finalizeGroupResult(stmt, result, expandedColumns, columnNames)
}

// canUseGroupAccum returns true when the fast accumulator path can handle the query.
func (e *Executor) canUseGroupAccum(stmt *parser.SelectStmt, expandedColumns []parser.SelectColumn) bool {
	if stmt.Having != nil {
		return false
	}
	for _, col := range expandedColumns {
		if !e.isAggregate(col.Expr) {
			continue
		}
		fn, ok := col.Expr.(*parser.FunctionCall)
		if !ok {
			return false
		}
		switch strings.ToUpper(fn.Name) {
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
		default:
			return false
		}
	}
	return true
}

// aggColInfo pairs a SELECT column index with its aggregate FunctionCall.
type aggColInfo struct {
	colIdx int
	fn     *parser.FunctionCall
}

// aggAccum holds running state for a single aggregate function.
type aggAccum struct {
	count   int64
	sumI    int64
	sumF    float64
	allInt  bool
	hasVal  bool
	extreme interface{}
	seen    map[interface{}]struct{} // for DISTINCT
}

// groupAccumState holds per-group state for the fast accumulator path.
type groupAccumState struct {
	firstRow storage.Row
	accums   []*aggAccum
}

// executeGroupByAccum is the fast GROUP BY path: increments per-group counters as rows
// arrive rather than materialising row slices, keeping O(1) state per group.
func (e *Executor) executeGroupByAccum(stmt *parser.SelectStmt, rows []storage.Row, result *Result, expandedColumns []parser.SelectColumn, columnNames []string) (*Result, error) {
	var aggCols []aggColInfo
	for i, col := range expandedColumns {
		if e.isAggregate(col.Expr) {
			aggCols = append(aggCols, aggColInfo{i, col.Expr.(*parser.FunctionCall)})
		}
	}

	states := make(map[string]*groupAccumState, 64)
	var keyOrder []string

	for _, row := range rows {
		key := e.buildGroupKey(stmt.GroupBy, row)
		state, exists := states[key]
		if !exists {
			accums := make([]*aggAccum, len(aggCols))
			for j, ac := range aggCols {
				a := &aggAccum{allInt: true}
				if ac.fn.Distinct {
					a.seen = make(map[interface{}]struct{})
				}
				accums[j] = a
			}
			state = &groupAccumState{firstRow: row, accums: accums}
			states[key] = state
			keyOrder = append(keyOrder, key)
		}
		for j, ac := range aggCols {
			e.feedAggAccum(state.accums[j], ac.fn, row)
		}
	}

	for _, key := range keyOrder {
		state := states[key]
		values := make([]interface{}, len(expandedColumns))
		for i, col := range expandedColumns {
			if e.isAggregate(col.Expr) {
				for j, ac := range aggCols {
					if ac.colIdx == i {
						values[i] = finalizeAggAccum(state.accums[j], ac.fn)
						break
					}
				}
			} else {
				val, _ := e.evalExpr(col.Expr, state.firstRow)
				values[i] = val
			}
		}
		result.AddRow(values...)
	}

	return e.finalizeGroupResult(stmt, result, expandedColumns, columnNames)
}

// feedAggAccum updates a running accumulator with one row.
func (e *Executor) feedAggAccum(a *aggAccum, fn *parser.FunctionCall, row storage.Row) {
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		if fn.Star {
			a.count++
			return
		}
		if len(fn.Args) == 0 {
			return
		}
		val, _ := e.evalExpr(fn.Args[0], row)
		if val == nil {
			return
		}
		if fn.Distinct {
			k := fmt.Sprintf("%v", val)
			if _, exists := a.seen[k]; exists {
				return
			}
			a.seen[k] = struct{}{}
		}
		a.count++

	case "SUM":
		if len(fn.Args) == 0 {
			return
		}
		val, _ := e.evalExpr(fn.Args[0], row)
		if val == nil {
			return
		}
		if fn.Distinct {
			k := fmt.Sprintf("%v", val)
			if _, exists := a.seen[k]; exists {
				return
			}
			a.seen[k] = struct{}{}
		}
		if isIntVal(val) {
			a.sumI += toInt64(val)
		} else {
			a.allInt = false
			a.sumF += toFloat(val)
		}
		a.hasVal = true

	case "AVG":
		if len(fn.Args) == 0 {
			return
		}
		val, _ := e.evalExpr(fn.Args[0], row)
		if val == nil {
			return
		}
		a.sumF += toFloat(val)
		a.count++
		a.hasVal = true

	case "MIN":
		if len(fn.Args) == 0 {
			return
		}
		val, _ := e.evalExpr(fn.Args[0], row)
		if val != nil && (a.extreme == nil || compare(val, a.extreme) < 0) {
			a.extreme = val
		}

	case "MAX":
		if len(fn.Args) == 0 {
			return
		}
		val, _ := e.evalExpr(fn.Args[0], row)
		if val != nil && (a.extreme == nil || compare(val, a.extreme) > 0) {
			a.extreme = val
		}
	}
}

// finalizeAggAccum computes the final aggregate value from a running accumulator.
func finalizeAggAccum(a *aggAccum, fn *parser.FunctionCall) interface{} {
	switch strings.ToUpper(fn.Name) {
	case "COUNT":
		return a.count
	case "SUM":
		if !a.hasVal {
			return nil
		}
		if a.allInt {
			return a.sumI
		}
		return a.sumF + float64(a.sumI)
	case "AVG":
		if !a.hasVal || a.count == 0 {
			return nil
		}
		return a.sumF / float64(a.count)
	case "MIN", "MAX":
		return a.extreme
	}
	return nil
}

// finalizeGroupResult applies DISTINCT, ORDER BY, and LIMIT/OFFSET to a GROUP BY result.
func (e *Executor) finalizeGroupResult(stmt *parser.SelectStmt, result *Result, expandedColumns []parser.SelectColumn, columnNames []string) (*Result, error) {
	if stmt.Distinct {
		result.Rows = e.applyDistinct(result.Rows)
	}
	e.orderAndLimitResultRows(result, stmt.OrderBy, stmt.Limit, stmt.Offset, expandedColumns, columnNames)
	return result, nil
}

// executeJoins recursively processes all JOIN clauses in a table reference.
func (e *Executor) executeJoins(tableRef parser.TableRef, leftRows []storage.Row) ([]storage.Row, error) {
	return e.executeJoinsWithMode(tableRef, leftRows, true)
}

func (e *Executor) executeJoinsWithMode(tableRef parser.TableRef, leftRows []storage.Row, qualifyLeft bool) ([]storage.Row, error) {
	if tableRef.Join == nil || tableRef.Join.Table == nil {
		return leftRows, nil
	}

	// Get the right table name. Equality joins against its primary key can probe
	// only the referenced rows instead of scanning the whole table.
	rightTableRef := tableRef.Join.Table
	rightTable := rightTableRef.Name

	// Perform the join between left and right
	var result []storage.Row
	leftTableName := tableRef.Name
	leftAlias := tableRef.Alias
	rightAlias := rightTableRef.Alias

	// If leftAlias is empty, use the table name
	if leftAlias == "" {
		leftAlias = leftTableName
	}
	if rightAlias == "" {
		rightAlias = rightTable
	}
	leftAliasForMerge := leftAlias
	if !qualifyLeft {
		leftAliasForMerge = ""
	}

	// Build a synthetic TableRef so we can reuse extractEqualityJoinKeys.
	syntheticLeft := parser.TableRef{Name: leftTableName, Alias: leftAlias}
	syntheticJoin := &parser.JoinClause{
		Type:      tableRef.Join.Type,
		Table:     &parser.TableRef{Name: rightTable, Alias: rightAlias},
		Condition: tableRef.Join.Condition,
	}
	leftKey, rightKey, canHash := extractEqualityJoinKeys(tableRef.Join.Condition, syntheticLeft, syntheticJoin)
	var rightRows []storage.Row
	var rightSchema *storage.Schema
	var err error
	switch {
	case rightTableRef.Subquery != nil:
		rightRows, rightSchema, err = e.materializeJoinSubquery(rightTableRef)
		if err != nil {
			return nil, err
		}
	case e.cteTableExists(rightTable):
		cte, _ := e.cteTableFor(rightTable)
		rightRows = cloneRows(cte.rows)
		rightSchema = schemaFromColumns(cte.columns)
	default:
		rightSchema, err = e.schema.GetSchema(rightTable)
		if err != nil {
			return nil, err
		}
		if canHash && strings.EqualFold(rightSchema.PrimaryKey, rightKey) && len(leftRows) <= 256 {
			seen := make(map[string]bool, len(leftRows))
			for _, left := range leftRows {
				key := joinKeyString(left, leftKey)
				if key == "\x00" || seen[key] {
					continue
				}
				seen[key] = true
				row, getErr := e.session.GetByPK(rightTable, key)
				if getErr == storage.ErrKeyNotFound {
					continue
				}
				if getErr != nil {
					return nil, getErr
				}
				normalizeRowBySchema(row, rightSchema)
				rightRows = append(rightRows, row)
			}
		} else {
			rightRows, err = e.session.Select(rightTable, nil)
			if err != nil {
				return nil, err
			}
			for _, row := range rightRows {
				normalizeRowBySchema(row, rightSchema)
			}
		}
	}

	switch tableRef.Join.Type {
	case parser.JoinInner:
		if canHash {
			hashTable := make(map[string][]storage.Row, len(rightRows))
			for _, right := range rightRows {
				k := joinKeyString(right, rightKey)
				hashTable[k] = append(hashTable[k], right)
			}
			for _, left := range leftRows {
				k := joinKeyString(left, leftKey)
				for _, right := range hashTable[k] {
					result = append(result, e.mergeRows(left, right, leftAliasForMerge, rightAlias))
				}
			}
		} else {
			for _, left := range leftRows {
				for _, right := range rightRows {
					merged := e.mergeRows(left, right, leftAliasForMerge, rightAlias)
					if tableRef.Join.Condition != nil {
						match, _ := e.evalExpr(tableRef.Join.Condition, merged)
						if toBool(match) {
							result = append(result, merged)
						}
					} else {
						result = append(result, merged)
					}
				}
			}
		}

	case parser.JoinLeft:
		if canHash {
			hashTable := make(map[string][]storage.Row, len(rightRows))
			for _, right := range rightRows {
				k := joinKeyString(right, rightKey)
				hashTable[k] = append(hashTable[k], right)
			}
			nullRight := makeNullRow(rightRows, rightTable, e)
			for _, left := range leftRows {
				k := joinKeyString(left, leftKey)
				matches := hashTable[k]
				if len(matches) == 0 {
					result = append(result, e.mergeRows(left, nullRight, leftAliasForMerge, rightAlias))
				} else {
					for _, right := range matches {
						result = append(result, e.mergeRows(left, right, leftAliasForMerge, rightAlias))
					}
				}
			}
		} else {
			for _, left := range leftRows {
				matched := false
				for _, right := range rightRows {
					merged := e.mergeRows(left, right, leftAliasForMerge, rightAlias)
					if tableRef.Join.Condition != nil {
						match, _ := e.evalExpr(tableRef.Join.Condition, merged)
						if toBool(match) {
							result = append(result, merged)
							matched = true
						}
					}
				}
				if !matched {
					nullRight := makeNullRow(rightRows, rightTable, e)
					result = append(result, e.mergeRows(left, nullRight, leftAliasForMerge, rightAlias))
				}
			}
		}

	case parser.JoinCross:
		for _, left := range leftRows {
			for _, right := range rightRows {
				result = append(result, e.mergeRows(left, right, leftAliasForMerge, rightAlias))
			}
		}
	}

	// Recursively process any additional joins
	if rightTableRef.Join != nil {
		return e.executeJoinsWithMode(*rightTableRef, result, false)
	}

	return result, nil
}

// executeJoin executes a JOIN operation.
func (e *Executor) executeJoin(tableRef parser.TableRef, leftRows []storage.Row) ([]storage.Row, error) {
	join := tableRef.Join
	if join == nil || join.Table == nil {
		return leftRows, nil
	}

	rightTable := join.Table.Name
	var rightRows []storage.Row
	var err error
	switch {
	case join.Table.Subquery != nil:
		rightRows, _, err = e.materializeJoinSubquery(join.Table)
	case e.cteTableExists(rightTable):
		cte, _ := e.cteTableFor(rightTable)
		rightRows = cloneRows(cte.rows)
	default:
		rightRows, err = e.session.Select(rightTable, nil)
	}
	if err != nil {
		return nil, err
	}

	var result []storage.Row

	switch join.Type {
	case parser.JoinInner:
		leftKey, rightKey, canHash := extractEqualityJoinKeys(join.Condition, tableRef, join)
		if canHash {
			// Hash join: build phase on right, probe phase on left — O(N+M) vs O(N*M)
			hashTable := make(map[string][]storage.Row, len(rightRows))
			for _, right := range rightRows {
				k := joinKeyString(right, rightKey)
				hashTable[k] = append(hashTable[k], right)
			}
			for _, left := range leftRows {
				k := joinKeyString(left, leftKey)
				for _, right := range hashTable[k] {
					result = append(result, e.mergeRows(left, right, tableRef.Alias, join.Table.Alias))
				}
			}
		} else {
			for _, left := range leftRows {
				for _, right := range rightRows {
					merged := e.mergeRows(left, right, tableRef.Alias, join.Table.Alias)
					if join.Condition != nil {
						match, _ := e.evalExpr(join.Condition, merged)
						if toBool(match) {
							result = append(result, merged)
						}
					} else {
						result = append(result, merged)
					}
				}
			}
		}

	case parser.JoinLeft:
		leftKey, rightKey, canHash := extractEqualityJoinKeys(join.Condition, tableRef, join)
		if canHash {
			hashTable := make(map[string][]storage.Row, len(rightRows))
			for _, right := range rightRows {
				k := joinKeyString(right, rightKey)
				hashTable[k] = append(hashTable[k], right)
			}
			nullRight := makeNullRow(rightRows, rightTable, e)
			for _, left := range leftRows {
				k := joinKeyString(left, leftKey)
				matches := hashTable[k]
				if len(matches) == 0 {
					result = append(result, e.mergeRows(left, nullRight, tableRef.Alias, join.Table.Alias))
				} else {
					for _, right := range matches {
						result = append(result, e.mergeRows(left, right, tableRef.Alias, join.Table.Alias))
					}
				}
			}
		} else {
			for _, left := range leftRows {
				matched := false
				for _, right := range rightRows {
					merged := e.mergeRows(left, right, tableRef.Alias, join.Table.Alias)
					if join.Condition != nil {
						match, _ := e.evalExpr(join.Condition, merged)
						if toBool(match) {
							result = append(result, merged)
							matched = true
						}
					}
				}
				if !matched {
					nullRight := makeNullRow(rightRows, rightTable, e)
					result = append(result, e.mergeRows(left, nullRight, tableRef.Alias, join.Table.Alias))
				}
			}
		}

	case parser.JoinCross:
		for _, left := range leftRows {
			for _, right := range rightRows {
				result = append(result, e.mergeRows(left, right, tableRef.Alias, join.Table.Alias))
			}
		}
	}

	return result, nil
}

// extractEqualityJoinKeys checks if a JOIN condition is a simple col = col equality
// and returns the key names to probe in left rows and build from right rows.
func extractEqualityJoinKeys(condition parser.Expr, leftRef parser.TableRef, join *parser.JoinClause) (leftKey, rightKey string, ok bool) {
	if condition == nil {
		return "", "", false
	}
	bin, isBin := condition.(*parser.BinaryExpr)
	if !isBin || bin.Op != lexer.TokenEq {
		return "", "", false
	}
	lRef, leftIsCol := bin.Left.(*parser.ColumnRef)
	rRef, rightIsCol := bin.Right.(*parser.ColumnRef)
	if !leftIsCol || !rightIsCol {
		return "", "", false
	}

	leftAlias := leftRef.Alias
	leftName := leftRef.Name
	rightAlias := join.Table.Alias
	rightName := join.Table.Name

	leftJoinKey := func(r *parser.ColumnRef) (string, bool) {
		if r.Table == "" || r.Table == leftAlias || r.Table == leftName {
			return r.Column, true
		}
		// In a chained explicit JOIN, the left row already contains every table
		// joined so far. Preserve qualified references such as "o.id" so joins
		// against earlier tables can still use the hash path.
		if r.Table != rightAlias && r.Table != rightName {
			return r.Table + "." + r.Column, true
		}
		return "", false
	}
	rightJoinKey := func(r *parser.ColumnRef) (string, bool) {
		if r.Table == "" || r.Table == rightAlias || r.Table == rightName {
			return r.Column, true
		}
		return "", false
	}

	if lk, leftOK := leftJoinKey(lRef); leftOK {
		if rk, rightOK := rightJoinKey(rRef); rightOK {
			return lk, rk, true
		}
	}
	if lk, leftOK := leftJoinKey(rRef); leftOK {
		if rk, rightOK := rightJoinKey(lRef); rightOK {
			return lk, rk, true
		}
	}
	return "", "", false
}

// joinKeyString returns a string representation of a row's join key for hashing.
func joinKeyString(row storage.Row, col string) string {
	if v, ok := row[col]; ok {
		return fmt.Sprintf("%v", v)
	}
	return "\x00"
}

// makeNullRow builds a null-valued row based on the right table's rows or schema.
func makeNullRow(rightRows []storage.Row, rightTable string, e *Executor) storage.Row {
	nullRight := make(storage.Row)
	if len(rightRows) > 0 {
		for k := range rightRows[0] {
			nullRight[k] = nil
		}
	} else {
		rightSchema, err := e.schema.GetSchema(rightTable)
		if err == nil {
			for _, col := range rightSchema.Columns {
				nullRight[col.Name] = nil
			}
		}
	}
	return nullRight
}

// mergeRows merges two rows with optional table aliases.
func (e *Executor) mergeRows(left, right storage.Row, leftAlias, rightAlias string) storage.Row {
	result := make(storage.Row)
	for k, v := range left {
		// Copy the key as-is (it might already be qualified)
		result[k] = v
		// Only add qualified name if the key is NOT already qualified and we have an alias
		if leftAlias != "" && !strings.Contains(k, ".") {
			result[leftAlias+"."+k] = v
		}
	}
	for k, v := range right {
		// For unqualified names, only add if they don't already exist
		// This prevents right table columns from overwriting left table columns
		if !strings.Contains(k, ".") {
			if _, exists := result[k]; !exists {
				result[k] = v
			}
			// Add qualified name for right table
			if rightAlias != "" {
				result[rightAlias+"."+k] = v
			}
		} else {
			// Already qualified, just copy it
			result[k] = v
		}
	}
	return result
}

// collectAllTableRefs returns a flat list of (alias, tableName) pairs for all tables
// referenced in a FROM clause, following both implicit (comma) and explicit JOIN chains.
func collectAllTableRefs(from []parser.TableRef) []parser.TableRef {
	var refs []parser.TableRef
	for _, tref := range from {
		cur := tref
		for {
			// Shallow copy to hold only this table (no join chain)
			flat := parser.TableRef{Name: cur.Name, Alias: cur.Alias}
			if flat.Alias == "" {
				flat.Alias = flat.Name
			}
			refs = append(refs, flat)
			if cur.Join == nil || cur.Join.Table == nil {
				break
			}
			cur = *cur.Join.Table
		}
	}
	return refs
}

// resolveTableStar resolves a qualified wildcard (table.*) qualifier to the
// matching table ref in a FROM clause. It returns the table's schema columns
// and the key prefix used to look values up in a joined/aliased row (the
// effective table alias). An unknown qualifier is an error, never a silent
// fallback to plain column expansion.
func (e *Executor) resolveTableStar(from []parser.TableRef, qualifier string) ([]storage.Column, string, error) {
	refs := collectAllTableRefs(from)
	for _, ref := range refs {
		if strings.EqualFold(ref.Alias, qualifier) {
			sch, err := e.schema.GetSchema(ref.Name)
			if err != nil {
				return nil, "", err
			}
			return sch.Columns, ref.Alias, nil
		}
	}
	for _, ref := range refs {
		if strings.EqualFold(ref.Name, qualifier) {
			sch, err := e.schema.GetSchema(ref.Name)
			if err != nil {
				return nil, "", err
			}
			return sch.Columns, ref.Alias, nil
		}
	}
	return nil, "", fmt.Errorf("no such table or alias: %s", qualifier)
}

// addTableAlias adds table-qualified names to a row.
// normalizeRowBySchema converts float64 values in integer-affinity columns to int64.
// This is needed because JSON deserialization always produces float64 for numbers.
func normalizeRowBySchema(row storage.Row, schema *storage.Schema) {
	if schema == nil {
		return
	}
	for _, col := range schema.Columns {
		upper := strings.ToUpper(col.Type)
		isInt := strings.Contains(upper, "INT") || upper == "BOOLEAN" || upper == "BOOL"
		if !isInt {
			continue
		}
		if f, ok := row[col.Name].(float64); ok {
			row[col.Name] = int64(f)
		}
	}
}

func (e *Executor) addTableAlias(row storage.Row, alias string) storage.Row {
	result := make(storage.Row)
	for k, v := range row {
		result[k] = v
		// Don't add alias to already-qualified names
		if !strings.Contains(k, ".") {
			result[alias+"."+k] = v
		}
	}
	return result
}

// isConflictError reports whether an insert failed because of a primary-key or
// unique-index conflict.
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "UNIQUE constraint failed")
}

// findConflictRow returns the first durable row whose target columns match the
// inserted row.
func (e *Executor) findConflictRow(table string, row storage.Row, columns []string) (storage.Row, bool) {
	rows, err := e.session.Select(table, func(existing storage.Row) bool {
		for _, col := range columns {
			if compare(existing[col], row[col]) != 0 {
				return false
			}
		}
		return true
	})
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
}

// applyUpsert implements ON CONFLICT (target) DO UPDATE SET ... for primary-key
// and unique-index conflicts, resolving excluded.<col> to the new row.
func (e *Executor) applyUpsert(table string, schema *storage.Schema, existing, row storage.Row, assignments []parser.Assignment) error {
	context := make(storage.Row)
	for k, v := range existing {
		context[k] = v
		context[table+"."+k] = v
	}
	for k, v := range row {
		context["excluded."+k] = v
	}

	pk := fmt.Sprintf("%v", existing[schema.PrimaryKey])
	_, updated, err := e.session.UpdateByPK(table, pk, func(storage.Row) (storage.Row, error) {
		updates := make(storage.Row)
		for _, assignment := range assignments {
			value, evalErr := e.evalExpr(assignment.Value, context)
			if evalErr != nil {
				return nil, evalErr
			}
			updates[assignment.Column] = value
		}
		return updates, nil
	})
	if err != nil {
		return err
	}
	if !updated {
		return fmt.Errorf("ON CONFLICT row disappeared during update")
	}
	return nil
}

// executeInsert executes an INSERT statement.
func (e *Executor) executeInsert(stmt *parser.InsertStmt) (*Result, error) {
	tableName := stmt.Table.Name
	schema, err := e.schema.GetSchema(tableName)
	if err != nil {
		return nil, err
	}

	// INSERT ... SELECT: materialise the SELECT result and bulk-insert.
	if stmt.Select != nil {
		sel, err := e.executeSelect(stmt.Select)
		if err != nil {
			return nil, err
		}
		rows := make([]storage.Row, 0, len(sel.Rows))
		for _, selRow := range sel.Rows {
			row := make(storage.Row)
			if len(stmt.Columns) > 0 {
				for i, col := range stmt.Columns {
					if i < len(selRow) {
						row[col] = selRow[i]
					}
				}
			} else {
				for i, col := range schema.Columns {
					if i < len(selRow) {
						row[col.Name] = selRow[i]
					}
				}
			}
			rows = append(rows, row)
		}
		var count int
		var lastRowID int64
		if e.inTransaction {
			for _, row := range rows {
				rid, err := e.session.InsertWithRowID(tableName, row)
				if err != nil {
					return nil, err
				}
				lastRowID = rid
				count++
			}
		} else {
			count, lastRowID, err = e.session.InsertBulkWithLastRowID(tableName, rows)
		}
		if err != nil {
			return nil, err
		}
		result := NewResult("INSERT")
		result.SetRowCount(count)
		result.SetLastInsertID(lastRowID)
		if count > 0 {
			e.lastInsertRowID = lastRowID
		}
		e.recordChanges(int64(count))
		return result, nil
	}

	count := 0
	var lastRowID int64
	err = e.runDMLAtomic(tableName, func() error {
		for _, values := range stmt.Values {
			row := make(storage.Row)

			if len(stmt.Columns) > 0 {
				// Named columns
				for i, col := range stmt.Columns {
					if i < len(values) {
						val, err := e.evalExpr(values[i], nil)
						if err != nil {
							return err
						}
						row[col] = val
					}
				}
			} else {
				// All columns in order
				for i, col := range schema.Columns {
					if i < len(values) {
						val, err := e.evalExpr(values[i], nil)
						if err != nil {
							return err
						}
						row[col.Name] = val
					}
				}
			}

			// ON CONFLICT is resolved before inserting. A unique-index conflict
			// may only surface at transaction commit, so the insert error is not
			// a reliable signal.
			if stmt.ConflictDoNothing || len(stmt.ConflictUpdate) > 0 {
				columns := stmt.ConflictTarget
				if len(columns) == 0 {
					columns = []string{schema.PrimaryKey}
				}
				if existing, found := e.findConflictRow(tableName, row, columns); found {
					if stmt.ConflictDoNothing {
						continue
					}
					if err := e.applyUpsert(tableName, schema, existing, row, stmt.ConflictUpdate); err != nil {
						return err
					}
					count++
					continue
				}
			}

			rowID, err := e.session.InsertWithRowID(tableName, row)
			if err != nil {
				// Handle conflict based on OnConflict action
				if isConflictError(err) {
					switch stmt.OnConflict {
					case parser.ConflictIgnore:
						// Silently ignore the duplicate
						continue
					case parser.ConflictReplace:
						// Delete existing row and insert new one
						pkValue := row[schema.PrimaryKey]
						if pkValue != nil {
							e.session.Delete(tableName, func(r storage.Row) bool {
								return fmt.Sprintf("%v", r[schema.PrimaryKey]) == fmt.Sprintf("%v", pkValue)
							})
							// Try insert again
							replacedID, replaceErr := e.session.InsertWithRowID(tableName, row)
							if replaceErr != nil {
								return replaceErr
							}
							lastRowID = replacedID
						}
					case parser.ConflictAbort, parser.ConflictFail:
						return err
					case parser.ConflictRollback:
						// In a real implementation, this would rollback the transaction
						return err
					default:
						return err
					}
				} else {
					return err
				}
			} else {
				lastRowID = rowID
			}
			count++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	result := NewResult("INSERT")
	result.SetRowCount(count)
	result.SetLastInsertID(lastRowID)
	if count > 0 {
		e.lastInsertRowID = lastRowID
	}
	e.recordChanges(int64(count))
	return result, nil
}

// recordChanges updates the session-local changes()/total_changes() state after
// a successful INSERT/UPDATE/DELETE. total_changes() is a monotonic counter of
// every completed DML statement (SQLite semantics): it is incremented even when
// the change is later undone by ROLLBACK or ROLLBACK TO, and is never decremented.
func (e *Executor) recordChanges(affected int64) {
	e.changes = affected
	e.totalChanges += affected
}

// runDMLAtomic runs apply, staging the DML in an implicit transaction when the
// target table carries a UNIQUE index and no explicit transaction is open, so a
// statement that fails partway (e.g. a multi-row UPDATE/INSERT that hits a unique
// violation after earlier rows staged) leaves no durable effects — matching
// SQLite's statement-level atomicity. It commits on success and rolls back on any
// error. When already in an explicit transaction, apply runs directly and the
// surrounding COMMIT/ROLLBACK governs durability.
func (e *Executor) runDMLAtomic(tableName string, apply func() error) error {
	if e.inTransaction {
		return apply()
	}
	uniq, err := e.table.HasUniqueIndex(tableName)
	if err != nil {
		return err
	}
	if !uniq {
		return apply()
	}
	if err := e.session.Begin(); err != nil {
		return err
	}
	if err := apply(); err != nil {
		_ = e.session.Rollback()
		return err
	}
	return e.session.Commit()
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// primeSubqueries pre-executes non-correlated subqueries referenced by a
// statement expression and stores their results in the per-query subquery cache.
// It runs before the storage session acquires its lock for a scan-based
// UPDATE/DELETE. Without this, evaluating a predicate such as
// "WHERE id IN (SELECT ...)" invokes the session recursively while the session
// lock is held, which deadlocks. The cache is only consulted when there is no
// outer row (see evalInExpr), so priming is limited to that same non-correlated
// case.
func (e *Executor) primeSubqueries(expr parser.Expr) {
	if expr == nil || e.subqueryCache == nil || e.outerRow != nil {
		return
	}
	prime := func(sub *parser.SelectStmt) {
		if sub == nil {
			return
		}
		if _, ok := e.subqueryCache[sub]; ok {
			return
		}
		if result, err := e.executeSelect(sub); err == nil {
			e.subqueryCache[sub] = result
		}
	}
	var walk func(parser.Expr)
	walk = func(x parser.Expr) {
		if x == nil {
			return
		}
		switch n := x.(type) {
		case *parser.InExpr:
			prime(n.Subquery)
			walk(n.Left)
			for _, v := range n.Values {
				walk(v)
			}
		case *parser.SubqueryExpr:
			prime(n.Query)
		case *parser.ExistsExpr:
			prime(n.Subquery)
		case *parser.BinaryExpr:
			walk(n.Left)
			walk(n.Right)
		case *parser.UnaryExpr:
			walk(n.Operand)
		case *parser.BetweenExpr:
			walk(n.Left)
			walk(n.Low)
			walk(n.High)
		case *parser.LikeExpr:
			walk(n.Left)
			walk(n.Pattern)
			walk(n.Escape)
		case *parser.IsNullExpr:
			walk(n.Left)
		case *parser.CaseExpr:
			walk(n.Operand)
			for _, w := range n.Whens {
				walk(w.Condition)
				walk(w.Result)
			}
			walk(n.Else)
		case *parser.FunctionCall:
			for _, a := range n.Args {
				walk(a)
			}
		case *parser.ParenExpr:
			walk(n.Expr)
		case *parser.CastExpr:
			walk(n.Expr)
		}
	}
	walk(expr)
}

// executeUpdate executes an UPDATE statement.
func (e *Executor) executeUpdate(stmt *parser.UpdateStmt) (*Result, error) {
	tableName := stmt.Table.Name
	schema, err := e.schema.GetSchema(tableName)
	if err != nil {
		return nil, err
	}

	// Build filter
	var filter func(storage.Row) bool
	if stmt.Where != nil {
		filter = func(row storage.Row) bool {
			val, err := e.evalExpr(stmt.Where, row)
			if err != nil {
				return false
			}
			return toBool(val)
		}
	}

	// Use UpdateFunc to evaluate expressions per-row (supports self-referencing like balance = balance + 100)
	updateFn := func(row storage.Row) (storage.Row, error) {
		updates := make(storage.Row)
		for _, assign := range stmt.Set {
			val, err := e.evalExpr(assign.Value, row)
			if err != nil {
				return nil, err
			}
			updates[assign.Column] = val
		}
		return updates, nil
	}
	if stmt.Where != nil {
		column, value, equality := e.extractIndexableCondition(stmt.Where)
		updatesPrimaryKey := false
		for _, assignment := range stmt.Set {
			if strings.EqualFold(assignment.Column, schema.PrimaryKey) {
				updatesPrimaryKey = true
				break
			}
		}
		if equality && strings.EqualFold(column, schema.PrimaryKey) && !updatesPrimaryKey {
			_, updated, err := e.session.UpdateByPK(tableName, fmt.Sprintf("%v", value), updateFn)
			if err != nil {
				return nil, err
			}
			count := 0
			if updated {
				count = 1
			}
			result := NewResult("UPDATE")
			result.SetRowCount(count)
			e.recordChanges(int64(count))
			return result, nil
		}
	}

	// Pre-evaluate subqueries in the predicate and assignments before the
	// scan-based update takes the session lock.
	e.primeSubqueries(stmt.Where)
	for _, assign := range stmt.Set {
		e.primeSubqueries(assign.Value)
	}

	count := 0
	err = e.runDMLAtomic(tableName, func() error {
		n, err := e.session.UpdateFunc(tableName, updateFn, filter)
		if err != nil {
			return err
		}
		count = n
		return nil
	})
	if err != nil {
		return nil, err
	}

	result := NewResult("UPDATE")
	result.SetRowCount(count)
	e.recordChanges(int64(count))
	return result, nil
}

// executeDelete executes a DELETE statement.
func (e *Executor) executeDelete(stmt *parser.DeleteStmt) (*Result, error) {
	tableName := stmt.Table.Name
	schema, err := e.schema.GetSchema(tableName)
	if err != nil {
		return nil, err
	}

	// Build filter
	var filter func(storage.Row) bool
	if stmt.Where != nil {
		filter = func(row storage.Row) bool {
			val, err := e.evalExpr(stmt.Where, row)
			if err != nil {
				return false
			}
			return toBool(val)
		}
	}
	if stmt.Where != nil {
		column, value, equality := e.extractIndexableCondition(stmt.Where)
		if equality && strings.EqualFold(column, schema.PrimaryKey) {
			_, deleted, err := e.session.DeleteByPK(tableName, fmt.Sprintf("%v", value))
			if err != nil {
				return nil, err
			}
			count := 0
			if deleted {
				count = 1
			}
			result := NewResult("DELETE")
			result.SetRowCount(count)
			e.recordChanges(int64(count))
			return result, nil
		}
	}

	// Pre-evaluate subqueries in the predicate before the scan-based delete
	// takes the session lock.
	e.primeSubqueries(stmt.Where)

	count, err := e.session.Delete(tableName, filter)
	if err != nil {
		return nil, err
	}

	result := NewResult("DELETE")
	result.SetRowCount(count)
	e.recordChanges(int64(count))
	return result, nil
}

// executeCreateTable executes a CREATE TABLE statement.
func (e *Executor) executeCreateTable(stmt *parser.CreateTableStmt) (*Result, error) {
	// Check if exists
	if e.schema.TableExists(stmt.Table.Name) {
		if stmt.IfNotExists {
			result := NewResult("CREATE TABLE")
			return result, nil
		}
		return nil, fmt.Errorf("table already exists: %s", stmt.Table.Name)
	}

	// Build schema
	schema := &storage.Schema{
		Name: stmt.Table.Name,
	}

	for _, colDef := range stmt.Columns {
		col := storage.Column{
			Name:     colDef.Name,
			Type:     colDef.Type.Name,
			Nullable: true,
		}

		for _, constraint := range colDef.Constraints {
			switch constraint.Type {
			case parser.ConstraintPrimaryKey:
				col.PrimaryKey = true
				col.Nullable = false
				schema.PrimaryKey = col.Name
			case parser.ConstraintNotNull:
				col.Nullable = false
			case parser.ConstraintDefault:
				if constraint.Default != nil {
					val, _ := e.evalExpr(constraint.Default, nil)
					col.Default = val
				}
			case parser.ConstraintAutoIncrement:
				schema.AutoIncrement = true
			}
		}

		schema.Columns = append(schema.Columns, col)
	}

	// Handle table-level constraints
	for _, constraint := range stmt.Constraints {
		if constraint.Type == parser.ConstraintPrimaryKey && len(constraint.Columns) > 0 {
			schema.PrimaryKey = constraint.Columns[0]
			for i := range schema.Columns {
				if strings.EqualFold(schema.Columns[i].Name, schema.PrimaryKey) {
					schema.Columns[i].PrimaryKey = true
					schema.Columns[i].Nullable = false
				}
			}
		}
	}

	// Translate inline/table UNIQUE constraints (Gogs/GORM style) into the same
	// scan-validated unique Index metadata that explicit CREATE UNIQUE INDEX
	// produces, so both enforcement and the catalog observe them.
	uniqIndexes := e.uniqueConstraintIndexes(stmt)

	if err := e.schema.CreateTable(schema); err != nil {
		if stmt.IfNotExists && strings.Contains(err.Error(), "table already exists") {
			return NewResult("CREATE TABLE"), nil
		}
		return nil, err
	}

	// Register each materialized unique index. On failure, best-effort cleanup of
	// the table and any indexes already created for it (DDL is not transactional).
	var created []*storage.Index
	for _, idx := range uniqIndexes {
		if err := e.schema.CreateIndex(idx); err != nil {
			for _, c := range created {
				e.schema.DropIndex(c.Name)
			}
			e.schema.DropTable(stmt.Table.Name)
			return nil, err
		}
		created = append(created, idx)
	}

	if err := e.SyncCatalog(); err != nil {
		return nil, err
	}

	result := NewResult("CREATE TABLE")
	return result, nil
}

// uniqueConstraintIndexes translates a CREATE TABLE's inline/table UNIQUE
// constraints into unique Index definitions. Named constraints keep their name;
// unnamed constraints (column-level UNIQUE, or bare UNIQUE(col,...)) receive a
// deterministic internal name derived from the table and columns.
func (e *Executor) uniqueConstraintIndexes(stmt *parser.CreateTableStmt) []*storage.Index {
	var indexes []*storage.Index
	used := make(map[string]bool)

	uniqueName := func(base string, columns []string) string {
		if base == "" {
			base = "uniq_" + strings.ToLower(stmt.Table.Name) + "_" + strings.ToLower(strings.Join(columns, "_"))
		}
		name := base
		for n := 2; used[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		used[strings.ToLower(name)] = true
		return name
	}

	add := func(name string, columns []string) {
		idx := &storage.Index{Name: uniqueName(name, columns), Table: stmt.Table.Name, Unique: true}
		for _, c := range columns {
			idx.Columns = append(idx.Columns, storage.IndexColumn{Name: c})
		}
		indexes = append(indexes, idx)
	}

	for _, constraint := range stmt.Constraints {
		if constraint.Type == parser.ConstraintUnique && len(constraint.Columns) > 0 {
			add(constraint.Name, constraint.Columns)
		}
	}
	for _, colDef := range stmt.Columns {
		for _, constraint := range colDef.Constraints {
			if constraint.Type == parser.ConstraintUnique {
				add("", []string{colDef.Name})
			}
		}
	}
	return indexes
}

// executeDropTable executes a DROP TABLE statement.
func (e *Executor) executeDropTable(stmt *parser.DropTableStmt) (*Result, error) {
	for _, tableRef := range stmt.Tables {
		if !e.schema.TableExists(tableRef.Name) {
			if stmt.IfExists {
				continue
			}
			return nil, fmt.Errorf("table not found: %s", tableRef.Name)
		}

		// First, drop all indexes associated with this table
		indexes, _ := e.schema.ListTableIndexes(tableRef.Name)
		for _, idx := range indexes {
			// Clear index entries
			columns := make([]string, len(idx.Columns))
			for i, col := range idx.Columns {
				columns[i] = col.Name
			}
			e.session.ClearIndex(idx.Name, tableRef.Name, columns)
			// Drop the index schema
			e.schema.DropIndex(idx.Name)
		}

		// DropTable removes durable rows and schema state together.
		if err := e.schema.DropTable(tableRef.Name); err != nil {
			return nil, err
		}
		e.session.InvalidateCache(tableRef.Name)

	}
	if err := e.SyncCatalog(); err != nil {
		return nil, err
	}

	result := NewResult("DROP TABLE")
	return result, nil
}

// executeCreateIndex creates a new index.
func (e *Executor) executeCreateIndex(stmt *parser.CreateIndexStmt) (*Result, error) {
	// Check if index already exists
	if e.schema.IndexExists(stmt.Name) {
		if stmt.IfNotExists {
			result := NewResult("CREATE INDEX")
			return result, nil
		}
		return nil, fmt.Errorf("index already exists: %s", stmt.Name)
	}

	// Verify table exists
	if !e.schema.TableExists(stmt.Table) {
		return nil, fmt.Errorf("table not found: %s", stmt.Table)
	}

	// Verify columns exist
	schema, err := e.schema.GetSchema(stmt.Table)
	if err != nil {
		return nil, err
	}

	for _, col := range stmt.Columns {
		if _, found := schema.GetColumn(col.Name); !found {
			return nil, fmt.Errorf("column not found: %s", col.Name)
		}
	}

	// Create storage index
	index := &storage.Index{
		Name:   stmt.Name,
		Table:  stmt.Table,
		Unique: stmt.Unique,
	}

	for _, col := range stmt.Columns {
		index.Columns = append(index.Columns, storage.IndexColumn{
			Name: col.Name,
			Desc: col.Desc,
		})
	}

	if stmt.Unique {
		// Validate existing rows and register the index under the table's
		// exclusive gate so no concurrent writer can insert a conflicting value
		// between validation and registration.
		if err := e.table.CreateUniqueIndex(index); err != nil {
			if stmt.IfNotExists && strings.Contains(err.Error(), "index already exists") {
				return NewResult("CREATE INDEX"), nil
			}
			return nil, err
		}
	} else {
		if err := e.schema.CreateIndex(index); err != nil {
			if stmt.IfNotExists && strings.Contains(err.Error(), "index already exists") {
				return NewResult("CREATE INDEX"), nil
			}
			return nil, err
		}
	}

	// Build index entries for existing rows
	columns := make([]string, len(stmt.Columns))
	for i, col := range stmt.Columns {
		columns[i] = col.Name
	}
	if err := e.session.BuildIndex(stmt.Name, stmt.Table, columns); err != nil {
		// Rollback index creation on failure
		e.schema.DropIndex(stmt.Name)
		return nil, fmt.Errorf("failed to build index: %w", err)
	}

	result := NewResult("CREATE INDEX")
	return result, nil
}

// executeDropIndex drops an index.
func (e *Executor) executeDropIndex(stmt *parser.DropIndexStmt) (*Result, error) {
	if !e.schema.IndexExists(stmt.Name) {
		if stmt.IfExists {
			result := NewResult("DROP INDEX")
			return result, nil
		}
		return nil, fmt.Errorf("index not found: %s", stmt.Name)
	}

	// Get index info to clear entries
	index, err := e.schema.GetIndex(stmt.Name)
	if err == nil && index != nil {
		columns := make([]string, len(index.Columns))
		for i, col := range index.Columns {
			columns[i] = col.Name
		}
		e.session.ClearIndex(stmt.Name, index.Table, columns)
	}

	if err := e.schema.DropIndex(stmt.Name); err != nil {
		return nil, err
	}

	result := NewResult("DROP INDEX")
	return result, nil
}

func (e *Executor) executeCreateView(stmt *parser.CreateViewStmt) (*Result, error) {
	name := strings.ToLower(stmt.View.Name)

	if _, exists := e.views[name]; exists {
		if stmt.IfNotExists {
			return NewResult("CREATE VIEW"), nil
		}
		return nil, fmt.Errorf("view already exists: %s", stmt.View.Name)
	}

	e.views[name] = stmt.Select

	// Derive view columns from SELECT list for catalog registration.
	var viewCols []analyzer.ColumnInfo
	hasStar := false
	for _, col := range stmt.Select.Columns {
		if col.Star {
			hasStar = true
			break
		}
		colName := col.Alias
		if colName == "" {
			if ref, ok := col.Expr.(*parser.ColumnRef); ok {
				colName = ref.Column
			} else {
				colName = fmt.Sprintf("col_%d", len(viewCols))
			}
		}
		viewCols = append(viewCols, analyzer.ColumnInfo{
			Name:      colName,
			TableName: stmt.View.Name,
			Type:      analyzer.TypeAny,
			Nullable:  true,
		})
	}
	// For SELECT *, pull columns from the underlying table(s).
	if hasStar && len(stmt.Select.From) > 0 {
		baseName := stmt.Select.From[0].Name
		if schema, err := e.schema.GetSchema(baseName); err == nil {
			for _, c := range schema.Columns {
				viewCols = append(viewCols, analyzer.ColumnInfo{
					Name:      c.Name,
					TableName: stmt.View.Name,
					Type:      analyzer.TypeAny,
					Nullable:  true,
				})
			}
		}
	}

	// Register in catalog so the analyzer accepts SELECT FROM this view.
	e.catalog.CreateTable(&analyzer.TableInfo{ //nolint:errcheck
		Name:    stmt.View.Name,
		Columns: viewCols,
		IsView:  true,
	})

	return NewResult("CREATE VIEW"), nil
}

func (e *Executor) executeDropView(stmt *parser.DropViewStmt) (*Result, error) {
	for _, ref := range stmt.Views {
		name := strings.ToLower(ref.Name)
		if _, exists := e.views[name]; !exists {
			if stmt.IfExists {
				continue
			}
			return nil, fmt.Errorf("view not found: %s", ref.Name)
		}
		delete(e.views, name)
		e.catalog.DropTable(ref.Name) //nolint:errcheck
	}
	return NewResult("DROP VIEW"), nil
}

// executeAlterTable executes an ALTER TABLE statement.
func (e *Executor) executeAlterTable(stmt *parser.AlterTableStmt) (*Result, error) {
	switch action := stmt.Action.(type) {
	case *parser.AddColumnAction:
		return e.executeAlterTableAddColumn(stmt.Table, action)
	case *parser.DropColumnAction:
		return e.executeAlterTableDropColumn(stmt.Table, action)
	case *parser.RenameTableAction:
		return e.executeAlterTableRename(stmt.Table, action)
	case *parser.RenameColumnAction:
		return e.executeAlterTableRenameColumn(stmt.Table, action)
	default:
		return nil, fmt.Errorf("unsupported ALTER TABLE action: %T", action)
	}
}

// executeAlterTableAddColumn adds a column to a table.
func (e *Executor) executeAlterTableAddColumn(table string, action *parser.AddColumnAction) (*Result, error) {
	if action.IfNotExists {
		schema, err := e.schema.GetSchema(table)
		if err != nil {
			return nil, err
		}
		if _, exists := schema.GetColumn(action.Column.Name); exists {
			return NewResult("ALTER TABLE"), nil
		}
	}
	col := storage.Column{
		Name:     action.Column.Name,
		Type:     action.Column.Type.Name,
		Nullable: true,
	}

	// Process column constraints
	for _, constraint := range action.Column.Constraints {
		switch constraint.Type {
		case parser.ConstraintPrimaryKey:
			col.PrimaryKey = true
			col.Nullable = false
		case parser.ConstraintNotNull:
			col.Nullable = false
		case parser.ConstraintDefault:
			if constraint.Default != nil {
				val, _ := e.evalExpr(constraint.Default, nil)
				col.Default = val
			}
		}
	}

	if err := e.schema.AddColumn(table, col); err != nil {
		if action.IfNotExists && strings.Contains(err.Error(), "column already exists") {
			return NewResult("ALTER TABLE"), nil
		}
		return nil, err
	}

	// Update catalog
	e.SyncCatalog()

	result := NewResult("ALTER TABLE")
	return result, nil
}

// executeAlterTableDropColumn drops a column from a table.
func (e *Executor) executeAlterTableDropColumn(table string, action *parser.DropColumnAction) (*Result, error) {
	if err := e.schema.DropColumn(table, action.Column); err != nil {
		return nil, err
	}

	// Update catalog
	e.SyncCatalog()

	result := NewResult("ALTER TABLE")
	return result, nil
}

// executeAlterTableRename renames a table.
func (e *Executor) executeAlterTableRename(table string, action *parser.RenameTableAction) (*Result, error) {
	if err := e.schema.RenameTable(table, action.NewName); err != nil {
		return nil, err
	}
	e.session.InvalidateCache(table)
	e.session.InvalidateCache(action.NewName)

	// Update catalog
	e.SyncCatalog()

	result := NewResult("ALTER TABLE")
	return result, nil
}

// executeAlterTableRenameColumn renames a column.
func (e *Executor) executeAlterTableRenameColumn(table string, action *parser.RenameColumnAction) (*Result, error) {
	if err := e.schema.RenameColumn(table, action.OldName, action.NewName); err != nil {
		return nil, err
	}

	// Update catalog
	e.SyncCatalog()

	result := NewResult("ALTER TABLE")
	return result, nil
}

// Transaction execution methods

// executeBegin starts a new transaction.
func (e *Executor) executeBegin(stmt *parser.BeginStmt) (*Result, error) {
	if e.inTransaction {
		return nil, fmt.Errorf("cannot start a transaction within a transaction")
	}

	if err := e.session.Begin(); err != nil {
		return nil, err
	}
	e.inTransaction = true
	e.savepoints = nil
	e.savepointPositions = nil

	result := NewResult("BEGIN")
	return result, nil
}

// executeCommit commits the current transaction.
func (e *Executor) executeCommit(stmt *parser.CommitStmt) (*Result, error) {
	if !e.inTransaction {
		return nil, fmt.Errorf("cannot commit: no transaction in progress")
	}

	if err := e.session.Commit(); err != nil {
		e.inTransaction = false
		e.savepoints = nil
		e.savepointPositions = nil
		return nil, err
	}

	e.inTransaction = false
	e.savepoints = nil
	e.savepointPositions = nil

	result := NewResult("COMMIT")
	return result, nil
}

// executeRollback rolls back the current transaction or to a savepoint.
func (e *Executor) executeRollback(stmt *parser.RollbackStmt) (*Result, error) {
	if !e.inTransaction {
		return nil, fmt.Errorf("cannot rollback: no transaction in progress")
	}

	if stmt.Savepoint != "" {
		// Rollback to savepoint
		return e.rollbackToSavepoint(stmt.Savepoint)
	}

	if err := e.session.Rollback(); err != nil {
		return nil, err
	}
	e.inTransaction = false
	e.savepoints = nil
	e.savepointPositions = nil
	// total_changes() is monotonic (SQLite semantics): it is NOT decremented on
	// rollback. last_insert_rowid() and changes() also intentionally hold.

	result := NewResult("ROLLBACK")
	return result, nil
}

// executeSavepoint creates a savepoint.
func (e *Executor) executeSavepoint(stmt *parser.SavepointStmt) (*Result, error) {
	if !e.inTransaction {
		// SQLite allows SAVEPOINT outside transaction (starts implicit transaction)
		if err := e.session.Begin(); err != nil {
			return nil, err
		}
		e.inTransaction = true
		e.savepoints = nil
		e.savepointPositions = nil
	}

	// Add savepoint marker
	e.savepoints = append(e.savepoints, stmt.Name)
	e.savepointPositions = append(e.savepointPositions, e.session.Snapshot())

	result := NewResult("SAVEPOINT")
	return result, nil
}

// executeRelease releases a savepoint.
func (e *Executor) executeRelease(stmt *parser.ReleaseStmt) (*Result, error) {
	if !e.inTransaction {
		return nil, fmt.Errorf("cannot release savepoint: no transaction in progress")
	}

	// Find and remove the savepoint
	found := false
	for i := len(e.savepoints) - 1; i >= 0; i-- {
		if e.savepoints[i] == stmt.Name {
			e.savepoints = e.savepoints[:i]
			e.savepointPositions = e.savepointPositions[:i]
			found = true
			break
		}
	}

	if !found {
		return nil, fmt.Errorf("no such savepoint: %s", stmt.Name)
	}

	result := NewResult("RELEASE")
	return result, nil
}

// executeAttach attaches a database.
func (e *Executor) executeAttach(stmt *parser.AttachStmt) (*Result, error) {
	// Check if alias already exists
	if _, exists := e.attachedDatabases[stmt.Alias]; exists {
		return nil, fmt.Errorf("database alias already exists: %s", stmt.Alias)
	}

	// Reserved alias check
	if strings.EqualFold(stmt.Alias, "temp") || strings.EqualFold(stmt.Alias, "temporary") {
		return nil, fmt.Errorf("reserved database alias: %s", stmt.Alias)
	}

	// Get the pool from the main schema manager
	pool := e.schema.GetPool()

	// Create new schema and table managers for the attached database
	// In PizzaKV, each database is just a different namespace/prefix
	schema := storage.NewSchemaManager(pool, stmt.FilePath)
	table := storage.NewTableManager(pool, schema, stmt.FilePath)

	// Register the database connection
	e.attachedDatabases[stmt.Alias] = &DatabaseConnection{
		Alias:  stmt.Alias,
		Path:   stmt.FilePath,
		Schema: schema,
		Table:  table,
	}

	// Sync the catalog with the attached database's tables
	tables, _ := schema.ListTables()
	for _, tableName := range tables {
		tSchema, err := schema.GetSchema(tableName)
		if err != nil {
			continue
		}
		// Add with database prefix
		tableInfo := tSchema.ToAnalyzerTableInfo()
		tableInfo.Name = stmt.Alias + "." + tableInfo.Name
		e.catalog.CreateTable(tableInfo)
	}

	result := NewResult("ATTACH")
	return result, nil
}

// executeDetach detaches a database.
func (e *Executor) executeDetach(stmt *parser.DetachStmt) (*Result, error) {
	// Cannot detach main database
	if strings.EqualFold(stmt.Alias, "main") {
		return nil, fmt.Errorf("cannot detach main database")
	}

	// Check if database exists
	if _, exists := e.attachedDatabases[stmt.Alias]; !exists {
		return nil, fmt.Errorf("no such database: %s", stmt.Alias)
	}

	// Remove from attached databases
	delete(e.attachedDatabases, stmt.Alias)

	// Note: We don't remove from catalog as that would be more complex
	// In a production system, we'd need to track which tables belong to which database

	result := NewResult("DETACH")
	return result, nil
}

// rollbackToSavepoint rolls back to a specific savepoint.
func (e *Executor) rollbackToSavepoint(name string) (*Result, error) {
	// Find savepoint index
	savepointIdx := -1
	for i := len(e.savepoints) - 1; i >= 0; i-- {
		if e.savepoints[i] == name {
			savepointIdx = i
			break
		}
	}

	if savepointIdx == -1 {
		return nil, fmt.Errorf("no such savepoint: %s", name)
	}

	logPosition := e.savepointPositions[savepointIdx]
	e.session.RollbackTo(logPosition)

	// Remove savepoints after the target
	e.savepoints = e.savepoints[:savepointIdx+1]
	e.savepointPositions = e.savepointPositions[:savepointIdx+1]

	result := NewResult("ROLLBACK")
	return result, nil
}

// RollbackActive rolls back an open transaction, such as when its client
// disconnects before sending COMMIT or ROLLBACK.
func (e *Executor) RollbackActive() error {
	if !e.inTransaction {
		return nil
	}
	_, err := e.executeRollback(&parser.RollbackStmt{})
	return err
}

// extractIndexableCondition extracts column name and value from a simple equality condition.
// Returns (column, value, true) if the expression is column = literal.
func (e *Executor) extractIndexableCondition(expr parser.Expr) (string, interface{}, bool) {
	binExpr, ok := expr.(*parser.BinaryExpr)
	if !ok {
		return "", nil, false
	}

	// Only handle equality for now
	if binExpr.Op != lexer.TokenEq {
		return "", nil, false
	}

	// Check for column = literal pattern
	colRef, leftIsCol := binExpr.Left.(*parser.ColumnRef)
	litExpr, rightIsLit := binExpr.Right.(*parser.LiteralExpr)

	if leftIsCol && rightIsLit {
		val, _ := e.evalLiteral(litExpr)
		return colRef.Column, val, true
	}

	// Check for literal = column pattern
	litExpr, leftIsLit := binExpr.Left.(*parser.LiteralExpr)
	colRef, rightIsCol := binExpr.Right.(*parser.ColumnRef)

	if leftIsLit && rightIsCol {
		val, _ := e.evalLiteral(litExpr)
		return colRef.Column, val, true
	}

	return "", nil, false
}

// executePragma executes a PRAGMA statement.
func (e *Executor) executePragma(stmt *parser.PragmaStmt) (*Result, error) {
	switch stmt.Name {
	case "table_info":
		return e.pragmaTableInfo(stmt.Arg)
	case "table_list":
		return e.pragmaTableList()
	case "database_list":
		return e.pragmaDatabaseList()
	case "version":
		return e.pragmaVersion()
	default:
		return nil, fmt.Errorf("unknown pragma: %s", stmt.Name)
	}
}

// pragmaTableInfo returns column information for a table.
func (e *Executor) pragmaTableInfo(tableName string) (*Result, error) {
	if tableName == "" {
		return nil, fmt.Errorf("table_info requires a table name")
	}

	schema, err := e.schema.GetSchema(tableName)
	if err != nil {
		return nil, err
	}

	result := NewResult("PRAGMA")
	result.AddColumn("cid")
	result.AddColumn("name")
	result.AddColumn("type")
	result.AddColumn("notnull")
	result.AddColumn("dflt_value")
	result.AddColumn("pk")

	for i, col := range schema.Columns {
		notnull := 0
		if !col.Nullable {
			notnull = 1
		}
		pk := 0
		if col.PrimaryKey {
			pk = 1
		}
		result.AddRow(int64(i), col.Name, col.Type, int64(notnull), col.Default, int64(pk))
	}

	return result, nil
}

// pragmaTableList returns a list of all tables.
func (e *Executor) pragmaTableList() (*Result, error) {
	tables, err := e.schema.ListTables()
	if err != nil {
		return nil, err
	}

	result := NewResult("PRAGMA")
	result.AddColumn("schema")
	result.AddColumn("name")
	result.AddColumn("type")

	for _, t := range tables {
		result.AddRow("main", t, "table")
	}

	return result, nil
}

// pragmaDatabaseList returns a list of databases.
func (e *Executor) pragmaDatabaseList() (*Result, error) {
	result := NewResult("PRAGMA")
	result.AddColumn("seq")
	result.AddColumn("name")
	result.AddColumn("file")

	// We only have one database
	result.AddRow(int64(0), "main", "")

	return result, nil
}

// pragmaVersion returns the PizzaSQL version.
func (e *Executor) pragmaVersion() (*Result, error) {
	result := NewResult("PRAGMA")
	result.AddColumn("version")
	result.AddRow(version.String())
	return result, nil
}

// executeExplain executes an EXPLAIN statement.
func (e *Executor) executeExplain(stmt *parser.ExplainStmt) (*Result, error) {
	result := NewResult("EXPLAIN")

	if stmt.QueryPlan {
		// EXPLAIN QUERY PLAN format
		result.AddColumn("id")
		result.AddColumn("parent")
		result.AddColumn("notused")
		result.AddColumn("detail")

		plan := e.generateQueryPlan(stmt.Statement)
		for i, step := range plan {
			result.AddRow(int64(i), int64(0), int64(0), step)
		}
	} else {
		// Simple EXPLAIN format
		result.AddColumn("addr")
		result.AddColumn("opcode")
		result.AddColumn("p1")
		result.AddColumn("p2")
		result.AddColumn("p3")
		result.AddColumn("p4")
		result.AddColumn("p5")
		result.AddColumn("comment")

		ops := e.generateOpcodes(stmt.Statement)
		for i, op := range ops {
			result.AddRow(int64(i), op, int64(0), int64(0), int64(0), "", int64(0), "")
		}
	}

	return result, nil
}

// generateQueryPlan generates a simple query plan description.
func (e *Executor) generateQueryPlan(stmt parser.Statement) []string {
	var plan []string

	switch s := stmt.(type) {
	case *parser.SelectStmt:
		if len(s.From) > 0 {
			plan = append(plan, fmt.Sprintf("SCAN TABLE %s", s.From[0].Name))
			if s.Where != nil {
				plan = append(plan, "FILTER")
			}
			if len(s.OrderBy) > 0 {
				plan = append(plan, "SORT")
			}
			if s.Limit != nil {
				plan = append(plan, "LIMIT")
			}
		} else {
			plan = append(plan, "SCALAR EXPRESSION")
		}
	case *parser.InsertStmt:
		plan = append(plan, fmt.Sprintf("INSERT INTO %s", s.Table.Name))
	case *parser.UpdateStmt:
		plan = append(plan, fmt.Sprintf("SCAN TABLE %s", s.Table.Name))
		plan = append(plan, "UPDATE")
	case *parser.DeleteStmt:
		plan = append(plan, fmt.Sprintf("SCAN TABLE %s", s.Table.Name))
		plan = append(plan, "DELETE")
	default:
		plan = append(plan, "EXECUTE")
	}

	return plan
}

// generateOpcodes generates simplified opcodes for EXPLAIN.
func (e *Executor) generateOpcodes(stmt parser.Statement) []string {
	var ops []string

	switch s := stmt.(type) {
	case *parser.SelectStmt:
		ops = append(ops, "Init")
		if len(s.From) > 0 {
			ops = append(ops, "OpenRead")
			ops = append(ops, "Rewind")
			ops = append(ops, "Column")
			ops = append(ops, "ResultRow")
			ops = append(ops, "Next")
			ops = append(ops, "Close")
		} else {
			ops = append(ops, "Integer")
			ops = append(ops, "ResultRow")
		}
		ops = append(ops, "Halt")
	case *parser.InsertStmt:
		ops = append(ops, "Init")
		ops = append(ops, "OpenWrite")
		ops = append(ops, "NewRowid")
		ops = append(ops, "Insert")
		ops = append(ops, "Close")
		ops = append(ops, "Halt")
	case *parser.UpdateStmt:
		ops = append(ops, "Init")
		ops = append(ops, "OpenWrite")
		ops = append(ops, "Rewind")
		ops = append(ops, "Column")
		ops = append(ops, "Update")
		ops = append(ops, "Next")
		ops = append(ops, "Close")
		ops = append(ops, "Halt")
	case *parser.DeleteStmt:
		ops = append(ops, "Init")
		ops = append(ops, "OpenWrite")
		ops = append(ops, "Rewind")
		ops = append(ops, "Delete")
		ops = append(ops, "Next")
		ops = append(ops, "Close")
		ops = append(ops, "Halt")
	default:
		ops = append(ops, "Init")
		ops = append(ops, "Halt")
	}

	return ops
}

// evalExpr evaluates an expression.
func (e *Executor) evalExpr(expr parser.Expr, row storage.Row) (interface{}, error) {
	if expr == nil {
		return nil, nil
	}
	switch ex := expr.(type) {
	case *parser.LiteralExpr:
		return e.evalLiteral(ex)
	case *parser.ColumnRef:
		return e.evalColumnRef(ex, row)
	case *parser.BinaryExpr:
		return e.evalBinaryExpr(ex, row)
	case *parser.UnaryExpr:
		return e.evalUnaryExpr(ex, row)
	case *parser.FunctionCall:
		return e.evalFunctionCall(ex, row)
	case *parser.ParenExpr:
		return e.evalExpr(ex.Expr, row)
	case *parser.CaseExpr:
		return e.evalCaseExpr(ex, row)
	case *parser.InExpr:
		return e.evalInExpr(ex, row)
	case *parser.BetweenExpr:
		return e.evalBetweenExpr(ex, row)
	case *parser.LikeExpr:
		return e.evalLikeExpr(ex, row)
	case *parser.IsNullExpr:
		return e.evalIsNullExpr(ex, row)
	case *parser.CastExpr:
		return e.evalCastExpr(ex, row)
	case *parser.SubqueryExpr:
		return e.evalSubqueryExpr(ex, row)
	case *parser.ExistsExpr:
		return e.evalExistsExpr(ex, row)
	default:
		return nil, fmt.Errorf("unsupported expression type: %T", expr)
	}
}

func (e *Executor) evalLiteral(lit *parser.LiteralExpr) (interface{}, error) {
	switch lit.Type {
	case lexer.TokenNumber:
		// Check for scientific notation (e.g., 1e+06) or decimal point
		if strings.Contains(lit.Value, ".") || strings.ContainsAny(lit.Value, "eE") {
			f, err := strconv.ParseFloat(lit.Value, 64)
			if err != nil {
				return nil, err
			}
			// If it's a whole number (no fractional part), return as int64
			if f == float64(int64(f)) {
				return int64(f), nil
			}
			return f, nil
		}
		return strconv.ParseInt(lit.Value, 10, 64)
	case lexer.TokenString:
		return lit.Value, nil
	case lexer.TokenNULL:
		return nil, nil
	case lexer.TokenTRUE:
		return true, nil
	case lexer.TokenFALSE:
		return false, nil
	default:
		return lit.Value, nil
	}
}

func (e *Executor) evalColumnRef(ref *parser.ColumnRef, row storage.Row) (interface{}, error) {
	if row == nil {
		return nil, fmt.Errorf("no row context for column: %s", ref.Column)
	}

	// For qualified column references (table.column):
	//
	// Resolution order:
	//   1. Exact qualified key in outer row  ("t1.b" → outer)
	//   2. Case-insensitive qualified in outer row
	//   3. Exact qualified key in current row ("x.b"  → inner alias)
	//   4. Case-insensitive qualified in current row
	//   5. Unqualified in outer row — only reached when qualified lookup in current
	//      row failed, meaning the qualifier refers to an outer table not the inner
	//      alias (e.g. "t1.b" in a subquery "FROM t1 AS x" resolves here).
	//   6. Unqualified in current row (last resort)
	if ref.Table != "" {
		if e.outerRow != nil {
			// Step 1-2: qualified lookup in outer row
			if val, ok := e.outerRow[ref.Table+"."+ref.Column]; ok {
				return val, nil
			}
			for k, v := range e.outerRow {
				if strings.EqualFold(k, ref.Table+"."+ref.Column) {
					return v, nil
				}
			}
		}

		// Step 3-4: qualified lookup in current row
		if val, ok := row[ref.Table+"."+ref.Column]; ok {
			return val, nil
		}
		for k, v := range row {
			if strings.EqualFold(k, ref.Table+"."+ref.Column) {
				return v, nil
			}
		}

		// Step 5: qualified lookup failed in current row — try outer row unqualified.
		// This handles correlated subqueries where the qualifier names an outer table
		// (e.g. "t1.b" when the inner FROM is "t1 AS x", so current row has "x.b"
		// but no "t1.b").
		if e.outerRow != nil {
			if val, ok := e.outerRow[ref.Column]; ok {
				return val, nil
			}
			for k, v := range e.outerRow {
				if strings.EqualFold(k, ref.Column) {
					return v, nil
				}
			}
		}
	}

	// Step 6: unqualified fallback in current row (handles unqualified refs and
	// single-table queries like "SELECT t1.a FROM t1" where rows have plain keys).
	if val, ok := row[ref.Column]; ok {
		return val, nil
	}
	for k, v := range row {
		if strings.EqualFold(k, ref.Column) {
			return v, nil
		}
	}

	// For unqualified refs with an outer row context (ref.Table == "").
	if e.outerRow != nil && ref.Table == "" {
		if val, ok := e.outerRow[ref.Column]; ok {
			return val, nil
		}
		for k, v := range e.outerRow {
			if strings.EqualFold(k, ref.Column) {
				return v, nil
			}
		}
	}

	// Hidden rowid alias (rowid/oid/_rowid_): only used when the row carries no
	// real column of that name. SQLite semantics give an explicit column named
	// oid/rowid/_rowid_ precedence over the hidden rowid alias, so this fallback
	// runs last.
	if storage.IsRowIDColumn(ref.Column) {
		if val, ok := row["_rowid_"]; ok {
			return val, nil
		}
	}

	return nil, nil // Column not found, return NULL
}

func (e *Executor) evalBinaryExpr(expr *parser.BinaryExpr, row storage.Row) (interface{}, error) {
	left, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}
	right, err := e.evalExpr(expr.Right, row)
	if err != nil {
		return nil, err
	}

	switch expr.Op {
	case lexer.TokenPlus:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) + toInt64(right), nil
		}
		return toFloat(left) + toFloat(right), nil
	case lexer.TokenMinus:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) - toInt64(right), nil
		}
		return toFloat(left) - toFloat(right), nil
	case lexer.TokenStar:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) * toInt64(right), nil
		}
		return toFloat(left) * toFloat(right), nil
	case lexer.TokenSlash:
		if left == nil || right == nil {
			return nil, nil
		}
		// Integer division when both operands are integers (truncates toward zero, matching SQLite)
		if isIntVal(left) && isIntVal(right) {
			ri := toInt64(right)
			if ri == 0 {
				return nil, nil
			}
			return toInt64(left) / ri, nil
		}
		r := toFloat(right)
		if r == 0 {
			return nil, nil // Division by zero returns NULL
		}
		return toFloat(left) / r, nil
	case lexer.TokenPercent:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			ri := toInt64(right)
			if ri == 0 {
				return nil, nil
			}
			return toInt64(left) % ri, nil
		}
		return int64(toFloat(left)) % int64(toFloat(right)), nil
	case lexer.TokenEq:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) == 0, nil
	case lexer.TokenNeq:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) != 0, nil
	case lexer.TokenLt:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) < 0, nil
	case lexer.TokenLte:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) <= 0, nil
	case lexer.TokenGt:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) > 0, nil
	case lexer.TokenGte:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) >= 0, nil
	case lexer.TokenAND:
		// Three-value logic: FALSE AND x = FALSE; NULL AND TRUE = NULL; TRUE AND TRUE = TRUE
		if left != nil && !toBool(left) {
			return false, nil
		}
		if right != nil && !toBool(right) {
			return false, nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		return true, nil
	case lexer.TokenOR:
		// Three-value logic: TRUE OR x = TRUE; NULL OR FALSE = NULL; FALSE OR FALSE = FALSE
		if left != nil && toBool(left) {
			return true, nil
		}
		if right != nil && toBool(right) {
			return true, nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		return false, nil
	case lexer.TokenConcat:
		return toString(left) + toString(right), nil
	default:
		return nil, fmt.Errorf("unsupported operator: %v", expr.Op)
	}
}

// applyBinaryOp applies a binary operator to two already-evaluated values.
func (e *Executor) applyBinaryOp(op lexer.TokenType, left, right interface{}) (interface{}, error) {
	dummy := &parser.BinaryExpr{Op: op}
	_ = dummy
	switch op {
	case lexer.TokenPlus:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) + toInt64(right), nil
		}
		return toFloat(left) + toFloat(right), nil
	case lexer.TokenMinus:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) - toInt64(right), nil
		}
		return toFloat(left) - toFloat(right), nil
	case lexer.TokenStar:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			return toInt64(left) * toInt64(right), nil
		}
		return toFloat(left) * toFloat(right), nil
	case lexer.TokenSlash:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			ri := toInt64(right)
			if ri == 0 {
				return nil, nil
			}
			return toInt64(left) / ri, nil
		}
		r := toFloat(right)
		if r == 0 {
			return nil, nil
		}
		return toFloat(left) / r, nil
	case lexer.TokenPercent:
		if left == nil || right == nil {
			return nil, nil
		}
		if isIntVal(left) && isIntVal(right) {
			ri := toInt64(right)
			if ri == 0 {
				return nil, nil
			}
			return toInt64(left) % ri, nil
		}
		return int64(toFloat(left)) % int64(toFloat(right)), nil
	case lexer.TokenEq:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) == 0, nil
	case lexer.TokenNeq:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) != 0, nil
	case lexer.TokenLt:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) < 0, nil
	case lexer.TokenLte:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) <= 0, nil
	case lexer.TokenGt:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) > 0, nil
	case lexer.TokenGte:
		if left == nil || right == nil {
			return nil, nil
		}
		return compare(left, right) >= 0, nil
	case lexer.TokenAND:
		if left != nil && !toBool(left) {
			return false, nil
		}
		if right != nil && !toBool(right) {
			return false, nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		return true, nil
	case lexer.TokenOR:
		if left != nil && toBool(left) {
			return true, nil
		}
		if right != nil && toBool(right) {
			return true, nil
		}
		if left == nil || right == nil {
			return nil, nil
		}
		return false, nil
	case lexer.TokenConcat:
		return toString(left) + toString(right), nil
	default:
		return nil, fmt.Errorf("unsupported operator: %v", op)
	}
}

// evalBuiltinFunction applies a named scalar function to pre-evaluated args.
func (e *Executor) evalBuiltinFunction(name string, args []interface{}) (interface{}, error) {
	switch name {
	case "NULLIF":
		if len(args) >= 2 && compare(args[0], args[1]) == 0 {
			return nil, nil
		}
		if len(args) > 0 {
			return args[0], nil
		}
	case "IFNULL", "NVL":
		if len(args) >= 2 {
			if args[0] == nil {
				return args[1], nil
			}
			return args[0], nil
		}
	case "COALESCE":
		for _, a := range args {
			if a != nil {
				return a, nil
			}
		}
		return nil, nil
	case "ABS":
		if len(args) > 0 && args[0] != nil {
			if isIntVal(args[0]) {
				v := toInt64(args[0])
				if v < 0 {
					return -v, nil
				}
				return v, nil
			}
			v := toFloat(args[0])
			if v < 0 {
				return -v, nil
			}
			return v, nil
		}
	case "LENGTH":
		if len(args) > 0 && args[0] != nil {
			return int64(len(fmt.Sprintf("%v", args[0]))), nil
		}
	}
	// Fall back: store pre-evaluated values in row and build column refs.
	row := make(storage.Row, len(args))
	fn := &parser.FunctionCall{Name: name}
	for i, a := range args {
		key := fmt.Sprintf("__arg%d__", i)
		row[key] = a
		fn.Args = append(fn.Args, &parser.ColumnRef{Column: key})
	}
	return e.evalFunctionCall(fn, row)
}

func (e *Executor) evalUnaryExpr(expr *parser.UnaryExpr, row storage.Row) (interface{}, error) {
	val, err := e.evalExpr(expr.Operand, row)
	if err != nil {
		return nil, err
	}

	switch expr.Op {
	case lexer.TokenMinus:
		if val == nil {
			return nil, nil
		}
		if isIntVal(val) {
			return -toInt64(val), nil
		}
		return -toFloat(val), nil
	case lexer.TokenPlus:
		if val == nil {
			return nil, nil
		}
		if isIntVal(val) {
			return toInt64(val), nil
		}
		return toFloat(val), nil
	case lexer.TokenNOT:
		if val == nil {
			return nil, nil // NOT NULL = NULL
		}
		return !toBool(val), nil
	default:
		return val, nil
	}
}

func (e *Executor) evalFunctionCall(fn *parser.FunctionCall, row storage.Row) (interface{}, error) {
	name := strings.ToUpper(fn.Name)

	// Evaluate arguments
	args := make([]interface{}, len(fn.Args))
	for i, arg := range fn.Args {
		val, err := e.evalExpr(arg, row)
		if err != nil {
			return nil, err
		}
		args[i] = val
	}

	switch name {
	case "UPPER":
		if len(args) > 0 {
			if args[0] == nil {
				return nil, nil // NULL propagation
			}
			return strings.ToUpper(toString(args[0])), nil
		}
	case "LOWER":
		if len(args) > 0 {
			if args[0] == nil {
				return nil, nil // NULL propagation
			}
			return strings.ToLower(toString(args[0])), nil
		}
	case "LENGTH":
		if len(args) > 0 {
			if args[0] == nil {
				return nil, nil // NULL propagation
			}
			return int64(len(toString(args[0]))), nil
		}
	case "ABS":
		if len(args) > 0 {
			if args[0] == nil {
				return nil, nil
			}
			v := toFloat(args[0])
			if v < 0 {
				return -v, nil
			}
			return v, nil
		}
	case "COALESCE":
		for _, arg := range args {
			if arg != nil {
				return arg, nil
			}
		}
		return nil, nil
	case "NULLIF":
		if len(args) >= 2 && compare(args[0], args[1]) == 0 {
			return nil, nil
		}
		if len(args) > 0 {
			return args[0], nil
		}
	case "IFNULL":
		if len(args) >= 2 {
			if args[0] == nil {
				return args[1], nil
			}
			return args[0], nil
		}
	case "TYPEOF":
		if len(args) > 0 {
			switch args[0].(type) {
			case nil:
				return "null", nil
			case int64, int:
				return "integer", nil
			case float64:
				return "real", nil
			case string:
				return "text", nil
			case []byte:
				return "blob", nil
			default:
				return "text", nil
			}
		}
	case "SUBSTR", "SUBSTRING":
		if len(args) >= 2 {
			s := toString(args[0])
			start := int(toFloat(args[1])) - 1 // SQL is 1-indexed
			if start < 0 {
				start = 0
			}
			if start >= len(s) {
				return "", nil
			}
			if len(args) >= 3 {
				length := int(toFloat(args[2]))
				if start+length > len(s) {
					length = len(s) - start
				}
				return s[start : start+length], nil
			}
			return s[start:], nil
		}
	case "TRIM":
		if len(args) > 0 {
			return strings.TrimSpace(toString(args[0])), nil
		}
	case "REPLACE":
		if len(args) >= 3 {
			return strings.ReplaceAll(toString(args[0]), toString(args[1]), toString(args[2])), nil
		}

	// Additional SQLite functions
	case "PRINTF":
		if len(args) > 0 {
			format := toString(args[0])
			fmtArgs := make([]interface{}, len(args)-1)
			for i := 1; i < len(args); i++ {
				fmtArgs[i-1] = args[i]
			}
			return fmt.Sprintf(format, fmtArgs...), nil
		}
	case "HEX":
		if len(args) > 0 {
			s := toString(args[0])
			return strings.ToUpper(fmt.Sprintf("%x", []byte(s))), nil
		}
	case "UNHEX":
		if len(args) > 0 {
			s := toString(args[0])
			var result []byte
			for i := 0; i < len(s)-1; i += 2 {
				var b byte
				fmt.Sscanf(s[i:i+2], "%x", &b)
				result = append(result, b)
			}
			return string(result), nil
		}
	case "RANDOM":
		return rand.Int63(), nil
	case "RANDOMBLOB":
		if len(args) > 0 {
			n := int(toFloat(args[0]))
			if n <= 0 {
				n = 1
			}
			if n > 1000000 {
				n = 1000000
			}
			blob := make([]byte, n)
			rand.Read(blob)
			return string(blob), nil
		}
	case "ZEROBLOB":
		if len(args) > 0 {
			n := int(toFloat(args[0]))
			if n <= 0 {
				n = 1
			}
			if n > 1000000 {
				n = 1000000
			}
			return string(make([]byte, n)), nil
		}
	case "INSTR":
		if len(args) >= 2 {
			s := toString(args[0])
			substr := toString(args[1])
			idx := strings.Index(s, substr)
			if idx < 0 {
				return int64(0), nil
			}
			return int64(idx + 1), nil // SQL is 1-indexed
		}
	case "GLOB":
		if len(args) >= 2 {
			pattern := toString(args[0])
			s := toString(args[1])
			return matchGlob(pattern, s), nil
		}
	case "ROUND":
		if len(args) > 0 {
			v := toFloat(args[0])
			decimals := 0
			if len(args) >= 2 {
				decimals = int(toFloat(args[1]))
			}
			mult := 1.0
			for i := 0; i < decimals; i++ {
				mult *= 10
			}
			return float64(int64(v*mult+0.5)) / mult, nil
		}
	case "MAX":
		if len(args) > 0 {
			max := args[0]
			for _, arg := range args[1:] {
				if compare(arg, max) > 0 {
					max = arg
				}
			}
			return max, nil
		}
	case "MIN":
		if len(args) > 0 {
			min := args[0]
			for _, arg := range args[1:] {
				if compare(arg, min) < 0 {
					min = arg
				}
			}
			return min, nil
		}
	case "CONCAT":
		var result strings.Builder
		for _, arg := range args {
			result.WriteString(toString(arg))
		}
		return result.String(), nil

	// Date/Time functions
	case "DATE":
		return evalDateFunc(args)
	case "TIME":
		return evalTimeFunc(args)
	case "DATETIME":
		return evalDatetimeFunc(args)
	case "JULIANDAY":
		return evalJuliandayFunc(args)
	case "UNIXEPOCH":
		return evalUnixepochFunc(args)
	case "STRFTIME":
		return evalStrftimeFunc(args)
	case "TIMEDIFF":
		return evalTimediffFunc(args)
	case "PIZZASQL_VERSION", "SQLITE_VERSION":
		return version.String(), nil
	case "LAST_INSERT_ROWID":
		return e.lastInsertRowID, nil
	case "CHANGES":
		return e.changes, nil
	case "TOTAL_CHANGES":
		return e.totalChanges, nil
	}

	return nil, nil
}

func (e *Executor) evalCaseExpr(expr *parser.CaseExpr, row storage.Row) (interface{}, error) {
	var operand interface{}
	if expr.Operand != nil {
		var err error
		operand, err = e.evalExpr(expr.Operand, row)
		if err != nil {
			return nil, err
		}
	}

	for _, when := range expr.Whens {
		cond, err := e.evalExpr(when.Condition, row)
		if err != nil {
			return nil, err
		}

		var match bool
		if expr.Operand != nil {
			// Simple CASE: CASE operand WHEN val THEN ... — NULL operand matches nothing
			if operand == nil {
				continue
			}
			match = compare(operand, cond) == 0
		} else {
			// Searched CASE: CASE WHEN cond THEN ... — NULL condition is falsy
			match = toBool(cond)
		}

		if match {
			return e.evalExpr(when.Result, row)
		}
	}

	if expr.Else != nil {
		return e.evalExpr(expr.Else, row)
	}

	return nil, nil
}

func (e *Executor) evalInExpr(expr *parser.InExpr, row storage.Row) (interface{}, error) {
	left, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}

	// Handle subquery: IN (SELECT ...)
	if expr.Subquery != nil {
		var result *Result
		var err error
		// Cache non-correlated subquery results for the duration of this query.
		// Safe when outerRow is nil (no outer context that the subquery could reference).
		if e.subqueryCache != nil && e.outerRow == nil {
			if cached, ok := e.subqueryCache[expr.Subquery]; ok {
				result = cached
			} else {
				result, err = e.executeSelect(expr.Subquery)
				if err == nil {
					e.subqueryCache[expr.Subquery] = result
				}
			}
		} else {
			result, err = e.executeSelect(expr.Subquery)
		}
		if err != nil {
			return nil, fmt.Errorf("IN subquery error: %w", err)
		}
		if len(result.Columns) != 1 {
			return nil, fmt.Errorf("subquery in IN must return exactly one column")
		}
		// SQL three-valued logic: if left is NULL → NULL; if any match → true; if any NULL → NULL; else false.
		if left == nil {
			return nil, nil
		}
		sawNull := false
		for _, resultRow := range result.Rows {
			if len(resultRow) == 0 {
				continue
			}
			v := resultRow[0]
			if v == nil {
				sawNull = true
				continue
			}
			if compare(left, v) == 0 {
				if expr.Not {
					return false, nil
				}
				return true, nil
			}
		}
		if sawNull {
			return nil, nil
		}
		if expr.Not {
			return true, nil
		}
		return false, nil
	}

	// Handle value list: IN (1, 2, 3).
	// Empty list: always FALSE (IN) / TRUE (NOT IN), even for NULL.
	if len(expr.Values) == 0 {
		return expr.Not, nil
	}
	// SQL three-valued logic: if left is NULL → NULL; if any match → true/false;
	// if list contains NULL and no match → NULL.
	if left == nil {
		return nil, nil
	}
	sawNull := false
	for _, val := range expr.Values {
		v, err := e.evalExpr(val, row)
		if err != nil {
			return nil, err
		}
		if v == nil {
			sawNull = true
			continue
		}
		if compare(left, v) == 0 {
			if expr.Not {
				return false, nil
			}
			return true, nil
		}
	}
	if sawNull {
		return nil, nil
	}
	if expr.Not {
		return true, nil
	}
	return false, nil
}

func (e *Executor) evalBetweenExpr(expr *parser.BetweenExpr, row storage.Row) (interface{}, error) {
	val, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}
	low, err := e.evalExpr(expr.Low, row)
	if err != nil {
		return nil, err
	}
	high, err := e.evalExpr(expr.High, row)
	if err != nil {
		return nil, err
	}

	if expr.Not {
		// NOT BETWEEN is equivalent to: val < low OR val > high
		// We need to handle NULL using three-valued OR logic:
		// NULL OR TRUE = TRUE
		// NULL OR FALSE = NULL
		// NULL OR NULL = NULL
		var lessThan, greaterThan interface{}

		if val == nil || low == nil {
			lessThan = nil // NULL
		} else {
			lessThan = compare(val, low) < 0
		}

		if val == nil || high == nil {
			greaterThan = nil // NULL
		} else {
			greaterThan = compare(val, high) > 0
		}

		// Implement three-valued OR
		if toBool(lessThan) || toBool(greaterThan) {
			return true, nil
		}
		if lessThan == nil || greaterThan == nil {
			return nil, nil // NULL
		}
		return false, nil
	} else {
		// BETWEEN is equivalent to: val >= low AND val <= high
		// Three-value logic: if val < low → FALSE (regardless of high); if val >= low and high is NULL → NULL
		if val == nil {
			return nil, nil
		}
		// x BETWEEN a AND b  =  (x >= a) AND (x <= b)
		// NULL AND FALSE = FALSE; NULL AND TRUE = NULL
		if low == nil {
			// x >= NULL = NULL; check upper bound for early FALSE
			if high != nil && compare(val, high) > 0 {
				return false, nil // NULL AND FALSE = FALSE
			}
			return nil, nil // NULL AND TRUE/NULL = NULL
		}
		if compare(val, low) < 0 {
			return false, nil // val < low → FALSE AND anything = FALSE
		}
		if high == nil {
			return nil, nil // TRUE AND NULL = NULL
		}
		return compare(val, high) <= 0, nil
	}
}

func (e *Executor) evalLikeExpr(expr *parser.LikeExpr, row storage.Row) (interface{}, error) {
	val, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}
	pattern, err := e.evalExpr(expr.Pattern, row)
	if err != nil {
		return nil, err
	}

	s := toString(val)
	p := toString(pattern)

	// Convert SQL LIKE pattern to simple matching
	// % matches any sequence, _ matches single character
	matched := matchLike(s, p)
	if expr.Not {
		return !matched, nil
	}
	return matched, nil
}

func (e *Executor) evalIsNullExpr(expr *parser.IsNullExpr, row storage.Row) (interface{}, error) {
	val, err := e.evalExpr(expr.Left, row)
	if err != nil {
		return nil, err
	}

	isNull := val == nil
	if expr.Not {
		return !isNull, nil
	}
	return isNull, nil
}

func (e *Executor) evalCastExpr(expr *parser.CastExpr, row storage.Row) (interface{}, error) {
	val, err := e.evalExpr(expr.Expr, row)
	if err != nil {
		return nil, err
	}

	if val == nil {
		return nil, nil // CAST(NULL AS any) = NULL
	}

	typeName := strings.ToUpper(expr.Type.Name)
	switch {
	case strings.Contains(typeName, "INT"):
		return int64(toFloat(val)), nil
	case strings.Contains(typeName, "REAL"), strings.Contains(typeName, "FLOAT"), strings.Contains(typeName, "DOUBLE"):
		return toFloat(val), nil
	case strings.Contains(typeName, "TEXT"), strings.Contains(typeName, "CHAR"):
		return toString(val), nil
	default:
		return val, nil
	}
}

// evalSubqueryExpr executes a scalar subquery and returns its value.
// A scalar subquery must return exactly one column. It returns:
// - The single value if the subquery returns one row
// - NULL if the subquery returns no rows
// - Error if the subquery returns more than one row (for strict SQL compliance)
func (e *Executor) evalSubqueryExpr(expr *parser.SubqueryExpr, row storage.Row) (interface{}, error) {
	if row != nil && e.correlatedAggCache != nil {
		if val, ok, err := e.evalDecorrelatedAggSubquery(expr.Query, row); ok || err != nil {
			return val, err
		}
	}

	// Save and set outer row context for correlated subqueries
	savedOuter := e.outerRow
	e.outerRow = row
	defer func() { e.outerRow = savedOuter }()

	// Execute the subquery
	result, err := e.executeSelect(expr.Query)
	if err != nil {
		return nil, fmt.Errorf("subquery error: %w", err)
	}

	// Check for empty result
	if result.RowCount == 0 {
		return nil, nil // Return NULL for empty subquery
	}

	// Check column count
	if len(result.Columns) == 0 {
		return nil, fmt.Errorf("subquery must return at least one column")
	}

	// For scalar subquery, return first column of first row
	// Note: Strict SQL would error if more than one row is returned
	// but we follow SQLite behavior which just returns the first value
	if len(result.Rows) > 0 && len(result.Rows[0]) > 0 {
		return result.Rows[0][0], nil
	}

	return nil, nil
}

func (e *Executor) evalDecorrelatedAggSubquery(query *parser.SelectStmt, outerRow storage.Row) (interface{}, bool, error) {
	spec, ok := e.correlatedAggSpec(query)
	if !ok {
		return nil, false, nil
	}

	outerVal, err := e.evalExpr(spec.outerKey, outerRow)
	if err != nil {
		return nil, true, err
	}
	cache, exists := e.correlatedAggCache[query]
	if !exists {
		cache, err = e.buildCorrelatedAggCache(query, spec)
		if err != nil {
			return nil, true, err
		}
		e.correlatedAggCache[query] = cache
	}
	if outerVal == nil {
		return cache.defaultValue, true, nil
	}
	if val, exists := cache.values[fmt.Sprintf("%v", outerVal)]; exists {
		return val, true, nil
	}
	return cache.defaultValue, true, nil
}

func (e *Executor) correlatedAggSpec(query *parser.SelectStmt) (correlatedAggSpec, bool) {
	if query == nil ||
		query.Compound != nil ||
		len(query.Columns) != 1 ||
		len(query.From) == 0 ||
		query.Where == nil ||
		len(query.GroupBy) > 0 ||
		query.Having != nil ||
		query.Limit != nil ||
		query.Offset != nil {
		return correlatedAggSpec{}, false
	}
	if query.Columns[0].Star {
		return correlatedAggSpec{}, false
	}
	agg, ok := query.Columns[0].Expr.(*parser.FunctionCall)
	if !ok {
		return correlatedAggSpec{}, false
	}
	switch strings.ToUpper(agg.Name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
	default:
		return correlatedAggSpec{}, false
	}

	innerAliases := collectFromAliases(query.From)
	bin, ok := query.Where.(*parser.BinaryExpr)
	if !ok || bin.Op != lexer.TokenEq {
		return correlatedAggSpec{}, false
	}
	leftRef, leftIsRef := bin.Left.(*parser.ColumnRef)
	rightRef, rightIsRef := bin.Right.(*parser.ColumnRef)
	if !leftIsRef || !rightIsRef {
		return correlatedAggSpec{}, false
	}

	leftInner := refBelongsToAliases(leftRef, innerAliases)
	rightInner := refBelongsToAliases(rightRef, innerAliases)
	if leftInner == rightInner {
		return correlatedAggSpec{}, false
	}
	if leftInner {
		return correlatedAggSpec{innerKey: leftRef, outerKey: rightRef, aggExpr: agg}, true
	}
	return correlatedAggSpec{innerKey: rightRef, outerKey: leftRef, aggExpr: agg}, true
}

func (e *Executor) buildCorrelatedAggCache(query *parser.SelectStmt, spec correlatedAggSpec) (*correlatedAggCache, error) {
	grouped := *query
	grouped.Where = nil
	grouped.GroupBy = []parser.Expr{spec.innerKey}
	grouped.Having = nil
	grouped.OrderBy = nil
	grouped.Limit = nil
	grouped.Offset = nil
	grouped.Columns = []parser.SelectColumn{
		{Expr: spec.innerKey, Alias: "__corr_key"},
		{Expr: spec.aggExpr, Alias: "__corr_value"},
	}

	savedOuter := e.outerRow
	e.outerRow = nil
	result, err := e.executeSelect(&grouped)
	e.outerRow = savedOuter
	if err != nil {
		return nil, fmt.Errorf("decorrelated aggregate subquery error: %w", err)
	}

	cache := &correlatedAggCache{
		values:       make(map[string]interface{}, len(result.Rows)),
		defaultValue: correlatedAggDefault(spec.aggExpr),
	}
	for _, row := range result.Rows {
		if len(row) < 2 || row[0] == nil {
			continue
		}
		cache.values[fmt.Sprintf("%v", row[0])] = row[1]
	}
	return cache, nil
}

func correlatedAggDefault(expr parser.Expr) interface{} {
	if fn, ok := expr.(*parser.FunctionCall); ok && strings.EqualFold(fn.Name, "COUNT") {
		return int64(0)
	}
	return nil
}

func collectFromAliases(from []parser.TableRef) map[string]struct{} {
	aliases := make(map[string]struct{})
	var addRef func(parser.TableRef)
	addRef = func(ref parser.TableRef) {
		if ref.Name != "" {
			aliases[strings.ToLower(ref.Name)] = struct{}{}
		}
		if ref.Alias != "" {
			aliases[strings.ToLower(ref.Alias)] = struct{}{}
		}
		if ref.Join != nil && ref.Join.Table != nil {
			addRef(*ref.Join.Table)
		}
	}
	for _, ref := range from {
		addRef(ref)
	}
	return aliases
}

func refBelongsToAliases(ref *parser.ColumnRef, aliases map[string]struct{}) bool {
	if ref == nil || ref.Table == "" {
		return false
	}
	_, ok := aliases[strings.ToLower(ref.Table)]
	return ok
}

// evalExistsExpr evaluates an EXISTS expression.
// Returns true if the subquery returns at least one row, false otherwise.
func (e *Executor) evalExistsExpr(expr *parser.ExistsExpr, row storage.Row) (interface{}, error) {
	// Save and set outer row context for correlated subqueries
	savedOuter := e.outerRow
	e.outerRow = row
	defer func() { e.outerRow = savedOuter }()

	// Execute the subquery
	result, err := e.executeSelect(expr.Subquery)
	if err != nil {
		return nil, fmt.Errorf("EXISTS subquery error: %w", err)
	}

	// EXISTS returns true if any rows are returned
	return len(result.Rows) > 0, nil
}

// evalAggregateExpr evaluates an aggregate expression over multiple rows.
func (e *Executor) evalAggregateExpr(expr parser.Expr, rows []storage.Row) (interface{}, error) {
	fn, ok := expr.(*parser.FunctionCall)
	if !ok {
		// Not a function call - could be a binary expression with aggregates inside
		// Evaluate it with the aggregate evaluation context
		return e.evalExprWithAggregates(expr, rows)
	}

	name := strings.ToUpper(fn.Name)

	switch name {
	case "COUNT":
		if fn.Star {
			return int64(len(rows)), nil
		}
		if fn.Distinct {
			seen := make(map[interface{}]struct{})
			for _, row := range rows {
				if len(fn.Args) > 0 {
					val, _ := e.evalExpr(fn.Args[0], row)
					if val != nil {
						seen[val] = struct{}{}
					}
				}
			}
			return int64(len(seen)), nil
		}
		count := int64(0)
		for _, row := range rows {
			if len(fn.Args) > 0 {
				val, _ := e.evalExpr(fn.Args[0], row)
				if val != nil {
					count++
				}
			}
		}
		return count, nil

	case "SUM":
		var sumInt int64
		var sumFloat float64
		allInt := true
		hasValues := false
		var seen map[interface{}]struct{}
		if fn.Distinct {
			seen = make(map[interface{}]struct{})
		}
		for _, row := range rows {
			if len(fn.Args) > 0 {
				val, _ := e.evalExpr(fn.Args[0], row)
				if val != nil {
					if fn.Distinct {
						key := fmt.Sprintf("%v", val)
						if _, exists := seen[key]; exists {
							continue
						}
						seen[key] = struct{}{}
					}
					if isIntVal(val) {
						sumInt += toInt64(val)
					} else {
						allInt = false
						sumFloat += toFloat(val)
					}
					hasValues = true
				}
			}
		}
		if !hasValues {
			return nil, nil
		}
		if allInt {
			return sumInt, nil
		}
		return sumFloat + float64(sumInt), nil

	case "AVG":
		var sum float64
		count := 0
		for _, row := range rows {
			if len(fn.Args) > 0 {
				val, _ := e.evalExpr(fn.Args[0], row)
				if val != nil {
					sum += toFloat(val)
					count++
				}
			}
		}
		if count == 0 {
			return nil, nil
		}
		return sum / float64(count), nil

	case "MIN":
		var min interface{}
		for _, row := range rows {
			if len(fn.Args) > 0 {
				val, _ := e.evalExpr(fn.Args[0], row)
				if val != nil && (min == nil || compare(val, min) < 0) {
					min = val
				}
			}
		}
		return min, nil

	case "MAX":
		var max interface{}
		for _, row := range rows {
			if len(fn.Args) > 0 {
				val, _ := e.evalExpr(fn.Args[0], row)
				if val != nil && (max == nil || compare(val, max) > 0) {
					max = val
				}
			}
		}
		return max, nil

	default:
		// Non-aggregate scalar function: evaluate args through aggregate context
		// (so COUNT/MIN/etc. inside NULLIF/COALESCE work correctly).
		return e.evalExprWithAggregates(expr, rows)
	}
}

// evalExprWithAggregates evaluates an expression that may contain aggregate functions
func (e *Executor) evalExprWithAggregates(expr parser.Expr, rows []storage.Row) (interface{}, error) {
	switch ex := expr.(type) {
	case *parser.BinaryExpr:
		left, err := e.evalExprWithAggregates(ex.Left, rows)
		if err != nil {
			return nil, err
		}
		right, err := e.evalExprWithAggregates(ex.Right, rows)
		if err != nil {
			return nil, err
		}
		// Use the same logic as evalBinaryExpr to preserve integer semantics.
		combined := &parser.BinaryExpr{Op: ex.Op}
		return e.applyBinaryOp(combined.Op, left, right)
	case *parser.FunctionCall:
		name := strings.ToUpper(ex.Name)
		switch name {
		case "COUNT", "SUM", "AVG", "MIN", "MAX", "TOTAL", "GROUP_CONCAT":
			return e.evalAggregateExpr(expr, rows)
		default:
			// Non-aggregate: evaluate each arg with aggregate context, then apply scalar.
			args := make([]interface{}, len(ex.Args))
			for i, arg := range ex.Args {
				v, err := e.evalExprWithAggregates(arg, rows)
				if err != nil {
					return nil, err
				}
				args[i] = v
			}
			return e.evalBuiltinFunction(name, args)
		}
	case *parser.ParenExpr:
		return e.evalExprWithAggregates(ex.Expr, rows)
	case *parser.UnaryExpr:
		operand, err := e.evalExprWithAggregates(ex.Operand, rows)
		if err != nil {
			return nil, err
		}
		switch ex.Op {
		case lexer.TokenPlus:
			return operand, nil
		case lexer.TokenMinus:
			if operand == nil {
				return nil, nil
			}
			if isIntVal(operand) {
				return -toInt64(operand), nil
			}
			return -toFloat(operand), nil
		case lexer.TokenNOT:
			if operand == nil {
				return nil, nil // NOT NULL = NULL
			}
			return !toBool(operand), nil
		default:
			return nil, fmt.Errorf("unsupported unary operator: %v", ex.Op)
		}
	case *parser.CastExpr:
		// Evaluate inner expression with aggregate context, then apply cast.
		val, err := e.evalExprWithAggregates(ex.Expr, rows)
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil
		}
		switch strings.ToUpper(ex.Type.Name) {
		case "INTEGER", "INT", "BIGINT", "SMALLINT", "TINYINT", "SIGNED":
			if isIntVal(val) {
				return toInt64(val), nil
			}
			return int64(toFloat(val)), nil
		case "REAL", "FLOAT", "DOUBLE", "NUMERIC", "DECIMAL":
			return toFloat(val), nil
		case "TEXT", "VARCHAR", "CHAR", "STRING":
			return fmt.Sprintf("%v", val), nil
		}
		return val, nil
	case *parser.CaseExpr:
		var operand interface{}
		if ex.Operand != nil {
			operand, _ = e.evalExprWithAggregates(ex.Operand, rows)
		}
		for _, when := range ex.Whens {
			condVal, _ := e.evalExprWithAggregates(when.Condition, rows)
			var matched bool
			if ex.Operand != nil {
				matched = operand != nil && condVal != nil && compare(operand, condVal) == 0
			} else {
				matched = toBool(condVal)
			}
			if matched {
				return e.evalExprWithAggregates(when.Result, rows)
			}
		}
		if ex.Else != nil {
			return e.evalExprWithAggregates(ex.Else, rows)
		}
		return nil, nil
	case *parser.IsNullExpr:
		val, err := e.evalExprWithAggregates(ex.Left, rows)
		if err != nil {
			return nil, err
		}
		isNull := val == nil
		if ex.Not {
			return !isNull, nil
		}
		return isNull, nil
	case *parser.BetweenExpr:
		val, err := e.evalExprWithAggregates(ex.Left, rows)
		if err != nil {
			return nil, err
		}
		low, err := e.evalExprWithAggregates(ex.Low, rows)
		if err != nil {
			return nil, err
		}
		high, err := e.evalExprWithAggregates(ex.High, rows)
		if err != nil {
			return nil, err
		}

		if ex.Not {
			// NOT BETWEEN: val < low OR val > high
			var lessThan, greaterThan interface{}

			if val == nil || low == nil {
				lessThan = nil
			} else {
				lessThan = compare(val, low) < 0
			}

			if val == nil || high == nil {
				greaterThan = nil
			} else {
				greaterThan = compare(val, high) > 0
			}

			// Three-valued OR
			if toBool(lessThan) || toBool(greaterThan) {
				return true, nil
			}
			if lessThan == nil || greaterThan == nil {
				return nil, nil
			}
			return false, nil
		} else {
			// BETWEEN: val >= low AND val <= high
			if val == nil {
				return nil, nil
			}
			if low == nil {
				if high != nil && compare(val, high) > 0 {
					return false, nil
				}
				return nil, nil
			}
			if compare(val, low) < 0 {
				return false, nil
			}
			if high == nil {
				return nil, nil
			}
			return compare(val, high) <= 0, nil
		}
	case *parser.InExpr:
		left, err := e.evalExprWithAggregates(ex.Left, rows)
		if err != nil {
			return nil, err
		}

		// Handle subquery
		if ex.Subquery != nil {
			var result *Result
			var err error
			if e.subqueryCache != nil && e.outerRow == nil {
				if cached, ok := e.subqueryCache[ex.Subquery]; ok {
					result = cached
				} else {
					result, err = e.executeSelect(ex.Subquery)
					if err == nil {
						e.subqueryCache[ex.Subquery] = result
					}
				}
			} else {
				result, err = e.executeSelect(ex.Subquery)
			}
			if err != nil {
				return nil, fmt.Errorf("IN subquery error: %w", err)
			}
			if len(result.Columns) != 1 {
				return nil, fmt.Errorf("subquery in IN must return exactly one column")
			}
			if left == nil {
				return nil, nil
			}
			sawNull := false
			for _, resultRow := range result.Rows {
				if len(resultRow) == 0 {
					continue
				}
				v := resultRow[0]
				if v == nil {
					sawNull = true
					continue
				}
				if compare(left, v) == 0 {
					if ex.Not {
						return false, nil
					}
					return true, nil
				}
			}
			if sawNull {
				return nil, nil
			}
			if ex.Not {
				return true, nil
			}
			return false, nil
		}

		// Handle value list
		if len(ex.Values) == 0 {
			return ex.Not, nil
		}
		if left == nil {
			return nil, nil
		}
		sawNull := false
		for _, val := range ex.Values {
			v, err := e.evalExprWithAggregates(val, rows)
			if err != nil {
				return nil, err
			}
			if v == nil {
				sawNull = true
				continue
			}
			if compare(left, v) == 0 {
				if ex.Not {
					return false, nil
				}
				return true, nil
			}
		}
		if sawNull {
			return nil, nil
		}
		if ex.Not {
			return true, nil
		}
		return false, nil
	default:
		// Literals and non-aggregate expressions.
		if len(rows) > 0 {
			return e.evalExpr(expr, rows[0])
		}
		return e.evalExpr(expr, storage.Row{})
	}
}

// Helper functions

func (e *Executor) getSelectColumns(stmt *parser.SelectStmt, schema *storage.Schema) []string {
	var columns []string
	for _, col := range stmt.Columns {
		if col.Star {
			for _, c := range schema.Columns {
				columns = append(columns, c.Name)
			}
		} else if col.Alias != "" {
			columns = append(columns, col.Alias)
		} else if ref, ok := col.Expr.(*parser.ColumnRef); ok {
			columns = append(columns, ref.Column)
		} else {
			columns = append(columns, fmt.Sprintf("column%d", len(columns)+1))
		}
	}
	return columns
}

func (e *Executor) hasAggregates(columns []parser.SelectColumn) bool {
	for _, col := range columns {
		if e.isAggregate(col.Expr) {
			return true
		}
	}
	return false
}

func (e *Executor) isAggregate(expr parser.Expr) bool {
	if _, ok := expr.(*parser.WindowExpr); ok {
		// Window functions are computed after grouping; they are not aggregates.
		return false
	}
	if fn, ok := expr.(*parser.FunctionCall); ok {
		name := strings.ToUpper(fn.Name)
		switch name {
		case "COUNT", "SUM", "AVG", "MIN", "MAX", "TOTAL", "GROUP_CONCAT":
			return true
		}
		// Non-aggregate function: check if any arg contains an aggregate.
		for _, arg := range fn.Args {
			if e.isAggregate(arg) {
				return true
			}
		}
		return false
	}
	switch ex := expr.(type) {
	case *parser.UnaryExpr:
		return e.isAggregate(ex.Operand)
	case *parser.BinaryExpr:
		return e.isAggregate(ex.Left) || e.isAggregate(ex.Right)
	case *parser.ParenExpr:
		return e.isAggregate(ex.Expr)
	case *parser.CaseExpr:
		if ex.Operand != nil && e.isAggregate(ex.Operand) {
			return true
		}
		for _, w := range ex.Whens {
			if e.isAggregate(w.Condition) || e.isAggregate(w.Result) {
				return true
			}
		}
		if ex.Else != nil {
			return e.isAggregate(ex.Else)
		}
	case *parser.CastExpr:
		return e.isAggregate(ex.Expr)
	case *parser.IsNullExpr:
		return e.isAggregate(ex.Left)
	case *parser.BetweenExpr:
		return e.isAggregate(ex.Left) || e.isAggregate(ex.Low) || e.isAggregate(ex.High)
	case *parser.InExpr:
		if e.isAggregate(ex.Left) {
			return true
		}
		for _, val := range ex.Values {
			if e.isAggregate(val) {
				return true
			}
		}
		return false
	}
	return false
}

func (e *Executor) buildGroupKey(groupBy []parser.Expr, row storage.Row) string {
	var parts []string
	for _, expr := range groupBy {
		val, _ := e.evalExpr(expr, row)
		parts = append(parts, fmt.Sprintf("%v", val))
	}
	return strings.Join(parts, "|")
}

// resolveOrderByPositions replaces positional ORDER BY expressions (e.g. ORDER BY 1)
// with the corresponding SELECT column expressions per SQL-92 semantics.
func resolveOrderByPositions(orderBy []parser.OrderByItem, selectCols []parser.SelectColumn) []parser.OrderByItem {
	result := make([]parser.OrderByItem, len(orderBy))
	for i, item := range orderBy {
		if lit, ok := item.Expr.(*parser.LiteralExpr); ok {
			if pos, err := strconv.Atoi(lit.Value); err == nil && pos >= 1 && pos <= len(selectCols) {
				col := selectCols[pos-1]
				if col.Expr != nil {
					result[i] = parser.OrderByItem{Expr: col.Expr, Desc: item.Desc, NullsOrder: item.NullsOrder}
					continue
				}
			}
		}
		result[i] = item
	}
	return result
}

// orderByLess reports whether key slice a sorts before b under orderBy.
// Keys are precomputed per-row ORDER BY expression values, one per item.
func orderByLess(a, b []interface{}, orderBy []parser.OrderByItem) bool {
	for i, item := range orderBy {
		av, bv := a[i], b[i]
		if av == nil || bv == nil {
			if av == nil && bv == nil {
				continue
			}
			// SQLite treats NULL as smaller than any value, so the default puts
			// NULLs first for ASC and last for DESC. NULLS FIRST/LAST overrides it.
			nullsFirst := !item.Desc
			switch item.NullsOrder {
			case parser.NullsFirst:
				nullsFirst = true
			case parser.NullsLast:
				nullsFirst = false
			}
			if av == nil {
				return nullsFirst
			}
			return !nullsFirst
		}
		cmp := compare(av, bv)
		if cmp != 0 {
			if item.Desc {
				return cmp > 0
			}
			return cmp < 0
		}
	}
	return false
}

// sortKeyOrder returns a permutation of [0..len(keys)) that sorts the keys
// ascending per orderBy.
func sortKeyOrder(keys [][]interface{}, orderBy []parser.OrderByItem) []int {
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return orderByLess(keys[order[a]], keys[order[b]], orderBy)
	})
	return order
}

// topNHeap is a bounded max-heap that keeps the k smallest elements (per
// orderByLess) seen so far.
type topNHeap struct {
	keys    [][]interface{}
	idx     []int
	orderBy []parser.OrderByItem
}

func (h *topNHeap) Len() int { return len(h.idx) }

func (h *topNHeap) Less(i, j int) bool {
	return orderByLess(h.keys[h.idx[j]], h.keys[h.idx[i]], h.orderBy)
}

func (h *topNHeap) Swap(i, j int) { h.idx[i], h.idx[j] = h.idx[j], h.idx[i] }

func (h *topNHeap) Push(x interface{}) { h.idx = append(h.idx, x.(int)) }

func (h *topNHeap) Pop() interface{} {
	n := len(h.idx)
	x := h.idx[n-1]
	h.idx = h.idx[:n-1]
	return x
}

// topNKeyOrder returns the indices of the k smallest keys (per orderByLess) in
// ascending order, without fully sorting all n elements. If k >= n it falls
// back to a full sort.
func topNKeyOrder(keys [][]interface{}, orderBy []parser.OrderByItem, k int) []int {
	if k <= 0 {
		return nil
	}
	if k >= len(keys) {
		return sortKeyOrder(keys, orderBy)
	}
	h := &topNHeap{keys: keys, orderBy: orderBy, idx: make([]int, 0, k)}
	for i := range keys {
		if h.Len() < k {
			heap.Push(h, i)
		} else if orderByLess(keys[i], keys[h.idx[0]], orderBy) {
			h.idx[0] = i
			heap.Fix(h, 0)
		}
	}
	selected := append([]int(nil), h.idx...)
	sort.Slice(selected, func(a, b int) bool {
		return orderByLess(keys[selected[a]], keys[selected[b]], orderBy)
	})
	return selected
}

func reorderRows(rows []storage.Row, order []int) {
	tmp := make([]storage.Row, len(rows))
	for i, idx := range order {
		tmp[i] = rows[idx]
	}
	copy(rows, tmp)
}

// sortRowKeys precomputes the ORDER BY expression value for each row so each
// expression is evaluated once per row instead of O(n log n) times.
func (e *Executor) sortRowKeys(rows []storage.Row, orderBy []parser.OrderByItem) [][]interface{} {
	keys := make([][]interface{}, len(rows))
	for i, row := range rows {
		ks := make([]interface{}, len(orderBy))
		for j, item := range orderBy {
			ks[j], _ = e.evalExpr(item.Expr, row)
		}
		keys[i] = ks
	}
	return keys
}

func (e *Executor) sortRows(rows []storage.Row, orderBy []parser.OrderByItem) {
	order := sortKeyOrder(e.sortRowKeys(rows, orderBy), orderBy)
	reorderRows(rows, order)
}

// topNRows sorts only enough to keep the k smallest rows (per orderBy).
func (e *Executor) topNRows(rows []storage.Row, orderBy []parser.OrderByItem, k int) []storage.Row {
	order := topNKeyOrder(e.sortRowKeys(rows, orderBy), orderBy, k)
	out := make([]storage.Row, len(order))
	for i, idx := range order {
		out[i] = rows[idx]
	}
	return out
}

// orderAndLimitRows applies ORDER BY (with a bounded top-N selection when a
// LIMIT is present), then OFFSET and LIMIT, preserving SQL semantics.
func (e *Executor) orderAndLimitRows(rows []storage.Row, orderBy []parser.OrderByItem, limitExpr, offsetExpr parser.Expr, selectCols []parser.SelectColumn) []storage.Row {
	orderBy = resolveOrderByPositions(orderBy, selectCols)

	var offset, limit int
	hasOffset := offsetExpr != nil
	hasLimit := limitExpr != nil
	if hasOffset {
		offset = e.evalIntExpr(offsetExpr)
	}
	if hasLimit {
		limit = e.evalIntExpr(limitExpr)
	}

	if len(orderBy) > 0 {
		if hasLimit && limit >= 0 {
			k := offset + limit
			if k >= 0 && k < len(rows) {
				rows = e.topNRows(rows, orderBy, k)
			} else {
				e.sortRows(rows, orderBy)
			}
		} else {
			e.sortRows(rows, orderBy)
		}
	}

	if hasOffset {
		if offset < len(rows) {
			rows = rows[offset:]
		} else {
			rows = nil
		}
	}
	if hasLimit {
		if limit < len(rows) {
			rows = rows[:limit]
		}
	}
	return rows
}

// resultRowKey evaluates a single ORDER BY item against a result row, honoring
// select-column aliases exactly like the previous sortResultRows implementation.
func (e *Executor) resultRowKey(result *Result, rowIdx int, item parser.OrderByItem, columnNames []string) interface{} {
	if ref, ok := item.Expr.(*parser.ColumnRef); ok && ref.Table == "" {
		for idx, name := range columnNames {
			if strings.EqualFold(name, ref.Column) {
				if idx < len(result.Rows[rowIdx]) {
					return result.Rows[rowIdx][idx]
				}
			}
		}
	}
	row := e.resultRowToStorageRow(result, rowIdx)
	v, _ := e.evalExpr(item.Expr, row)
	return v
}

// resultRowKeys precomputes the ORDER BY expression value for each result row.
func (e *Executor) resultRowKeys(result *Result, orderBy []parser.OrderByItem, columnNames []string) [][]interface{} {
	keys := make([][]interface{}, len(result.Rows))
	for i := range result.Rows {
		ks := make([]interface{}, len(orderBy))
		for j, item := range orderBy {
			ks[j] = e.resultRowKey(result, i, item, columnNames)
		}
		keys[i] = ks
	}
	return keys
}

// sortResultRows sorts Result.Rows based on ORDER BY clauses.
// It handles column aliases by matching them against the select columns.
func (e *Executor) sortResultRows(result *Result, orderBy []parser.OrderByItem, selectColumns []parser.SelectColumn, columnNames []string) {
	orderBy = resolveOrderByPositions(orderBy, selectColumns)
	order := sortKeyOrder(e.resultRowKeys(result, orderBy, columnNames), orderBy)
	rows := make([][]interface{}, len(order))
	for i, idx := range order {
		rows[i] = result.Rows[idx]
	}
	result.Rows = rows
}

// orderAndLimitResultRows applies ORDER BY (with bounded top-N selection when a
// LIMIT is present), then OFFSET and LIMIT, to a Result's rows.
func (e *Executor) orderAndLimitResultRows(result *Result, orderBy []parser.OrderByItem, limitExpr, offsetExpr parser.Expr, selectCols []parser.SelectColumn, columnNames []string) {
	orderBy = resolveOrderByPositions(orderBy, selectCols)

	var offset, limit int
	hasOffset := offsetExpr != nil
	hasLimit := limitExpr != nil
	if hasOffset {
		offset = e.evalIntExpr(offsetExpr)
	}
	if hasLimit {
		limit = e.evalIntExpr(limitExpr)
	}

	if len(orderBy) > 0 {
		if hasLimit && limit >= 0 {
			k := offset + limit
			if k >= 0 && k < len(result.Rows) {
				order := topNKeyOrder(e.resultRowKeys(result, orderBy, columnNames), orderBy, k)
				rows := make([][]interface{}, len(order))
				for i, idx := range order {
					rows[i] = result.Rows[idx]
				}
				result.Rows = rows
			} else {
				e.sortResultRows(result, orderBy, nil, columnNames)
			}
		} else {
			e.sortResultRows(result, orderBy, nil, columnNames)
		}
	}

	if hasOffset {
		if offset < len(result.Rows) {
			result.Rows = result.Rows[offset:]
		} else {
			result.Rows = nil
		}
		result.RowCount = len(result.Rows)
	}
	if hasLimit {
		if limit < len(result.Rows) {
			result.Rows = result.Rows[:limit]
		}
		result.RowCount = len(result.Rows)
	}
}

// resultRowToStorageRow converts a Result row back to storage.Row for expression evaluation.
func (e *Executor) resultRowToStorageRow(result *Result, rowIdx int) storage.Row {
	row := make(storage.Row)
	for colIdx, colName := range result.Columns {
		if colIdx < len(result.Rows[rowIdx]) {
			row[colName] = result.Rows[rowIdx][colIdx]
		}
	}
	return row
}

func (e *Executor) evalIntExpr(expr parser.Expr) int {
	val, _ := e.evalExpr(expr, nil)
	return int(toFloat(val))
}

// Type conversion helpers

func isIntVal(v interface{}) bool {
	switch v.(type) {
	case int64, int, bool:
		return true
	default:
		return false
	}
}

func toInt64(v interface{}) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case int:
		return int64(val)
	case float64:
		return int64(val)
	case bool:
		if val {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func toFloat(v interface{}) float64 {
	switch val := v.(type) {
	case nil:
		return 0
	case int64:
		return float64(val)
	case int:
		return float64(val)
	case float64:
		return val
	case bool:
		if val {
			return 1
		}
		return 0
	case string:
		f, _ := strconv.ParseFloat(val, 64)
		return f
	default:
		return 0
	}
}

func toBool(v interface{}) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case int64:
		return val != 0
	case int:
		return val != 0
	case float64:
		return val != 0
	case string:
		return val != "" && val != "0" && strings.ToLower(val) != "false"
	default:
		return false
	}
}

func toString(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func compare(a, b interface{}) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}

	// Try numeric comparison
	fa, oka := toNumeric(a)
	fb, okb := toNumeric(b)
	if oka && okb {
		if fa < fb {
			return -1
		}
		if fa > fb {
			return 1
		}
		return 0
	}

	// String comparison
	sa := toString(a)
	sb := toString(b)
	return strings.Compare(sa, sb)
}

func toNumeric(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case int64:
		return float64(val), true
	case int:
		return float64(val), true
	case float64:
		return val, true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// splitANDClauses flattens a tree of AND binary expressions into a slice of leaf conditions.
func splitANDClauses(expr parser.Expr) []parser.Expr {
	if bin, ok := expr.(*parser.BinaryExpr); ok && bin.Op == lexer.TokenAND {
		left := splitANDClauses(bin.Left)
		right := splitANDClauses(bin.Right)
		return append(left, right...)
	}
	return []parser.Expr{expr}
}

// collectColumnRefs returns all unqualified column names referenced in an expression.
func collectColumnRefs(expr parser.Expr) []string {
	var refs []string
	var hasSubquery bool
	var walk func(parser.Expr)
	walk = func(e parser.Expr) {
		if e == nil {
			return
		}
		switch n := e.(type) {
		case *parser.ColumnRef:
			refs = append(refs, n.Column)
		case *parser.BinaryExpr:
			walk(n.Left)
			walk(n.Right)
		case *parser.UnaryExpr:
			walk(n.Operand)
		case *parser.InExpr:
			walk(n.Left)
			// Check for subquery
			if n.Subquery != nil {
				hasSubquery = true
			}
			for _, v := range n.Values {
				walk(v)
			}
		case *parser.BetweenExpr:
			walk(n.Left)
			walk(n.Low)
			walk(n.High)
		case *parser.LikeExpr:
			walk(n.Left)
			walk(n.Pattern)
		case *parser.IsNullExpr:
			walk(n.Left)
		case *parser.CaseExpr:
			walk(n.Operand)
			for _, w := range n.Whens {
				walk(w.Condition)
				walk(w.Result)
			}
			walk(n.Else)
		case *parser.FunctionCall:
			for _, a := range n.Args {
				walk(a)
			}
		case *parser.ParenExpr:
			walk(n.Expr)
		case *parser.CastExpr:
			walk(n.Expr)
		case *parser.SubqueryExpr:
			// Subqueries may reference outer columns
			hasSubquery = true
		case *parser.ExistsExpr:
			// EXISTS subqueries may reference outer columns
			hasSubquery = true
		case *parser.LiteralExpr:
			// Literals have no column refs
		}
	}
	walk(expr)
	// If we have subqueries, add a sentinel value to indicate non-constant
	if hasSubquery {
		refs = append(refs, "__subquery__")
	}
	return refs
}

type tableColRef struct{ tbl, col string }

// collectTableColumnRefs returns all column references with their table qualifier (may be "").
func collectTableColumnRefs(expr parser.Expr) []tableColRef {
	var refs []tableColRef
	var walk func(parser.Expr)
	walk = func(e parser.Expr) {
		if e == nil {
			return
		}
		switch n := e.(type) {
		case *parser.ColumnRef:
			refs = append(refs, tableColRef{tbl: n.Table, col: n.Column})
		case *parser.BinaryExpr:
			walk(n.Left)
			walk(n.Right)
		case *parser.UnaryExpr:
			walk(n.Operand)
		case *parser.InExpr:
			walk(n.Left)
			for _, v := range n.Values {
				walk(v)
			}
		case *parser.BetweenExpr:
			walk(n.Left)
			walk(n.Low)
			walk(n.High)
		case *parser.LikeExpr:
			walk(n.Left)
			walk(n.Pattern)
		case *parser.IsNullExpr:
			walk(n.Left)
		case *parser.CaseExpr:
			walk(n.Operand)
			for _, w := range n.Whens {
				walk(w.Condition)
				walk(w.Result)
			}
			walk(n.Else)
		case *parser.FunctionCall:
			for _, a := range n.Args {
				walk(a)
			}
		}
	}
	walk(expr)
	return refs
}

// combineAND combines a list of expressions with AND.
func combineAND(clauses []parser.Expr) parser.Expr {
	if len(clauses) == 0 {
		return nil
	}
	result := clauses[0]
	for _, c := range clauses[1:] {
		result = &parser.BinaryExpr{Left: result, Op: lexer.TokenAND, Right: c}
	}
	return result
}

// matchLike matches a string against a SQL LIKE pattern.
func matchLike(s, pattern string) bool {
	// Simple implementation - convert to lowercase for case-insensitive matching
	s = strings.ToLower(s)
	pattern = strings.ToLower(pattern)

	return matchLikeHelper(s, pattern)
}

func matchLikeHelper(s, p string) bool {
	if p == "" {
		return s == ""
	}

	if p[0] == '%' {
		// % matches any sequence
		for i := 0; i <= len(s); i++ {
			if matchLikeHelper(s[i:], p[1:]) {
				return true
			}
		}
		return false
	}

	if s == "" {
		return false
	}

	if p[0] == '_' || p[0] == s[0] {
		return matchLikeHelper(s[1:], p[1:])
	}

	return false
}

// matchGlob matches a string against a GLOB pattern.
// GLOB uses * for any sequence and ? for single character (case-sensitive).
func matchGlob(pattern, s string) bool {
	return matchGlobHelper(pattern, s)
}

func matchGlobHelper(p, s string) bool {
	if p == "" {
		return s == ""
	}

	if p[0] == '*' {
		// * matches any sequence
		for i := 0; i <= len(s); i++ {
			if matchGlobHelper(p[1:], s[i:]) {
				return true
			}
		}
		return false
	}

	if s == "" {
		return false
	}

	if p[0] == '?' || p[0] == s[0] {
		return matchGlobHelper(p[1:], s[1:])
	}

	// Handle character classes [...]
	if p[0] == '[' {
		end := strings.Index(p, "]")
		if end > 0 {
			class := p[1:end]
			match := false
			negate := false
			if len(class) > 0 && class[0] == '^' {
				negate = true
				class = class[1:]
			}
			for _, c := range class {
				if byte(c) == s[0] {
					match = true
					break
				}
			}
			if negate {
				match = !match
			}
			if match {
				return matchGlobHelper(p[end+1:], s[1:])
			}
		}
	}

	return false
}

// applyDistinct removes duplicate rows from the result
func (e *Executor) applyDistinct(rows [][]interface{}) [][]interface{} {
	if len(rows) == 0 {
		return rows
	}

	seen := make(map[string]bool)
	uniqueRows := make([][]interface{}, 0)

	for _, row := range rows {
		// Create a key from all column values
		key := ""
		for i, val := range row {
			if i > 0 {
				key += "\x00" // Use null byte as separator
			}
			key += fmt.Sprintf("%v", val)
		}

		if !seen[key] {
			seen[key] = true
			uniqueRows = append(uniqueRows, row)
		}
	}

	return uniqueRows
}
