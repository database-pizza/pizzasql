package version

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// PostgreSQLCompatVersion is the PostgreSQL server version reported to clients
// for driver compatibility. It is intentionally independent of the PizzaSQL
// build version returned by String() and pizzasql_version().
const PostgreSQLCompatVersion = "14.0"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
	Dirty   = ""
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if Commit == "unknown" && setting.Value != "" {
				Commit = shortCommit(setting.Value)
			}
		case "vcs.time":
			if Date == "unknown" && setting.Value != "" {
				Date = setting.Value
			}
		case "vcs.modified":
			if setting.Value == "true" {
				Dirty = "-dirty"
			}
		}
	}
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func String() string {
	v := Version
	if v == "" || v == "dev" {
		v = Commit
	}
	if v == "" || v == "unknown" {
		v = "dev"
	}
	if Dirty != "" && !strings.HasSuffix(v, Dirty) {
		v += Dirty
	}
	if Date == "" || Date == "unknown" {
		return v
	}
	return fmt.Sprintf("%s_%s", v, normalizeDate(Date))
}

func normalizeDate(date string) string {
	d := strings.TrimSpace(date)
	if idx := strings.LastIndexAny(d, "+-"); idx > 0 {
		d = d[:idx] + "TZ" + d[idx+1:]
	}
	d = strings.ReplaceAll(d, "-", "_")
	d = strings.ReplaceAll(d, ":", "_")
	d = strings.ReplaceAll(d, " ", "_")
	return d
}

func PrometheusLabel() string {
	return strings.ReplaceAll(String(), "\"", "")
}
