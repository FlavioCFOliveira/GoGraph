// temporal.go — Cypher temporal constructor and accessor built-ins.
//
// This file registers the following functions on [DefaultRegistry]:
//
//	date(string|map|nothing)              → Date
//	localdatetime(string|map|nothing)     → LocalDateTime
//	datetime(string|map|nothing)          → DateTime
//	localtime(string|map|nothing)         → LocalTime
//	time(string|map|nothing)              → Time
//	duration(string|map)                  → Duration
//	duration.between(t1, t2)              → Duration
//	duration.inMonths|inDays|inSeconds(d) → Duration projection
//
// Constructors accept three argument forms per openCypher §3.4:
//
//   - Zero args: returns the current instant. We use time.Now().UTC() for
//     determinism and document the deviation in godoc.
//   - One String arg: parsed via expr.Parse*.
//   - One Map arg: built from component keys (year, month, day, ...).
//
// NULL inputs propagate to NULL.
//
// Accessor properties (e.g. d.year, t.hour) are handled directly by
// [expr.evalProperty] via the [temporalAccessor] helper exported through the
// expr package surface; the constructor functions stay in this file.

package funcs

import (
	"fmt"
	"strings"
	"time"

	"gograph/cypher/expr"
)

// registerTemporal wires every temporal constructor and projection into r.
// It is invoked from [buildDefaultRegistry] so the functions become available
// without further wiring at call sites.
func registerTemporal(r *Registry) {
	r.Register("date", fnDate)
	r.Register("localdatetime", fnLocalDateTime)
	r.Register("datetime", fnDateTime)
	r.Register("localtime", fnLocalTime)
	r.Register("time", fnTime)
	r.Register("duration", fnDuration)
	r.Register("duration.between", fnDurationBetween)
	r.Register("duration.inmonths", fnDurationInMonths)
	r.Register("duration.indays", fnDurationInDays)
	r.Register("duration.inseconds", fnDurationInSeconds)
	// Truncation (T937/T941): each X.truncate(unit, source [, fields]) → X.
	r.Register("date.truncate", fnDateTruncate)
	r.Register("datetime.truncate", fnDateTimeTruncate)
	r.Register("localdatetime.truncate", fnLocalDateTimeTruncate)
	r.Register("time.truncate", fnTimeTruncate)
	r.Register("localtime.truncate", fnLocalTimeTruncate)
	// Clock-source variants: per openCypher, .transaction / .statement /
	// .realtime all return the current instant; the distinction matters
	// in a clustered setting but not in our single-process engine. All
	// three aliases dispatch to the same 0-arg constructor which uses
	// time.Now().UTC().
	for _, kind := range []struct {
		base string
		fn   func([]expr.Value) (expr.Value, error)
	}{
		{"date", fnDate},
		{"localtime", fnLocalTime},
		{"time", fnTime},
		{"localdatetime", fnLocalDateTime},
		{"datetime", fnDateTime},
	} {
		fn := kind.fn
		for _, suffix := range []string{"transaction", "statement", "realtime"} {
			r.Register(kind.base+"."+suffix, fn)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// date()
// ─────────────────────────────────────────────────────────────────────────────

// fnDate constructs a [expr.DateValue] from a string, a component map, or
// the current date when called with no arguments.
func fnDate(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 0:
		return expr.DateFromTime(time.Now().UTC()), nil
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		switch v := args[0].(type) {
		case expr.StringValue:
			d, err := expr.ParseDate(string(v))
			if err != nil {
				return expr.Null, nil //nolint:nilerr // invalid date → NULL per openCypher
			}
			return d, nil
		case expr.MapValue:
			return dateFromMap(v)
		case expr.DateValue:
			return v, nil
		case expr.LocalDateTimeValue:
			return expr.DateFromTime(v.T), nil
		case expr.DateTimeValue:
			return expr.DateFromTime(v.T), nil
		default:
			return nil, &TypeError{Function: "date", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
		}
	}
	return nil, &ArityError{Function: "date", Got: len(args), Want: "0..1"}
}

// dateFromMap builds a DateValue from a component map. Supports:
//
//   - Calendar:    {year, month, day}    (month/day default to 1)
//   - Ordinal:     {year, ordinalDay}    (ordinal day-of-year)
//   - Week:        {year, week, dayOfWeek} (ISO week date, dow defaults 1)
//   - Quarter:     {year, quarter, dayOfQuarter} (quarter→month, dayOfQuarter→day)
//   - From base:   {date: D, ...overrides...} — D may be Date, LocalDateTime,
//     or DateTime; the date component is extracted and overrides are applied.
//     When week is overridden without an explicit year, the base's ISO
//     week-year is used. When week is overridden without an explicit dayOfWeek,
//     the base's ISO day-of-week is inherited. When quarter is overridden
//     without an explicit month, the base's month-within-quarter offset is
//     preserved (e.g. November is the 2nd month of Q4 → August in Q3).
//
//nolint:gocyclo // Sequential overlay of map fields onto a DateValue; each branch is uniform — splitting hides the field-priority logic.
func dateFromMap(m expr.MapValue) (expr.Value, error) {
	// Base from {date: ...} if present. The base may be any temporal kind
	// carrying a date component (Date, LocalDateTime, DateTime).
	base := expr.DateValue{Year: 1970, Month: 1, Day: 1}
	hasBase := false
	if dv, ok := m["date"]; ok {
		switch d := dv.(type) {
		case expr.DateValue:
			base = d
			hasBase = true
		case expr.LocalDateTimeValue:
			base = expr.DateFromTime(d.T)
			hasBase = true
		case expr.DateTimeValue:
			base = expr.DateFromTime(d.T)
			hasBase = true
		}
	}
	year := base.Year
	month := base.Month
	day := base.Day
	yearExplicit := false
	if v, ok := m["year"]; ok {
		i, ok2 := intFromValue(v)
		if !ok2 {
			return expr.Null, nil
		}
		year = int(i)
		yearExplicit = true
	}
	// Week form takes priority when "week" is present.
	if wv, hasWeek := m["week"]; hasWeek {
		w, ok := intFromValue(wv)
		if !ok {
			return expr.Null, nil
		}
		// ISO year: when base present and year is not explicitly overridden,
		// use the base's ISO week-year (which may differ from the calendar
		// year at year boundaries, e.g. 1816-12-30 is in ISO year 1817).
		isoYear := year
		if hasBase && !yearExplicit {
			bt := time.Date(base.Year, time.Month(base.Month), base.Day, 0, 0, 0, 0, time.UTC)
			iy, _ := bt.ISOWeek()
			isoYear = iy
		}
		// dayOfWeek: when base present and dow not explicitly overridden,
		// inherit the base date's ISO day-of-week (1=Mon..7=Sun) rather than
		// defaulting to 1.
		dow := 1
		if dv, ok := m["dayOfWeek"]; ok {
			i, ok2 := intFromValue(dv)
			if !ok2 {
				return expr.Null, nil
			}
			dow = int(i)
		} else if hasBase {
			bt := time.Date(base.Year, time.Month(base.Month), base.Day, 0, 0, 0, 0, time.UTC)
			wd := int(bt.Weekday())
			if wd == 0 {
				wd = 7
			}
			dow = wd
		}
		dv, err := isoWeekDate(isoYear, int(w), dow)
		if err != nil {
			return expr.Null, nil //nolint:nilerr // invalid components → NULL
		}
		return dv, nil
	}
	// Ordinal form when "ordinalDay" present (preferred per openCypher) or
	// the legacy "dayOfYear" alias — both accepted, without "month".
	doyVal, hasDoy := m["ordinalDay"]
	if !hasDoy {
		doyVal, hasDoy = m["dayOfYear"]
	}
	if hasDoy {
		if _, hasMonth := m["month"]; !hasMonth {
			doy, ok := intFromValue(doyVal)
			if !ok {
				return expr.Null, nil
			}
			t := time.Date(year, 1, int(doy), 0, 0, 0, 0, time.UTC)
			return expr.DateFromTime(t), nil
		}
	}
	if mv, ok := m["month"]; ok {
		i, ok2 := intFromValue(mv)
		if !ok2 {
			return expr.Null, nil
		}
		month = int(i)
		// Without a base, day defaults to 1 when only month is specified.
		// With a base, the base's day is preserved unless explicitly
		// overridden by the day key below.
		if !hasBase {
			day = 1
		}
	}
	if dv, ok := m["day"]; ok {
		i, ok2 := intFromValue(dv)
		if !ok2 {
			return expr.Null, nil
		}
		day = int(i)
	}
	if quarter, ok := m["quarter"]; ok {
		i, ok2 := intFromValue(quarter)
		if !ok2 {
			return expr.Null, nil
		}
		// Quarter→month: when no explicit month was provided, compute the
		// new month from the quarter index. With a base date, preserve the
		// month-within-quarter offset (0/1/2) so e.g. November (2nd month
		// of Q4) projects to August (2nd month of Q3).
		if _, hasMonth := m["month"]; !hasMonth {
			if hasBase {
				offset := (base.Month - 1) % 3
				month = (int(i)-1)*3 + 1 + offset
			} else {
				month = 1 + (int(i)-1)*3
			}
		}
		if dq, ok := m["dayOfQuarter"]; ok {
			ii, ok3 := intFromValue(dq)
			if !ok3 {
				return expr.Null, nil
			}
			// dayOfQuarter is the 1-based ordinal day within the quarter,
			// anchored at the first month of the quarter. time.Date
			// normalises overflow (e.g. day 92 in July becomes Sep 30).
			qStart := (int(i)-1)*3 + 1
			t := time.Date(year, time.Month(qStart), int(ii), 0, 0, 0, 0, time.UTC)
			return expr.DateFromTime(t), nil
		}
	}
	return expr.NewDate(year, month, day), nil
}

// isoWeekDate constructs a [expr.DateValue] from (year, week, dayOfWeek).
// The result follows ISO 8601 week date rules.
func isoWeekDate(isoYear, isoWeek, dow int) (expr.DateValue, error) {
	if dow < 1 || dow > 7 || isoWeek < 1 || isoWeek > 53 {
		return expr.DateValue{}, fmt.Errorf("invalid week date components")
	}
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	w0 := int(jan4.Weekday())
	if w0 == 0 {
		w0 = 7
	}
	mondayWeek1 := jan4.AddDate(0, 0, -(w0 - 1))
	t := mondayWeek1.AddDate(0, 0, (isoWeek-1)*7+(dow-1))
	return expr.DateFromTime(t), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// localdatetime() / datetime()
// ─────────────────────────────────────────────────────────────────────────────

func fnLocalDateTime(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 0:
		return expr.LocalDateTimeValue{T: time.Now().UTC()}, nil
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		switch v := args[0].(type) {
		case expr.StringValue:
			d, err := expr.ParseLocalDateTime(string(v))
			if err != nil {
				return expr.Null, nil //nolint:nilerr // invalid → NULL
			}
			return d, nil
		case expr.MapValue:
			return localDateTimeFromMap(v)
		case expr.LocalDateTimeValue:
			return v, nil
		case expr.DateTimeValue:
			return expr.LocalDateTimeValue{T: v.T.UTC()}, nil
		case expr.DateValue:
			return expr.NewLocalDateTime(v.Year, v.Month, v.Day, 0, 0, 0, 0), nil
		default:
			return nil, &TypeError{Function: "localdatetime", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
		}
	}
	return nil, &ArityError{Function: "localdatetime", Got: len(args), Want: "0..1"}
}

func fnDateTime(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 0:
		return expr.DateTimeValue{T: time.Now().UTC()}, nil
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		switch v := args[0].(type) {
		case expr.StringValue:
			d, err := expr.ParseDateTime(string(v))
			if err != nil {
				return expr.Null, nil //nolint:nilerr // invalid → NULL
			}
			return d, nil
		case expr.MapValue:
			return dateTimeFromMap(v)
		case expr.DateTimeValue:
			return v, nil
		case expr.LocalDateTimeValue:
			return expr.DateTimeValue(v), nil
		case expr.DateValue:
			return expr.NewDateTime(v.Year, v.Month, v.Day, 0, 0, 0, 0, time.UTC), nil
		default:
			return nil, &TypeError{Function: "datetime", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
		}
	}
	return nil, &ArityError{Function: "datetime", Got: len(args), Want: "0..1"}
}

// localDateTimeFromMap builds a LocalDateTimeValue from a component map.
func localDateTimeFromMap(m expr.MapValue) (expr.Value, error) {
	dv, err := dateFromMap(m)
	if err != nil {
		return expr.Null, nil //nolint:nilerr
	}
	d, ok := dv.(expr.DateValue)
	if !ok {
		return expr.Null, nil
	}
	h, mn, s, ns := timeComponentsFromMap(m)
	return expr.NewLocalDateTime(d.Year, d.Month, d.Day, h, mn, s, ns), nil
}

// dateTimeFromMap builds a DateTimeValue from a component map. The zone is
// derived from "timezone" key (string), defaulting to UTC.
//
// openCypher 9 §3.10.5: when the map contains both a base time/datetime
// (under "time" or "datetime") whose own offset differs from an explicit
// "timezone" override, the wall-clock is shifted so the instant is
// preserved (timezone conversion). Explicit hour/minute keys on the
// override map still win over the shift.
func dateTimeFromMap(m expr.MapValue) (expr.Value, error) {
	dv, err := dateFromMap(m)
	if err != nil {
		return expr.Null, nil //nolint:nilerr
	}
	d, ok := dv.(expr.DateValue)
	if !ok {
		return expr.Null, nil
	}
	h, mn, s, ns := timeComponentsFromMap(m)
	loc := zoneFromMap(m)

	// Detect base-vs-target offset mismatch and apply the conversion shift.
	baseOff, haveBaseOff := baseOffsetFromMap(m)
	_, explicitTZ := m["timezone"]
	if haveBaseOff && explicitTZ {
		_, newOff := time.Date(d.Year, time.Month(d.Month), d.Day, h, mn, s, ns, loc).Zone()
		if newOff != baseOff {
			if _, hasH := m["hour"]; !hasH {
				if _, hasMn := m["minute"]; !hasMn {
					shift := time.Duration(newOff-baseOff) * time.Second
					shifted := time.Date(d.Year, time.Month(d.Month), d.Day, h, mn, s, ns, time.UTC).Add(shift)
					d.Year, d.Month, d.Day = shifted.Year(), int(shifted.Month()), shifted.Day()
					h, mn, s, ns = shifted.Hour(), shifted.Minute(), shifted.Second(), shifted.Nanosecond()
				}
			}
		}
	}
	return expr.NewDateTime(d.Year, d.Month, d.Day, h, mn, s, ns, loc), nil
}

// baseOffsetFromMap returns the offset (seconds east of UTC) of the
// time/datetime carried under m's "time" or "datetime" key, if any.
func baseOffsetFromMap(m expr.MapValue) (int, bool) {
	if tv, ok := m["time"]; ok {
		switch t := tv.(type) {
		case expr.TimeValue:
			return int(t.OffsetSec), true
		case expr.DateTimeValue:
			_, o := t.T.Zone()
			return o, true
		}
	}
	if dv, ok := m["datetime"]; ok {
		if t, ok2 := dv.(expr.DateTimeValue); ok2 {
			_, o := t.T.Zone()
			return o, true
		}
	}
	return 0, false
}

// timeComponentsFromMap extracts hour/minute/second/nanosecond from m.
//
// When m contains a "time" key whose value is a temporal value carrying a
// time-of-day (LocalTime, Time, LocalDateTime, DateTime), its components
// are used as the base; explicit hour/minute/second/nanosecond/millisecond/
// microsecond keys override the base component-by-component.
func timeComponentsFromMap(m expr.MapValue) (h, mn, s, ns int) {
	// Base from {time: ...} if present.
	if tv, ok := m["time"]; ok {
		switch t := tv.(type) {
		case expr.LocalTimeValue:
			hh, mm, ss, nn := splitNanos(t.Nanos)
			h, mn, s, ns = hh, mm, ss, nn
		case expr.TimeValue:
			hh, mm, ss, nn := splitNanos(t.Nanos)
			h, mn, s, ns = hh, mm, ss, nn
		case expr.LocalDateTimeValue:
			h, mn, s, ns = t.T.Hour(), t.T.Minute(), t.T.Second(), t.T.Nanosecond()
		case expr.DateTimeValue:
			h, mn, s, ns = t.T.Hour(), t.T.Minute(), t.T.Second(), t.T.Nanosecond()
		}
	}
	if v, ok := m["hour"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			h = int(i)
		}
	}
	if v, ok := m["minute"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			mn = int(i)
		}
	}
	if v, ok := m["second"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			s = int(i)
		}
	}
	// Sub-second fields are ADDITIVE per openCypher 9 §3.10.1: when
	// millisecond / microsecond / nanosecond are all supplied, the total
	// nanosecond component is ms·1_000_000 + us·1_000 + ns. Any base
	// nanosecond inherited from a {time: …} key is replaced entirely
	// once any sub-second key is supplied.
	_, hasMs := m["millisecond"]
	_, hasUs := m["microsecond"]
	_, hasNs := m["nanosecond"]
	if hasMs || hasUs || hasNs {
		var sub int
		if v, ok := m["millisecond"]; ok {
			if i, ok2 := intFromValue(v); ok2 {
				sub += int(i) * 1_000_000
			}
		}
		if v, ok := m["microsecond"]; ok {
			if i, ok2 := intFromValue(v); ok2 {
				sub += int(i) * 1_000
			}
		}
		if v, ok := m["nanosecond"]; ok {
			if i, ok2 := intFromValue(v); ok2 {
				sub += int(i)
			}
		}
		ns = sub
	}
	return
}

// zoneFromTemporal returns the fixed-offset location of a temporal value
// carrying a zone (TimeValue, DateTimeValue), or nil for kinds without one.
func zoneFromTemporal(v expr.Value) *time.Location {
	switch t := v.(type) {
	case expr.TimeValue:
		return time.FixedZone("offset", int(t.OffsetSec))
	case expr.DateTimeValue:
		return t.T.Location()
	}
	return nil
}

// splitNanos converts an absolute nanosecond-of-day count into
// (hour, minute, second, nanosecond) components.
func splitNanos(n int64) (h, mn, s, ns int) {
	const (
		nsPerHour   = int64(time.Hour)
		nsPerMinute = int64(time.Minute)
		nsPerSecond = int64(time.Second)
	)
	h = int(n / nsPerHour)
	n %= nsPerHour
	mn = int(n / nsPerMinute)
	n %= nsPerMinute
	s = int(n / nsPerSecond)
	ns = int(n % nsPerSecond)
	return
}

// zoneFromMap resolves a "timezone" key to a *time.Location. Recognises:
//
//   - "Z" or "UTC"          → time.UTC
//   - "+HH:MM" or "-HH:MM"  → time.FixedZone with that offset
//   - Named zone string     → time.LoadLocation, falling back to UTC on error
//
// When no timezone key is present, the timezone is inherited from a {time:..}
// or {datetime:..} base value when those carry a fixed offset (Time,
// DateTime). When no base timezone is available either, returns time.UTC.
func zoneFromMap(m expr.MapValue) *time.Location {
	v, ok := m["timezone"]
	if !ok {
		// Inherit from {time:..} or {datetime:..} base if present.
		if tv, ok := m["time"]; ok {
			if loc := zoneFromTemporal(tv); loc != nil {
				return loc
			}
		}
		if tv, ok := m["datetime"]; ok {
			if loc := zoneFromTemporal(tv); loc != nil {
				return loc
			}
		}
		return time.UTC
	}
	s, ok := v.(expr.StringValue)
	if !ok {
		return time.UTC
	}
	str := strings.TrimSpace(string(s))
	if str == "" || str == "Z" || strings.EqualFold(str, "UTC") {
		return time.UTC
	}
	if strings.HasPrefix(str, "+") || strings.HasPrefix(str, "-") {
		// Numeric offset.
		if off, err := parseSignedOffset(str); err == nil {
			return time.FixedZone(str, off)
		}
		return time.UTC
	}
	if loc, err := time.LoadLocation(str); err == nil {
		return loc
	}
	return time.UTC
}

// parseSignedOffset parses "+HH:MM" or "-HHMM" into seconds east of UTC.
func parseSignedOffset(s string) (int, error) {
	if len(s) < 3 {
		return 0, fmt.Errorf("offset too short")
	}
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	rest := s[1:]
	rest = strings.ReplaceAll(rest, ":", "")
	switch len(rest) {
	case 2:
		var h int
		_, err := fmt.Sscanf(rest, "%2d", &h)
		if err != nil {
			return 0, err
		}
		return sign * h * 3600, nil
	case 4:
		var h, m int
		_, err := fmt.Sscanf(rest, "%2d%2d", &h, &m)
		if err != nil {
			return 0, err
		}
		return sign * (h*3600 + m*60), nil
	case 6:
		var h, m, sec int
		_, err := fmt.Sscanf(rest, "%2d%2d%2d", &h, &m, &sec)
		if err != nil {
			return 0, err
		}
		return sign * (h*3600 + m*60 + sec), nil
	default:
		return 0, fmt.Errorf("offset length unexpected: %q", s)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// localtime() / time()
// ─────────────────────────────────────────────────────────────────────────────

func fnLocalTime(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 0:
		t := time.Now().UTC()
		return expr.NewLocalTime(t.Hour(), t.Minute(), t.Second(), t.Nanosecond()), nil
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		switch v := args[0].(type) {
		case expr.StringValue:
			d, err := expr.ParseLocalTime(string(v))
			if err != nil {
				return expr.Null, nil //nolint:nilerr // invalid → NULL
			}
			return d, nil
		case expr.MapValue:
			h, mn, s, ns := timeComponentsFromMap(v)
			return expr.NewLocalTime(h, mn, s, ns), nil
		case expr.LocalTimeValue:
			return v, nil
		case expr.TimeValue:
			return expr.LocalTimeValue{Nanos: v.Nanos}, nil
		case expr.LocalDateTimeValue:
			return expr.NewLocalTime(v.T.Hour(), v.T.Minute(), v.T.Second(), v.T.Nanosecond()), nil
		case expr.DateTimeValue:
			return expr.NewLocalTime(v.T.Hour(), v.T.Minute(), v.T.Second(), v.T.Nanosecond()), nil
		default:
			return nil, &TypeError{Function: "localtime", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
		}
	}
	return nil, &ArityError{Function: "localtime", Got: len(args), Want: "0..1"}
}

//nolint:gocyclo // Sequential type-switch over the documented arg shapes; each branch is one short conversion.
func fnTime(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 0:
		t := time.Now().UTC()
		return expr.NewTime(t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), 0), nil
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		switch v := args[0].(type) {
		case expr.StringValue:
			d, err := expr.ParseTime(string(v))
			if err != nil {
				return expr.Null, nil //nolint:nilerr // invalid → NULL
			}
			return d, nil
		case expr.MapValue:
			h, mn, s, ns := timeComponentsFromMap(v)
			// Resolve the new (target) offset and the base offset (if any).
			// When BOTH are present, the base's wall-clock is converted to
			// preserve the instant (openCypher 9 §3.10.5: time({time: t,
			// timezone: …}) shifts t's local time into the new zone). When
			// only the base has an offset, we inherit it unchanged.
			var newOff int
			haveNewOff := false
			if zv, ok := v["timezone"]; ok {
				if sv, ok2 := zv.(expr.StringValue); ok2 {
					if str := strings.TrimSpace(string(sv)); str != "" && str != "Z" {
						if o, err := parseSignedOffset(str); err == nil {
							newOff = o
							haveNewOff = true
						}
					} else if str == "Z" {
						newOff = 0
						haveNewOff = true
					}
				}
			}
			baseOff := 0
			haveBaseOff := false
			if tv, ok := v["time"]; ok {
				switch t := tv.(type) {
				case expr.TimeValue:
					baseOff = int(t.OffsetSec)
					haveBaseOff = true
				case expr.DateTimeValue:
					_, o := t.T.Zone()
					baseOff = o
					haveBaseOff = true
				}
			}
			off := 0
			switch {
			case haveNewOff && haveBaseOff:
				// Convert: shift wall-clock by (newOff - baseOff), but only
				// when no explicit hour/minute/second key overrides the
				// inherited wall-clock. Explicit overrides take priority
				// over the zone-conversion shift.
				if _, hasH := v["hour"]; !hasH {
					if _, hasMn := v["minute"]; !hasMn {
						deltaSec := newOff - baseOff
						totalNs := int64(h)*3600*int64(time.Second) +
							int64(mn)*60*int64(time.Second) +
							int64(s)*int64(time.Second) +
							int64(ns) +
							int64(deltaSec)*int64(time.Second)
						const day = int64(24) * int64(time.Hour)
						totalNs = ((totalNs % day) + day) % day
						h = int(totalNs / int64(time.Hour))
						totalNs -= int64(h) * int64(time.Hour)
						mn = int(totalNs / int64(time.Minute))
						totalNs -= int64(mn) * int64(time.Minute)
						s = int(totalNs / int64(time.Second))
						ns = int(totalNs - int64(s)*int64(time.Second))
					}
				}
				off = newOff
			case haveNewOff:
				off = newOff
			case haveBaseOff:
				off = baseOff
			}
			return expr.NewTime(h, mn, s, ns, off), nil
		case expr.TimeValue:
			return v, nil
		case expr.LocalTimeValue:
			return expr.TimeValue{Nanos: v.Nanos, OffsetSec: 0}, nil
		case expr.DateTimeValue:
			_, off := v.T.Zone()
			return expr.NewTime(v.T.Hour(), v.T.Minute(), v.T.Second(), v.T.Nanosecond(), off), nil
		case expr.LocalDateTimeValue:
			return expr.NewTime(v.T.Hour(), v.T.Minute(), v.T.Second(), v.T.Nanosecond(), 0), nil
		default:
			return nil, &TypeError{Function: "time", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
		}
	}
	return nil, &ArityError{Function: "time", Got: len(args), Want: "0..1"}
}

// ─────────────────────────────────────────────────────────────────────────────
// duration()
// ─────────────────────────────────────────────────────────────────────────────

func fnDuration(args []expr.Value) (expr.Value, error) {
	if err := requireArity("duration", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.StringValue:
		d, err := expr.ParseDuration(string(v))
		if err != nil {
			return expr.Null, nil //nolint:nilerr
		}
		return d, nil
	case expr.MapValue:
		return durationFromMap(v), nil
	case expr.DurationValue:
		return v, nil
	default:
		return nil, &TypeError{Function: "duration", ArgIndex: 0, Got: args[0].Kind(), Want: "String or Map"}
	}
}

// durationFromMap builds a DurationValue from a component map. Recognised
// keys (singular and plural):
//
//	years, months, weeks, days,
//	hours, minutes, seconds, milliseconds, microseconds, nanoseconds.
//
// Fractional values are supported.
//
//nolint:gocyclo // Sequential extraction; each branch is uniform.
func durationFromMap(m expr.MapValue) expr.Value {
	var (
		months  float64
		days    float64
		seconds float64
	)
	if v, ok := m["years"]; ok {
		months += floatFromValue(v) * 12
	}
	if v, ok := m["months"]; ok {
		months += floatFromValue(v)
	}
	if v, ok := m["weeks"]; ok {
		days += floatFromValue(v) * 7
	}
	if v, ok := m["days"]; ok {
		days += floatFromValue(v)
	}
	if v, ok := m["hours"]; ok {
		seconds += floatFromValue(v) * 3600
	}
	if v, ok := m["minutes"]; ok {
		seconds += floatFromValue(v) * 60
	}
	if v, ok := m["seconds"]; ok {
		seconds += floatFromValue(v)
	}
	if v, ok := m["milliseconds"]; ok {
		seconds += floatFromValue(v) * 1e-3
	}
	if v, ok := m["microseconds"]; ok {
		seconds += floatFromValue(v) * 1e-6
	}
	if v, ok := m["nanoseconds"]; ok {
		seconds += floatFromValue(v) * 1e-9
	}
	// Fold months and days into canonical form.
	mInt := int64(months)
	mFrac := months - float64(mInt)
	// Fractional months → days approximation (30.4375).
	days += mFrac * 30.4375
	dInt := int64(days)
	dFrac := days - float64(dInt)
	// Fractional days → seconds.
	seconds += dFrac * 86400
	sInt := int64(seconds)
	sFrac := seconds - float64(sInt)
	nanos := int32(sFrac * 1_000_000_000)
	return expr.NewDuration(mInt, dInt, sInt, nanos)
}

// fnDurationBetween computes the duration from t1 to t2 for any two
// temporal values. Same-kind inputs delegate to the dedicated Sub*
// helpers; mixed-kind inputs are projected to a common representation
// per the openCypher rules:
//
//   - DateValue / LocalDateTimeValue / DateTimeValue (date-bearing) pairs
//     project both sides to LocalDateTime (midnight for bare dates;
//     zone-stripped for DateTime) and subtract via the wall clock.
//   - LocalTimeValue / TimeValue (time-only) pairs subtract on the
//     nanosecond axis; zone offsets are ignored, matching SubTimes.
//   - A time-only input paired with a date-bearing input subtracts the
//     time-of-day component; the date component is dropped.
//
// NULL on either side propagates to NULL.
func fnDurationBetween(args []expr.Value) (expr.Value, error) {
	if err := requireArity("duration.between", args, 2); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	if d, ok := durationBetweenSameKind(args[0], args[1]); ok {
		return d, nil
	}
	if d, ok := durationBetweenDateBearing(args[0], args[1]); ok {
		return d, nil
	}
	if d, ok := durationBetweenTimeOnly(args[0], args[1]); ok {
		return d, nil
	}
	return expr.Null, nil
}

// durationBetweenSameKind handles the original same-kind cases: two
// dates, two local-date-times, two date-times, two local-times, two
// times. Returns ok=false when the two values differ in kind.
func durationBetweenSameKind(a, b expr.Value) (expr.Value, bool) {
	switch va := a.(type) {
	case expr.DateValue:
		if vb, ok := b.(expr.DateValue); ok {
			return expr.SubDates(vb, va), true
		}
	case expr.LocalDateTimeValue:
		if vb, ok := b.(expr.LocalDateTimeValue); ok {
			return expr.SubLocalDateTimes(vb, va), true
		}
	case expr.DateTimeValue:
		if vb, ok := b.(expr.DateTimeValue); ok {
			return expr.SubDateTimes(vb, va), true
		}
	case expr.LocalTimeValue:
		if vb, ok := b.(expr.LocalTimeValue); ok {
			return expr.SubLocalTimes(vb, va), true
		}
	case expr.TimeValue:
		if vb, ok := b.(expr.TimeValue); ok {
			return expr.SubTimes(vb, va), true
		}
	}
	return nil, false
}

// durationBetweenDateBearing handles mixed-kind pairs where both sides
// carry a date component (DateValue, LocalDateTimeValue,
// DateTimeValue). Each side is projected to LocalDateTimeValue (date
// at midnight, datetime stripped of its zone) and subtracted.
func durationBetweenDateBearing(a, b expr.Value) (expr.Value, bool) {
	la, oka := toLocalDateTime(a)
	if !oka {
		return nil, false
	}
	lb, okb := toLocalDateTime(b)
	if !okb {
		return nil, false
	}
	return expr.SubLocalDateTimes(lb, la), true
}

// durationBetweenTimeOnly handles pairs where AT LEAST ONE side is a
// time-only value (LocalTimeValue / TimeValue). Both sides are
// projected to a nanosecond-since-midnight count via toNanosOfDay; the
// date component, when present, is dropped — duration.between with a
// time-only argument is defined on the time-of-day axis only.
func durationBetweenTimeOnly(a, b expr.Value) (expr.Value, bool) {
	na, oka := toNanosOfDay(a)
	if !oka {
		return nil, false
	}
	nb, okb := toNanosOfDay(b)
	if !okb {
		return nil, false
	}
	diff := nb - na
	return expr.NewDuration(0, 0, diff/1_000_000_000, int32(diff%1_000_000_000)), true
}

// toLocalDateTime projects a date-bearing temporal value to
// LocalDateTimeValue. Bare DateValues become midnight on that date;
// DateTimeValue's zone is dropped (the difference of two date-times is
// independent of their zone offsets — both wall-clocks are observed in
// the same reference frame). Returns ok=false for time-only values.
func toLocalDateTime(v expr.Value) (expr.LocalDateTimeValue, bool) {
	switch vv := v.(type) {
	case expr.DateValue:
		return expr.LocalDateTimeValue{T: vv.ToTime()}, true
	case expr.LocalDateTimeValue:
		return vv, true
	case expr.DateTimeValue:
		// Strip the zone by re-anchoring the wall-clock components in
		// UTC without shifting. vv.T.UTC() would *convert* the instant,
		// changing the hour for any non-UTC zone (so the +01:00 reading
		// 21:40:32.142 becomes 20:40:32.142). The duration calculation
		// uses the local wall-clock, so we copy the calendar fields
		// verbatim.
		y, mo, d := vv.T.Date()
		h, mn, s := vv.T.Clock()
		return expr.LocalDateTimeValue{T: time.Date(y, mo, d, h, mn, s, vv.T.Nanosecond(), time.UTC)}, true
	}
	return expr.LocalDateTimeValue{}, false
}

// toNanosOfDay returns the nanoseconds-since-midnight component of any
// temporal value. For DateValue this is zero (date with no time means
// midnight); for date-bearing types the time component is extracted
// from the wall clock; for time-only types the underlying Nanos field
// is returned directly.
func toNanosOfDay(v expr.Value) (int64, bool) {
	switch vv := v.(type) {
	case expr.DateValue:
		return 0, true
	case expr.LocalDateTimeValue:
		return nanosOfDay(vv.T), true
	case expr.DateTimeValue:
		return nanosOfDay(vv.T), true
	case expr.LocalTimeValue:
		return vv.Nanos, true
	case expr.TimeValue:
		return vv.Nanos, true
	}
	return 0, false
}

// nanosOfDay extracts the time-of-day nanosecond count from a Go
// [time.Time] (hour, minute, second, nanosecond components only).
func nanosOfDay(t time.Time) int64 {
	return int64(t.Hour())*int64(time.Hour) +
		int64(t.Minute())*int64(time.Minute) +
		int64(t.Second())*int64(time.Second) +
		int64(t.Nanosecond())
}

// fnDurationInMonths supports two arities:
//
//	duration.inMonths(d)                — 1-arg: extract the months component
//	                                      of an existing Duration, dropping
//	                                      days/seconds/nanos.
//	duration.inMonths(t1, t2)           — 2-arg: compute the duration from
//	                                      t1 to t2 (as duration.between) and
//	                                      project it to months.
func fnDurationInMonths(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		d, ok := args[0].(expr.DurationValue)
		if !ok {
			return nil, &TypeError{Function: "duration.inMonths", ArgIndex: 0, Got: args[0].Kind(), Want: "Duration"}
		}
		return expr.NewDuration(d.Months, 0, 0, 0), nil
	case 2:
		between, err := fnDurationBetween(args)
		if err != nil {
			return nil, err
		}
		if expr.IsNull(between) {
			return expr.Null, nil
		}
		d, _ := between.(expr.DurationValue)
		return expr.NewDuration(d.Months, 0, 0, 0), nil
	default:
		return nil, &ArityError{Function: "duration.inMonths", Got: len(args), Want: "1..2"}
	}
}

// fnDurationInDays supports two arities mirroring [fnDurationInMonths].
//
// The 2-arg form is NOT the residual-days of a calendar-decomposed
// duration.between — that would discard the months stride and silently
// undercount by orders of magnitude. Instead it projects the elapsed
// time from t1 to t2 to a pure days+seconds axis (zero months) and
// reports the whole-day component.
func fnDurationInDays(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		d, ok := args[0].(expr.DurationValue)
		if !ok {
			return nil, &TypeError{Function: "duration.inDays", ArgIndex: 0, Got: args[0].Kind(), Want: "Duration"}
		}
		totalDays := d.Days + (d.Seconds / 86400)
		return expr.NewDuration(0, totalDays, 0, 0), nil
	case 2:
		if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
			return expr.Null, nil
		}
		elapsedNs, ok := elapsedNanos(args[0], args[1])
		if !ok {
			return expr.Null, nil
		}
		const nsPerDay = int64(86_400) * int64(1_000_000_000)
		days := elapsedNs / nsPerDay
		return expr.NewDuration(0, days, 0, 0), nil
	default:
		return nil, &ArityError{Function: "duration.inDays", Got: len(args), Want: "1..2"}
	}
}

// fnDurationInSeconds supports two arities mirroring [fnDurationInMonths].
//
// The 2-arg form projects the elapsed time from t1 to t2 to a pure
// seconds+nanos axis (zero months, zero days), so the day stride of
// duration.between is rolled up into the seconds count.
func fnDurationInSeconds(args []expr.Value) (expr.Value, error) {
	switch len(args) {
	case 1:
		if expr.IsNull(args[0]) {
			return expr.Null, nil
		}
		d, ok := args[0].(expr.DurationValue)
		if !ok {
			return nil, &TypeError{Function: "duration.inSeconds", ArgIndex: 0, Got: args[0].Kind(), Want: "Duration"}
		}
		totalSecs := d.Days*86400 + d.Seconds
		return expr.NewDuration(0, 0, totalSecs, d.Nanos), nil
	case 2:
		if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
			return expr.Null, nil
		}
		elapsedNs, ok := elapsedNanos(args[0], args[1])
		if !ok {
			return expr.Null, nil
		}
		const nsPerSec = int64(1_000_000_000)
		secs := elapsedNs / nsPerSec
		nanos := int32(elapsedNs % nsPerSec)
		return expr.NewDuration(0, 0, secs, nanos), nil
	default:
		return nil, &ArityError{Function: "duration.inSeconds", Got: len(args), Want: "1..2"}
	}
}

// elapsedNanos returns (b - a) in absolute elapsed nanoseconds, used by
// the 2-arg projection functions duration.inDays / duration.inSeconds.
//
// Same-kind date-bearing pairs subtract via their UTC instants — this
// matters for DateTime, where the two operands may live in different
// zones and wall-clock subtraction would drift by the zone delta.
// Time-only pairs subtract on the nanos-of-day axis.
//
// Cross-kind pairs follow duration.between's projection rules: any
// pair mixing a time-only side with a date-bearing side reduces to the
// time-of-day diff; two date-bearing values project to LocalDateTime
// and subtract as wall-clocks (zone-stripped).
//
// Returns ok=false when neither projection applies.
func elapsedNanos(a, b expr.Value) (int64, bool) {
	if _, aTimeOnly := isTimeOnly(a); aTimeOnly {
		return elapsedNanosTimeOnly(a, b)
	}
	if _, bTimeOnly := isTimeOnly(b); bTimeOnly {
		return elapsedNanosTimeOnly(a, b)
	}
	// Both date-bearing. DateTime same-kind pairs subtract as instants;
	// any other combination projects to LocalDateTime wall-clock.
	if va, ok := a.(expr.DateTimeValue); ok {
		if vb, ok := b.(expr.DateTimeValue); ok {
			return vb.T.Sub(va.T).Nanoseconds(), true
		}
	}
	la, oka := toLocalDateTime(a)
	if !oka {
		return 0, false
	}
	lb, okb := toLocalDateTime(b)
	if !okb {
		return 0, false
	}
	return lb.T.Sub(la.T).Nanoseconds(), true
}

// elapsedNanosTimeOnly computes (b - a) on the nanos-of-day axis. It
// is the projection used when at least one side is time-only.
func elapsedNanosTimeOnly(a, b expr.Value) (int64, bool) {
	na, oka := toNanosOfDay(a)
	if !oka {
		return 0, false
	}
	nb, okb := toNanosOfDay(b)
	if !okb {
		return 0, false
	}
	return nb - na, true
}

// isTimeOnly reports whether v is a LocalTimeValue or TimeValue.
func isTimeOnly(v expr.Value) (expr.Value, bool) {
	switch v.(type) {
	case expr.LocalTimeValue, expr.TimeValue:
		return v, true
	}
	return nil, false
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// intFromValue coerces a numeric expr.Value to int64. Returns false for any
// other kind.
func intFromValue(v expr.Value) (int64, bool) {
	switch x := v.(type) {
	case expr.IntegerValue:
		return int64(x), true
	case expr.FloatValue:
		return int64(float64(x)), true
	default:
		return 0, false
	}
}

// floatFromValue coerces a numeric expr.Value to float64. Returns 0 for any
// non-numeric kind.
func floatFromValue(v expr.Value) float64 {
	switch x := v.(type) {
	case expr.IntegerValue:
		return float64(int64(x))
	case expr.FloatValue:
		return float64(x)
	default:
		return 0
	}
}
