package executor

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SQLiteCompatVersion is the SQLite version reported by sqlite_version(). It is
// deliberately decoupled from PizzaSQL's own version (pizzasql_version()) so
// clients that gate behavior on a minimum SQLite version see a consistent,
// documented compatibility floor. 3.35.0 is the first release with
// INSERT/UPDATE/DELETE ... RETURNING, which GoatCounter's release-2.7 port
// relies on; the JSON1 and generated-column surface this engine implements is
// documented against that same floor.
const SQLiteCompatVersion = "3.35.0"

// evalPercentDiff implements the scalar percent_diff(start, final) function used
// by GoatCounter's hit_list.DiffTotal query. It matches the existing function:
// a zero start yields +Inf, and standard SQL NULL propagation applies. Fewer or
// more than two arguments is an error.
func evalPercentDiff(args []interface{}) (interface{}, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("percent_diff() requires exactly 2 arguments")
	}
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	start := toFloat(args[0])
	final := toFloat(args[1])
	if start == 0 {
		return math.Inf(1), nil
	}
	return (final - start) / start * 100.0, nil
}

// formatSQLiteReal renders a REAL the way SQLite's text conversion does for
// concatenation: the shortest round-tripping decimal, with a fractional part
// retained for integral values so `1.0 || 'px'` yields "1.0px" like SQLite. The
// generated-size queries concatenate numeric columns, so this must not fall back
// to Go's "1" or "true"/"1e+06" spellings.
func formatSQLiteReal(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	// SQLite's %!.15g keeps 15 significant digits and always shows a decimal
	// point for non-integral values; integral values get a trailing ".0".
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// sqliteCurrentTimeValue resolves the SQLite special date/time keywords
// CURRENT_TIMESTAMP, CURRENT_DATE, and CURRENT_TIME. The lexer treats them as
// plain identifiers, so they are recognized here (only when the row has no real
// column of that name) to support DEFAULT current_timestamp and expressions
// like strftime('%Y', current_timestamp).
func sqliteCurrentTimeValue(name string) (interface{}, bool) {
	now := time.Now().UTC()
	switch strings.ToLower(name) {
	case "current_timestamp":
		return now.Format("2006-01-02 15:04:05"), true
	case "current_date":
		return now.Format("2006-01-02"), true
	case "current_time":
		return now.Format("15:04:05"), true
	}
	return nil, false
}
