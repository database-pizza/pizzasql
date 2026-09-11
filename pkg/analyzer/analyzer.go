package analyzer

import (
	"fmt"
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/lexer"
	"github.com/danfragoso/pizzasql-next/pkg/parser"
)

// ErrorType categorizes analysis errors.
type ErrorType int

const (
	ErrUnknown ErrorType = iota
	ErrTableNotFound
	ErrTableExists
	ErrColumnNotFound
	ErrColumnAmbiguous
	ErrTypeMismatch
	ErrInvalidFunction
	ErrInvalidArgCount
	ErrAggregateInWhere
	ErrNonAggregateInSelect
	ErrInvalidGroupBy
)

// AnalysisError represents a semantic analysis error.
type AnalysisError struct {
	Type    ErrorType
	Message string
	Line    int
	Column  int
	Context string
}

func (e *AnalysisError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("analysis error at line %d, column %d: %s", e.Line, e.Column, e.Message)
	}
	return fmt.Sprintf("analysis error: %s", e.Message)
}

// Analyzer performs semantic analysis on parsed SQL statements.
type Analyzer struct {
	catalog *Catalog
	scope   *Scope
	errors  []*AnalysisError
}

// New creates a new Analyzer with the given catalog.
func New(catalog *Catalog) *Analyzer {
	if catalog == nil {
		catalog = NewCatalog()
	}
	return &Analyzer{
		catalog: catalog,
	}
}

// Analyze performs semantic analysis on a statement.
func (a *Analyzer) Analyze(stmt parser.Statement) error {
	a.errors = nil
	a.scope = NewScope(nil)

	switch s := stmt.(type) {
	case *parser.SelectStmt:
		return a.analyzeSelect(s)
	case *parser.InsertStmt:
		return a.analyzeInsert(s)
	case *parser.UpdateStmt:
		return a.analyzeUpdate(s)
	case *parser.DeleteStmt:
		return a.analyzeDelete(s)
	case *parser.CreateTableStmt:
		return a.analyzeCreateTable(s)
	case *parser.DropTableStmt:
		return a.analyzeDropTable(s)
	case *parser.AlterTableStmt:
		// ALTER TABLE is handled directly by executor, no semantic analysis needed
		return nil
	case *parser.AttachStmt:
		// ATTACH DATABASE is handled directly by executor
		return nil
	case *parser.DetachStmt:
		// DETACH DATABASE is handled directly by executor
		return nil
	case *parser.BeginStmt, *parser.CommitStmt, *parser.RollbackStmt,
		*parser.SavepointStmt, *parser.ReleaseStmt:
		// Transaction statements don't need semantic analysis
		return nil
	case *parser.CreateIndexStmt, *parser.DropIndexStmt:
		// Index statements don't need semantic analysis
		return nil
	default:
		return &AnalysisError{
			Type:    ErrUnknown,
			Message: fmt.Sprintf("unknown statement type: %T", stmt),
		}
	}
}

// GetCatalog returns the analyzer's catalog.
func (a *Analyzer) GetCatalog() *Catalog {
	return a.catalog
}

// analyzeSelect analyzes a SELECT statement.
func (a *Analyzer) analyzeSelect(stmt *parser.SelectStmt) error {
	// Register CTE names before resolving FROM so both the main query and the
	// recursive legs can reference them.
	for _, cte := range stmt.With {
		a.scope.DefineTable(cteTableInfo(cte))
	}

	// First, resolve tables in FROM clause
	if err := a.resolveFromClause(stmt.From); err != nil {
		return err
	}

	// Analyze WHERE clause
	if stmt.Where != nil {
		info, err := a.analyzeExpr(stmt.Where)
		if err != nil {
			return err
		}
		// WHERE clause cannot contain aggregates
		if info.IsAggregate {
			return &AnalysisError{
				Type:    ErrAggregateInWhere,
				Message: "aggregate functions not allowed in WHERE clause",
			}
		}
	}

	// Determine if this is an aggregate query
	hasAggregate := false
	hasGroupBy := len(stmt.GroupBy) > 0

	// Analyze GROUP BY expressions first
	for _, expr := range stmt.GroupBy {
		if _, err := a.analyzeExpr(expr); err != nil {
			return err
		}
	}

	// Analyze SELECT columns and collect aliases for ORDER BY/HAVING reference
	selectAliases := make(map[string]*ExprInfo)
	for _, col := range stmt.Columns {
		if col.Star {
			// SELECT * - all columns from all tables
			continue
		}
		if col.TableStar != "" {
			// Qualified wildcard (table.*) - validate the qualifier names a
			// table or alias in scope; expansion happens at execution time.
			if err := a.validateTableStar(col.TableStar); err != nil {
				return err
			}
			continue
		}

		info, err := a.analyzeExpr(col.Expr)
		if err != nil {
			return err
		}

		if info.IsAggregate {
			hasAggregate = true
		}

		// Track column aliases so ORDER BY and HAVING can reference them
		if col.Alias != "" {
			selectAliases[strings.ToUpper(col.Alias)] = info
		}
	}

	// Register SELECT aliases as virtual columns for ORDER BY/HAVING reference
	for alias, info := range selectAliases {
		a.scope.DefineSelectAlias(alias, info.Type)
	}

	// Validate GROUP BY semantics
	if hasAggregate && !hasGroupBy {
		// Aggregate query without GROUP BY - all non-aggregate columns must be constants
		for _, col := range stmt.Columns {
			if col.Star {
				return &AnalysisError{
					Type:    ErrNonAggregateInSelect,
					Message: "SELECT * not allowed with aggregate functions without GROUP BY",
				}
			}
			if col.TableStar != "" {
				return &AnalysisError{
					Type:    ErrNonAggregateInSelect,
					Message: fmt.Sprintf("SELECT %s.* not allowed with aggregate functions without GROUP BY", col.TableStar),
				}
			}
			info, exprErr := a.analyzeExpr(col.Expr)
			if exprErr != nil || info == nil {
				continue
			}
			if !info.IsAggregate && !info.IsConstant {
				// Check if it's a simple column reference
				if ref, ok := col.Expr.(*parser.ColumnRef); ok {
					return &AnalysisError{
						Type:    ErrNonAggregateInSelect,
						Message: fmt.Sprintf("column %q must appear in GROUP BY clause or be in an aggregate function", ref.Column),
					}
				}
			}
		}
	}

	// Analyze HAVING clause
	if stmt.Having != nil {
		info, err := a.analyzeExpr(stmt.Having)
		if err != nil {
			return err
		}
		// HAVING without GROUP BY requires aggregates
		if !hasGroupBy && !info.IsAggregate {
			return &AnalysisError{
				Type:    ErrInvalidGroupBy,
				Message: "HAVING clause requires GROUP BY or aggregate function",
			}
		}
	}

	// Analyze ORDER BY
	for _, item := range stmt.OrderBy {
		if _, err := a.analyzeExpr(item.Expr); err != nil {
			return err
		}
	}

	// Analyze LIMIT/OFFSET
	if stmt.Limit != nil {
		info, err := a.analyzeExpr(stmt.Limit)
		if err != nil {
			return err
		}
		if !info.Type.IsNumeric() && info.Type != TypeNull {
			return &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: "LIMIT must be numeric",
			}
		}
	}

	if stmt.Offset != nil {
		info, err := a.analyzeExpr(stmt.Offset)
		if err != nil {
			return err
		}
		if !info.Type.IsNumeric() && info.Type != TypeNull {
			return &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: "OFFSET must be numeric",
			}
		}
	}

	return nil
}

// resolveFromClause adds tables from FROM clause to scope.
func (a *Analyzer) resolveFromClause(tables []parser.TableRef) error {
	for _, ref := range tables {
		if err := a.defineTableRef(ref); err != nil {
			return err
		}

		// Handle JOINs
		if ref.Join != nil {
			if err := a.resolveJoin(ref.Join); err != nil {
				return err
			}
		}
	}
	return nil
}

// defineTableRef adds a single FROM/JOIN table to scope. The reference may be a
// derived table (subquery), a CTE, or a real catalog table.
func (a *Analyzer) defineTableRef(ref parser.TableRef) error {
	if ref.Subquery != nil {
		if err := a.analyzeSelect(ref.Subquery); err != nil {
			return err
		}
		a.scope.DefineTable(a.derivedTableInfo(ref))
		return nil
	}

	if cte, ok := a.scope.LookupTable(ref.Name); ok {
		a.scope.DefineTable(&TableInfo{
			Name:    cte.Name,
			Columns: cte.Columns,
			Alias:   ref.Alias,
			IsView:  cte.IsView,
		})
		return nil
	}

	table, ok := a.catalog.GetTable(ref.Name)
	if !ok {
		return &AnalysisError{
			Type:    ErrTableNotFound,
			Message: fmt.Sprintf("table not found: %s", ref.Name),
		}
	}
	a.scope.DefineTable(&TableInfo{
		Name:    table.Name,
		Columns: table.Columns,
		Alias:   ref.Alias,
		IsView:  table.IsView,
	})
	return nil
}

// derivedTableInfo builds the scope entry for a derived table. A compound
// subquery (UNION/…) keeps its projection on the first leg. A wildcard expands
// the columns of the subquery's source tables so callers can reference them.
func (a *Analyzer) derivedTableInfo(ref parser.TableRef) *TableInfo {
	info := &TableInfo{Name: ref.Alias, Alias: ref.Alias}
	leg := firstSelectLeg(ref.Subquery)
	if leg == nil {
		return info
	}
	for _, col := range leg.Columns {
		if col.Star {
			for _, tref := range collectAllTableRefs(leg.From) {
				if t, ok := a.scope.LookupTable(tref.Name); ok {
					for _, c := range t.Columns {
						info.Columns = append(info.Columns, ColumnInfo{Name: c.Name, TableName: ref.Alias, Type: c.Type})
					}
				} else if t, ok := a.catalog.GetTable(tref.Name); ok {
					for _, c := range t.Columns {
						info.Columns = append(info.Columns, ColumnInfo{Name: c.Name, TableName: ref.Alias, Type: c.Type})
					}
				}
			}
			continue
		}
		name := ""
		switch {
		case col.Alias != "":
			name = col.Alias
		case col.Expr != nil:
			if colRef, ok := col.Expr.(*parser.ColumnRef); ok {
				name = colRef.Column
			}
		}
		if name == "" {
			name = fmt.Sprintf("col_%d", len(info.Columns))
		}
		info.Columns = append(info.Columns, ColumnInfo{Name: name, TableName: ref.Alias, Type: TypeAny})
	}
	return info
}

// collectAllTableRefs flattens a FROM list and its join chains.
func collectAllTableRefs(from []parser.TableRef) []parser.TableRef {
	var refs []parser.TableRef
	var add func(parser.TableRef)
	add = func(ref parser.TableRef) {
		if ref.Subquery != nil {
			return
		}
		refs = append(refs, ref)
		if ref.Join != nil && ref.Join.Table != nil {
			add(*ref.Join.Table)
		}
	}
	for _, ref := range from {
		add(ref)
	}
	return refs
}

// cteTableInfo describes the columns a CTE exposes. The declared column list
// wins; otherwise the anchor/first-leg projection names are used.
func cteTableInfo(cte *parser.CTE) *TableInfo {
	leg := firstSelectLeg(cte.Query)
	info := &TableInfo{Name: cte.Name}
	if leg == nil {
		return info
	}
	for i, col := range leg.Columns {
		name := ""
		switch {
		case i < len(cte.Columns):
			name = cte.Columns[i]
		case col.Alias != "":
			name = col.Alias
		case col.Expr != nil:
			if ref, ok := col.Expr.(*parser.ColumnRef); ok {
				name = ref.Column
			}
		}
		if name == "" {
			name = fmt.Sprintf("col_%d", i)
		}
		info.Columns = append(info.Columns, ColumnInfo{
			Name:      name,
			TableName: cte.Name,
			Type:      TypeAny,
		})
	}
	return info
}

// firstSelectLeg returns the SELECT whose projection describes a (possibly
// compound) subquery's output columns. A UNION/INTERSECT/EXCEPT keeps the
// projection on its first leg.
func firstSelectLeg(sel *parser.SelectStmt) *parser.SelectStmt {
	for sel != nil && sel.Compound != nil {
		sel = sel.Compound.Left
	}
	return sel
}

// validateTableStar checks that a qualified wildcard qualifier names a table or
// alias present in scope, returning an error for unknown qualifiers instead of
// silently falling back to a plain column expansion.
func (a *Analyzer) validateTableStar(qualifier string) error {
	if _, ok := a.scope.LookupTable(qualifier); !ok {
		return &AnalysisError{
			Type:    ErrTableNotFound,
			Message: fmt.Sprintf("no such table or alias: %s", qualifier),
		}
	}
	return nil
}

// resolveJoin resolves a JOIN clause.
func (a *Analyzer) resolveJoin(join *parser.JoinClause) error {
	if join.Table == nil {
		return nil
	}

	if err := a.defineTableRef(*join.Table); err != nil {
		return err
	}

	// Analyze ON condition
	if join.Condition != nil {
		if _, err := a.analyzeExpr(join.Condition); err != nil {
			return err
		}
	}

	// Handle USING clause
	for _, colName := range join.Using {
		_, _, ok := a.scope.LookupColumn("", colName)
		if !ok {
			return &AnalysisError{
				Type:    ErrColumnNotFound,
				Message: fmt.Sprintf("column not found in USING clause: %s", colName),
			}
		}
	}

	// Recursively handle chained JOINs
	if join.Table.Join != nil {
		if err := a.resolveJoin(join.Table.Join); err != nil {
			return err
		}
	}

	return nil
}

// analyzeInsert analyzes an INSERT statement.
func (a *Analyzer) analyzeInsert(stmt *parser.InsertStmt) error {
	table, ok := a.catalog.GetTable(stmt.Table.Name)
	if !ok {
		return &AnalysisError{
			Type:    ErrTableNotFound,
			Message: fmt.Sprintf("table not found: %s", stmt.Table.Name),
		}
	}

	// Validate column list if specified
	var targetCols []ColumnInfo
	if len(stmt.Columns) > 0 {
		for _, colName := range stmt.Columns {
			col, ok := table.GetColumn(colName)
			if !ok {
				return &AnalysisError{
					Type:    ErrColumnNotFound,
					Message: fmt.Sprintf("column not found: %s", colName),
				}
			}
			targetCols = append(targetCols, *col)
		}
	} else {
		targetCols = table.Columns
	}

	// Validate VALUES
	for _, row := range stmt.Values {
		if len(row) != len(targetCols) {
			return &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: fmt.Sprintf("INSERT has %d columns but %d values", len(targetCols), len(row)),
			}
		}

		for i, expr := range row {
			info, err := a.analyzeExpr(expr)
			if err != nil {
				return err
			}

			// Check type compatibility
			if !info.Type.IsComparable(targetCols[i].Type) && info.Type != TypeNull {
				return &AnalysisError{
					Type: ErrTypeMismatch,
					Message: fmt.Sprintf("type mismatch for column %s: expected %s, got %s",
						targetCols[i].Name, targetCols[i].Type, info.Type),
				}
			}
		}
	}

	// Analyze INSERT ... SELECT
	if stmt.Select != nil {
		a.scope.DefineTable(&TableInfo{Name: table.Name, Columns: table.Columns, IsView: table.IsView})
		if err := a.analyzeSelect(stmt.Select); err != nil {
			return err
		}
	}

	return nil
}

// analyzeUpdate analyzes an UPDATE statement.
func (a *Analyzer) analyzeUpdate(stmt *parser.UpdateStmt) error {
	table, ok := a.catalog.GetTable(stmt.Table.Name)
	if !ok {
		return &AnalysisError{
			Type:    ErrTableNotFound,
			Message: fmt.Sprintf("table not found: %s", stmt.Table.Name),
		}
	}

	a.scope.DefineTable(&TableInfo{Name: table.Name, Columns: table.Columns, Alias: stmt.Table.Alias, IsView: table.IsView})

	// Validate SET assignments
	for _, assign := range stmt.Set {
		col, ok := table.GetColumn(assign.Column)
		if !ok {
			return &AnalysisError{
				Type:    ErrColumnNotFound,
				Message: fmt.Sprintf("column not found: %s", assign.Column),
			}
		}

		info, err := a.analyzeExpr(assign.Value)
		if err != nil {
			return err
		}

		if !info.Type.IsComparable(col.Type) && info.Type != TypeNull {
			return &AnalysisError{
				Type: ErrTypeMismatch,
				Message: fmt.Sprintf("type mismatch for column %s: expected %s, got %s",
					col.Name, col.Type, info.Type),
			}
		}
	}

	// Analyze WHERE clause
	if stmt.Where != nil {
		info, err := a.analyzeExpr(stmt.Where)
		if err != nil {
			return err
		}
		if info.IsAggregate {
			return &AnalysisError{
				Type:    ErrAggregateInWhere,
				Message: "aggregate functions not allowed in WHERE clause",
			}
		}
	}

	return nil
}

// analyzeDelete analyzes a DELETE statement.
func (a *Analyzer) analyzeDelete(stmt *parser.DeleteStmt) error {
	table, ok := a.catalog.GetTable(stmt.Table.Name)
	if !ok {
		return &AnalysisError{
			Type:    ErrTableNotFound,
			Message: fmt.Sprintf("table not found: %s", stmt.Table.Name),
		}
	}

	a.scope.DefineTable(&TableInfo{Name: table.Name, Columns: table.Columns, IsView: table.IsView})

	// Analyze WHERE clause
	if stmt.Where != nil {
		info, err := a.analyzeExpr(stmt.Where)
		if err != nil {
			return err
		}
		if info.IsAggregate {
			return &AnalysisError{
				Type:    ErrAggregateInWhere,
				Message: "aggregate functions not allowed in WHERE clause",
			}
		}
	}

	return nil
}

// analyzeCreateTable analyzes a CREATE TABLE statement.
func (a *Analyzer) analyzeCreateTable(stmt *parser.CreateTableStmt) error {
	// Check if table already exists
	if a.catalog.TableExists(stmt.Table.Name) {
		if stmt.IfNotExists {
			return nil // Silently succeed
		}
		return &AnalysisError{
			Type:    ErrTableExists,
			Message: fmt.Sprintf("table already exists: %s", stmt.Table.Name),
		}
	}

	// Build table info
	tableInfo := &TableInfo{
		Name: stmt.Table.Name,
	}

	columnNames := make(map[string]bool)
	for _, colDef := range stmt.Columns {
		upperName := strings.ToUpper(colDef.Name)
		if columnNames[upperName] {
			return &AnalysisError{
				Type:    ErrColumnAmbiguous,
				Message: fmt.Sprintf("duplicate column name: %s", colDef.Name),
			}
		}
		columnNames[upperName] = true

		colInfo := ColumnInfo{
			Name:      colDef.Name,
			Type:      TypeFromName(colDef.Type.Name),
			Nullable:  true,
			TableName: stmt.Table.Name,
		}

		// Process constraints
		for _, constraint := range colDef.Constraints {
			switch constraint.Type {
			case parser.ConstraintPrimaryKey:
				colInfo.PrimaryKey = true
				colInfo.Nullable = false
			case parser.ConstraintNotNull:
				colInfo.Nullable = false
			case parser.ConstraintDefault:
				// Store default value (not evaluated here)
				colInfo.Default = constraint.Default
			}
		}

		tableInfo.Columns = append(tableInfo.Columns, colInfo)
	}

	// Process table-level constraints
	for _, constraint := range stmt.Constraints {
		switch constraint.Type {
		case parser.ConstraintPrimaryKey:
			for _, colName := range constraint.Columns {
				for i := range tableInfo.Columns {
					if strings.EqualFold(tableInfo.Columns[i].Name, colName) {
						tableInfo.Columns[i].PrimaryKey = true
						tableInfo.Columns[i].Nullable = false
					}
				}
			}
		}
	}

	// Analysis is validation-only. Publishing the table before the durable
	// schema write can leave a phantom catalog entry when that write fails.
	return nil
}

// analyzeDropTable analyzes a DROP TABLE statement.
func (a *Analyzer) analyzeDropTable(stmt *parser.DropTableStmt) error {
	for _, tableRef := range stmt.Tables {
		if !a.catalog.TableExists(tableRef.Name) {
			if stmt.IfExists {
				continue // Silently succeed
			}
			return &AnalysisError{
				Type:    ErrTableNotFound,
				Message: fmt.Sprintf("table not found: %s", tableRef.Name),
			}
		}
		if err := a.catalog.DropTable(tableRef.Name); err != nil {
			return err
		}
	}
	return nil
}

// analyzeExpr analyzes an expression and returns type information.
func (a *Analyzer) analyzeExpr(expr parser.Expr) (*ExprInfo, error) {
	switch e := expr.(type) {
	case *parser.LiteralExpr:
		return a.analyzeLiteral(e)
	case *parser.ColumnRef:
		return a.analyzeColumnRef(e)
	case *parser.BinaryExpr:
		return a.analyzeBinaryExpr(e)
	case *parser.UnaryExpr:
		return a.analyzeUnaryExpr(e)
	case *parser.FunctionCall:
		return a.analyzeFunctionCall(e)
	case *parser.ParenExpr:
		return a.analyzeExpr(e.Expr)
	case *parser.CaseExpr:
		return a.analyzeCaseExpr(e)
	case *parser.CastExpr:
		return a.analyzeCastExpr(e)
	case *parser.InExpr:
		return a.analyzeInExpr(e)
	case *parser.BetweenExpr:
		return a.analyzeBetweenExpr(e)
	case *parser.LikeExpr:
		return a.analyzeLikeExpr(e)
	case *parser.IsNullExpr:
		return a.analyzeIsNullExpr(e)
	case *parser.ExistsExpr:
		return a.analyzeExistsExpr(e)
	case *parser.SubqueryExpr:
		return a.analyzeSubqueryExpr(e)
	case *parser.WindowExpr:
		return a.analyzeWindowExpr(e)
	default:
		return &ExprInfo{Type: TypeUnknown}, nil
	}
}

// analyzeWindowExpr analyzes a window function. Window functions are not
// aggregates for the purpose of GROUP BY handling.
func (a *Analyzer) analyzeWindowExpr(e *parser.WindowExpr) (*ExprInfo, error) {
	if e.Func != nil {
		for _, arg := range e.Func.Args {
			if _, err := a.analyzeExpr(arg); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range e.PartitionBy {
		if _, err := a.analyzeExpr(p); err != nil {
			return nil, err
		}
	}
	for _, o := range e.OrderBy {
		if _, err := a.analyzeExpr(o.Expr); err != nil {
			return nil, err
		}
	}
	ret := TypeInteger
	if e.Func != nil {
		if sig, ok := builtinFunctions[e.Func.Name]; ok {
			ret = sig.ReturnType
		}
	}
	return &ExprInfo{Type: ret}, nil
}

func (a *Analyzer) analyzeLiteral(e *parser.LiteralExpr) (*ExprInfo, error) {
	info := &ExprInfo{IsConstant: true}

	switch e.Type {
	case lexer.TokenNumber:
		if strings.Contains(e.Value, ".") || strings.Contains(strings.ToLower(e.Value), "e") {
			info.Type = TypeReal
		} else {
			info.Type = TypeInteger
		}
	case lexer.TokenString:
		info.Type = TypeText
	case lexer.TokenNULL:
		info.Type = TypeNull
		info.Nullable = true
	case lexer.TokenTRUE, lexer.TokenFALSE:
		info.Type = TypeBoolean
	case lexer.TokenStar:
		info.Type = TypeAny
	default:
		info.Type = TypeUnknown
	}

	return info, nil
}

func (a *Analyzer) analyzeColumnRef(e *parser.ColumnRef) (*ExprInfo, error) {
	col, _, ok := a.scope.LookupColumn(e.Table, e.Column)
	if !ok {
		// If no tables are in scope, treat as unknown (for standalone expressions)
		if len(a.scope.GetTables()) == 0 {
			return &ExprInfo{Type: TypeUnknown}, nil
		}
		// Hidden rowid alias (rowid/oid/_rowid_) when the table has no real column
		// of that name. LookupColumn already resolved a real column, so this only
		// fires for the alias (SQLite gives explicit columns precedence).
		if isRowIDAlias(e.Column) {
			if e.Table != "" {
				if _, found := a.scope.LookupTable(e.Table); !found {
					return nil, &AnalysisError{
						Type:    ErrColumnNotFound,
						Message: fmt.Sprintf("column not found: %s", formatColumnRef(e)),
					}
				}
			}
			return &ExprInfo{Type: TypeInteger}, nil
		}
		return nil, &AnalysisError{
			Type:    ErrColumnNotFound,
			Message: fmt.Sprintf("column not found: %s", formatColumnRef(e)),
		}
	}

	return &ExprInfo{
		Type:     col.Type,
		Nullable: col.Nullable,
	}, nil
}

// isRowIDAlias reports whether a column name is a hidden rowid alias (rowid, oid,
// or _rowid_), matching the storage layer's IsRowIDColumn.
func isRowIDAlias(name string) bool {
	switch strings.ToLower(name) {
	case "rowid", "oid", "_rowid_":
		return true
	}
	return false
}

func formatColumnRef(e *parser.ColumnRef) string {
	if e.Table != "" {
		return e.Table + "." + e.Column
	}
	return e.Column
}

func (a *Analyzer) analyzeBinaryExpr(e *parser.BinaryExpr) (*ExprInfo, error) {
	left, err := a.analyzeExpr(e.Left)
	if err != nil {
		return nil, err
	}

	right, err := a.analyzeExpr(e.Right)
	if err != nil {
		return nil, err
	}

	info := &ExprInfo{
		IsAggregate: left.IsAggregate || right.IsAggregate,
		IsConstant:  left.IsConstant && right.IsConstant,
		Nullable:    left.Nullable || right.Nullable,
	}

	switch e.Op {
	case lexer.TokenPlus, lexer.TokenMinus, lexer.TokenStar, lexer.TokenSlash, lexer.TokenPercent:
		// Arithmetic operators. TypeAny comes from derived tables and CTEs whose
		// column types are not tracked, so it is allowed rather than rejected.
		info.Type = CommonType(left.Type, right.Type)
		if !left.Type.IsNumeric() && left.Type != TypeNull && left.Type != TypeUnknown && left.Type != TypeAny {
			return nil, &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: fmt.Sprintf("arithmetic operator requires numeric type, got %s", left.Type),
			}
		}
	case lexer.TokenEq, lexer.TokenNeq, lexer.TokenLt, lexer.TokenLte, lexer.TokenGt, lexer.TokenGte:
		// Comparison operators
		info.Type = TypeBoolean
		if !left.Type.IsComparable(right.Type) {
			return nil, &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: fmt.Sprintf("cannot compare %s with %s", left.Type, right.Type),
			}
		}
	case lexer.TokenAND, lexer.TokenOR:
		// Logical operators
		info.Type = TypeBoolean
	case lexer.TokenConcat:
		// String concatenation
		info.Type = TypeText
	default:
		info.Type = TypeUnknown
	}

	return info, nil
}

func (a *Analyzer) analyzeUnaryExpr(e *parser.UnaryExpr) (*ExprInfo, error) {
	operand, err := a.analyzeExpr(e.Operand)
	if err != nil {
		return nil, err
	}

	info := &ExprInfo{
		IsAggregate: operand.IsAggregate,
		IsConstant:  operand.IsConstant,
		Nullable:    operand.Nullable,
	}

	switch e.Op {
	case lexer.TokenPlus:
		// Unary + is a no-op in SQLite — passes any type through unchanged.
		info.Type = operand.Type
	case lexer.TokenMinus:
		info.Type = operand.Type
		if !operand.Type.IsNumeric() && operand.Type != TypeNull && operand.Type != TypeUnknown {
			return nil, &AnalysisError{
				Type:    ErrTypeMismatch,
				Message: fmt.Sprintf("unary - requires numeric type, got %s", operand.Type),
			}
		}
	case lexer.TokenNOT:
		info.Type = TypeBoolean
	default:
		info.Type = operand.Type
	}

	return info, nil
}

func (a *Analyzer) analyzeFunctionCall(e *parser.FunctionCall) (*ExprInfo, error) {
	sig, ok := LookupFunction(e.Name)
	if !ok {
		return nil, &AnalysisError{
			Type:    ErrInvalidFunction,
			Message: fmt.Sprintf("unknown function: %s", e.Name),
		}
	}

	// Handle COUNT(*)
	argCount := len(e.Args)
	if e.Star {
		argCount = 0 // COUNT(*) has 0 real args
	}

	// Check argument count
	if argCount < sig.MinArgs {
		return nil, &AnalysisError{
			Type:    ErrInvalidArgCount,
			Message: fmt.Sprintf("function %s requires at least %d arguments, got %d", e.Name, sig.MinArgs, argCount),
		}
	}
	if sig.MaxArgs >= 0 && argCount > sig.MaxArgs {
		return nil, &AnalysisError{
			Type:    ErrInvalidArgCount,
			Message: fmt.Sprintf("function %s accepts at most %d arguments, got %d", e.Name, sig.MaxArgs, argCount),
		}
	}

	// Analyze arguments
	info := &ExprInfo{
		Type:        sig.ReturnType,
		IsAggregate: sig.IsAggregate,
	}

	for _, arg := range e.Args {
		argInfo, err := a.analyzeExpr(arg)
		if err != nil {
			return nil, err
		}
		if argInfo.Nullable {
			info.Nullable = true
		}
		// Propagate aggregate status from arguments
		if argInfo.IsAggregate && !sig.IsAggregate {
			info.IsAggregate = true
		}
	}

	// Special case: MIN/MAX/COALESCE return type depends on argument
	if sig.ReturnType == TypeAny && len(e.Args) > 0 {
		argInfo, _ := a.analyzeExpr(e.Args[0])
		if argInfo != nil {
			info.Type = argInfo.Type
		}
	}

	return info, nil
}

func (a *Analyzer) analyzeCaseExpr(e *parser.CaseExpr) (*ExprInfo, error) {
	info := &ExprInfo{
		Nullable: true, // CASE can return NULL
	}

	// Analyze operand if present (simple CASE)
	if e.Operand != nil {
		opInfo, err := a.analyzeExpr(e.Operand)
		if err != nil {
			return nil, err
		}
		if opInfo.IsAggregate {
			info.IsAggregate = true
		}
	}

	// Analyze WHEN clauses
	var resultType Type
	for _, when := range e.Whens {
		condInfo, err := a.analyzeExpr(when.Condition)
		if err != nil {
			return nil, err
		}
		if condInfo.IsAggregate {
			info.IsAggregate = true
		}

		resInfo, err := a.analyzeExpr(when.Result)
		if err != nil {
			return nil, err
		}
		if resInfo.IsAggregate {
			info.IsAggregate = true
		}

		if resultType == TypeUnknown {
			resultType = resInfo.Type
		} else {
			resultType = CommonType(resultType, resInfo.Type)
		}
	}

	// Analyze ELSE clause
	if e.Else != nil {
		elseInfo, err := a.analyzeExpr(e.Else)
		if err != nil {
			return nil, err
		}
		if elseInfo.IsAggregate {
			info.IsAggregate = true
		}
		resultType = CommonType(resultType, elseInfo.Type)
	}

	info.Type = resultType
	return info, nil
}

func (a *Analyzer) analyzeCastExpr(e *parser.CastExpr) (*ExprInfo, error) {
	exprInfo, err := a.analyzeExpr(e.Expr)
	if err != nil {
		return nil, err
	}

	return &ExprInfo{
		Type:        TypeFromName(e.Type.Name),
		IsAggregate: exprInfo.IsAggregate,
		IsConstant:  exprInfo.IsConstant,
		Nullable:    exprInfo.Nullable,
	}, nil
}

func (a *Analyzer) analyzeInExpr(e *parser.InExpr) (*ExprInfo, error) {
	leftInfo, err := a.analyzeExpr(e.Left)
	if err != nil {
		return nil, err
	}

	info := &ExprInfo{
		Type:        TypeBoolean,
		IsAggregate: leftInfo.IsAggregate,
	}

	// Analyze value list
	for _, val := range e.Values {
		valInfo, err := a.analyzeExpr(val)
		if err != nil {
			return nil, err
		}
		if valInfo.IsAggregate {
			info.IsAggregate = true
		}
	}

	// Analyze subquery
	if e.Subquery != nil {
		subScope := NewScope(a.scope)
		oldScope := a.scope
		a.scope = subScope
		err := a.analyzeSelect(e.Subquery)
		a.scope = oldScope
		if err != nil {
			return nil, err
		}
	}

	return info, nil
}

func (a *Analyzer) analyzeBetweenExpr(e *parser.BetweenExpr) (*ExprInfo, error) {
	leftInfo, err := a.analyzeExpr(e.Left)
	if err != nil {
		return nil, err
	}

	lowInfo, err := a.analyzeExpr(e.Low)
	if err != nil {
		return nil, err
	}

	highInfo, err := a.analyzeExpr(e.High)
	if err != nil {
		return nil, err
	}

	return &ExprInfo{
		Type:        TypeBoolean,
		IsAggregate: leftInfo.IsAggregate || lowInfo.IsAggregate || highInfo.IsAggregate,
		Nullable:    leftInfo.Nullable || lowInfo.Nullable || highInfo.Nullable,
	}, nil
}

func (a *Analyzer) analyzeLikeExpr(e *parser.LikeExpr) (*ExprInfo, error) {
	leftInfo, err := a.analyzeExpr(e.Left)
	if err != nil {
		return nil, err
	}

	patternInfo, err := a.analyzeExpr(e.Pattern)
	if err != nil {
		return nil, err
	}

	info := &ExprInfo{
		Type:        TypeBoolean,
		IsAggregate: leftInfo.IsAggregate || patternInfo.IsAggregate,
		Nullable:    leftInfo.Nullable || patternInfo.Nullable,
	}

	if e.Escape != nil {
		escInfo, err := a.analyzeExpr(e.Escape)
		if err != nil {
			return nil, err
		}
		if escInfo.IsAggregate {
			info.IsAggregate = true
		}
	}

	return info, nil
}

func (a *Analyzer) analyzeIsNullExpr(e *parser.IsNullExpr) (*ExprInfo, error) {
	leftInfo, err := a.analyzeExpr(e.Left)
	if err != nil {
		return nil, err
	}

	return &ExprInfo{
		Type:        TypeBoolean,
		IsAggregate: leftInfo.IsAggregate,
		IsConstant:  leftInfo.IsConstant,
	}, nil
}

func (a *Analyzer) analyzeExistsExpr(e *parser.ExistsExpr) (*ExprInfo, error) {
	// Analyze subquery in its own scope
	subScope := NewScope(a.scope)
	oldScope := a.scope
	a.scope = subScope
	err := a.analyzeSelect(e.Subquery)
	a.scope = oldScope

	if err != nil {
		return nil, err
	}

	return &ExprInfo{
		Type: TypeBoolean,
	}, nil
}

func (a *Analyzer) analyzeSubqueryExpr(e *parser.SubqueryExpr) (*ExprInfo, error) {
	// Analyze subquery in its own scope
	subScope := NewScope(a.scope)
	oldScope := a.scope
	a.scope = subScope
	err := a.analyzeSelect(e.Query)
	a.scope = oldScope

	if err != nil {
		return nil, err
	}

	// Scalar subquery - return type of first column
	// For simplicity, return TypeAny
	return &ExprInfo{
		Type: TypeAny,
	}, nil
}
