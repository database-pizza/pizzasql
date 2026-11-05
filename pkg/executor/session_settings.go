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
		e.searchPath = known
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
		if len(e.searchPath) == 0 {
			value = publicSchemaName
		} else {
			value = strings.Join(e.searchPath, ", ")
		}
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
