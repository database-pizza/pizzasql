package executor

import (
	"strings"

	"github.com/danfragoso/pizzasql-next/pkg/parser"
	"github.com/danfragoso/pizzasql-next/pkg/pgerror"
	"github.com/danfragoso/pizzasql-next/pkg/version"
)

// Session settings.
//
// Drivers and ORMs open a connection with SET statements for parameters that
// have no PizzaSQL equivalent (client_encoding, extra_float_digits,
// statement_timeout, ...). Rejecting them makes an unmodified client unusable,
// so the engine acknowledges every SET and retains only search_path, which
// current_schema() and SHOW report. The engine has no other configurable
// settings; SHOW returns honest constants for the values it exposes.

// normalizeSettingName lowercases a setting name and collapses whitespace so
// SET TIME ZONE and SHOW time zone agree.
func normalizeSettingName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// effectiveSearchPath is the search path the session currently observes: SET
// LOCAL while a transaction is open, otherwise the session value.
func (e *Executor) effectiveSearchPath() []string {
	if len(e.localSearchPath) > 0 {
		return e.localSearchPath
	}
	if len(e.searchPath) == 0 {
		return []string{publicSchemaName}
	}
	return e.searchPath
}

// beginSettingScope records the session search path so ROLLBACK can restore it
// and clears any SET LOCAL left by a previous transaction.
func (e *Executor) beginSettingScope() {
	e.txSearchPath = e.searchPath
	e.localSearchPath = nil
}

// commitSettingScope ends a transaction: SET LOCAL expires and a session SET
// stays in effect.
func (e *Executor) commitSettingScope() {
	e.localSearchPath = nil
	e.txSearchPath = nil
	e.pgCatalog = nil
}

// rollbackSettingScope restores the session search path captured at BEGIN, so a
// session SET inside an aborted transaction has no lasting effect.
func (e *Executor) rollbackSettingScope() {
	if e.txSearchPath != nil {
		e.searchPath = e.txSearchPath
	}
	e.localSearchPath = nil
	e.txSearchPath = nil
	e.pgCatalog = nil
}

// executeSet acknowledges a session setting.
func (e *Executor) executeSet(stmt *parser.SetStmt) (*Result, error) {
	name := normalizeSettingName(stmt.Name)
	if name == "search_path" {
		var entries []string
		for _, value := range stmt.Values {
			for _, part := range strings.Split(value, ",") {
				part = strings.Trim(strings.TrimSpace(part), `"`)
				if part != "" {
					entries = append(entries, part)
				}
			}
		}
		// Only the emulated schemas exist; drop anything else so SHOW and
		// current_schema() describe a schema the engine can actually resolve.
		known := make([]string, 0, len(entries))
		for _, entry := range entries {
			switch strings.ToLower(entry) {
			case publicSchemaName, pgCatalogSchemaName, informationSchemaName:
				known = append(known, strings.ToLower(entry))
			}
		}
		if len(known) == 0 {
			known = []string{publicSchemaName}
		}
		switch {
		case stmt.Local:
			// SET LOCAL is transaction-scoped: it has no effect outside a
			// transaction block and expires at COMMIT or ROLLBACK.
			if e.inTransaction {
				e.localSearchPath = known
			}
		default:
			// A session SET inside a transaction is undone by ROLLBACK, so
			// BEGIN keeps the previous value.
			e.searchPath = known
			e.localSearchPath = nil
		}
		// A different search path changes which schema is visible; drop the
		// statement-scoped catalog snapshot so the next read re-evaluates it.
		e.pgCatalog = nil
	}
	return NewResult("SET"), nil
}

// executeShow returns the value of a setting. Values that the engine does not
// model are reported as constants that describe the actual behavior, and
// unknown names fail rather than inventing a value.
func (e *Executor) executeShow(stmt *parser.ShowStmt) (*Result, error) {
	name := normalizeSettingName(stmt.Name)
	value, err := e.settingValue(name)
	if err != nil {
		return nil, err
	}

	result := NewResult("SHOW")
	result.AddColumnWithType(name, "TEXT")
	result.AddRow(value)
	return result, nil
}

// settingValue reports the value of a setting exposed by SHOW and
// current_setting(). Unknown names are rejected rather than invented.
func (e *Executor) settingValue(name string) (string, error) {
	name = normalizeSettingName(name)
	var value string
	switch name {
	case "search_path":
		value = strings.Join(e.effectiveSearchPath(), ", ")
	case "server_version":
		value = version.PostgreSQLCompatVersion
	case "server_version_num":
		// PostgreSQL encodes major*10000 + minor*100.
		value = "140000"
	case "server_encoding", "client_encoding":
		value = "UTF8"
	case "transaction_isolation", "default_transaction_isolation":
		value = "read committed"
	case "standard_conforming_strings", "integer_datetimes":
		value = "on"
	case "datestyle":
		value = "ISO, MDY"
	case "timezone", "time zone":
		value = "UTC"
	case "application_name":
		value = ""
	case "is_superuser":
		value = "off"
	default:
		return "", pgerror.Unsupported("unrecognized configuration parameter %q", name)
	}
	return value, nil
}
