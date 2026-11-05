// Package capabilities describes the SQL surface an engine build supports.
//
// The document is machine-readable and versioned separately from the engine
// build. Clients should compare SchemaVersion when parsing it and use
// EngineVersion to pin behavior to a specific PizzaSQL release. Keep this file
// in sync with the tests it advertises: every claim here must be covered by an
// engine test in the same repository.
package capabilities

import (
	"time"

	"github.com/danfragoso/pizzasql-next/pkg/version"
)

// SchemaVersion changes only when the document's shape or semantics change in a
// way clients must handle. Additive fields do not require a bump.
const SchemaVersion = 1

// Feature reports whether a capability is supported and, when it is not,
// whether the engine rejects the syntax or accepts it with different semantics.
type Feature struct {
	Supported bool   `json:"supported"`
	Status    string `json:"status"`
	SQLState  string `json:"sqlstate,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

// Document is the machine-readable capability description served by
// GET /capabilities.
type Document struct {
	SchemaVersion int    `json:"schema_version"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engine_version"`
	Commit        string `json:"commit"`
	GeneratedAt   string `json:"generated_at,omitempty"`

	Protocol      Protocol          `json:"protocol"`
	Notifications Notifications     `json:"notifications"`
	Constraints   Constraints       `json:"constraints"`
	DDL           DDL               `json:"ddl"`
	Transactions  Transactions      `json:"transactions"`
	Types         Types             `json:"types"`
	Parameters    Parameters        `json:"parameters"`
	JSON          JSON              `json:"json"`
	ErrorCodes    map[string]string `json:"error_codes"`
	Documentation Documentation     `json:"documentation"`
}

type Protocol struct {
	PostgresCompat string  `json:"postgres_compat"`
	SQLiteCompat   string  `json:"sqlite_compat"`
	SimpleQuery    bool    `json:"simple_query"`
	ExtendedQuery  bool    `json:"extended_query"`
	Describe       Feature `json:"describe"`
	BinaryResults  Feature `json:"binary_results"`
	BoundedExecute Feature `json:"bounded_execute"`
	ProxyErrors    bool    `json:"proxy_error_relay"`
	TLS            string  `json:"tls"`
}

type Notifications struct {
	Listen         Feature `json:"listen"`
	Unlisten       Feature `json:"unlisten"`
	Notify         Feature `json:"notify"`
	PayloadLimit   int     `json:"payload_limit_bytes"`
	ChannelLimit   int     `json:"channel_limit_bytes"`
	Transactional  bool    `json:"transactional"`
	DurableReplay  bool    `json:"durable_replay"`
	CommitLSNParam string  `json:"commit_lsn_parameter"`
}

type Constraints struct {
	NotNull       Feature `json:"not_null"`
	Unique        Feature `json:"unique"`
	PrimaryKey    Feature `json:"primary_key"`
	CompositePK   Feature `json:"composite_primary_key"`
	ForeignKey    Feature `json:"foreign_key"`
	Check         Feature `json:"check"`
	DefaultValues Feature `json:"default_values"`
}

type DDL struct {
	AddColumn              Feature `json:"add_column"`
	AddColumnBackfill      Feature `json:"add_column_backfill"`
	DropColumn             Feature `json:"drop_column"`
	RenameColumn           Feature `json:"rename_column"`
	RenameColumnIndexes    Feature `json:"rename_column_updates_indexes"`
	CreateTableIfNotExists bool    `json:"create_table_if_not_exists"`
	AddColumnIfNotExists   bool    `json:"add_column_if_not_exists"`
}

type Transactions struct {
	Model             string  `json:"model"`
	Isolation         string  `json:"isolation"`
	SerializationCode string  `json:"serialization_sqlstate"`
	RowLocks          Feature `json:"row_locks"`
	Savepoints        Feature `json:"savepoints"`
	RetryRecommended  bool    `json:"retry_recommended"`
}

type Types struct {
	Integer  bool    `json:"integer"`
	Bigint   bool    `json:"bigint"`
	Real     bool    `json:"real"`
	Text     bool    `json:"text"`
	Blob     bool    `json:"blob"`
	Boolean  bool    `json:"boolean"`
	DateTime bool    `json:"datetime"`
	JSONText bool    `json:"json_as_text"`
	JSONB    Feature `json:"jsonb"`
	Arrays   Feature `json:"arrays"`
	RowID    Feature `json:"rowid"`
}

type Parameters struct {
	Positional    bool    `json:"positional"`
	MaxOID        string  `json:"max_oid"`
	Arrays        Feature `json:"arrays"`
	BinaryNumeric bool    `json:"binary_numeric"`
}

type JSON struct {
	Storage        string  `json:"storage"`
	Functions      string  `json:"functions"`
	ArrowOperators Feature `json:"arrow_operators"`
	InvalidInput   string  `json:"invalid_input"`
}

type Documentation struct {
	Compatibility string `json:"compatibility"`
	Isolation     string `json:"isolation"`
}

// Current returns the capability document for this build.
func Current() *Document {
	return &Document{
		SchemaVersion: SchemaVersion,
		Engine:        "pizzasql",
		EngineVersion: version.String(),
		Commit:        version.Commit,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),

		Protocol: Protocol{
			PostgresCompat: "14.0",
			SQLiteCompat:   "3.35.0",
			SimpleQuery:    true,
			ExtendedQuery:  true,
			Describe:       Feature{Supported: true, Status: "compatibility_no_data", Notes: "statement Describe returns ParameterDescription then NoData; portal Describe returns NoData; Execute still emits the real RowDescription"},
			BinaryResults:  Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "all-text result formats only; per-column format lists are also rejected"},
			BoundedExecute: Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "Execute maxRows must be zero; portals are one-shot, not resumable"},
			ProxyErrors:    true,
			TLS:            "terminated by the managed pgproxy, not the engine listener",
		},

		Notifications: Notifications{
			Listen:         Feature{Supported: true, Status: "enforced"},
			Unlisten:       Feature{Supported: true, Status: "enforced", Notes: "UNLISTEN * removes every subscription"},
			Notify:         Feature{Supported: true, Status: "enforced", Notes: "duplicate (channel,payload) pairs collapse within one transaction"},
			PayloadLimit:   8000,
			ChannelLimit:   63,
			Transactional:  true,
			DurableReplay:  false,
			CommitLSNParam: "pizzasql.lsn",
		},

		Constraints: Constraints{
			NotNull:       Feature{Supported: true, Status: "enforced", SQLState: "23502", Notes: "validated after defaults and assignments; explicit NULL does not select a default"},
			Unique:        Feature{Supported: true, Status: "enforced", SQLState: "23505", Notes: "unique indexes, INLINE UNIQUE, and ON CONFLICT are enforced atomically"},
			PrimaryKey:    Feature{Supported: true, Status: "enforced", SQLState: "23505", Notes: "inline and table-level single-column keys; INTEGER PRIMARY KEY remains the rowid alias"},
			CompositePK:   Feature{Supported: true, Status: "enforced", SQLState: "23505", Notes: "table-level PRIMARY KEY (a, b) with ordered columns; key columns are implicitly NOT NULL (23502) and a composite key is never a rowid alias; two inline PRIMARY KEY declarations or an inline/table-level mix are rejected with 0A000; the key is durable under a length-prefixed encoding and reported by PRAGMA table_info pk ordinals and sqlite_master table SQL, but no implicit index is created for it; a conflict target that names the key columns matches them in any order, and ON CONFLICT DO UPDATE cannot assign a key column (0A000)"},
			ForeignKey:    Feature{Supported: true, Status: "enforced", SQLState: "23503", Notes: "inline REFERENCES and table-level FOREIGN KEY (named or unnamed, single or multi-column) are accepted in CREATE TABLE; the parent table must already exist and the referenced columns must be its PRIMARY KEY or exactly the column set of a UNIQUE index, otherwise DDL fails with 0A000 (foreign key mismatch); a dangling reference is 23503 on INSERT, multi-row INSERT, UPDATE, and parent DELETE/UPDATE, and COMMIT re-validates the transaction's final state so a staged child insert and a staged parent delete cannot both land; MATCH SIMPLE (a NULL component skips the check) and self-references are supported; ON DELETE/ON UPDATE CASCADE, SET NULL, SET DEFAULT, and DEFERRABLE are rejected with 0A000; reported in sqlite_master/sqlite_schema table SQL only (pg_constraint and information_schema foreign keys are not emulated)"},
			Check:         Feature{Supported: true, Status: "enforced", SQLState: "23514", Notes: "inline and table-level CHECK are evaluated on INSERT, multi-row INSERT, UPDATE, and ON CONFLICT DO UPDATE; NULL/unknown results pass; predicates are restricted to deterministic, subquery-free expressions over unqualified columns; exposed in sqlite_master/sqlite_schema table SQL only (pg_constraint and information_schema.check_constraints are not emulated)"},
			DefaultValues: Feature{Supported: true, Status: "evaluated_at_write_time", SQLState: "0A000", Notes: "constant defaults are stored as their evaluated value; CURRENT_TIMESTAMP, CURRENT_DATE, CURRENT_TIME, datetime('now'), date('now'), and time('now') are stored as SQL text and evaluated once per statement on write (once per ALTER statement for an ADD COLUMN backfill), so they are never frozen at DDL time; any other non-constant default is rejected with 0A000"},
		},

		DDL: DDL{
			AddColumn:              Feature{Supported: true, Status: "materializes_defaults", Notes: "adds one nullable column; a constant DEFAULT is written into every existing row, and a whitelisted clock default is evaluated once per ALTER statement, so the rewrite cost is O(rows) and holds the table's exclusive gate"},
			AddColumnBackfill:      Feature{Supported: true, Status: "rewrites_rows", Notes: "a DEFAULT is materialized into existing rows during the rewrite: constants as their stored value, whitelisted clock defaults evaluated at one instant per ALTER statement; ADD COLUMN ... NOT NULL without a DEFAULT fails with 23502 on a non-empty table, ADD COLUMN ... UNIQUE conflicts with 23505 when backfilling a non-NULL default into more than one row (a constant default is predicted before the rewrite, a clock default is validated against the values the rewrite writes), ADD COLUMN ... CHECK fails with 23514 when an existing row or the backfilled default violates the predicate, and the column is published together with its CHECK constraints and UNIQUE index in one atomic schema write"},
			DropColumn:             Feature{Supported: true, Status: "rewrites_rows", Notes: "rejected with 0A000 when a persisted CHECK constraint references the column; the stored predicate text cannot be rewritten"},
			RenameColumn:           Feature{Supported: true, Status: "rewrites_rows", Notes: "rejected with 0A000 when a persisted CHECK constraint or expression index references the column; the stored expression text cannot be rewritten"},
			RenameColumnIndexes:    Feature{Supported: true, Status: "rewrites_index_metadata", Notes: "column references in index definitions are rewritten in place, including composite and unique indexes; index names are unchanged; RENAME COLUMN is rejected when an expression index references the column (drop that index, rename, then recreate it)"},
			CreateTableIfNotExists: true,
			AddColumnIfNotExists:   true,
		},

		Transactions: Transactions{
			Model:             "optimistic compare-batch commit; no MVCC or snapshots",
			Isolation:         "serializable-equivalent for single-row compare-batch commits; write skew is possible across independent rows",
			SerializationCode: "40001",
			RowLocks:          Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "SELECT ... FOR UPDATE/SHARE and NOWAIT/SKIP LOCKED are rejected"},
			Savepoints:        Feature{Supported: true, Status: "enforced"},
			RetryRecommended:  true,
		},

		Types: Types{
			Integer:  true,
			Bigint:   true,
			Real:     true,
			Text:     true,
			Blob:     true,
			Boolean:  true,
			DateTime: true,
			JSONText: true,
			JSONB:    Feature{Supported: false, Status: "type_name_alias", Notes: "JSONB is accepted as a column type name with no native semantics; values are stored as text and reported as TEXT over the wire"},
			Arrays:   Feature{Supported: false, Status: "rejected", SQLState: "0A000"},
			RowID:    Feature{Supported: true, Status: "auto_assigned", Notes: "rowid/oid/_rowid_ are BIGINT over the wire; monotonic when the engine assigns the rowid, stable across UPDATE, not monotonic for client-assigned descending INTEGER PRIMARY KEY values"},
		},

		Parameters: Parameters{
			Positional:    true,
			MaxOID:        "int8/text/bool/bytea/float; arrays are rejected",
			Arrays:        Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "id = ANY($1) is rejected; expand to IN (...) with individual parameters"},
			BinaryNumeric: true,
		},

		JSON: JSON{
			Storage:        "TEXT (JSON and JSONB type names are accepted without native semantics; insert JSON as text)",
			Functions:      "json, json_extract, json_set, json_insert, json_replace, json_remove, json_valid, json_type, json_array, json_array_length, json_object, json_quote, json_group_array; PostgreSQL text compatibility: concat_ws, string_agg, pg_backend_pid",
			ArrowOperators: Feature{Supported: true, Status: "supported", Notes: "-> returns JSON text for a member or element, ->> returns the SQL value (text, number, 1/0 for booleans, NULL for JSON null); right operand is a label, an array index, or a $ path; missing paths and NULL operands return NULL; chaining is left-to-right"},
			InvalidInput:   "json_extract/json_set and ->/->> throw 'malformed JSON'; json_valid returns 0",
		},

		ErrorCodes: map[string]string{
			"23502": "not_null_violation",
			"23505": "unique_violation",
			"23503": "foreign_key_violation",
			"23514": "check_violation",
			"40001": "serialization_failure",
			"0A000": "feature_not_supported",
			"42501": "insufficient_privilege (managed proxy)",
			"42601": "syntax_error",
			"25P02": "in_failed_sql_transaction",
			"08P01": "protocol_violation (managed proxy: TLS required)",
			"53300": "too_many_connections (managed proxy: organization quota)",
			"XX000": "internal_error",
		},

		Documentation: Documentation{
			Compatibility: "https://docs.database.pizza/sql-reference/compatibility/",
			Isolation:     "https://docs.database.pizza/engine/concurrency/",
		},
	}
}
