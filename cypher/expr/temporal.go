// temporal.go — openCypher temporal value kinds.
//
// This file introduces six new [Value] kinds backed by Go time primitives:
//
//   - [DateValue]            calendar date (year/month/day), no time, no zone.
//   - [LocalDateTimeValue]   date + time, no zone.
//   - [DateTimeValue]        date + time + zone.
//   - [LocalTimeValue]       time-of-day, no zone.
//   - [TimeValue]            time-of-day + zone offset.
//   - [DurationValue]        ISO-8601 duration: (months, days, seconds, nanos).
//
// # Semantics
//
// The semantics follow openCypher 9 §3.4 (Temporal values). Comparison and
// equality use three-valued logic — values of mixed kinds compare to NULL,
// matching numeric comparisons across non-promotable types. Arithmetic is
// implemented in [evalArith] (eval.go) via the helpers exposed here.
//
// Duration carries four independent components (months, days, seconds, nanos)
// because months and days do not reduce to fixed seconds (calendar arithmetic
// must apply them step by step). Comparison between two durations is only
// defined for component-wise equality; ordering on Duration is undefined per
// openCypher and we model it via hash to keep [Compare] total.
//
// # Concurrency
//
// All temporal Value implementations are immutable after construction.

package expr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Kind extensions
// ─────────────────────────────────────────────────────────────────────────────

// Temporal kind tags. These are appended to the [Kind] enumeration after the
// existing graph kinds; ordering for [Compare] is documented in
// [kindOrder].
const (
	// KindDate identifies a calendar date (year/month/day).
	KindDate Kind = iota + 16
	// KindLocalDateTime identifies a naive date-time (no zone).
	KindLocalDateTime
	// KindDateTime identifies a zoned date-time.
	KindDateTime
	// KindLocalTime identifies a naive time-of-day.
	KindLocalTime
	// KindTime identifies a zoned time-of-day.
	KindTime
	// KindDuration identifies an ISO-8601 duration.
	KindDuration
)

// temporalKindLabel returns a human-readable label for temporal kinds. It is
// invoked by [Kind.String] for tags >= [KindDate].
func temporalKindLabel(k Kind) string {
	switch k {
	case KindDate:
		return "Date"
	case KindLocalDateTime:
		return "LocalDateTime"
	case KindDateTime:
		return "DateTime"
	case KindLocalTime:
		return "LocalTime"
	case KindTime:
		return "Time"
	case KindDuration:
		return "Duration"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DateValue
// ─────────────────────────────────────────────────────────────────────────────

// DateValue is a calendar date (year/month/day) with no time-of-day and no
// time zone. The zero value (0001-01-01) is the proleptic Gregorian epoch
// matching Go's [time.Time] zero.
type DateValue struct {
	// Year is the Gregorian year.
	Year int
	// Month is 1–12.
	Month int
	// Day is 1–31, bounded by the days in Month.
	Day int
}

// NewDate constructs a [DateValue] from y/m/d, normalising overflow via
// [time.Date] semantics (e.g. month 13 wraps to next year).
func NewDate(y, m, d int) DateValue {
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return DateValue{Year: t.Year(), Month: int(t.Month()), Day: t.Day()}
}

// DateFromTime builds a DateValue from the calendar date of t (in t's own
// location).
func DateFromTime(t time.Time) DateValue {
	return DateValue{Year: t.Year(), Month: int(t.Month()), Day: t.Day()}
}

// ToTime returns the DateValue as a [time.Time] anchored at 00:00:00 UTC.
func (v DateValue) ToTime() time.Time {
	return time.Date(v.Year, time.Month(v.Month), v.Day, 0, 0, 0, 0, time.UTC)
}

// Kind implements [Value].
func (v DateValue) Kind() Kind { return KindDate }

// Hash implements [Value].
func (v DateValue) Hash() uint64 {
	h := uint64(v.Year)*131_071 + uint64(v.Month)*257 + uint64(v.Day)
	return h ^ (h >> 32)
}

// String renders the date in ISO-8601 extended form: YYYY-MM-DD.
func (v DateValue) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", v.Year, v.Month, v.Day)
}

// Equal implements [Value] with 3VL semantics.
func (v DateValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(DateValue)
	return BoolValue(ok && v == o)
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalDateTimeValue
// ─────────────────────────────────────────────────────────────────────────────

// LocalDateTimeValue is a date + time with no zone. Internally stored as
// [time.Time] in UTC; the UTC interpretation is a sentinel, not a real zone.
type LocalDateTimeValue struct {
	// T carries date and time components. The location is always set to UTC
	// for canonical storage; callers must not interpret it as a real offset.
	T time.Time
}

// NewLocalDateTime constructs a LocalDateTimeValue from components.
func NewLocalDateTime(y, mo, d, h, mi, s, ns int) LocalDateTimeValue {
	return LocalDateTimeValue{T: time.Date(y, time.Month(mo), d, h, mi, s, ns, time.UTC)}
}

// Kind implements [Value].
func (v LocalDateTimeValue) Kind() Kind { return KindLocalDateTime }

// Hash implements [Value].
func (v LocalDateTimeValue) Hash() uint64 {
	n := v.T.UnixNano()
	return uint64(n) ^ (uint64(n) >> 32)
}

// String renders ISO-8601 yyyy-mm-ddThh:mm:ss[.frac] with no zone suffix.
func (v LocalDateTimeValue) String() string { return formatLocalDateTime(v.T) }

// Equal implements [Value] with 3VL semantics. Two LocalDateTimeValues are
// equal iff their components are identical to the nanosecond.
func (v LocalDateTimeValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(LocalDateTimeValue)
	return BoolValue(ok && v.T.Equal(o.T))
}

// ─────────────────────────────────────────────────────────────────────────────
// DateTimeValue
// ─────────────────────────────────────────────────────────────────────────────

// DateTimeValue is a date + time + zone.
type DateTimeValue struct {
	// T carries date, time, and offset.
	T time.Time
}

// NewDateTime constructs a DateTimeValue from components and a [time.Location].
func NewDateTime(y, mo, d, h, mi, s, ns int, loc *time.Location) DateTimeValue {
	if loc == nil {
		loc = time.UTC
	}
	return DateTimeValue{T: time.Date(y, time.Month(mo), d, h, mi, s, ns, loc)}
}

// Kind implements [Value].
func (v DateTimeValue) Kind() Kind { return KindDateTime }

// Hash implements [Value].
func (v DateTimeValue) Hash() uint64 {
	n := v.T.UnixNano()
	return uint64(n) ^ (uint64(n) >> 32)
}

// String renders ISO-8601 yyyy-mm-ddThh:mm:ss[.frac] followed by the zone
// offset (Z for UTC, +HH:MM otherwise).
func (v DateTimeValue) String() string { return formatDateTime(v.T) }

// Equal implements [Value] with 3VL semantics. Two DateTimeValues are equal
// iff they refer to the same instant; the textual zone is not considered.
func (v DateTimeValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(DateTimeValue)
	return BoolValue(ok && v.T.Equal(o.T))
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalTimeValue
// ─────────────────────────────────────────────────────────────────────────────

// LocalTimeValue is a time-of-day with nanosecond precision and no zone.
type LocalTimeValue struct {
	// Nanos is the number of nanoseconds since midnight, in [0, 86_400_000_000_000).
	Nanos int64
}

// NewLocalTime constructs a LocalTimeValue from clock components.
func NewLocalTime(h, m, s, ns int) LocalTimeValue {
	total := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) +
		int64(s)*int64(time.Second) + int64(ns)
	return LocalTimeValue{Nanos: total}
}

// Kind implements [Value].
func (v LocalTimeValue) Kind() Kind { return KindLocalTime }

// Hash implements [Value].
func (v LocalTimeValue) Hash() uint64 { return uint64(v.Nanos) ^ (uint64(v.Nanos) >> 32) }

// String renders ISO-8601 hh:mm:ss[.frac].
func (v LocalTimeValue) String() string { return formatNanosToTime(v.Nanos) }

// Equal implements [Value] with 3VL semantics.
func (v LocalTimeValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(LocalTimeValue)
	return BoolValue(ok && v.Nanos == o.Nanos)
}

// ─────────────────────────────────────────────────────────────────────────────
// TimeValue
// ─────────────────────────────────────────────────────────────────────────────

// TimeValue is a time-of-day with zone offset (seconds east of UTC).
type TimeValue struct {
	// Nanos is the number of nanoseconds since midnight, in [0, 86_400_000_000_000).
	Nanos int64
	// OffsetSec is the zone offset in seconds east of UTC. UTC is 0.
	OffsetSec int32
}

// NewTime constructs a TimeValue from clock components and a zone offset in
// seconds east of UTC.
func NewTime(h, m, s, ns, offsetSec int) TimeValue {
	total := int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) +
		int64(s)*int64(time.Second) + int64(ns)
	return TimeValue{Nanos: total, OffsetSec: int32(offsetSec)}
}

// Kind implements [Value].
func (v TimeValue) Kind() Kind { return KindTime }

// Hash implements [Value].
func (v TimeValue) Hash() uint64 {
	h := uint64(v.Nanos)*257 + uint64(int64(v.OffsetSec))
	return h ^ (h >> 32)
}

// String renders ISO-8601 hh:mm:ss[.frac] + zone offset.
func (v TimeValue) String() string {
	return formatNanosToTime(v.Nanos) + formatOffsetSec(int(v.OffsetSec))
}

// Equal implements [Value] with 3VL semantics. Two TimeValues are equal iff
// both Nanos and OffsetSec are identical (note: 12:00+00 ≠ 13:00+01:00 even
// though they refer to the same instant; this matches openCypher).
func (v TimeValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(TimeValue)
	return BoolValue(ok && v.Nanos == o.Nanos && v.OffsetSec == o.OffsetSec)
}

// ─────────────────────────────────────────────────────────────────────────────
// DurationValue
// ─────────────────────────────────────────────────────────────────────────────

// DurationValue is an ISO-8601 duration with four independent components.
// Months and days do not reduce to seconds; the executor applies them
// calendar-aware when added to a temporal value.
type DurationValue struct {
	// Months is the number of whole months (may be negative).
	Months int64
	// Days is the number of whole days (may be negative).
	Days int64
	// Seconds is the number of whole seconds (may be negative).
	Seconds int64
	// Nanos is the sub-second component in [0, 999_999_999]; the sign is
	// carried entirely by Seconds for canonical form.
	Nanos int32
}

// NewDuration normalises the components to canonical form (Nanos in
// [0, 999_999_999], sign carried by Seconds).
func NewDuration(months, days, seconds int64, nanos int32) DurationValue {
	const nanoPerSec = int32(1_000_000_000)
	// Normalise Nanos to [0, 1e9). The carry can be positive or negative.
	if nanos <= -nanoPerSec || nanos >= nanoPerSec {
		seconds += int64(nanos / nanoPerSec)
		nanos %= nanoPerSec
	}
	// Borrow when nanos is negative so Nanos lands in [0, 1e9).
	if nanos < 0 {
		nanos += nanoPerSec
		seconds--
	}
	return DurationValue{Months: months, Days: days, Seconds: seconds, Nanos: nanos}
}

// Kind implements [Value].
func (v DurationValue) Kind() Kind { return KindDuration }

// Hash implements [Value].
func (v DurationValue) Hash() uint64 {
	h := uint64(v.Months)*1_000_003 +
		uint64(v.Days)*131_071 +
		uint64(v.Seconds)*257 +
		uint64(v.Nanos)
	return h ^ (h >> 32)
}

// String renders ISO-8601 duration: P[nM][nD]T[nS]. Always emits the leading
// "P" and the "T" separator. Zero components are omitted; the all-zero
// duration renders as "PT0S".
func (v DurationValue) String() string { return formatDuration(v) }

// Equal implements [Value] with 3VL semantics. Component-wise equality.
func (v DurationValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(DurationValue)
	return BoolValue(ok && v == o)
}

// ─────────────────────────────────────────────────────────────────────────────
// Formatting helpers
// ─────────────────────────────────────────────────────────────────────────────

// formatLocalDateTime renders t as ISO-8601 yyyy-mm-ddThh:mm[:ss[.frac]]
// with no zone suffix. The seconds field is elided when seconds and
// nanoseconds are both zero, matching the openCypher TCK canonical
// shortest form.
func formatLocalDateTime(t time.Time) string {
	dateHeader := fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
	timeTail := formatHMSNanos(t.Hour(), t.Minute(), t.Second(), t.Nanosecond())
	return dateHeader + "T" + timeTail
}

// formatDateTime renders t as ISO-8601 yyyy-mm-ddThh:mm[:ss[.frac]]±HH:MM
// (or "Z" for UTC). Like formatLocalDateTime, the seconds field is
// elided when seconds and nanoseconds are both zero.
func formatDateTime(t time.Time) string {
	zoneName, offset := t.Zone()
	zone := formatOffsetSec(offset)
	dateHeader := fmt.Sprintf("%04d-%02d-%02d", t.Year(), int(t.Month()), t.Day())
	timeTail := formatHMSNanos(t.Hour(), t.Minute(), t.Second(), t.Nanosecond())
	// Append the IANA timezone name in square brackets when the location is a
	// named timezone (not a raw UTC-offset zone). Named timezones can be
	// identified by the presence of "/" in the name (IANA format) or by the
	// location name not matching the offset abbreviation pattern (e.g.
	// "UTC+1", "UTC", "GMT"). The openCypher TCK uses the format
	// "yyyy-mm-ddThh:mm:ss+hh:mm[TZ/Name]" for named-timezone datetimes.
	loc := t.Location()
	locName := loc.String()
	if locName != "UTC" && locName != "Local" && locName != "" &&
		locName != zoneName && strings.Contains(locName, "/") {
		zone = zone + "[" + locName + "]"
	}
	return dateHeader + "T" + timeTail + zone
}

// formatHMSNanos returns hh:mm or hh:mm:ss[.frac] depending on whether
// the seconds/nanoseconds components are observably non-zero. Shared
// between formatLocalDateTime, formatDateTime and (indirectly via the
// LocalTimeValue / TimeValue String methods) formatNanosToTime.
func formatHMSNanos(h, m, s, nano int) string {
	if s == 0 && nano == 0 {
		return fmt.Sprintf("%02d:%02d", h, m)
	}
	return fmt.Sprintf("%02d:%02d:%02d%s", h, m, s, formatFraction(nano))
}

// formatNanosToTime renders ns (since midnight) in the openCypher
// canonical textual form:
//
//	hh:mm                              when seconds and nanoseconds are zero
//	hh:mm:ss                           when only nanoseconds are zero
//	hh:mm:ss.frac                      otherwise (trailing zeros trimmed)
//
// The TCK uses the shortest representation that round-trips, so a
// time at the top of the hour or minute should not surface an
// explicit ":00" trailer.
func formatNanosToTime(ns int64) string {
	if ns < 0 {
		ns = 0
	}
	h := ns / int64(time.Hour)
	rem := ns % int64(time.Hour)
	m := rem / int64(time.Minute)
	rem %= int64(time.Minute)
	s := rem / int64(time.Second)
	nano := int(rem % int64(time.Second))
	if s == 0 && nano == 0 {
		return fmt.Sprintf("%02d:%02d", h, m)
	}
	frac := formatFraction(nano)
	return fmt.Sprintf("%02d:%02d:%02d%s", h, m, s, frac)
}

// formatFraction returns ".nnnnnnnnn" with trailing zeros trimmed, or "" when
// nanos is zero.
func formatFraction(nanos int) string {
	if nanos == 0 {
		return ""
	}
	s := fmt.Sprintf(".%09d", nanos)
	return strings.TrimRight(s, "0")
}

// formatOffsetSec renders an offset in seconds as "Z" (when zero) or "±HH:MM".
func formatOffsetSec(secs int) string {
	if secs == 0 {
		return "Z"
	}
	sign := "+"
	if secs < 0 {
		sign = "-"
		secs = -secs
	}
	// Some IANA zones carry historical sub-minute offsets (e.g.
	// Europe/Stockholm before 1900 used +00:53:28). Append the seconds
	// component only when it is non-zero so the common minute-aligned
	// form `+HH:MM` is preserved for modern data.
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if s != 0 {
		return fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, s)
	}
	return fmt.Sprintf("%s%02d:%02d", sign, h, m)
}

// formatDuration renders a [DurationValue] in canonical ISO-8601 form.
//
// The textual form follows openCypher §3.4.4: years and months are emitted as
// distinct components, days separately, and the time portion as H/M/S
// sub-components. Zero components are omitted; the all-zero duration renders
// as "PT0S". Sub-second precision (Nanos) is folded into the seconds
// component as a decimal fraction.
//
//nolint:gocyclo // Sequential ISO-8601 component emission; the branches are flat and uniform — splitting hides the layout.
func formatDuration(d DurationValue) string {
	if d.Months == 0 && d.Days == 0 && d.Seconds == 0 && d.Nanos == 0 {
		return "PT0S"
	}
	var b strings.Builder
	b.WriteByte('P')
	if d.Months != 0 {
		years := d.Months / 12
		months := d.Months % 12
		if years != 0 {
			b.WriteString(strconv.FormatInt(years, 10))
			b.WriteByte('Y')
		}
		if months != 0 {
			b.WriteString(strconv.FormatInt(months, 10))
			b.WriteByte('M')
		}
	}
	if d.Days != 0 {
		b.WriteString(strconv.FormatInt(d.Days, 10))
		b.WriteByte('D')
	}
	if d.Seconds != 0 || d.Nanos != 0 {
		b.WriteByte('T')
		// Normalise (Seconds, Nanos) for splitting into H/M/S. Canonical
		// form has Nanos in [0,1e9); when Seconds is negative we keep the
		// sign in the magnitude.
		secs := d.Seconds
		nanos := int64(d.Nanos)
		// Effective signed nanoseconds.
		totalNs := secs*1_000_000_000 + nanos
		negative := totalNs < 0
		if negative {
			totalNs = -totalNs
		}
		hours := totalNs / int64(time.Hour)
		totalNs -= hours * int64(time.Hour)
		minutes := totalNs / int64(time.Minute)
		totalNs -= minutes * int64(time.Minute)
		sWhole := totalNs / int64(time.Second)
		sFrac := totalNs - sWhole*int64(time.Second)
		sign := ""
		if negative {
			sign = "-"
		}
		if hours != 0 {
			b.WriteString(sign)
			b.WriteString(strconv.FormatInt(hours, 10))
			b.WriteByte('H')
		}
		if minutes != 0 {
			b.WriteString(sign)
			b.WriteString(strconv.FormatInt(minutes, 10))
			b.WriteByte('M')
		}
		if sWhole != 0 || sFrac != 0 || (hours == 0 && minutes == 0) {
			b.WriteString(sign)
			b.WriteString(strconv.FormatInt(sWhole, 10))
			if sFrac != 0 {
				fracStr := fmt.Sprintf(".%09d", sFrac)
				fracStr = strings.TrimRight(fracStr, "0")
				b.WriteString(fracStr)
			}
			b.WriteByte('S')
		}
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────────
// Parsing helpers
// ─────────────────────────────────────────────────────────────────────────────

// ParseDate parses an ISO-8601 date string. Accepted forms (calendar, week,
// ordinal):
//
//	YYYY-MM-DD, YYYYMMDD, YYYY-MM, YYYYMM, YYYY,
//	YYYY-Www-D, YYYYWwwD, YYYY-Www, YYYYWww,
//	YYYY-DDD, YYYYDDD.
//
// Returns NULL-able semantics: callers receiving an error should treat the
// value as openCypher TYPE_ERROR (today: surfaced as expr.Null by the caller).
//
//nolint:gocyclo // ISO-8601 date is a multi-shape grammar; one switch over length+separator presence is the clearest realisation.
func ParseDate(s string) (DateValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DateValue{}, fmt.Errorf("empty date string")
	}

	// Ordinal: YYYY-DDD or YYYYDDD (length 8 with no dash).
	if strings.Contains(s, "-") {
		// Try YYYY-DDD (length 8 — must be exactly digits/dash).
		if len(s) == 8 && s[4] == '-' {
			if y, ok := parseUint(s[:4]); ok {
				if doy, ok2 := parseUint(s[5:]); ok2 {
					return dateFromOrdinal(int(y), int(doy))
				}
			}
		}
	} else {
		// YYYYDDD (length 7) — ordinal compact.
		if len(s) == 7 {
			if y, ok := parseUint(s[:4]); ok {
				if doy, ok2 := parseUint(s[4:]); ok2 {
					return dateFromOrdinal(int(y), int(doy))
				}
			}
		}
	}

	// Week: YYYY-Www-D, YYYYWwwD, YYYY-Www, YYYYWww.
	if strings.Contains(s, "W") || strings.Contains(s, "w") {
		return parseWeekDate(s)
	}

	// Calendar: YYYY-MM-DD or YYYYMMDD.
	if strings.Contains(s, "-") {
		// YYYY-MM-DD
		if len(s) == 10 {
			y, ok1 := parseUint(s[:4])
			m, ok2 := parseUint(s[5:7])
			d, ok3 := parseUint(s[8:])
			if ok1 && ok2 && ok3 && s[4] == '-' && s[7] == '-' {
				return NewDate(int(y), int(m), int(d)), nil
			}
		}
		// YYYY-MM
		if len(s) == 7 && s[4] == '-' {
			y, ok1 := parseUint(s[:4])
			m, ok2 := parseUint(s[5:])
			if ok1 && ok2 {
				return NewDate(int(y), int(m), 1), nil
			}
		}
	} else {
		// YYYYMMDD
		if len(s) == 8 {
			y, ok1 := parseUint(s[:4])
			m, ok2 := parseUint(s[4:6])
			d, ok3 := parseUint(s[6:])
			if ok1 && ok2 && ok3 {
				return NewDate(int(y), int(m), int(d)), nil
			}
		}
		// YYYYMM
		if len(s) == 6 {
			y, ok1 := parseUint(s[:4])
			m, ok2 := parseUint(s[4:])
			if ok1 && ok2 {
				return NewDate(int(y), int(m), 1), nil
			}
		}
		// YYYY
		if len(s) == 4 {
			y, ok := parseUint(s)
			if ok {
				return NewDate(int(y), 1, 1), nil
			}
		}
	}
	return DateValue{}, fmt.Errorf("invalid date string: %q", s)
}

// dateFromOrdinal returns the date for the given ordinal day-of-year.
func dateFromOrdinal(year, doy int) (DateValue, error) {
	if doy < 1 || doy > 366 {
		return DateValue{}, fmt.Errorf("ordinal day %d out of range", doy)
	}
	t := time.Date(year, 1, doy, 0, 0, 0, 0, time.UTC)
	return DateFromTime(t), nil
}

// parseWeekDate parses ISO-8601 week dates: YYYY-Www-D, YYYYWwwD,
// YYYY-Www, YYYYWww.
func parseWeekDate(s string) (DateValue, error) {
	// Normalise: locate the 'W' (or 'w') position. Extended form has a dash
	// between the year and the 'W' (`2015-W30`), compact form does not
	// (`2015W30`). Accept either; the W must therefore appear at index 4 or
	// 5 of the original string.
	wi := strings.IndexAny(s, "Ww")
	if wi != 4 && wi != 5 {
		return DateValue{}, fmt.Errorf("invalid week date: %q", s)
	}
	if wi == 5 && s[4] != '-' {
		return DateValue{}, fmt.Errorf("invalid week date: %q", s)
	}
	yStr := s[:4]
	rest := s[wi+1:]

	y, ok := parseUint(yStr)
	if !ok {
		return DateValue{}, fmt.Errorf("invalid week-date year: %q", s)
	}
	// Strip leading dash between W-block and day component (extended form).
	rest = strings.TrimPrefix(rest, "-")
	if len(rest) < 2 {
		return DateValue{}, fmt.Errorf("invalid week-date: %q", s)
	}
	// Week (always two digits).
	w, ok := parseUint(rest[:2])
	if !ok {
		return DateValue{}, fmt.Errorf("invalid week number: %q", s)
	}
	week := int(w)
	rest = rest[2:]
	// Optional day-of-week.
	dow := 0
	if rest != "" {
		rest = strings.TrimPrefix(rest, "-")
		if rest != "" {
			d, ok := parseUint(rest)
			if !ok {
				return DateValue{}, fmt.Errorf("invalid weekday: %q", s)
			}
			dow = int(d)
		}
	}
	if dow == 0 {
		dow = 1 // Monday default per openCypher
	}
	return dateFromIsoWeek(int(y), week, dow)
}

// dateFromIsoWeek converts (ISO year, ISO week, day-of-week 1=Mon..7=Sun) to
// a [DateValue].
func dateFromIsoWeek(isoYear, isoWeek, dow int) (DateValue, error) {
	if dow < 1 || dow > 7 || isoWeek < 1 || isoWeek > 53 {
		return DateValue{}, fmt.Errorf("invalid ISO week: year=%d week=%d dow=%d", isoYear, isoWeek, dow)
	}
	// 4 January is always in ISO week 1.
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	// Monday of week 1.
	w0 := int(jan4.Weekday())
	if w0 == 0 { // Sunday → 7 in ISO numbering
		w0 = 7
	}
	mondayWeek1 := jan4.AddDate(0, 0, -(w0 - 1))
	t := mondayWeek1.AddDate(0, 0, (isoWeek-1)*7+(dow-1))
	return DateFromTime(t), nil
}

// parseUint parses a non-negative integer.
func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ParseLocalTime parses ISO-8601 time strings without zone: HH:MM:SS[.frac],
// HHMMSS[.frac], HH:MM, HHMM, HH.
func ParseLocalTime(s string) (LocalTimeValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LocalTimeValue{}, fmt.Errorf("empty time string")
	}
	h, m, sec, ns, _, err := parseTimeComponents(s)
	if err != nil {
		return LocalTimeValue{}, err
	}
	return NewLocalTime(h, m, sec, ns), nil
}

// ParseTime parses ISO-8601 time strings WITH zone: HH:MM:SS[.frac]±HH:MM or
// HH:MM:SS[.frac]Z. Bare time strings (no zone) are rejected.
func ParseTime(s string) (TimeValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return TimeValue{}, fmt.Errorf("empty time string")
	}
	h, m, sec, ns, off, err := parseTimeComponents(s)
	if err != nil {
		return TimeValue{}, err
	}
	return NewTime(h, m, sec, ns, off), nil
}

// parseTimeComponents extracts (h, m, s, ns, offsetSec) from a time string.
// offsetSec is set to 0 when no zone is present. Both compact (HHMMSS) and
// extended (HH:MM:SS) forms are accepted.
//
//nolint:gocyclo,gocritic // Sequential extract of multiple optional fields; the named-result signature is the natural Go idiom for this multi-value parser.
func parseTimeComponents(s string) (h, m, sec, ns, offsetSec int, err error) {
	// Extract the zone suffix first.
	rest := s
	offsetSec = 0
	if idx := indexZoneStart(s); idx >= 0 {
		rest = s[:idx]
		off, oerr := parseOffset(s[idx:])
		if oerr != nil {
			err = oerr
			return
		}
		offsetSec = off
	}
	// Split on '.' for fractional seconds.
	mainPart, fracStr, _ := strings.Cut(rest, ".")
	// Parse the H/M/S triple.
	hh, mm, ss, perr := parseHMS(mainPart)
	if perr != nil {
		err = perr
		return
	}
	h, m, sec = hh, mm, ss
	// Parse fractional seconds (max 9 digits).
	if fracStr != "" {
		if len(fracStr) > 9 {
			fracStr = fracStr[:9]
		}
		// Right-pad with zeros to 9 digits to get nanoseconds.
		padded := fracStr + strings.Repeat("0", 9-len(fracStr))
		nv, perr := strconv.ParseUint(padded, 10, 64)
		if perr != nil {
			err = fmt.Errorf("invalid fractional seconds: %q", fracStr)
			return
		}
		ns = int(nv)
	}
	return
}

// parseHMS extracts hours/minutes/seconds from a compact or extended time
// string. Supported: HH, HHMM, HHMMSS, HH:MM, HH:MM:SS.
func parseHMS(s string) (h, m, sec int, err error) {
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		switch len(parts) {
		case 1:
			h, err = parseInt(parts[0])
		case 2:
			h, err = parseInt(parts[0])
			if err == nil {
				m, err = parseInt(parts[1])
			}
		case 3:
			h, err = parseInt(parts[0])
			if err == nil {
				m, err = parseInt(parts[1])
				if err == nil {
					sec, err = parseInt(parts[2])
				}
			}
		default:
			err = fmt.Errorf("invalid time triple: %q", s)
		}
		return
	}
	// Compact.
	switch len(s) {
	case 2:
		h, err = parseInt(s)
	case 4:
		h, err = parseInt(s[:2])
		if err == nil {
			m, err = parseInt(s[2:])
		}
	case 6:
		h, err = parseInt(s[:2])
		if err == nil {
			m, err = parseInt(s[2:4])
			if err == nil {
				sec, err = parseInt(s[4:])
			}
		}
	default:
		err = fmt.Errorf("invalid compact time: %q", s)
	}
	return
}

// parseInt parses a non-negative decimal integer into int.
func parseInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty integer")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// indexZoneStart returns the index of the zone-suffix start, or -1 if none.
// The zone starts at 'Z', '+', or '-'. A leading '-' in the year portion is
// out of scope (callers pass only the time tail).
func indexZoneStart(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 'Z' || c == '+' {
			return i
		}
		// '-' is a zone indicator only if preceded by digits (HMS body).
		// We look at the position: after at least 2 chars of HMS.
		if c == '-' && i >= 2 {
			return i
		}
	}
	return -1
}

// parseOffset parses a zone offset suffix: "Z", "+HH:MM", "+HHMM", "+HH",
// "-HH:MM", "-HHMM", or "-HH". An optional trailing bracketed IANA zone name
// (e.g. "+02:00[Europe/Stockholm]") is stripped — the numeric offset is the
// canonical wall-clock anchor and is preserved verbatim in the result.
func parseOffset(s string) (int, error) {
	// Drop a trailing bracketed IANA zone name. The named location is not
	// preserved in [TimeValue]; the numeric offset is what later arithmetic
	// uses, so dropping the bracket here keeps the parser tolerant of the
	// extended Neo4j-style zone suffix accepted by openCypher tests.
	if i := strings.IndexByte(s, '['); i >= 0 {
		s = s[:i]
	}
	if s == "Z" {
		return 0, nil
	}
	if len(s) < 3 {
		return 0, fmt.Errorf("invalid offset: %q", s)
	}
	var sign int
	switch s[0] {
	case '+':
		sign = 1
	case '-':
		sign = -1
	default:
		return 0, fmt.Errorf("invalid offset sign: %q", s)
	}
	rest := s[1:]
	// Strip colon if present.
	rest = strings.ReplaceAll(rest, ":", "")
	switch len(rest) {
	case 2: // HH
		h, err := strconv.Atoi(rest)
		if err != nil {
			return 0, err
		}
		return sign * h * 3600, nil
	case 4: // HHMM
		h, err := strconv.Atoi(rest[:2])
		if err != nil {
			return 0, err
		}
		m, err := strconv.Atoi(rest[2:])
		if err != nil {
			return 0, err
		}
		return sign * (h*3600 + m*60), nil
	case 6: // HHMMSS — historical sub-minute IANA zones (e.g. +00:53:28).
		h, err := strconv.Atoi(rest[:2])
		if err != nil {
			return 0, err
		}
		m, err := strconv.Atoi(rest[2:4])
		if err != nil {
			return 0, err
		}
		s, err := strconv.Atoi(rest[4:])
		if err != nil {
			return 0, err
		}
		return sign * (h*3600 + m*60 + s), nil
	default:
		return 0, fmt.Errorf("invalid offset body: %q", rest)
	}
}

// ParseLocalDateTime parses ISO-8601 local date-time: YYYY-MM-DDTHH:MM:SS[.frac]
// with no zone suffix. Both extended (with separators) and compact forms are
// accepted; the 'T' separator may be lowercase.
func ParseLocalDateTime(s string) (LocalDateTimeValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return LocalDateTimeValue{}, fmt.Errorf("empty local date-time")
	}
	idx := strings.IndexAny(s, "Tt")
	if idx < 0 {
		return LocalDateTimeValue{}, fmt.Errorf("missing T separator: %q", s)
	}
	dv, err := ParseDate(s[:idx])
	if err != nil {
		return LocalDateTimeValue{}, err
	}
	lt, err := ParseLocalTime(s[idx+1:])
	if err != nil {
		return LocalDateTimeValue{}, err
	}
	hh := int(lt.Nanos / int64(time.Hour))
	rem := lt.Nanos % int64(time.Hour)
	mm := int(rem / int64(time.Minute))
	rem %= int64(time.Minute)
	ss := int(rem / int64(time.Second))
	nn := int(rem % int64(time.Second))
	return NewLocalDateTime(dv.Year, dv.Month, dv.Day, hh, mm, ss, nn), nil
}

// ParseDateTime parses ISO-8601 zoned date-time: YYYY-MM-DDTHH:MM:SS[.frac][±HH:MM|Z].
// A zone suffix is required. An optional trailing bracketed IANA zone name
// (e.g. "[Europe/Stockholm]") is honoured when [time.LoadLocation] resolves
// it; otherwise the numeric offset is used to build a fixed-zone location.
func ParseDateTime(s string) (DateTimeValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DateTimeValue{}, fmt.Errorf("empty date-time")
	}
	// Extract an optional [Zone/Name] suffix before slicing the body —
	// ParseTime drops the bracket section silently via parseOffset, so we
	// need to peel it here to honour the named zone for the DateTime.
	var zoneName string
	if i := strings.LastIndexByte(s, '['); i >= 0 && strings.HasSuffix(s, "]") {
		zoneName = s[i+1 : len(s)-1]
		s = s[:i]
	}
	idx := strings.IndexAny(s, "Tt")
	if idx < 0 {
		return DateTimeValue{}, fmt.Errorf("missing T separator: %q", s)
	}
	dv, err := ParseDate(s[:idx])
	if err != nil {
		return DateTimeValue{}, err
	}
	tv, err := ParseTime(s[idx+1:])
	if err != nil {
		return DateTimeValue{}, err
	}
	loc := offsetLocation(int(tv.OffsetSec))
	if zoneName != "" {
		if l, lerr := time.LoadLocation(zoneName); lerr == nil {
			loc = l
		}
	}
	hh := int(tv.Nanos / int64(time.Hour))
	rem := tv.Nanos % int64(time.Hour)
	mm := int(rem / int64(time.Minute))
	rem %= int64(time.Minute)
	ss := int(rem / int64(time.Second))
	nn := int(rem % int64(time.Second))
	return NewDateTime(dv.Year, dv.Month, dv.Day, hh, mm, ss, nn, loc), nil
}

// offsetLocation returns a [time.Location] for the given UTC-offset seconds.
// UTC is returned for offset 0.
func offsetLocation(offsetSec int) *time.Location {
	if offsetSec == 0 {
		return time.UTC
	}
	return time.FixedZone(formatOffsetSec(offsetSec), offsetSec)
}

// ParseDuration parses an ISO-8601 duration: P[nY][nMo][nW][nD][T[nH][nMi][nS]].
// Fractional components are supported for the final emitted unit (per openCypher);
// internally the parser absorbs fractional months/days as fractional seconds added
// to the appropriate component.
//
//nolint:gocyclo // Sequential state-machine over ISO-8601 duration components.
func ParseDuration(s string) (DurationValue, error) {
	s = strings.TrimSpace(s)
	if s == "" || (s[0] != 'P' && s[0] != 'p') {
		return DurationValue{}, fmt.Errorf("invalid duration prefix: %q", s)
	}
	rest := s[1:]
	var (
		months  int64
		days    int64
		seconds int64
		nanos   int32
	)
	inTime := false
	for rest != "" {
		if rest[0] == 'T' || rest[0] == 't' {
			inTime = true
			rest = rest[1:]
			continue
		}
		// Parse numeric prefix (possibly negative, possibly fractional).
		num, unit, tail, err := scanDurationToken(rest)
		if err != nil {
			return DurationValue{}, err
		}
		rest = tail
		intPart, fracPart := splitFractional(num)
		switch {
		case !inTime && unit == 'Y':
			months += intPart * 12
			// Fractional years → fractional months → carry into days/seconds later.
			fracMonths := fracPart * 12
			fInt, fFrac := splitFloat(fracMonths)
			months += fInt
			days += daysPerMonthEstimate(fFrac)
		case !inTime && unit == 'M':
			months += intPart
			// Fractional months → carry into seconds (avg month =
			// 30.4368750 days = 365.2425/12, the Gregorian average
			// per openCypher).
			daysFloat := fracPart * 30.436875
			d, sNano := splitDaysToSeconds(daysFloat)
			days += d
			seconds += sNano / 1_000_000_000
			nanos += int32(sNano % 1_000_000_000)
		case !inTime && unit == 'W':
			days += intPart * 7
			// Fractional weeks → seconds.
			daysFloat := fracPart * 7
			d, sNano := splitDaysToSeconds(daysFloat)
			days += d
			seconds += sNano / 1_000_000_000
			nanos += int32(sNano % 1_000_000_000)
		case !inTime && unit == 'D':
			days += intPart
			daysFloat := fracPart
			_, sNano := splitDaysToSeconds(daysFloat)
			seconds += sNano / 1_000_000_000
			nanos += int32(sNano % 1_000_000_000)
		case inTime && unit == 'H':
			seconds += intPart * 3600
			// Fractional hours → seconds + nanos.
			extraNs := int64(fracPart * 3600 * 1_000_000_000)
			seconds += extraNs / 1_000_000_000
			nanos += int32(extraNs % 1_000_000_000)
		case inTime && unit == 'M':
			seconds += intPart * 60
			extraNs := int64(fracPart * 60 * 1_000_000_000)
			seconds += extraNs / 1_000_000_000
			nanos += int32(extraNs % 1_000_000_000)
		case inTime && unit == 'S':
			seconds += intPart
			extraNs := int64(fracPart * 1_000_000_000)
			seconds += extraNs / 1_000_000_000
			nanos += int32(extraNs % 1_000_000_000)
		default:
			return DurationValue{}, fmt.Errorf("unexpected duration unit %q in %q", unit, s)
		}
	}
	return NewDuration(months, days, seconds, nanos), nil
}

// scanDurationToken returns (numeric, unit, tail) extracted from the head of s.
// Negative numbers are supported via a leading '-'. Decimal fractions are
// supported via '.' or ',' (ISO allows comma).
func scanDurationToken(s string) (num float64, unit byte, tail string, err error) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	for i < len(s) && (isDigit(s[i]) || s[i] == '.' || s[i] == ',') {
		i++
	}
	if i == 0 || i == len(s) {
		err = fmt.Errorf("missing duration unit in %q", s)
		return
	}
	body := s[:i]
	body = strings.Replace(body, ",", ".", 1)
	num, err = strconv.ParseFloat(body, 64)
	if err != nil {
		return
	}
	unit = s[i]
	tail = s[i+1:]
	return
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// splitFractional returns the integer floor and fractional remainder of f.
func splitFractional(f float64) (intPart int64, fracPart float64) {
	intPart = int64(f)
	fracPart = f - float64(intPart)
	return
}

// splitFloat returns the integer floor and the leftover fraction.
func splitFloat(f float64) (intPart int64, fracPart float64) {
	intPart = int64(f)
	fracPart = f - float64(intPart)
	return
}

// daysPerMonthEstimate returns the approximate number of whole days for the
// fractional months input, using the Gregorian average month
// (365.2425/12 = 30.4368750 days) — the constant openCypher uses for
// fractional-month-to-day projection in its duration model.
func daysPerMonthEstimate(fracMonths float64) int64 {
	return int64(fracMonths * 30.436875)
}

// splitDaysToSeconds returns (whole days, total nanoseconds for the rest).
func splitDaysToSeconds(d float64) (days, nanos int64) {
	whole := int64(d)
	frac := d - float64(whole)
	ns := int64(math.Round(frac * 86400 * 1_000_000_000))
	return whole, ns
}

// ─────────────────────────────────────────────────────────────────────────────
// Arithmetic primitives (used by evalArith)
// ─────────────────────────────────────────────────────────────────────────────

// AddDurationToDate adds d to dv with calendar-aware rules: months are applied
// first (clamping to month-end), then days, then sub-day components are dropped
// (Date has no sub-day component).
func AddDurationToDate(dv DateValue, d DurationValue) DateValue {
	t := time.Date(dv.Year, time.Month(dv.Month), dv.Day, 0, 0, 0, 0, time.UTC)
	t = t.AddDate(0, int(d.Months), int(d.Days))
	// Apply seconds & nanos by truncating to the integer day boundary.
	if d.Seconds != 0 || d.Nanos != 0 {
		totalNs := d.Seconds*1_000_000_000 + int64(d.Nanos)
		dayNs := int64(86_400) * 1_000_000_000
		dayShift := totalNs / dayNs
		t = t.Add(time.Duration(dayShift * dayNs))
	}
	return DateFromTime(t)
}

// SubDurationFromDate is the inverse of [AddDurationToDate].
func SubDurationFromDate(dv DateValue, d DurationValue) DateValue {
	return AddDurationToDate(dv, NegateDuration(d))
}

// AddDurationToLocalDateTime adds d to v: months → days → seconds → nanos.
func AddDurationToLocalDateTime(v LocalDateTimeValue, d DurationValue) LocalDateTimeValue {
	t := v.T.AddDate(0, int(d.Months), int(d.Days))
	t = t.Add(time.Duration(d.Seconds)*time.Second + time.Duration(d.Nanos)*time.Nanosecond)
	return LocalDateTimeValue{T: t}
}

// SubDurationFromLocalDateTime is the inverse of [AddDurationToLocalDateTime].
func SubDurationFromLocalDateTime(v LocalDateTimeValue, d DurationValue) LocalDateTimeValue {
	return AddDurationToLocalDateTime(v, NegateDuration(d))
}

// AddDurationToDateTime adds d to v, preserving v's zone.
func AddDurationToDateTime(v DateTimeValue, d DurationValue) DateTimeValue {
	t := v.T.AddDate(0, int(d.Months), int(d.Days))
	t = t.Add(time.Duration(d.Seconds)*time.Second + time.Duration(d.Nanos)*time.Nanosecond)
	return DateTimeValue{T: t}
}

// SubDurationFromDateTime is the inverse of [AddDurationToDateTime].
func SubDurationFromDateTime(v DateTimeValue, d DurationValue) DateTimeValue {
	return AddDurationToDateTime(v, NegateDuration(d))
}

// AddDurationToLocalTime wraps modulo 24h.
func AddDurationToLocalTime(v LocalTimeValue, d DurationValue) LocalTimeValue {
	const dayNs = int64(86_400) * 1_000_000_000
	add := d.Seconds*1_000_000_000 + int64(d.Nanos)
	total := (v.Nanos + add) % dayNs
	if total < 0 {
		total += dayNs
	}
	return LocalTimeValue{Nanos: total}
}

// SubDurationFromLocalTime is the inverse of [AddDurationToLocalTime].
func SubDurationFromLocalTime(v LocalTimeValue, d DurationValue) LocalTimeValue {
	return AddDurationToLocalTime(v, NegateDuration(d))
}

// AddDurationToTime wraps modulo 24h, preserving zone offset.
func AddDurationToTime(v TimeValue, d DurationValue) TimeValue {
	lt := AddDurationToLocalTime(LocalTimeValue{Nanos: v.Nanos}, d)
	return TimeValue{Nanos: lt.Nanos, OffsetSec: v.OffsetSec}
}

// SubDurationFromTime is the inverse of [AddDurationToTime].
func SubDurationFromTime(v TimeValue, d DurationValue) TimeValue {
	return AddDurationToTime(v, NegateDuration(d))
}

// AddDurations returns a new Duration that is the sum of a and b.
func AddDurations(a, b DurationValue) DurationValue {
	return NewDuration(a.Months+b.Months, a.Days+b.Days, a.Seconds+b.Seconds, a.Nanos+b.Nanos)
}

// SubDurations returns a-b.
func SubDurations(a, b DurationValue) DurationValue {
	return AddDurations(a, NegateDuration(b))
}

// NegateDuration returns -d.
func NegateDuration(d DurationValue) DurationValue {
	return NewDuration(-d.Months, -d.Days, -d.Seconds, -d.Nanos)
}

// MulDuration scales d by an integer factor.
func MulDuration(d DurationValue, k int64) DurationValue {
	return NewDuration(d.Months*k, d.Days*k, d.Seconds*k, int32(int64(d.Nanos)*k))
}

// MulDurationFloat scales d by a floating-point factor. The result rounds
// fractional months/days into the seconds component using the same
// 30.4368750-day Gregorian-month approximation as [ParseDuration].
func MulDurationFloat(d DurationValue, k float64) DurationValue {
	months := float64(d.Months) * k
	days := float64(d.Days) * k
	seconds := float64(d.Seconds) * k
	nanos := float64(d.Nanos) * k

	mInt, mFrac := splitFloat(months)
	// Fractional months → seconds via Gregorian 30.4368750-day estimate.
	extraDays := mFrac * 30.436875
	dInt, dFrac := splitFloat(days + extraDays)
	// Fractional days → seconds.
	extraNs := int64(math.Round(dFrac * 86400 * 1_000_000_000))
	sInt, sFrac := splitFloat(seconds)
	extraNs += int64(math.Round(sFrac * 1_000_000_000))
	totalNanos := int64(math.Round(nanos)) + extraNs
	return NewDuration(mInt, dInt, sInt+totalNanos/1_000_000_000, int32(totalNanos%1_000_000_000))
}

// DivDurationFloat divides d by k. Returns the zero duration when k is zero.
func DivDurationFloat(d DurationValue, k float64) DurationValue {
	if k == 0 {
		return DurationValue{}
	}
	return MulDurationFloat(d, 1.0/k)
}

// SubDates returns the duration from b to a (a - b) using calendar-based
// decomposition into (months, days). Per openCypher,
// duration.between(date('1984-10-11'), date('2015-06-24')) yields
// P30Y8M13D — a calendar-anchored count of whole months between the
// boundaries, with the leftover days projected onto the closing month.
func SubDates(a, b DateValue) DurationValue {
	months, days := calendarDateDiff(a, b)
	return NewDuration(months, days, 0, 0)
}

// calendarDateDiff computes the calendar-based difference (a - b) as
// (months, days). The day count is the residual after the whole-month
// stride; it may be negative when b is after a.
func calendarDateDiff(a, b DateValue) (months, days int64) {
	years := a.Year - b.Year
	mo := a.Month - b.Month
	dy := a.Day - b.Day
	// Borrow days from the prior month of a when dy is negative.
	if dy < 0 {
		// Last day of the month preceding a.Month in a.Year.
		prev := time.Date(a.Year, time.Month(a.Month), 0, 0, 0, 0, 0, time.UTC)
		dy += prev.Day()
		mo--
	}
	// Borrow months from years when mo is negative after the day borrow.
	if mo < 0 {
		mo += 12
		years--
	}
	return int64(years*12 + mo), int64(dy)
}

// SubLocalDateTimes returns a-b as a duration with calendar-based
// (months, days) plus the wall-clock (hours, minutes, seconds, nanos)
// remainder. Per openCypher, duration.between of two date-bearing
// temporals decomposes into the canonical PnYnMnDTnHnMnS form.
func SubLocalDateTimes(a, b LocalDateTimeValue) DurationValue {
	return calendarDateTimeDiff(a.T, b.T)
}

// SubDateTimes returns a-b as a duration with the same calendar-based
// decomposition as SubLocalDateTimes, but anchored on the absolute
// instant (both sides shifted to UTC before the calendar walk). Per
// openCypher, the duration between two DateTime values is independent
// of their local zones — what matters is the elapsed time on the
// global timeline. Without the UTC shift, the wall-clock-anchored
// diff drifts by the zone offset (e.g. 2014-07-21T21:40+0200 to
// 2015-07-21T21:40+0100 would report P11M instead of P1Y).
func SubDateTimes(a, b DateTimeValue) DurationValue {
	return calendarDateTimeDiff(a.T.UTC(), b.T.UTC())
}

// calendarDateTimeDiff computes a-b as (months, days, seconds, nanos)
// where months and days are calendar-anchored on a's reference frame and
// the time-of-day remainder is the residual wall-clock difference after
// the day stride. Negative durations (b after a) are emitted with all
// components carrying the negative sign.
func calendarDateTimeDiff(a, b time.Time) DurationValue {
	const nsPerSec = int64(1_000_000_000)
	const nsPerHour = int64(time.Hour)
	const nsPerMinute = int64(time.Minute)
	// Sign-normalise: compute as |a-b| then negate at the end.
	neg := a.Before(b)
	lo, hi := a, b
	if !neg {
		lo, hi = b, a
	}
	years := hi.Year() - lo.Year()
	months := int(hi.Month()) - int(lo.Month())
	days := hi.Day() - lo.Day()
	// Time-of-day remainder: hi.tod - lo.tod (may be negative; borrow a day).
	hiTod := int64(hi.Hour())*nsPerHour + int64(hi.Minute())*nsPerMinute +
		int64(hi.Second())*nsPerSec + int64(hi.Nanosecond())
	loTod := int64(lo.Hour())*nsPerHour + int64(lo.Minute())*nsPerMinute +
		int64(lo.Second())*nsPerSec + int64(lo.Nanosecond())
	tod := hiTod - loTod
	if tod < 0 {
		tod += 86_400 * nsPerSec
		days--
	}
	if days < 0 {
		prev := time.Date(hi.Year(), hi.Month(), 0, 0, 0, 0, 0, time.UTC)
		days += prev.Day()
		months--
	}
	if months < 0 {
		months += 12
		years--
	}
	totalMonths := int64(years*12 + months)
	seconds := tod / nsPerSec
	nanos := int32(tod % nsPerSec)
	if neg {
		return NewDuration(-totalMonths, -int64(days), -seconds, -nanos)
	}
	return NewDuration(totalMonths, int64(days), seconds, nanos)
}

// SubLocalTimes returns a-b as a duration in (Seconds, Nanos).
func SubLocalTimes(a, b LocalTimeValue) DurationValue {
	diffNs := a.Nanos - b.Nanos
	return NewDuration(0, 0, diffNs/1_000_000_000, int32(diffNs%1_000_000_000))
}

// SubTimes returns a-b as a duration in (Seconds, Nanos), normalising both
// sides to UTC before subtracting. Per openCypher,
// duration.between(time('14:30'), time('16:30+0100')) yields PT1H — the
// +01:00 zone shifts the second value's UTC equivalent to 15:30, so the
// difference is one hour, not two.
func SubTimes(a, b TimeValue) DurationValue {
	const nsPerSec = int64(1_000_000_000)
	aUTC := a.Nanos - int64(a.OffsetSec)*nsPerSec
	bUTC := b.Nanos - int64(b.OffsetSec)*nsPerSec
	diffNs := aUTC - bUTC
	return NewDuration(0, 0, diffNs/nsPerSec, int32(diffNs%nsPerSec))
}

// temporalAccessor implements the openCypher component accessors (.year,
// .month, .hour, etc.) on temporal values. It returns (value, true) when key
// is recognised for the receiver's kind, or (nil, false) otherwise.
//
// Implemented keys per openCypher 9 §3.4:
//
//	Date/LocalDateTime/DateTime:
//	  year, month, day, week, weekYear, dayOfWeek, dayOfQuarter, quarter,
//	  ordinalDay
//	LocalDateTime/DateTime/LocalTime/Time:
//	  hour, minute, second, millisecond, microsecond, nanosecond
//	DateTime/Time:
//	  offset, offsetSeconds, offsetMinutes, timezone
//	Duration:
//	  years, months, days, hours, minutes, seconds, milliseconds, microseconds,
//	  nanoseconds, monthsOfYear, ...
//
//nolint:gocyclo // Sequential field dispatch over six temporal kinds; splitting hides the mapping.
func temporalAccessor(v Value, key string) (Value, bool) {
	switch t := v.(type) {
	case DateValue:
		return dateAccessor(t, key)
	case LocalDateTimeValue:
		return localDateTimeAccessor(t, key)
	case DateTimeValue:
		return dateTimeAccessor(t, key)
	case LocalTimeValue:
		return localTimeAccessor(t, key)
	case TimeValue:
		return timeAccessor(t, key)
	case DurationValue:
		return durationAccessor(t, key)
	}
	return nil, false
}

// dateAccessor returns components of a Date value.
//
//nolint:gocyclo // Direct accessor table; splitting would hide the field mapping.
func dateAccessor(d DateValue, key string) (Value, bool) {
	t := d.ToTime()
	switch key {
	case "year":
		return IntegerValue(int64(d.Year)), true
	case "month":
		return IntegerValue(int64(d.Month)), true
	case "day":
		return IntegerValue(int64(d.Day)), true
	case "week":
		_, w := t.ISOWeek()
		return IntegerValue(int64(w)), true
	case "weekYear":
		y, _ := t.ISOWeek()
		return IntegerValue(int64(y)), true
	case "dayOfWeek", "weekDay":
		dow := int(t.Weekday())
		if dow == 0 {
			dow = 7
		}
		return IntegerValue(int64(dow)), true
	case "ordinalDay":
		return IntegerValue(int64(t.YearDay())), true
	case "quarter":
		return IntegerValue(int64((d.Month-1)/3 + 1)), true
	case "dayOfQuarter":
		quarterStart := time.Date(d.Year, time.Month((d.Month-1)/3*3+1), 1, 0, 0, 0, 0, time.UTC)
		days := int(t.Sub(quarterStart).Hours() / 24)
		return IntegerValue(int64(days + 1)), true
	}
	return nil, false
}

// localDateTimeAccessor returns components of a LocalDateTime.
//
//nolint:gocyclo // Direct accessor table; splitting would hide the field mapping.
func localDateTimeAccessor(v LocalDateTimeValue, key string) (Value, bool) {
	if dv, ok := dateAccessor(DateFromTime(v.T), key); ok {
		return dv, true
	}
	t := v.T
	switch key {
	case "hour":
		return IntegerValue(int64(t.Hour())), true
	case "minute":
		return IntegerValue(int64(t.Minute())), true
	case "second":
		return IntegerValue(int64(t.Second())), true
	case "millisecond":
		return IntegerValue(int64(t.Nanosecond() / 1_000_000)), true
	case "microsecond":
		return IntegerValue(int64(t.Nanosecond() / 1_000)), true
	case "nanosecond":
		return IntegerValue(int64(t.Nanosecond())), true
	case "epochSeconds":
		return IntegerValue(t.Unix()), true
	case "epochMillis":
		return IntegerValue(t.UnixMilli()), true
	}
	return nil, false
}

// dateTimeAccessor returns components of a DateTime.
//
//nolint:gocyclo // Direct accessor table; splitting would hide the field mapping.
func dateTimeAccessor(v DateTimeValue, key string) (Value, bool) {
	if dv, ok := localDateTimeAccessor(LocalDateTimeValue(v), key); ok {
		return dv, true
	}
	_, off := v.T.Zone()
	switch key {
	case "offset":
		return StringValue(formatOffsetSec(off)), true
	case "offsetSeconds":
		return IntegerValue(int64(off)), true
	case "offsetMinutes":
		return IntegerValue(int64(off / 60)), true
	case "timezone":
		zone, _ := v.T.Zone()
		return StringValue(zone), true
	}
	return nil, false
}

// localTimeAccessor returns components of a LocalTime.
func localTimeAccessor(v LocalTimeValue, key string) (Value, bool) {
	h := v.Nanos / int64(time.Hour)
	rem := v.Nanos % int64(time.Hour)
	m := rem / int64(time.Minute)
	rem %= int64(time.Minute)
	s := rem / int64(time.Second)
	ns := rem % int64(time.Second)
	switch key {
	case "hour":
		return IntegerValue(h), true
	case "minute":
		return IntegerValue(m), true
	case "second":
		return IntegerValue(s), true
	case "millisecond":
		return IntegerValue(ns / 1_000_000), true
	case "microsecond":
		return IntegerValue(ns / 1_000), true
	case "nanosecond":
		return IntegerValue(ns), true
	}
	return nil, false
}

// timeAccessor returns components of a Time (zoned).
func timeAccessor(v TimeValue, key string) (Value, bool) {
	if lv, ok := localTimeAccessor(LocalTimeValue{Nanos: v.Nanos}, key); ok {
		return lv, true
	}
	switch key {
	case "offset", "timezone":
		return StringValue(formatOffsetSec(int(v.OffsetSec))), true
	case "offsetSeconds":
		return IntegerValue(int64(v.OffsetSec)), true
	case "offsetMinutes":
		return IntegerValue(int64(v.OffsetSec / 60)), true
	}
	return nil, false
}

// durationAccessor returns components of a Duration.
//
//nolint:gocyclo // Direct accessor table; splitting would hide the field mapping.
func durationAccessor(d DurationValue, key string) (Value, bool) {
	switch key {
	case "years":
		return IntegerValue(d.Months / 12), true
	case "months":
		return IntegerValue(d.Months), true
	case "weeks":
		return IntegerValue(d.Days / 7), true
	case "days":
		return IntegerValue(d.Days), true
	case "hours":
		return IntegerValue(d.Seconds / 3600), true
	case "minutes":
		return IntegerValue(d.Seconds / 60), true
	case "seconds":
		return IntegerValue(d.Seconds), true
	case "milliseconds":
		return IntegerValue(d.Seconds*1000 + int64(d.Nanos)/1_000_000), true
	case "microseconds":
		return IntegerValue(d.Seconds*1_000_000 + int64(d.Nanos)/1_000), true
	case "nanoseconds":
		return IntegerValue(d.Seconds*1_000_000_000 + int64(d.Nanos)), true
	case "monthsOfYear":
		return IntegerValue(d.Months % 12), true
	case "monthsOfQuarter":
		return IntegerValue(d.Months % 3), true
	case "quartersOfYear":
		return IntegerValue((d.Months / 3) % 4), true
	case "quarters":
		return IntegerValue(d.Months / 3), true
	case "daysOfWeek":
		return IntegerValue(d.Days % 7), true
	case "minutesOfHour":
		return IntegerValue((d.Seconds / 60) % 60), true
	case "secondsOfMinute":
		return IntegerValue(d.Seconds % 60), true
	case "millisecondsOfSecond":
		return IntegerValue(int64(d.Nanos) / 1_000_000), true
	case "microsecondsOfSecond":
		return IntegerValue(int64(d.Nanos) / 1_000), true
	case "nanosecondsOfSecond":
		return IntegerValue(int64(d.Nanos)), true
	}
	return nil, false
}

// durationFromGoDuration converts a Go [time.Duration] to a [DurationValue]
// with (Seconds, Nanos) components and no months/days carry. The day component
// is left at zero because converting elapsed nanoseconds to whole days would
// lose precision on DST or leap-second boundaries.
func durationFromGoDuration(d time.Duration) DurationValue {
	ns := int64(d)
	return NewDuration(0, 0, ns/1_000_000_000, int32(ns%1_000_000_000))
}
