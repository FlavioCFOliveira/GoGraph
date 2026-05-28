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
//   - Zero args: returns the current instant. We use StatementNow() for
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
	"math"
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
	// Epoch-based constructors (Temporal1 [11]).
	r.Register("datetime.fromepoch", fnDateTimeFromEpoch)
	r.Register("datetime.fromepochmillis", fnDateTimeFromEpochMillis)
	// Clock-source variants: per openCypher, .transaction / .statement /
	// .realtime all return the current instant; the distinction matters
	// in a clustered setting but not in our single-process engine. All
	// three aliases dispatch to the same 0-arg constructor which uses
	// StatementNow().
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
		return expr.DateFromTime(StatementNow()), nil
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
	// Base from {date: ...} or {datetime: ...} if present. The base may be
	// any temporal kind carrying a date component (Date, LocalDateTime,
	// DateTime). The {date: ...} key wins when both are supplied, matching
	// the explicit-over-implicit precedence used elsewhere in the map
	// constructors.
	base := expr.DateValue{Year: 1970, Month: 1, Day: 1}
	hasBase := false
	pick := func(dv expr.Value) {
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
	if dv, ok := m["datetime"]; ok {
		pick(dv)
	}
	if dv, ok := m["date"]; ok {
		pick(dv)
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
		return expr.LocalDateTimeValue{T: StatementNow()}, nil
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
			// openCypher: localdatetime(datetime) strips the zone but
			// preserves the wall-clock components — converting v.T via
			// .UTC() shifts the wall clock by the source offset, which
			// is the opposite of "drop the timezone". Re-anchor the
			// wall-clock numbers to UTC instead so the resulting
			// LocalDateTime reads back as the original H:M:S.
			return expr.NewLocalDateTime(v.T.Year(), int(v.T.Month()), v.T.Day(),
				v.T.Hour(), v.T.Minute(), v.T.Second(), v.T.Nanosecond()), nil
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
		return expr.DateTimeValue{T: StatementNow()}, nil
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

// fnDateTimeFromEpoch constructs a UTC DateTimeValue from a (seconds,
// nanos) pair of integer offsets relative to the Unix epoch. openCypher
// (Temporal1 [11]):
//
//	datetime.fromepoch(seconds :: INTEGER, nanoseconds :: INTEGER) :: DATETIME
//
// NULL on either argument propagates to NULL.
func fnDateTimeFromEpoch(args []expr.Value) (expr.Value, error) {
	if err := requireArity("datetime.fromepoch", args, 2); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	secs, ok := intFromValue(args[0])
	if !ok {
		return nil, &TypeError{Function: "datetime.fromepoch", ArgIndex: 0, Got: args[0].Kind(), Want: "Integer"}
	}
	nanos, ok := intFromValue(args[1])
	if !ok {
		return nil, &TypeError{Function: "datetime.fromepoch", ArgIndex: 1, Got: args[1].Kind(), Want: "Integer"}
	}
	return expr.DateTimeValue{T: time.Unix(secs, nanos).UTC()}, nil
}

// fnDateTimeFromEpochMillis constructs a UTC DateTimeValue from a
// millisecond offset relative to the Unix epoch. The whole-second part
// goes into time.Unix and the residual milliseconds carry into the
// nanosecond component (× 1,000,000).
func fnDateTimeFromEpochMillis(args []expr.Value) (expr.Value, error) {
	if err := requireArity("datetime.fromepochmillis", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	ms, ok := intFromValue(args[0])
	if !ok {
		return nil, &TypeError{Function: "datetime.fromepochmillis", ArgIndex: 0, Got: args[0].Kind(), Want: "Integer"}
	}
	return expr.DateTimeValue{T: time.UnixMilli(ms).UTC()}, nil
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
//
// When the map also carries date-override keys (year/month/day/etc.)
// or its own explicit date, the offset is recomputed for the OVERRIDDEN
// date in the SAME zone as the base. This matters across DST
// boundaries: October 11 Stockholm is CET (+01:00) but March 28
// Stockholm is CEST (+02:00); a datetime built with the October
// instant's wall clock applied on March 28 must convert using the
// March 28 offset, not the October one (Temporal3 [10]).
func baseOffsetFromMap(m expr.MapValue) (int, bool) {
	baseTime := func(t time.Time) (int, bool) {
		// Recompute the offset on the override date (if any) in the
		// base's location, so a DST flip on the new date is honoured.
		if d, ok := datePartFromMap(m, t); ok {
			_, o := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, t.Location()).Zone()
			return o, true
		}
		_, o := t.Zone()
		return o, true
	}
	if tv, ok := m["time"]; ok {
		switch t := tv.(type) {
		case expr.TimeValue:
			return int(t.OffsetSec), true
		case expr.DateTimeValue:
			return baseTime(t.T)
		}
	}
	if dv, ok := m["datetime"]; ok {
		if t, ok2 := dv.(expr.DateTimeValue); ok2 {
			return baseTime(t.T)
		}
	}
	return 0, false
}

// datePartFromMap returns the override date assembled from m's keys
// (year/month/day/week/dayOfWeek/quarter/dayOfQuarter/ordinalDay) on
// top of fallback. Returns ok=false when m carries no date keys at
// all, so the caller can decide whether to use the base's own date.
func datePartFromMap(m expr.MapValue, fallback time.Time) (time.Time, bool) {
	dv, err := dateFromMap(m)
	if err != nil {
		return fallback, false
	}
	d, ok := dv.(expr.DateValue)
	if !ok {
		return fallback, false
	}
	return time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, fallback.Location()), true
}

// timeComponentsFromMap extracts hour/minute/second/nanosecond from m.
//
// When m contains a "time" key whose value is a temporal value carrying a
// time-of-day (LocalTime, Time, LocalDateTime, DateTime), its components
// are used as the base; explicit hour/minute/second/nanosecond/millisecond/
// microsecond keys override the base component-by-component.
func timeComponentsFromMap(m expr.MapValue) (h, mn, s, ns int) {
	// Base from {datetime: ...} or {time: ...} if present. The {time: ...}
	// key wins when both are supplied, matching the explicit-over-implicit
	// precedence used elsewhere in the map constructors.
	pick := func(tv expr.Value) {
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
	if tv, ok := m["datetime"]; ok {
		pick(tv)
	}
	if tv, ok := m["time"]; ok {
		pick(tv)
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
	if loc, ok := parseTimezoneString(string(s)); ok {
		return loc
	}
	return time.UTC
}

// parseTimezoneString resolves an openCypher timezone literal to a
// [*time.Location]. Accepts the same forms as [zoneFromMap]: "Z" / "UTC" /
// "" → time.UTC; "+HH[:MM]" / "-HH[:MM]" → time.FixedZone; otherwise an IANA
// zone name passed through time.LoadLocation. Returns (nil, false) when the
// string is non-empty but unparseable so callers can leave the prior zone in
// place (truncate field overrides) or fall back to UTC.
func parseTimezoneString(raw string) (*time.Location, bool) {
	str := strings.TrimSpace(raw)
	if str == "" || str == "Z" || strings.EqualFold(str, "UTC") {
		return time.UTC, true
	}
	if strings.HasPrefix(str, "+") || strings.HasPrefix(str, "-") {
		if off, err := parseSignedOffset(str); err == nil {
			return time.FixedZone(str, off), true
		}
		return nil, false
	}
	if loc, err := time.LoadLocation(str); err == nil {
		return loc, true
	}
	return nil, false
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
		t := StatementNow()
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
		t := StatementNow()
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
	// Fractional months → days approximation. openCypher's reference
	// constant is the Gregorian average month — 365.2425 days / 12 =
	// 30.4368750 days = 2_629_746 seconds exactly. Using the Julian
	// 30.4375 instead introduces a 40-second drift on the smallest
	// fractional input (months: 0.75 → P22DT19H51M49.5S vs P22DT19H52M30S).
	days += mFrac * 30.436875
	dInt := int64(days)
	dFrac := days - float64(dInt)
	// Fractional days → seconds.
	seconds += dFrac * 86400
	sInt := int64(seconds)
	sFrac := seconds - float64(sInt)
	// math.Round defends against float64 precision drift — e.g.
	// nanoseconds:1 produces sFrac ≈ 1e-9 whose product with 1e9 is
	// 0.999999..., and a bare int32 cast would truncate the lone
	// nanosecond away.
	nanos := int32(math.Round(sFrac * 1_000_000_000))
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
// projected to a nanosecond-since-midnight count via timeOfDayNanos;
// the date component, when present, is dropped — duration.between with
// a time-only argument is defined on the time-of-day axis only.
//
// When BOTH sides carry a zone (TimeValue or DateTimeValue), the
// time-of-day axis is taken in UTC (the zone offsets are subtracted)
// so that the difference reflects the elapsed wall time across zones:
// duration.between(time('14:30'), time('16:30+0100')) is PT1H, not
// PT2H. When at least one side is local (LocalTimeValue,
// LocalDateTimeValue, DateValue), the time-of-day axis is taken in
// the wall-clock frame on both sides — the zoned side's offset is
// dropped — so the difference reflects the literal hands-of-the-clock
// gap (matching openCypher's mixed-kind semantics).
func durationBetweenTimeOnly(a, b expr.Value) (expr.Value, bool) {
	useUTC := isZoned(a) && isZoned(b)
	na, oka := timeOfDayNanos(a, useUTC)
	if !oka {
		return nil, false
	}
	nb, okb := timeOfDayNanos(b, useUTC)
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

// isZoned reports whether v carries an explicit zone offset. Only the
// zoned temporal kinds (TimeValue, DateTimeValue) return true; the
// local-only kinds (LocalTimeValue, LocalDateTimeValue, DateValue) and
// non-temporal values return false.
func isZoned(v expr.Value) bool {
	switch v.(type) {
	case expr.TimeValue, expr.DateTimeValue:
		return true
	}
	return false
}

// timeOfDayNanos returns the nanoseconds-since-midnight component of
// any temporal value, with a frame-of-reference toggle that selects
// between UTC and wall-clock projection for the zoned kinds.
//
// useUTC=false (wall-clock frame): the visible HH:MM:SS.ns components
// are returned verbatim. The zone offset on TimeValue / DateTimeValue
// is ignored. This is the correct frame when at least one side of a
// duration computation is zone-less, so the diff reflects the
// hands-of-the-clock gap.
//
// useUTC=true (UTC frame): TimeValue subtracts OffsetSec; DateTimeValue
// takes .T.UTC() before extraction. This is the correct frame when
// both sides carry a zone, so the diff reflects the elapsed wall time
// (duration.between(time('14:30'), time('16:30+0100')) → PT1H).
//
// DateValue is always 00:00 (no time component). LocalTimeValue
// and LocalDateTimeValue have no zone, so the toggle has no effect on
// them.
func timeOfDayNanos(v expr.Value, useUTC bool) (int64, bool) {
	const nsPerSec = int64(1_000_000_000)
	switch vv := v.(type) {
	case expr.DateValue:
		return 0, true
	case expr.LocalDateTimeValue:
		return nanosOfDay(vv.T), true
	case expr.DateTimeValue:
		if useUTC {
			return nanosOfDay(vv.T.UTC()), true
		}
		return nanosOfDay(vv.T), true
	case expr.LocalTimeValue:
		return vv.Nanos, true
	case expr.TimeValue:
		if useUTC {
			return vv.Nanos - int64(vv.OffsetSec)*nsPerSec, true
		}
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
		secs, _, ok := elapsedSecsAndNanos(args[0], args[1])
		if !ok {
			return expr.Null, nil
		}
		return expr.NewDuration(0, secs/86_400, 0, 0), nil
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
		secs, nanos, ok := elapsedSecsAndNanos(args[0], args[1])
		if !ok {
			return expr.Null, nil
		}
		return expr.NewDuration(0, 0, secs, nanos), nil
	default:
		return nil, &ArityError{Function: "duration.inSeconds", Got: len(args), Want: "1..2"}
	}
}

// elapsedNanos returns (b - a) in absolute elapsed nanoseconds, used by
// the 2-arg projection functions duration.inDays / duration.inSeconds.
//
// Dispatch order:
//
//   - Time-only pairs (at least one side is LocalTime/Time) take the
//     nanos-of-day axis via [elapsedNanosTimeOnly] (which itself
//     handles DST-aware projection when a DateTime is paired with a
//     time-only local).
//   - DST-aware path for date-bearing pairs: when one side is
//     DateTime (zoned) and the other is local-date-bearing
//     (LocalDateTime or Date), the local side is rebuilt in the
//     DateTime's *time.Location with its own wall components (and
//     midnight for Date). Both are then subtracted as instants, which
//     correctly counts the extra/missing hour across DST transitions
//     (datetime(Europe/Stockholm, 2017-10-29T00:00) → date(2017-10-30)
//     is PT25H, not PT24H). [dstAwareInstants] also handles same-kind
//     DateTime pairs as a no-op projection.
//   - Both local: project to LocalDateTime wall-clock and subtract;
//     the two times share the UTC reference frame so the subtraction
//     is well-defined.
//
// Returns ok=false when neither projection applies.
func elapsedNanos(a, b expr.Value) (int64, bool) {
	if _, aTimeOnly := isTimeOnly(a); aTimeOnly {
		return elapsedNanosTimeOnly(a, b)
	}
	if _, bTimeOnly := isTimeOnly(b); bTimeOnly {
		return elapsedNanosTimeOnly(a, b)
	}
	if ta, tb, ok := dstAwareInstants(a, b); ok {
		return tb.Sub(ta).Nanoseconds(), true
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

// elapsedNanosTimeOnly computes (b - a) in nanoseconds for pairs in
// which at least one side is a time-only kind (LocalTime / Time).
//
// DST-aware first: when one side is a zoned DateTime and the other a
// local time-only (LocalTime or Date), [dstAwareInstants] projects the
// local side into the DateTime's zone+date so the two operands can be
// subtracted as instants. This is what makes
// duration.inSeconds(localtime(0), datetime({…hour:4,timezone:'Europe/Stockholm'}))
// report PT5H on a DST-end day rather than PT4H.
//
// Otherwise the result is the time-of-day axis difference: UTC frame
// when both sides are zoned (so the offset matters), wall-clock frame
// when at least one side is local.
func elapsedNanosTimeOnly(a, b expr.Value) (int64, bool) {
	if ta, tb, ok := dstAwareInstants(a, b); ok {
		return tb.Sub(ta).Nanoseconds(), true
	}
	useUTC := isZoned(a) && isZoned(b)
	na, oka := timeOfDayNanos(a, useUTC)
	if !oka {
		return 0, false
	}
	nb, okb := timeOfDayNanos(b, useUTC)
	if !okb {
		return 0, false
	}
	return nb - na, true
}

// dstAwareInstants projects a pair of temporal values into the same
// reference frame (UTC instants in a shared *time.Location), so that
// time.Time.Sub returns the elapsed wall time even when the zoned side
// straddles a daylight-saving transition.
//
// The projection rules:
//
//   - DateTime + DateTime → both already instants; returned as-is.
//   - DateTime + local (LocalDateTime / LocalTime / Date) → the local
//     side is rebuilt with [time.Date] using the DateTime's
//     *time.Location, the local side's wall components, and (for
//     time-only locals) the DateTime's calendar date. The same applies
//     in the reverse direction.
//   - Anything else (both local, or one TimeValue which is zoned but
//     not date-bearing) → ok=false; the caller falls back to its own
//     wall-clock or UTC-time-of-day path.
//
// Using [time.Date] with the named *time.Location is what makes the
// projection DST-aware: Go's time package picks the correct offset for
// the chosen wall instant (or shifts forward through a spring-ahead
// gap), so the diff naturally absorbs the 23h/24h/25h day variants.
func dstAwareInstants(a, b expr.Value) (time.Time, time.Time, bool) {
	aDT, aIsDT := a.(expr.DateTimeValue)
	bDT, bIsDT := b.(expr.DateTimeValue)
	switch {
	case aIsDT && bIsDT:
		return aDT.T, bDT.T, true
	case aIsDT:
		t, ok := projectLocalIntoZone(b, aDT)
		if !ok {
			return time.Time{}, time.Time{}, false
		}
		return aDT.T, t, true
	case bIsDT:
		t, ok := projectLocalIntoZone(a, bDT)
		if !ok {
			return time.Time{}, time.Time{}, false
		}
		return t, bDT.T, true
	}
	return time.Time{}, time.Time{}, false
}

// projectLocalIntoZone takes a local temporal value (DateValue,
// LocalDateTimeValue or LocalTimeValue) and an anchor DateTimeValue,
// and returns a [time.Time] positioned in the anchor's *time.Location
// with the local value's wall-clock components. Time-only locals
// borrow the anchor's calendar date. Returns ok=false for non-local
// inputs (TimeValue is zoned and therefore not "local" in this sense).
func projectLocalIntoZone(local expr.Value, anchor expr.DateTimeValue) (time.Time, bool) {
	loc := anchor.T.Location()
	switch v := local.(type) {
	case expr.DateValue:
		return time.Date(v.Year, time.Month(v.Month), v.Day, 0, 0, 0, 0, loc), true
	case expr.LocalDateTimeValue:
		y, mo, d := v.T.Date()
		h, mn, s := v.T.Clock()
		return time.Date(y, mo, d, h, mn, s, v.T.Nanosecond(), loc), true
	case expr.LocalTimeValue:
		const (
			nsPerHour   = int64(time.Hour)
			nsPerMinute = int64(time.Minute)
			nsPerSecond = int64(time.Second)
		)
		ns := v.Nanos
		h := int(ns / nsPerHour)
		ns -= int64(h) * nsPerHour
		mn := int(ns / nsPerMinute)
		ns -= int64(mn) * nsPerMinute
		s := int(ns / nsPerSecond)
		ns -= int64(s) * nsPerSecond
		y, mo, d := anchor.T.Date()
		return time.Date(y, mo, d, h, mn, s, int(ns), loc), true
	}
	return time.Time{}, false
}

// elapsedSecsAndNanos returns (b - a) decomposed into (whole seconds,
// residual nanoseconds in [0, 1e9)). Designed to survive extreme date
// ranges (±999999999 years) where time.Time.Sub would saturate at
// time.Duration's int64-nanosecond cap (~292 years). Used by the
// 2-arg duration.inDays / duration.inSeconds projections, which need
// elapsed magnitudes that can exceed int64-nanos but still fit in
// int64-seconds (6.3e16 for the TCK's ±999999999-year span).
//
// Dispatch mirrors [elapsedNanos]: time-only paths via the nanos-of-day
// axis (small magnitudes, no overflow risk); DST-aware projection for
// zoned+local date-bearing pairs; both-local date-bearing via a
// time.Time.Sub fast path when the year delta is moderate, falling
// through to the Julian-Day-Number arithmetic in [subSecsAndNanos] for
// extreme spans.
func elapsedSecsAndNanos(a, b expr.Value) (int64, int32, bool) {
	if _, aTO := isTimeOnly(a); aTO {
		ns, ok := elapsedNanosTimeOnly(a, b)
		if !ok {
			return 0, 0, false
		}
		s, n := secsNanosFromNanos(ns)
		return s, n, true
	}
	if _, bTO := isTimeOnly(b); bTO {
		ns, ok := elapsedNanosTimeOnly(a, b)
		if !ok {
			return 0, 0, false
		}
		s, n := secsNanosFromNanos(ns)
		return s, n, true
	}
	if ta, tb, ok := dstAwareInstants(a, b); ok {
		s, n := subSecsAndNanos(ta, tb)
		return s, n, true
	}
	la, oka := toLocalDateTime(a)
	if !oka {
		return 0, 0, false
	}
	lb, okb := toLocalDateTime(b)
	if !okb {
		return 0, 0, false
	}
	s, n := subSecsAndNanos(la.T, lb.T)
	return s, n, true
}

// secsNanosFromNanos splits a signed nanosecond count into (whole
// seconds, residual nanoseconds in [0, 1e9)). Negative inputs are
// normalised so the residual stays in [0, 1e9) and the seconds
// component absorbs the borrow.
func secsNanosFromNanos(ns int64) (int64, int32) {
	const nsPerSec = int64(1_000_000_000)
	s := ns / nsPerSec
	n := ns % nsPerSec
	if n < 0 {
		n += nsPerSec
		s--
	}
	return s, int32(n)
}

// subSecsAndNanos computes (b - a) as (seconds, residual-nanos)
// without losing precision when the span exceeds time.Duration's
// ±292-year cap. For inputs within ±200 years of each other the
// time.Time.Sub fast path is used (saturation cannot occur there);
// for wider spans the day count is taken from the Julian Day Number
// formula on the calendar dates and the time-of-day diff is added
// separately.
func subSecsAndNanos(a, b time.Time) (int64, int32) {
	const nsPerSec = int64(1_000_000_000)
	if d := int64(b.Year()) - int64(a.Year()); d > -200 && d < 200 {
		ns := b.Sub(a).Nanoseconds()
		return secsNanosFromNanos(ns)
	}
	days := julianDayGregorian(b.Year(), int(b.Month()), b.Day()) -
		julianDayGregorian(a.Year(), int(a.Month()), a.Day())
	secs := days * 86_400
	todA := int64(a.Hour())*3600 + int64(a.Minute())*60 + int64(a.Second())
	todB := int64(b.Hour())*3600 + int64(b.Minute())*60 + int64(b.Second())
	secs += todB - todA
	nanos := int64(b.Nanosecond()) - int64(a.Nanosecond())
	if nanos < 0 {
		nanos += nsPerSec
		secs--
	}
	return secs, int32(nanos)
}

// julianDayGregorian returns the Julian Day Number for the given
// proleptic Gregorian date. Uses the Fliegel–Van Flandern formula
// with int64 arithmetic throughout so years on the order of ±10^9
// are representable without overflow (largest intermediate, 365*y, is
// ~3.65e11 — comfortably within int64).
//
// The formula assumes integer division truncates toward zero. The
// initial year shift (y = year + 4800 - a) keeps y comfortably
// positive for all astronomically-relevant inputs (a is 0 or 1, so y
// is at least year + 4799), but for the openCypher extreme of
// year=-999999999 the shifted y still lands at -999995200 — a
// negative value cleanly divisible by 4, 100 and 400, so truncation
// and flooring coincide. No additional bias is required.
func julianDayGregorian(year, month, day int) int64 {
	a := int64((14 - month) / 12)
	y := int64(year) + 4800 - a
	m := int64(month) + 12*a - 3
	return int64(day) + (153*m+2)/5 + 365*y + y/4 - y/100 + y/400 - 32045
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
