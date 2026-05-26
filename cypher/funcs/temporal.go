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
//   - Ordinal:     {year, dayOfYear}     (day-of-year override)
//   - Week:        {year, week, dayOfWeek} (ISO week date, dow defaults 1)
//   - From base:   {date: D, ...overrides...} — copies D and applies overrides.
//
//nolint:gocyclo // Sequential overlay of map fields onto a DateValue; each branch is uniform — splitting hides the field-priority logic.
func dateFromMap(m expr.MapValue) (expr.Value, error) {
	// Base from {date: ...} if present.
	base := expr.DateValue{Year: 1970, Month: 1, Day: 1}
	if dv, ok := m["date"]; ok {
		if d, ok2 := dv.(expr.DateValue); ok2 {
			base = d
		}
	}
	year := base.Year
	month := base.Month
	day := base.Day
	if v, ok := m["year"]; ok {
		i, ok2 := intFromValue(v)
		if !ok2 {
			return expr.Null, nil
		}
		year = int(i)
	}
	// Week form takes priority when "week" is present.
	if wv, hasWeek := m["week"]; hasWeek {
		w, ok := intFromValue(wv)
		if !ok {
			return expr.Null, nil
		}
		dow := 1
		if dv, ok := m["dayOfWeek"]; ok {
			i, ok2 := intFromValue(dv)
			if !ok2 {
				return expr.Null, nil
			}
			dow = int(i)
		}
		dv, err := isoWeekDate(year, int(w), dow)
		if err != nil {
			return expr.Null, nil //nolint:nilerr // invalid components → NULL
		}
		return dv, nil
	}
	// Ordinal form when "dayOfYear" present without "month".
	if doyVal, hasDoy := m["dayOfYear"]; hasDoy {
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
		day = 1
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
		// Quarter→month=1+(q-1)*3 when no explicit month was provided.
		if _, hasMonth := m["month"]; !hasMonth {
			month = 1 + (int(i)-1)*3
		}
		if dq, ok := m["dayOfQuarter"]; ok {
			ii, ok3 := intFromValue(dq)
			if !ok3 {
				return expr.Null, nil
			}
			t := time.Date(year, time.Month(month), int(ii), 0, 0, 0, 0, time.UTC)
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
	return expr.NewDateTime(d.Year, d.Month, d.Day, h, mn, s, ns, loc), nil
}

// timeComponentsFromMap extracts hour/minute/second/nanosecond from m.
func timeComponentsFromMap(m expr.MapValue) (h, mn, s, ns int) {
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
	if v, ok := m["nanosecond"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			ns = int(i)
		}
	} else if v, ok := m["millisecond"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			ns = int(i) * 1_000_000
		}
	} else if v, ok := m["microsecond"]; ok {
		if i, ok2 := intFromValue(v); ok2 {
			ns = int(i) * 1_000
		}
	}
	return
}

// zoneFromMap resolves a "timezone" key to a *time.Location. Recognises:
//
//   - "Z" or "UTC"          → time.UTC
//   - "+HH:MM" or "-HH:MM"  → time.FixedZone with that offset
//   - Named zone string     → time.LoadLocation, falling back to UTC on error
//
// When no timezone key is present, returns time.UTC.
func zoneFromMap(m expr.MapValue) *time.Location {
	v, ok := m["timezone"]
	if !ok {
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
			off := 0
			if zv, ok := v["timezone"]; ok {
				if sv, ok2 := zv.(expr.StringValue); ok2 {
					if str := strings.TrimSpace(string(sv)); str != "" && str != "Z" {
						if o, err := parseSignedOffset(str); err == nil {
							off = o
						}
					}
				}
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

// fnDurationBetween computes the duration from t1 to t2 for any two temporal
// values of the same kind. Mixed-kind inputs return NULL per openCypher.
func fnDurationBetween(args []expr.Value) (expr.Value, error) {
	if err := requireArity("duration.between", args, 2); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) || expr.IsNull(args[1]) {
		return expr.Null, nil
	}
	switch a := args[0].(type) {
	case expr.DateValue:
		if b, ok := args[1].(expr.DateValue); ok {
			return expr.SubDates(b, a), nil
		}
	case expr.LocalDateTimeValue:
		if b, ok := args[1].(expr.LocalDateTimeValue); ok {
			return expr.SubLocalDateTimes(b, a), nil
		}
	case expr.DateTimeValue:
		if b, ok := args[1].(expr.DateTimeValue); ok {
			return expr.SubDateTimes(b, a), nil
		}
	case expr.LocalTimeValue:
		if b, ok := args[1].(expr.LocalTimeValue); ok {
			return expr.SubLocalTimes(b, a), nil
		}
	case expr.TimeValue:
		if b, ok := args[1].(expr.TimeValue); ok {
			return expr.SubTimes(b, a), nil
		}
	}
	return expr.Null, nil
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
		between, err := fnDurationBetween(args)
		if err != nil {
			return nil, err
		}
		if expr.IsNull(between) {
			return expr.Null, nil
		}
		d, _ := between.(expr.DurationValue)
		totalDays := d.Days + (d.Seconds / 86400)
		return expr.NewDuration(0, totalDays, 0, 0), nil
	default:
		return nil, &ArityError{Function: "duration.inDays", Got: len(args), Want: "1..2"}
	}
}

// fnDurationInSeconds supports two arities mirroring [fnDurationInMonths].
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
		between, err := fnDurationBetween(args)
		if err != nil {
			return nil, err
		}
		if expr.IsNull(between) {
			return expr.Null, nil
		}
		d, _ := between.(expr.DurationValue)
		totalSecs := d.Days*86400 + d.Seconds
		return expr.NewDuration(0, 0, totalSecs, d.Nanos), nil
	default:
		return nil, &ArityError{Function: "duration.inSeconds", Got: len(args), Want: "1..2"}
	}
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
