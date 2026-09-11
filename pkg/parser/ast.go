package parser

import "github.com/danfragoso/pizzasql-next/pkg/lexer"

// Node is the base interface for all AST nodes.
type Node interface {
	node()
}

// Statement represents a SQL statement.
type Statement interface {
	Node
	stmtNode()
}

// Expr represents an expression.
type Expr interface {
	Node
	exprNode()
}

// SetOpType represents a set operation type.
type SetOpType int

const (
	SetOpUnion SetOpType = iota
	SetOpUnionAll
	SetOpIntersect
	SetOpExcept
)

// CompoundSelect chains two SELECT statements with a set operation.
type CompoundSelect struct {
	Left    *SelectStmt
	Op      SetOpType
	Right   *SelectStmt // may itself have Compound set for chained ops
	OrderBy []OrderByItem
	Limit   Expr
	Offset  Expr
}

func (c *CompoundSelect) node()     {}
func (c *CompoundSelect) stmtNode() {}

// SelectStmt represents a SELECT statement.
type SelectStmt struct {
	Distinct bool
	Columns  []SelectColumn
	From     []TableRef
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderByItem
	Limit    Expr
	Offset   Expr
	// Compound chains a set operation onto this SELECT (UNION/INTERSECT/EXCEPT).
	Compound *CompoundSelect
	// With holds common table expressions that must be materialized before this
	// SELECT runs. Non-recursive CTEs are desugared by the parser instead; this
	// list carries recursive CTEs (and the CTEs that depend on them).
	With []*CTE
}

// CTE is a common table expression from a WITH clause.
type CTE struct {
	Name      string
	Columns   []string
	Recursive bool
	Query     *SelectStmt
}

func (s *SelectStmt) node()     {}
func (s *SelectStmt) stmtNode() {}

// SelectColumn represents a column in SELECT.
type SelectColumn struct {
	Expr      Expr
	Alias     string
	Star      bool   // true if this is *
	TableStar string // table name/alias if this is a qualified wildcard (table.*)
}

// TableRef represents a table reference.
type TableRef struct {
	Schema   string
	Name     string
	Alias    string
	Subquery *SelectStmt // for derived tables (SELECT ... FROM (SELECT ...) AS alias)
	Join     *JoinClause // for joined tables
}

// JoinClause represents a JOIN clause.
type JoinClause struct {
	Type      JoinType
	Table     *TableRef
	Condition Expr     // ON condition
	Using     []string // USING columns
}

// JoinType represents the type of JOIN.
type JoinType int

const (
	JoinInner JoinType = iota
	JoinLeft
	JoinRight
	JoinFull
	JoinCross
)

// NullsOrder selects where NULLs sort in an ORDER BY item.
type NullsOrder int

const (
	NullsDefault NullsOrder = iota // SQLite default: NULLs are smallest
	NullsFirst
	NullsLast
)

// OrderByItem represents an ORDER BY item.
type OrderByItem struct {
	Expr       Expr
	Desc       bool
	NullsOrder NullsOrder
}

// ConflictAction represents the action to take on conflict.
type ConflictAction int

const (
	ConflictAbort    ConflictAction = iota // Default
	ConflictReplace                        // INSERT OR REPLACE
	ConflictIgnore                         // INSERT OR IGNORE
	ConflictFail                           // INSERT OR FAIL
	ConflictRollback                       // INSERT OR ROLLBACK
)

// InsertStmt represents an INSERT statement.
type InsertStmt struct {
	Table             *TableRef
	Columns           []string
	Values            [][]Expr
	Select            *SelectStmt    // INSERT ... SELECT
	OnConflict        ConflictAction // OR REPLACE/IGNORE/etc.
	ConflictTarget    []string
	ConflictUpdate    []Assignment
	ConflictDoNothing bool
	Returning         []SelectColumn
}

func (s *InsertStmt) node()     {}
func (s *InsertStmt) stmtNode() {}

// UpdateStmt represents an UPDATE statement.
type UpdateStmt struct {
	Table     *TableRef
	Set       []Assignment
	From      []TableRef
	Where     Expr
	Returning []SelectColumn
}

func (s *UpdateStmt) node()     {}
func (s *UpdateStmt) stmtNode() {}

// Assignment represents a SET assignment.
type Assignment struct {
	Column string
	Value  Expr
}

// DeleteStmt represents a DELETE statement.
type DeleteStmt struct {
	Table     *TableRef
	Where     Expr
	Returning []SelectColumn
}

func (s *DeleteStmt) node()     {}
func (s *DeleteStmt) stmtNode() {}

// CreateTableStmt represents a CREATE TABLE statement.
type CreateTableStmt struct {
	IfNotExists bool
	Table       *TableRef
	Columns     []ColumnDef
	Constraints []TableConstraint
}

func (s *CreateTableStmt) node()     {}
func (s *CreateTableStmt) stmtNode() {}

// ColumnDef represents a column definition.
type ColumnDef struct {
	Name        string
	Type        DataType
	Constraints []ColumnConstraint
	// GeneratedExpr is non-nil for a GENERATED ALWAYS AS (expr) column. A
	// generated column's value is computed rather than supplied by the user.
	GeneratedExpr   Expr
	GeneratedStored bool // true for STORED, false for VIRTUAL
}

// DataType represents a SQL data type.
type DataType struct {
	Name      string
	Precision int // for VARCHAR(n), NUMERIC(p,s)
	Scale     int // for NUMERIC(p,s)
}

// ColumnConstraint represents a column-level constraint.
type ColumnConstraint struct {
	Type      ConstraintType
	Name      string // optional constraint name
	Default   Expr   // for DEFAULT
	RefTable  string // for REFERENCES
	RefColumn string // for REFERENCES
	Check     Expr   // for CHECK
	// OnConflict is the conflict resolution algorithm declared with
	// ON CONFLICT REPLACE/etc. (ConflictAbort is the zero value/default).
	OnConflict    ConflictAction
	HasOnConflict bool
}

// ConstraintType represents the type of constraint.
type ConstraintType int

const (
	ConstraintPrimaryKey ConstraintType = iota
	ConstraintNotNull
	ConstraintUnique
	ConstraintDefault
	ConstraintCheck
	ConstraintForeignKey
	ConstraintAutoIncrement
)

// TableConstraint represents a table-level constraint.
type TableConstraint struct {
	Type       ConstraintType
	Name       string   // optional constraint name
	Columns    []string // columns involved
	RefTable   string   // for FOREIGN KEY
	RefColumns []string // for FOREIGN KEY
	Check      Expr     // for CHECK
	// OnConflict is the conflict resolution declared with ON CONFLICT
	// REPLACE/etc.; HasOnConflict distinguishes it from the default ABORT.
	OnConflict    ConflictAction
	HasOnConflict bool
}

// DropTableStmt represents a DROP TABLE statement.
type DropTableStmt struct {
	IfExists bool
	Tables   []*TableRef
}

func (s *DropTableStmt) node()     {}
func (s *DropTableStmt) stmtNode() {}

// CreateIndexStmt represents a CREATE INDEX statement.
type CreateIndexStmt struct {
	IfNotExists bool
	Unique      bool
	Name        string
	Table       string
	Columns     []IndexColumn
}

func (s *CreateIndexStmt) node()     {}
func (s *CreateIndexStmt) stmtNode() {}

// IndexColumn represents a column in an index. Expr is set for expression
// indexes (e.g. an index on lower(email)); a plain column index leaves it nil
// and uses Name.
type IndexColumn struct {
	Name string
	Desc bool // true for DESC ordering
	Expr Expr
}

// DropIndexStmt represents a DROP INDEX statement.
type DropIndexStmt struct {
	IfExists bool
	Name     string
}

func (s *DropIndexStmt) node()     {}
func (s *DropIndexStmt) stmtNode() {}

// CreateViewStmt represents a CREATE VIEW statement.
type CreateViewStmt struct {
	IfNotExists bool
	View        *TableRef
	Select      *SelectStmt
}

func (s *CreateViewStmt) node()     {}
func (s *CreateViewStmt) stmtNode() {}

// DropViewStmt represents a DROP VIEW statement.
type DropViewStmt struct {
	IfExists bool
	Views    []*TableRef
}

func (s *DropViewStmt) node()     {}
func (s *DropViewStmt) stmtNode() {}

// AlterTableStmt represents an ALTER TABLE statement.
type AlterTableStmt struct {
	Table  string
	Action AlterAction
}

func (s *AlterTableStmt) node()     {}
func (s *AlterTableStmt) stmtNode() {}

// AlterAction represents an ALTER TABLE action.
type AlterAction interface {
	Node
	alterAction()
}

// AddColumnAction represents ADD COLUMN action.
type AddColumnAction struct {
	Column      *ColumnDef
	IfNotExists bool
}

func (a *AddColumnAction) node()        {}
func (a *AddColumnAction) alterAction() {}

// DropColumnAction represents DROP COLUMN action.
type DropColumnAction struct {
	Column string
}

func (a *DropColumnAction) node()        {}
func (a *DropColumnAction) alterAction() {}

// RenameTableAction represents RENAME TO action.
type RenameTableAction struct {
	NewName string
}

func (a *RenameTableAction) node()        {}
func (a *RenameTableAction) alterAction() {}

// RenameColumnAction represents RENAME COLUMN action.
type RenameColumnAction struct {
	OldName string
	NewName string
}

func (a *RenameColumnAction) node()        {}
func (a *RenameColumnAction) alterAction() {}

// AttachStmt represents an ATTACH DATABASE statement.
type AttachStmt struct {
	FilePath string // Database file path or identifier
	Alias    string // Database alias name
}

func (s *AttachStmt) node()     {}
func (s *AttachStmt) stmtNode() {}

// DetachStmt represents a DETACH DATABASE statement.
type DetachStmt struct {
	Alias string // Database alias to detach
}

func (s *DetachStmt) node()     {}
func (s *DetachStmt) stmtNode() {}

// PragmaStmt represents a PRAGMA statement.
type PragmaStmt struct {
	Name  string // pragma name (e.g., "table_info")
	Arg   string // optional argument (e.g., table name)
	Value Expr   // optional value for SET pragmas
}

func (s *PragmaStmt) node()     {}
func (s *PragmaStmt) stmtNode() {}

// AnalyzeStmt represents an ANALYZE statement. PizzaSQL does not maintain
// optimizer statistics, so it is accepted and executed as a documented no-op.
type AnalyzeStmt struct {
	Name string // optional table name
}

func (s *AnalyzeStmt) node()     {}
func (s *AnalyzeStmt) stmtNode() {}

// ExplainStmt represents an EXPLAIN statement.
type ExplainStmt struct {
	QueryPlan bool      // true for EXPLAIN QUERY PLAN
	Statement Statement // the statement being explained
}

func (s *ExplainStmt) node()     {}
func (s *ExplainStmt) stmtNode() {}

// Transaction statements

// BeginStmt represents a BEGIN TRANSACTION statement.
type BeginStmt struct {
	// Transaction mode (DEFERRED, IMMEDIATE, EXCLUSIVE) - for future use
	Mode string
}

func (s *BeginStmt) node()     {}
func (s *BeginStmt) stmtNode() {}

// CommitStmt represents a COMMIT statement.
type CommitStmt struct{}

func (s *CommitStmt) node()     {}
func (s *CommitStmt) stmtNode() {}

// RollbackStmt represents a ROLLBACK statement.
type RollbackStmt struct {
	Savepoint string // for ROLLBACK TO SAVEPOINT
}

func (s *RollbackStmt) node()     {}
func (s *RollbackStmt) stmtNode() {}

// SavepointStmt represents a SAVEPOINT statement.
type SavepointStmt struct {
	Name string
}

func (s *SavepointStmt) node()     {}
func (s *SavepointStmt) stmtNode() {}

// ReleaseStmt represents a RELEASE SAVEPOINT statement.
type ReleaseStmt struct {
	Name string
}

func (s *ReleaseStmt) node()     {}
func (s *ReleaseStmt) stmtNode() {}

// Expression types

// BinaryExpr represents a binary expression.
type BinaryExpr struct {
	Left  Expr
	Op    lexer.TokenType
	Right Expr
}

func (e *BinaryExpr) node()     {}
func (e *BinaryExpr) exprNode() {}

// UnaryExpr represents a unary expression.
type UnaryExpr struct {
	Op      lexer.TokenType
	Operand Expr
}

func (e *UnaryExpr) node()     {}
func (e *UnaryExpr) exprNode() {}

// LiteralExpr represents a literal value.
type LiteralExpr struct {
	Type  lexer.TokenType // TokenNumber, TokenString, TokenNULL, TokenTRUE, TokenFALSE
	Value string
}

func (e *LiteralExpr) node()     {}
func (e *LiteralExpr) exprNode() {}

// ColumnRef represents a column reference.
type ColumnRef struct {
	Table  string
	Column string
}

func (e *ColumnRef) node()     {}
func (e *ColumnRef) exprNode() {}

// FunctionCall represents a function call.
type FunctionCall struct {
	Name     string
	Args     []Expr
	Distinct bool // for COUNT(DISTINCT x)
	Star     bool // for COUNT(*)
}

func (e *FunctionCall) node()     {}
func (e *FunctionCall) exprNode() {}

// WindowExpr represents a function call with an OVER clause.
type WindowExpr struct {
	Func        *FunctionCall
	PartitionBy []Expr
	OrderBy     []OrderByItem
}

func (e *WindowExpr) node()     {}
func (e *WindowExpr) exprNode() {}

// SubqueryExpr represents a subquery expression.
type SubqueryExpr struct {
	Query *SelectStmt
}

func (e *SubqueryExpr) node()     {}
func (e *SubqueryExpr) exprNode() {}

// CaseExpr represents a CASE expression.
type CaseExpr struct {
	Operand Expr // for CASE operand WHEN...
	Whens   []WhenClause
	Else    Expr
}

func (e *CaseExpr) node()     {}
func (e *CaseExpr) exprNode() {}

// WhenClause represents a WHEN clause in CASE.
type WhenClause struct {
	Condition Expr
	Result    Expr
}

// InExpr represents an IN expression.
type InExpr struct {
	Left     Expr
	Not      bool
	Values   []Expr      // IN (1, 2, 3)
	Subquery *SelectStmt // IN (SELECT ...)
}

func (e *InExpr) node()     {}
func (e *InExpr) exprNode() {}

// BetweenExpr represents a BETWEEN expression.
type BetweenExpr struct {
	Left Expr
	Not  bool
	Low  Expr
	High Expr
}

func (e *BetweenExpr) node()     {}
func (e *BetweenExpr) exprNode() {}

// LikeExpr represents a LIKE expression.
type LikeExpr struct {
	Left    Expr
	Not     bool
	Pattern Expr
	Escape  Expr
}

func (e *LikeExpr) node()     {}
func (e *LikeExpr) exprNode() {}

// IsNullExpr represents an IS NULL expression.
type IsNullExpr struct {
	Left Expr
	Not  bool
}

func (e *IsNullExpr) node()     {}
func (e *IsNullExpr) exprNode() {}

// IsDistinctExpr represents `left IS DISTINCT FROM right` (Not=false) or
// `left IS NOT DISTINCT FROM right` (Not=true). Unlike `=`, NULLs compare
// equal to each other and distinct from non-NULLs.
type IsDistinctExpr struct {
	Left  Expr
	Right Expr
	Not   bool
}

func (e *IsDistinctExpr) node()     {}
func (e *IsDistinctExpr) exprNode() {}

// CastExpr represents a CAST expression.
type CastExpr struct {
	Expr Expr
	Type DataType
}

func (e *CastExpr) node()     {}
func (e *CastExpr) exprNode() {}

// ExistsExpr represents an EXISTS expression.
type ExistsExpr struct {
	Subquery *SelectStmt
}

func (e *ExistsExpr) node()     {}
func (e *ExistsExpr) exprNode() {}

// ParenExpr represents a parenthesized expression.
type ParenExpr struct {
	Expr Expr
}

func (e *ParenExpr) node()     {}
func (e *ParenExpr) exprNode() {}
