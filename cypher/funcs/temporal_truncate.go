package funcs

// temporal_truncate.go — date/datetime/localdatetime/time/localtime.truncate.
//
// Each truncate function accepts:
//
//	X.truncate(unit, source [, fields])
//
// where `unit` is a string naming the truncation boundary ("year", "month",
// …), `source` is a temporal value, and `fields` is an optional MapValue
// of openCypher temporal component overrides applied after truncation
// (e.g. `{day: 2}` to set the day-of-month to 2). The return kind matches
// the function called: date.truncate → DateValue, datetime.truncate →
// DateTimeValue, etc.
//
// Coverage:
//   - Units: millennium, century, decade, year, weekYear, quarter, month,
//     week, day, hour, minute, second, millisecond, microsecond, nanosecond.
//   - Source kinds: DateValue, LocalDateTimeValue, DateTimeValue, TimeValue,
//     LocalTimeValue.
//   - Fields overrides: every numeric/string component the constructor maps
//     accept. Unknown keys are ignored.

import (
	"strings"
	"time"

	"gograph/cypher/expr"
)

// truncateUnit truncates t to the start of the named unit, preserving the
// original location. Unknown units leave t unchanged.
//
//nolint:gocyclo // wide unit switch is uniform and the alternative (a lookup table) trades clarity for indirection
func truncateUnit(t time.Time, unit string) time.Time {
	loc := t.Location()
	switch strings.ToLower(unit) {
	case "millennium":
		y := (t.Year() / 1000) * 1000
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	case "century":
		y := (t.Year() / 100) * 100
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	case "decade":
		y := (t.Year() / 10) * 10
		return time.Date(y, 1, 1, 0, 0, 0, 0, loc)
	case "year":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, loc)
	case "weekyear":
		// Truncate to the Monday of ISO week 1 of the source's ISO year.
		// ISO year may differ from calendar year at the boundaries
		// (e.g. 1984-01-01 is in ISO year 1983).
		isoYear, _ := t.ISOWeek()
		jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, loc)
		wd := int(jan4.Weekday())
		if wd == 0 {
			wd = 7
		}
		monday := jan4.AddDate(0, 0, -(wd - 1))
		return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, loc)
	case "quarter":
		m := ((int(t.Month())-1)/3)*3 + 1
		return time.Date(t.Year(), time.Month(m), 1, 0, 0, 0, 0, loc)
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
	case "week":
		// ISO 8601 week starts on Monday.
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		return time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, loc)
	case "day":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	case "hour":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
	case "minute":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	case "second":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
	case "millisecond":
		ns := (t.Nanosecond() / 1_000_000) * 1_000_000
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ns, loc)
	case "microsecond":
		ns := (t.Nanosecond() / 1_000) * 1_000
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), ns, loc)
	case "nanosecond":
		return t
	}
	return t
}

// sourceToTime converts a temporal value to a [time.Time]. The returned
// location matches the source kind: DateTimeValue and TimeValue carry their
// own zone offset; every other kind is anchored to UTC. Time-only kinds
// (TimeValue, LocalTimeValue) project their nanoseconds onto a synthetic
// 1970-01-01 epoch date so the same time.Time-based truncation machinery
// applies uniformly.
func sourceToTime(v expr.Value) (time.Time, bool) {
	switch s := v.(type) {
	case expr.DateValue:
		return s.ToTime(), true
	case expr.LocalDateTimeValue:
		return s.T, true
	case expr.DateTimeValue:
		return s.T, true
	case expr.LocalTimeValue:
		return time.Unix(0, s.Nanos).UTC(), true
	case expr.TimeValue:
		loc := time.FixedZone("offset", int(s.OffsetSec))
		return time.Unix(0, s.Nanos).In(loc), true
	}
	return time.Time{}, false
}

// applyOverrides applies each (key, value) entry in fields to t. Keys are
// matched case-insensitively against the standard temporal component names.
// Unknown keys are silently ignored — the caller has already validated the
// source kind.
//
// timezone overrides require a [time.Location] passed via newLoc. When
// newLoc is nil and the override is present, the function falls back to
// time.UTC.
//
//nolint:gocyclo // openCypher fields is a flat dispatch; splitting per kind would obscure the contract
func applyOverrides(t time.Time, fields expr.MapValue) time.Time {
	if fields == nil {
		return t
	}
	year, month, day := t.Year(), int(t.Month()), t.Day()
	hour, minute, second := t.Hour(), t.Minute(), t.Second()
	// Decompose the nanosecond-of-second into hierarchical components
	// (millisecond-of-second, microsecond-of-millisecond, nanosecond-of-
	// microsecond). The openCypher truncate override applies each sub-
	// second key independently — `{nanosecond: 2}` after truncate to the
	// millisecond keeps the truncated ms value and just sets the sub-
	// microsecond nanoseconds, producing e.g. .645000002.
	nsRaw := t.Nanosecond()
	ms := nsRaw / 1_000_000
	us := (nsRaw / 1_000) % 1_000
	nsec := nsRaw % 1_000
	loc := t.Location()
	// dayOfWeek is applied after year/month/day so it adjusts the final date
	// by the difference between the requested ISO weekday and the current
	// weekday (1=Mon..7=Sun). A negative value disables the adjustment.
	dayOfWeek := -1
	for k, v := range fields {
		iv, isInt := intArg(v)
		switch strings.ToLower(k) {
		case "year":
			if isInt {
				year = int(iv)
			}
		case "month":
			if isInt {
				month = int(iv)
			}
		case "day":
			if isInt {
				day = int(iv)
			}
		case "dayofweek":
			if isInt {
				dayOfWeek = int(iv)
			}
		case "hour":
			if isInt {
				hour = int(iv)
			}
		case "minute":
			if isInt {
				minute = int(iv)
			}
		case "second":
			if isInt {
				second = int(iv)
			}
		case "millisecond":
			if isInt {
				ms = int(iv)
			}
		case "microsecond":
			if isInt {
				us = int(iv)
			}
		case "nanosecond":
			if isInt {
				nsec = int(iv)
			}
		case "timezone":
			if s, ok := v.(expr.StringValue); ok {
				if l, err := time.LoadLocation(string(s)); err == nil {
					loc = l
				}
			}
		}
	}
	ns := ms*1_000_000 + us*1_000 + nsec
	out := time.Date(year, time.Month(month), day, hour, minute, second, ns, loc)
	if dayOfWeek >= 1 && dayOfWeek <= 7 {
		cur := int(out.Weekday())
		if cur == 0 {
			cur = 7
		}
		out = out.AddDate(0, 0, dayOfWeek-cur)
	}
	return out
}

// intArg coerces an expr.Value to int64, accepting both IntegerValue and
// FloatValue (truncated). Returns false for any other kind.
func intArg(v expr.Value) (int64, bool) {
	switch n := v.(type) {
	case expr.IntegerValue:
		return int64(n), true
	case expr.FloatValue:
		return int64(n), true
	}
	return 0, false
}

// extractTruncateArgs validates the (unit, source [, fields]) tuple and
// returns the unit string, the time.Time, and the fields map (which may
// be nil).
func extractTruncateArgs(fn string, args []expr.Value) (string, time.Time, expr.MapValue, expr.Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return "", time.Time{}, nil, nil, &ArityError{Function: fn, Got: len(args), Want: "2..3"}
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return "", time.Time{}, nil, expr.Null, nil
	}
	unitV, ok := args[0].(expr.StringValue)
	if !ok {
		return "", time.Time{}, nil, nil, &TypeError{Function: fn, ArgIndex: 0, Got: args[0].Kind(), Want: "String"}
	}
	src, ok := sourceToTime(args[1])
	if !ok {
		return "", time.Time{}, nil, nil, &TypeError{Function: fn, ArgIndex: 1, Got: args[1].Kind(), Want: "Date/DateTime/LocalDateTime/Time/LocalTime"}
	}
	var fields expr.MapValue
	if len(args) == 3 {
		if expr.IsNull(args[2]) {
			fields = nil
		} else if m, ok := args[2].(expr.MapValue); ok {
			fields = m
		} else {
			return "", time.Time{}, nil, nil, &TypeError{Function: fn, ArgIndex: 2, Got: args[2].Kind(), Want: "Map"}
		}
	}
	return string(unitV), src, fields, nil, nil
}

// fnDateTruncate implements date.truncate(unit, source [, fields]).
func fnDateTruncate(args []expr.Value) (expr.Value, error) {
	unit, t, fields, early, err := extractTruncateArgs("date.truncate", args)
	if err != nil || early != nil {
		return early, err
	}
	t = truncateUnit(t, unit)
	t = applyOverrides(t, fields)
	return expr.DateFromTime(t), nil
}

// fnDateTimeTruncate implements datetime.truncate(unit, source [, fields]).
func fnDateTimeTruncate(args []expr.Value) (expr.Value, error) {
	unit, t, fields, early, err := extractTruncateArgs("datetime.truncate", args)
	if err != nil || early != nil {
		return early, err
	}
	t = truncateUnit(t, unit)
	t = applyOverrides(t, fields)
	return expr.DateTimeValue{T: t}, nil
}

// fnLocalDateTimeTruncate implements localdatetime.truncate(unit, source [, fields]).
func fnLocalDateTimeTruncate(args []expr.Value) (expr.Value, error) {
	unit, t, fields, early, err := extractTruncateArgs("localdatetime.truncate", args)
	if err != nil || early != nil {
		return early, err
	}
	t = truncateUnit(t, unit)
	t = applyOverrides(t, fields)
	// LocalDateTime drops the timezone — re-anchor to UTC.
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	return expr.LocalDateTimeValue{T: t}, nil
}

// fnTimeTruncate implements time.truncate(unit, source [, fields]).
func fnTimeTruncate(args []expr.Value) (expr.Value, error) {
	unit, t, fields, early, err := extractTruncateArgs("time.truncate", args)
	if err != nil || early != nil {
		return early, err
	}
	t = truncateUnit(t, unit)
	t = applyOverrides(t, fields)
	_, off := t.Zone()
	nanos := int64(t.Hour())*int64(time.Hour) + int64(t.Minute())*int64(time.Minute) +
		int64(t.Second())*int64(time.Second) + int64(t.Nanosecond())
	return expr.TimeValue{Nanos: nanos, OffsetSec: int32(off)}, nil
}

// fnLocalTimeTruncate implements localtime.truncate(unit, source [, fields]).
func fnLocalTimeTruncate(args []expr.Value) (expr.Value, error) {
	unit, t, fields, early, err := extractTruncateArgs("localtime.truncate", args)
	if err != nil || early != nil {
		return early, err
	}
	t = truncateUnit(t, unit)
	t = applyOverrides(t, fields)
	nanos := int64(t.Hour())*int64(time.Hour) + int64(t.Minute())*int64(time.Minute) +
		int64(t.Second())*int64(time.Second) + int64(t.Nanosecond())
	return expr.LocalTimeValue{Nanos: nanos}, nil
}
