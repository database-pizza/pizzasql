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
	Transactional          Feature `json:"transactional"`
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
	JSONB          JSONB   `json:"jsonb"`
}

// JSONB describes the native jsonb type: canonical text storage, operators,
// functions, ordering, parameters, and the shapes the engine rejects.
type JSONB struct {
	Canonicalization string  `json:"canonicalization"`
	Ordering         string  `json:"ordering"`
	Operators        Feature `json:"operators"`
	Functions        Feature `json:"functions"`
	SetReturning     Feature `json:"set_returning"`
	Parameters       Feature `json:"parameters"`
	Rejected         Feature `json:"rejected"`
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
			ForeignKey:    Feature{Supported: true, Status: "enforced", SQLState: "23503", Notes: "inline REFERENCES and table-level FOREIGN KEY (named or unnamed, single or multi-column) are accepted in CREATE TABLE; the parent table must already exist and the referenced columns must be its PRIMARY KEY or exactly the column set of a UNIQUE index, otherwise DDL fails with 0A000 (foreign key mismatch); a dangling reference is 23503 on INSERT, multi-row INSERT, UPDATE, and parent DELETE/UPDATE, and COMMIT re-validates the transaction's final state so a staged child insert and a staged parent delete cannot both land; MATCH SIMPLE (a NULL component skips the check) and self-references are supported; the MATCH keyword is not parsed, so MATCH SIMPLE is the only behavior; ON DELETE and ON UPDATE accept NO ACTION, RESTRICT, CASCADE, SET NULL, and SET DEFAULT; cascading actions are applied with the statement in autocommit and no later than COMMIT inside an explicit transaction: CASCADE deletes referencing rows or rewrites their foreign-key columns to the new parent key (transitively, including multi-level and self-referential graphs; a mutual cycle can only exist in a hand-authored catalog because DDL rejects forward references), SET NULL writes NULL and fails with 23502 when a child column is NOT NULL, and SET DEFAULT writes the child column defaults and still enforces NOT NULL, CHECK, UNIQUE, and FOREIGN KEY on the result; inside an explicit transaction a NO ACTION/RESTRICT violation is detected at COMMIT and the statement itself succeeds, while a cascade constraint failure rolls back to the statement savepoint; cascading point DML is O(FK component), not O(1): each cascaded parent row scans the referencing child tables and COMMIT materializes every row of every FK-connected table; DEFERRABLE is rejected with 0A000; foreign keys are always enforced, so PRAGMA foreign_keys reports 1 and rejects OFF (SQLite defaults it to OFF) and PRAGMA foreign_key_list is not emulated; ALTER TABLE DROP/RENAME COLUMN and RENAME TABLE are rejected with 0A000 when a constraint depends on the column or names the table as its parent, and DROP TABLE of a referenced parent (including a self-reference) is rejected with 0A000; writes touching a participating table serialize on one database-wide foreign-key write gate; the clause is reported in sqlite_master/sqlite_schema table SQL, including declared ON DELETE/ON UPDATE actions, but pg_constraint and information_schema foreign keys are not emulated"},
			Check:         Feature{Supported: true, Status: "enforced", SQLState: "23514", Notes: "inline and table-level CHECK are evaluated on INSERT, multi-row INSERT, UPDATE, and ON CONFLICT DO UPDATE; NULL/unknown results pass; predicates are restricted to deterministic, subquery-free expressions over unqualified columns; exposed in sqlite_master/sqlite_schema table SQL only (pg_constraint and information_schema.check_constraints are not emulated)"},
			DefaultValues: Feature{Supported: true, Status: "evaluated_at_write_time", SQLState: "0A000", Notes: "constant defaults are stored as their evaluated value; CURRENT_TIMESTAMP, CURRENT_DATE, CURRENT_TIME, datetime('now'), date('now'), and time('now') are stored as SQL text and evaluated once per statement on write (once per ALTER statement for an ADD COLUMN backfill), so they are never frozen at DDL time; any other non-constant default is rejected with 0A000"},
		},

		DDL: DDL{
			Transactional:          Feature{Supported: true, Status: "staged_until_commit", Notes: "inside an explicit transaction CREATE TABLE, DROP TABLE, ALTER TABLE (add/drop/rename column, rename table), and CREATE/DROP INDEX are staged in a session-scoped catalog overlay: the issuing session reads its own DDL, other sessions keep reading the committed catalog, ROLLBACK discards the overlay and the transaction's rows together, and COMMIT publishes both in one atomic compare-batch write; validations use the staged catalog, and a staged catalog write that conflicts with a concurrently committed DDL fails with 40001; a failed DDL statement leaves the transaction's earlier schema intact; autocommit DDL keeps the same immediate behavior; because a transaction publishes one compare-batch, a DDL row rewrite (ALTER TABLE backfill, DROP TABLE, RENAME TABLE) is bounded by the storage batch ceiling (65535 operations or 64 MiB) and an oversized migration fails at COMMIT with an explicit limit error instead of being split, while the same statement outside a transaction pages its writes"},
			AddColumn:              Feature{Supported: true, Status: "materializes_defaults", Notes: "adds one nullable column; a constant DEFAULT is written into every existing row, and a whitelisted clock default is evaluated once per ALTER statement, so the rewrite cost is O(rows) and holds the table's exclusive gate"},
			AddColumnBackfill:      Feature{Supported: true, Status: "rewrites_rows", Notes: "a DEFAULT is materialized into existing rows during the rewrite: constants as their stored value, whitelisted clock defaults evaluated at one instant per ALTER statement; ADD COLUMN ... NOT NULL without a DEFAULT fails with 23502 on a non-empty table, ADD COLUMN ... UNIQUE conflicts with 23505 when backfilling a non-NULL default into more than one row (a constant default is predicted before the rewrite, a clock default is validated against the values the rewrite writes), ADD COLUMN ... CHECK fails with 23514 when an existing row or the backfilled default violates the predicate, and the column is published together with its CHECK constraints and UNIQUE index in one atomic schema write"},
			DropColumn:             Feature{Supported: true, Status: "rewrites_rows", Notes: "rejected with 0A000 when a persisted CHECK constraint references the column or a FOREIGN KEY depends on it (child column or referenced parent column); the stored constraint text cannot be rewritten"},
			RenameColumn:           Feature{Supported: true, Status: "rewrites_rows", Notes: "rejected with 0A000 when a persisted CHECK constraint, expression index, or FOREIGN KEY depends on the column (child column or referenced parent column); the stored constraint text cannot be rewritten"},
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
			JSONB:    Feature{Supported: true, Status: "native", SQLState: "0A000", Notes: "JSONB is a native column type stored as canonical JSON text: object keys sorted, duplicate keys last-wins, no insignificant whitespace, and numbers normalized to exact decimals (1.50 becomes 1.5, 1e2 becomes 100); the canonical form is idempotent and stable across reloads; malformed jsonb input reports 22P02, equality is canonical-text equality (key order and number spelling do not matter), and JSON/TEXT columns keep their existing text-backed JSON1 behavior; literals, ::jsonb and CAST(x AS JSONB), to_jsonb(any), and text-format and PostgreSQL binary-format jsonb parameters (OID 3802) produce jsonb, canonicalized at Bind; direct projections report OID 3802 while JSON stays TEXT; jsonpath, GIN and expression indexes over jsonb, and wildcard/slice/unbounded jsonb paths are rejected with 0A000; a jsonb value cannot be an array element type (0A000)"},
			Arrays:   Feature{Supported: true, Status: "one_dimensional", SQLState: "0A000", Notes: "column types TEXT[], VARCHAR(n)[], INTEGER[], BIGINT[], SMALLINT[], BOOLEAN[], REAL[], DOUBLE PRECISION[], and NUMERIC[] in CREATE TABLE and ALTER TABLE ADD COLUMN; values are PostgreSQL array text ({a,b},NULL) stored canonically, with NULL and the empty array distinct; literals, ARRAY[...], ::type[] and CAST(x AS type[]), 1-based arr[n], array_length(arr, 1), and unnest(array) as the single FROM table function are supported; multidimensional arrays, arrays of other element types, and ANY/ALL over subqueries are rejected with 0A000; malformed array input reports 22P02 and a non-integer subscript or array_length dimension reports 42804; NUMERIC[] and DECIMAL[] elements are validated but stored as their exact text, so digits above 2^53 and trailing zeros survive, and NUMERIC comparisons are exact (1.10 = 1.1, and digits above 2^53 are not rounded); column types survive derived tables, CTEs, and a derived table or CTE joined on the right for subscripts and comparisons (CHECK constraints and generated columns evaluate through the stateless evaluator without a table schema, so array subscripts there keep the element text); UNIQUE indexes and primary keys still use the canonical raw text, so 1.10 and 1.1 are distinct index keys even though they compare equal"},
			RowID:    Feature{Supported: true, Status: "auto_assigned", Notes: "rowid/oid/_rowid_ are BIGINT over the wire; monotonic when the engine assigns the rowid, stable across UPDATE, not monotonic for client-assigned descending INTEGER PRIMARY KEY values"},
		},

		Parameters: Parameters{
			Positional:    true,
			MaxOID:        "int8/text/bool/bytea/float plus OID 3802 jsonb and the array OIDs text[]/varchar[]/int4[]/int8[]/int2[]/bool[]/float4[]/float8[]/numeric[]",
			Arrays:        Feature{Supported: true, Status: "text_format", SQLState: "0A000", Notes: "a text-format array parameter is bound as a typed array literal, so id = ANY($1) works with OIDs 1009 text[], 1015 varchar[], 1007 int4[], 1016 int8[], 1005 int2[], 1000 bool[], 1021 float4[], 1022 float8[], and 1231 numeric[]; elements are validated and canonicalized; binary-format array parameters are rejected with 0A000 instead of being misread"},
			BinaryNumeric: true,
		},

		JSON: JSON{
			Storage:        "JSON is TEXT with the JSON1 surface; JSONB is a native type stored as canonical JSON text (see types.jsonb)",
			Functions:      "json, json_extract, json_set, json_insert, json_replace, json_remove, json_valid, json_type, json_array, json_array_length, json_object, json_quote, json_group_array; PostgreSQL text compatibility: concat_ws, string_agg, pg_backend_pid",
			ArrowOperators: Feature{Supported: true, Status: "supported", Notes: "on TEXT/JSON, -> returns JSON text for a member or element and ->> returns the SQL value (text, number, 1/0 for booleans, NULL for JSON null); right operand is a label, an array index, or a $ path; missing paths and NULL operands return NULL; chaining is left-to-right; on jsonb, ->> returns PostgreSQL text rendering (true/false, exact decimal numbers) and a JSON null member is SQL NULL"},
			InvalidInput:   "json_extract/json_set and ->/->> on text throw 'malformed JSON'; json_valid returns 0; jsonb input and casts report 22P02",
			JSONB: JSONB{
				Canonicalization: "canonical JSON text: object keys sorted byte-wise, duplicate object keys last-wins, no insignificant whitespace, numbers exact normalized decimals (1.50 becomes 1.5, 1e2 becomes 100; no exponent, no trailing fractional zeros, negative zero becomes 0); canonicalization is idempotent and every write path stores the canonical bytes, so a reload reads back the same text",
				Ordering:         "jsonb equality and comparison operators compare exact canonical text bytes, so key order and number spelling are irrelevant but 1 and 1.0000000000000000001 are distinct and integers above 2^53 are not rounded; DISTINCT and GROUP BY use the canonical value (JSONB columns are canonicalized on read too); ORDER BY follows the engine's existing value comparison on canonical text, where both-numeric pairs compare numerically and every other pair compares byte-wise",
				Operators:        Feature{Supported: true, Status: "supported", SQLState: "42804", Notes: "-> and ->> extract a member/element ($ path also accepted); #> and #>> extract by a text[] path (or a $ path); @> and <@ test containment with PostgreSQL's object/array/scalar rules; ? tests an object key or top-level string array element; ?| and ?& test a text[] of keys; || merges objects, concatenates arrays, and wraps non-arrays as single-element arrays; - deletes an object key, array index, or text[] of keys/elements; #- deletes a path; NULL operands return NULL, missing keys return SQL NULL (a JSON null member is a jsonb 'null' for -> and SQL NULL for ->>), and @> sees a JSON null member as a value; array containment matches each candidate element to a distinct container element; a text-only @> or <@ is rejected with 42804; - with an integer on an object reports 22023; the HTTP /query API substitutes ? textually, so a statement using the ? operator cannot also use ? positional parameters there"},
				Functions:        Feature{Supported: true, Status: "supported", SQLState: "22023", Notes: "jsonb_typeof, jsonb_array_length, jsonb_strip_nulls, jsonb_pretty, jsonb_build_object, jsonb_build_array, to_jsonb, jsonb_set, jsonb_insert; jsonb_set with an empty path leaves the target unchanged, jsonb_set appends a missing array position and clamps a too-low index when create_if_missing is true, jsonb_insert clamps and appends array positions, and jsonb_insert onto an existing object key reports 22023 (cannot replace existing key); the JSON1 jsonb_* aliases (jsonb_set, jsonb_insert, jsonb_replace, jsonb_remove with $ paths) keep their previous value behavior and return canonical jsonb; wrong document kinds report 22023 and malformed input 22P02; the JSON (text) and JSON1 function surface is unchanged"},
				SetReturning:     Feature{Supported: true, Status: "single_from_item", SQLState: "0A000", Notes: "jsonb_each, jsonb_each_text, jsonb_array_elements, jsonb_array_elements_text, and jsonb_object_keys are supported only as the single FROM item (no joins); calling one in a projection is rejected with 0A000; a NULL document produces no rows"},
				Parameters:       Feature{Supported: true, Status: "text_and_binary", SQLState: "22P02", Notes: "text-format OID 3802 parameters are canonicalized at Bind; PostgreSQL binary jsonb parameters (version byte 0x01 followed by the document text, which pgx sends by default) are accepted and canonicalized, while a wrong version or malformed body is 22P02; an invalid document fails at Bind and operators see a real jsonb value; OID 114 (JSON) behavior is unchanged"},
				Rejected:         Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "jsonpath (jsonb_path_*), the @? and @@ jsonpath operators, GIN indexes, expression indexes whose result is jsonb, wildcard/slice/unbounded jsonb paths, and jsonb array element types"},
			},
		},

		ErrorCodes: map[string]string{
			"23502": "not_null_violation",
			"23505": "unique_violation",
			"23503": "foreign_key_violation",
			"23514": "check_violation",
			"40001": "serialization_failure",
			"0A000": "feature_not_supported",
			"22023": "invalid_parameter_value",
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
