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
	PostgresCompat string `json:"postgres_compat"`
	SQLiteCompat   string `json:"sqlite_compat"`
	SimpleQuery    bool   `json:"simple_query"`
	ExtendedQuery  bool   `json:"extended_query"`
	TLS            string `json:"tls"`
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
			NotNull:       Feature{Supported: true, Status: "enforced"},
			Unique:        Feature{Supported: true, Status: "enforced", SQLState: "23505", Notes: "unique indexes, INLINE UNIQUE, and ON CONFLICT are enforced atomically"},
			PrimaryKey:    Feature{Supported: true, Status: "enforced", Notes: "single-column only"},
			CompositePK:   Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "declare a single-column primary key"},
			ForeignKey:    Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "inline and table-level FOREIGN KEY are rejected; enforce referential integrity in the application, cascades are not implemented"},
			Check:         Feature{Supported: false, Status: "rejected", SQLState: "0A000", Notes: "inline and table-level CHECK are rejected; enforce the rule in the application"},
			DefaultValues: Feature{Supported: true, Status: "constant_only", SQLState: "0A000", Notes: "non-constant defaults (CURRENT_TIMESTAMP, function calls) are rejected instead of being frozen at DDL time"},
		},

		DDL: DDL{
			AddColumn:              Feature{Supported: true, Status: "metadata_only"},
			AddColumnBackfill:      Feature{Supported: false, Status: "no_backfill", Notes: "existing rows read NULL; a DEFAULT applies to later INSERTs only"},
			DropColumn:             Feature{Supported: true, Status: "rewrites_rows"},
			RenameColumn:           Feature{Supported: true, Status: "rewrites_rows"},
			RenameColumnIndexes:    Feature{Supported: false, Status: "stale_index_metadata", Notes: "drop and recreate indexes that reference a renamed column"},
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
			Functions:      "json, json_extract, json_set, json_insert, json_replace, json_remove, json_valid, json_type, json_array, json_object, json_quote, json_group_array",
			ArrowOperators: Feature{Supported: false, Status: "rejected", SQLState: "42601", Notes: "-> and ->> are not implemented; use json_extract"},
			InvalidInput:   "json_extract/json_set throw 'malformed JSON'; json_valid returns 0",
		},

		ErrorCodes: map[string]string{
			"23505": "unique_violation",
			"40001": "serialization_failure",
			"0A000": "feature_not_supported",
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
