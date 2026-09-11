package executor

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// julianDayToTime converts a Julian Day Number to time.Time (UTC).
// Julian day 2440587.5 = 1970-01-01 00:00:00 UTC.
func julianDayToTime(jd float64) time.Time {
	unixSec := (jd - 2440587.5) * 86400.0
	sec := int64(unixSec)
	nsec := int64(math.Round((unixSec - float64(sec)) * 1e9))
	if nsec < 0 {
		sec--
		nsec += 1e9
	}
	return time.Unix(sec, nsec).UTC()
}

func timeToJulianDay(t time.Time) float64 {
	return float64(t.UnixNano())/86400e9 + 2440587.5
}

// parseISO8601 parses the ISO-8601 subsets that SQLite supports.
func parseISO8601(s string) (time.Time, error) {
	loc := time.UTC
	core := s

	// Strip trailing Z
	if len(core) > 0 && (core[len(core)-1] == 'Z' || core[len(core)-1] == 'z') {
		core = core[:len(core)-1]
	} else {
		// Strip ±HH:MM timezone suffix (only if after at least YYYY-MM-DD)
		if len(core) >= 16 {
			for i := len(core) - 6; i >= 10; i-- {
				if core[i] == '+' || core[i] == '-' {
					possible := core[i:]
					if len(possible) == 6 && possible[3] == ':' {
						var hh, mm int
						fmt.Sscanf(possible[1:], "%d:%d", &hh, &mm)
						sign := 1
						if possible[0] == '-' {
							sign = -1
						}
						offset := sign * (hh*3600 + mm*60)
						loc = time.FixedZone("", offset)
						core = core[:i]
						break
					}
				}
			}
		}
	}

	// Normalize T separator to space
	core = strings.Replace(core, "T", " ", 1)
	timeOnly := !strings.Contains(core, "-")

	layouts := []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"15:04:05.999999999",
		"15:04:05",
		"15:04",
	}

	for _, layout := range layouts {
		t, err := time.ParseInLocation(layout, core, loc)
		if err == nil {
			if timeOnly {
				t = time.Date(2000, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
			}
			return t.UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse time value: %q", s)
}

// parseDateArgs extracts time.Time and modifier strings from evaluated function args.
// Handles 'now' default, numeric (Julian/Unix with modifier), and ISO-8601 text.
func parseDateArgs(args []interface{}) (time.Time, []string, error) {
	if len(args) == 0 {
		return time.Now().UTC(), nil, nil
	}

	mods := make([]string, 0, len(args)-1)
	for _, a := range args[1:] {
		mods = append(mods, toString(a))
	}

	firstStr := strings.TrimSpace(toString(args[0]))
	firstLower := strings.ToLower(firstStr)

	// 'subsec'/'subsecond' as first arg means time-value defaults to 'now'
	if firstLower == "subsec" || firstLower == "subsecond" {
		return time.Now().UTC(), append([]string{firstStr}, mods...), nil
	}

	if firstLower == "now" {
		return time.Now().UTC(), mods, nil
	}

	// Numeric: Julian day by default; first modifier may change interpretation
	if f, err := strconv.ParseFloat(firstStr, 64); err == nil {
		if len(mods) > 0 {
			switch strings.ToLower(strings.TrimSpace(mods[0])) {
			case "unixepoch":
				sec := int64(f)
				nsec := int64(math.Round((f - float64(sec)) * 1e9))
				return time.Unix(sec, nsec).UTC(), mods[1:], nil
			case "auto":
				if f >= 0.0 && f <= 5373484.499999 {
					return julianDayToTime(f), mods[1:], nil
				}
				if f >= -210866760000 && f <= 253402300799 {
					return time.Unix(int64(f), 0).UTC(), mods[1:], nil
				}
				return time.Time{}, nil, fmt.Errorf("time value out of range for auto")
			case "julianday":
				return julianDayToTime(f), mods[1:], nil
			}
		}
		return julianDayToTime(f), mods, nil
	}

	// ISO-8601 text
	t, err := parseISO8601(firstStr)
	if err != nil {
		return time.Time{}, nil, err
	}
	return t, mods, nil
}

// applyModifiers applies SQLite date/time modifiers sequentially.
// Returns modified time, subsec flag, and error.
func applyModifiers(t time.Time, mods []string) (time.Time, bool, error) {
	subsec := false

	for _, mod := range mods {
		mod = strings.TrimSpace(mod)
		modLower := strings.ToLower(mod)

		switch modLower {
		case "subsec", "subsecond":
			subsec = true
		case "utc":
			t = t.UTC()
		case "localtime":
			t = t.Local()
		case "ceiling", "floor":
			// Affects ambiguous month-shift results; treated as no-op here
		case "unixepoch", "julianday", "auto":
			// Only valid as first modifier after numeric time-value; consumed by parseDateArgs
		case "start of month":
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		case "start of year":
			t = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, t.Location())
		case "start of day":
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		default:
			if strings.HasPrefix(modLower, "weekday ") {
				nStr := strings.TrimPrefix(modLower, "weekday ")
				if n, err := strconv.Atoi(nStr); err == nil {
					target := time.Weekday(n % 7)
					for t.Weekday() != target {
						t = t.AddDate(0, 0, 1)
					}
				}
				continue
			}
			if t2, ok := applyRelativeMod(t, mod); ok {
				t = t2
				continue
			}
			if t2, ok := applyTimeDiffMod(t, mod); ok {
				t = t2
			}
			// Unknown modifiers are silently ignored (SQLite returns NULL; we're lenient)
		}
	}

	return t, subsec, nil
}

// applyRelativeMod handles "NNN days", "NNN hours", "NNN minutes", "NNN seconds",
// "NNN months", "NNN years" (trailing 's' optional, sign prefix allowed).
func applyRelativeMod(t time.Time, mod string) (time.Time, bool) {
	parts := strings.Fields(mod)
	if len(parts) != 2 {
		return t, false
	}
	f, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return t, false
	}
	unit := strings.ToLower(strings.TrimSuffix(parts[1], "s"))

	switch unit {
	case "day":
		return t.Add(time.Duration(f * float64(24*time.Hour))), true
	case "hour":
		return t.Add(time.Duration(f * float64(time.Hour))), true
	case "minute":
		return t.Add(time.Duration(f * float64(time.Minute))), true
	case "second":
		return t.Add(time.Duration(f * float64(time.Second))), true
	case "month":
		whole := int(f)
		frac := f - float64(whole)
		t = t.AddDate(0, whole, 0)
		if frac != 0 {
			t = t.Add(time.Duration(frac * float64(30*24*time.Hour)))
		}
		return t, true
	case "year":
		whole := int(f)
		frac := f - float64(whole)
		t = t.AddDate(whole, 0, 0)
		if frac != 0 {
			t = t.Add(time.Duration(frac * float64(365*24*time.Hour)))
		}
		return t, true
	}
	return t, false
}

// applyTimeDiffMod handles timediff-output style modifiers: ±YYYY-MM-DD HH:MM:SS.SSS
func applyTimeDiffMod(t time.Time, mod string) (time.Time, bool) {
	if len(mod) == 0 {
		return t, false
	}
	sign := 1
	s := mod
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		sign = -1
		s = s[1:]
	default:
		return t, false
	}

	parts := strings.SplitN(s, " ", 2)
	dateSubs := strings.Split(parts[0], "-")
	if len(dateSubs) != 3 {
		return t, false
	}
	years, e1 := strconv.Atoi(dateSubs[0])
	months, e2 := strconv.Atoi(dateSubs[1])
	days, e3 := strconv.Atoi(dateSubs[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return t, false
	}

	hours, minutes, secs, millis := 0, 0, 0, 0
	if len(parts) == 2 {
		tp := strings.SplitN(parts[1], ":", 3)
		if len(tp) >= 1 {
			hours, _ = strconv.Atoi(tp[0])
		}
		if len(tp) >= 2 {
			minutes, _ = strconv.Atoi(tp[1])
		}
		if len(tp) >= 3 {
			sp := strings.SplitN(tp[2], ".", 2)
			secs, _ = strconv.Atoi(sp[0])
			if len(sp) > 1 {
				ms := sp[1]
				for len(ms) < 3 {
					ms += "0"
				}
				millis, _ = strconv.Atoi(ms[:3])
			}
		}
	}

	t = t.AddDate(sign*years, sign*months, sign*days)
	dur := time.Duration(sign) * (time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(secs)*time.Second +
		time.Duration(millis)*time.Millisecond)
	return t.Add(dur), true
}

// sqliteStrftime formats t using SQLite strftime codes.
func sqliteStrftime(format string, t time.Time, subsec bool) string {
	var b strings.Builder
	jd := timeToJulianDay(t)

	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&b, "%d", t.Day())
		case 'f':
			sec := float64(t.Second()) + float64(t.Nanosecond())/1e9
			fmt.Fprintf(&b, "%06.3f", sec)
		case 'F':
			fmt.Fprintf(&b, "%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
		case 'G':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%04d", y)
		case 'g':
			y, _ := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", y%100)
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&b, "%02d", h)
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'J':
			fmt.Fprintf(&b, "%.10f", jd)
		case 'k':
			fmt.Fprintf(&b, "%d", t.Hour())
		case 'l':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&b, "%d", h)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'P':
			if t.Hour() < 12 {
				b.WriteString("am")
			} else {
				b.WriteString("pm")
			}
		case 'R':
			fmt.Fprintf(&b, "%02d:%02d", t.Hour(), t.Minute())
		case 's':
			if subsec {
				fmt.Fprintf(&b, "%.3f", float64(t.Unix())+float64(t.Nanosecond())/1e9)
			} else {
				fmt.Fprintf(&b, "%d", t.Unix())
			}
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'T':
			fmt.Fprintf(&b, "%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second())
		case 'U':
			fmt.Fprintf(&b, "%02d", sundayWeek(t))
		case 'u':
			w := int(t.Weekday())
			if w == 0 {
				w = 7
			}
			fmt.Fprintf(&b, "%d", w)
		case 'V':
			_, week := t.ISOWeek()
			fmt.Fprintf(&b, "%02d", week)
		case 'w':
			fmt.Fprintf(&b, "%d", int(t.Weekday()))
		case 'W':
			fmt.Fprintf(&b, "%02d", mondayWeek(t))
		case 'Y':
			fmt.Fprintf(&b, "%04d", t.Year())
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

func sundayWeek(t time.Time) int {
	yd := t.YearDay()
	dow := int(t.Weekday()) // 0=Sunday
	return (yd - dow + 6) / 7
}

func mondayWeek(t time.Time) int {
	yd := t.YearDay()
	dow := int(t.Weekday())
	if dow == 0 {
		dow = 7
	}
	return (yd - dow + 7) / 7
}

func evalDateFunc(args []interface{}) (interface{}, error) {
	t, mods, err := parseDateArgs(args)
	if err != nil {
		return nil, nil
	}
	t, _, err = applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	return fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day()), nil
}

func evalTimeFunc(args []interface{}) (interface{}, error) {
	t, mods, err := parseDateArgs(args)
	if err != nil {
		return nil, nil
	}
	t, subsec, err := applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	if subsec {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1e6), nil
	}
	return fmt.Sprintf("%02d:%02d:%02d", t.Hour(), t.Minute(), t.Second()), nil
}

func evalDatetimeFunc(args []interface{}) (interface{}, error) {
	t, mods, err := parseDateArgs(args)
	if err != nil {
		return nil, nil
	}
	t, subsec, err := applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	if subsec {
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%03d",
			t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1e6), nil
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second()), nil
}

func evalJuliandayFunc(args []interface{}) (interface{}, error) {
	t, mods, err := parseDateArgs(args)
	if err != nil {
		return nil, nil
	}
	t, _, err = applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	return timeToJulianDay(t), nil
}

func evalUnixepochFunc(args []interface{}) (interface{}, error) {
	t, mods, err := parseDateArgs(args)
	if err != nil {
		return nil, nil
	}
	t, subsec, err := applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	if subsec {
		return float64(t.Unix()) + float64(t.Nanosecond())/1e9, nil
	}
	return t.Unix(), nil
}

func evalStrftimeFunc(args []interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, nil
	}
	format := toString(args[0])
	t, mods, err := parseDateArgs(args[1:])
	if err != nil {
		return nil, nil
	}
	t, subsec, err := applyModifiers(t, mods)
	if err != nil {
		return nil, nil
	}
	return sqliteStrftime(format, t, subsec), nil
}

// evalTimediffFunc implements timediff(A, B): returns ±YYYY-MM-DD HH:MM:SS.SSS
// representing the amount of time to add to B to reach A.
func evalTimediffFunc(args []interface{}) (interface{}, error) {
	if len(args) < 2 {
		return nil, nil
	}
	tA, _, err := parseDateArgs(args[0:1])
	if err != nil {
		return nil, nil
	}
	tB, _, err := parseDateArgs(args[1:2])
	if err != nil {
		return nil, nil
	}

	sign := "+"
	a, b := tA, tB
	if a.Before(b) {
		sign = "-"
		a, b = b, a
	}

	// Greedy calendar subtraction: find years, months, days, then sub-day duration.
	years := a.Year() - b.Year()
	cursor := b.AddDate(years, 0, 0)
	if cursor.After(a) {
		years--
		cursor = b.AddDate(years, 0, 0)
	}

	months := 0
	for cursor.AddDate(0, 1, 0).Before(a) || cursor.AddDate(0, 1, 0).Equal(a) {
		months++
		cursor = cursor.AddDate(0, 1, 0)
	}

	days := 0
	for cursor.AddDate(0, 0, 1).Before(a) || cursor.AddDate(0, 0, 1).Equal(a) {
		days++
		cursor = cursor.AddDate(0, 0, 1)
	}

	remaining := a.Sub(cursor)
	h := int(remaining.Hours())
	remaining -= time.Duration(h) * time.Hour
	m := int(remaining.Minutes())
	remaining -= time.Duration(m) * time.Minute
	s := int(remaining.Seconds())
	remaining -= time.Duration(s) * time.Second
	ms := int(remaining.Milliseconds())

	return fmt.Sprintf("%s%04d-%02d-%02d %02d:%02d:%02d.%03d",
		sign, years, months, days, h, m, s, ms), nil
}
