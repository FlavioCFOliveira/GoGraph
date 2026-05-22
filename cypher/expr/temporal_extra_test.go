package expr

// temporal_extra_test.go — supplementary white-box tests for cypher/expr
// temporal types and helpers. Targets the large uncovered surface in
// temporal.go (constructors, parsers, accessors, arithmetic) so that the
// per-package coverage gate (>= 75%) is met.
//
// Tests live in the `expr` (not `expr_test`) package so they can exercise
// unexported helpers — temporalKindLabel, dateAccessor, localTimeAccessor,
// timeAccessor, dateTimeAccessor, localDateTimeAccessor, durationAccessor,
// temporalAccessor, formatLocalDateTime, formatNanosToTime, formatOffsetSec,
// parseHMS, parseInt, parseUint, parseOffset, indexZoneStart, scanDurationToken,
// splitFractional, splitFloat, daysPerMonthEstimate, splitDaysToSeconds,
// dateFromOrdinal, parseWeekDate, dateFromIsoWeek, offsetLocation,
// durationFromGoDuration, isDigit.

import (
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Kind labels
// ─────────────────────────────────────────────────────────────────────────────

func TestTemporalKindLabel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    Kind
		want string
	}{
		{KindDate, "Date"},
		{KindLocalDateTime, "LocalDateTime"},
		{KindDateTime, "DateTime"},
		{KindLocalTime, "LocalTime"},
		{KindTime, "Time"},
		{KindDuration, "Duration"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			if got := temporalKindLabel(c.k); got != c.want {
				t.Errorf("temporalKindLabel(%d)=%q; want %q", c.k, got, c.want)
			}
			// Kind.String must agree.
			if got := c.k.String(); got != c.want {
				t.Errorf("Kind.String for %d = %q; want %q", c.k, got, c.want)
			}
		})
	}
	// Unknown temporal kind falls through to the Kind(N) form.
	if got := temporalKindLabel(Kind(250)); !strings.HasPrefix(got, "Kind(") {
		t.Errorf("unknown temporal kind label = %q; want Kind(...)", got)
	}
}

// TestKindString_Unknown covers the `default` arm of Kind.String for a value
// outside both the graph-kind and temporal-kind ranges.
func TestKindString_Unknown(t *testing.T) {
	t.Parallel()
	k := Kind(200)
	got := k.String()
	if !strings.HasPrefix(got, "Kind(") {
		t.Errorf("Kind(200).String() = %q; want Kind(...) prefix", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DateValue: methods
// ─────────────────────────────────────────────────────────────────────────────

func TestDateValue_HashStringKind(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 5, 15)
	if d.Kind() != KindDate {
		t.Errorf("Kind()=%v; want KindDate", d.Kind())
	}
	if d.String() != "2020-05-15" {
		t.Errorf("String()=%q; want 2020-05-15", d.String())
	}
	// Hash is deterministic and same for two equal Dates.
	d2 := NewDate(2020, 5, 15)
	if d.Hash() != d2.Hash() {
		t.Errorf("Hash mismatch for equal Dates: %d vs %d", d.Hash(), d2.Hash())
	}
}

func TestDateFromTime(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("+05:00", 5*3600)
	tt := time.Date(2025, 12, 31, 23, 0, 0, 0, loc)
	got := DateFromTime(tt)
	if got.Year != 2025 || got.Month != 12 || got.Day != 31 {
		t.Errorf("DateFromTime(%v) = %+v", tt, got)
	}
}

func TestDateToTime(t *testing.T) {
	t.Parallel()
	d := NewDate(2025, 6, 7)
	tt := d.ToTime()
	if tt.Year() != 2025 || tt.Month() != time.June || tt.Day() != 7 {
		t.Errorf("ToTime() = %v", tt)
	}
	if tt.Hour() != 0 || tt.Minute() != 0 || tt.Second() != 0 || tt.Nanosecond() != 0 {
		t.Errorf("ToTime() should be midnight: %v", tt)
	}
	if tt.Location() != time.UTC {
		t.Errorf("ToTime() should be UTC: %v", tt.Location())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalDateTimeValue
// ─────────────────────────────────────────────────────────────────────────────

func TestLocalDateTimeValue_All(t *testing.T) {
	t.Parallel()
	v := NewLocalDateTime(2020, 5, 15, 12, 30, 45, 123_456_789)
	if v.Kind() != KindLocalDateTime {
		t.Errorf("Kind()=%v", v.Kind())
	}
	if got := v.String(); got != "2020-05-15T12:30:45.123456789" {
		t.Errorf("String()=%q", got)
	}
	if v.Hash() == 0 {
		t.Errorf("Hash returned zero, suspicious")
	}
	// Equal: with Null → Null
	if r := v.Equal(Null); r != Null {
		t.Errorf("Equal(Null) = %v; want Null", r)
	}
	// Equal: with same value → true
	v2 := NewLocalDateTime(2020, 5, 15, 12, 30, 45, 123_456_789)
	if r := v.Equal(v2); !IsTruthy(r) {
		t.Errorf("Equal(=) = %v; want true", r)
	}
	// Equal: with different kind → false
	if r := v.Equal(IntegerValue(0)); r != BoolValue(false) {
		t.Errorf("Equal(Int) = %v; want false", r)
	}
	// Equal: with different value → false
	v3 := NewLocalDateTime(2020, 5, 15, 12, 30, 45, 0)
	if r := v.Equal(v3); r != BoolValue(false) {
		t.Errorf("Equal(!=) = %v; want false", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DateTimeValue
// ─────────────────────────────────────────────────────────────────────────────

func TestDateTimeValue_All(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("+02:00", 2*3600)
	v := NewDateTime(2020, 5, 15, 12, 30, 45, 0, loc)
	if v.Kind() != KindDateTime {
		t.Errorf("Kind()=%v", v.Kind())
	}
	if got := v.String(); got != "2020-05-15T12:30:45+02:00" {
		t.Errorf("String()=%q", got)
	}
	// Hash deterministic.
	v2 := NewDateTime(2020, 5, 15, 12, 30, 45, 0, loc)
	if v.Hash() != v2.Hash() {
		t.Errorf("Hash mismatch for equivalent zoned DateTimes")
	}
	// nil loc defaults to UTC.
	z := NewDateTime(2020, 5, 15, 12, 30, 45, 0, nil)
	if z.T.Location() != time.UTC {
		t.Errorf("nil loc should default to UTC, got %v", z.T.Location())
	}
	// Equal: 3VL
	if r := v.Equal(Null); r != Null {
		t.Errorf("Equal(Null) = %v; want Null", r)
	}
	if r := v.Equal(v2); !IsTruthy(r) {
		t.Errorf("Equal(self) = %v; want true", r)
	}
	if r := v.Equal(IntegerValue(0)); r != BoolValue(false) {
		t.Errorf("Equal(Int) = %v; want false", r)
	}
	// Different instant → false.
	other := NewDateTime(2020, 5, 15, 13, 30, 45, 0, loc)
	if r := v.Equal(other); r != BoolValue(false) {
		t.Errorf("Equal(!=) = %v; want false", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// LocalTimeValue
// ─────────────────────────────────────────────────────────────────────────────

func TestLocalTimeValue_All(t *testing.T) {
	t.Parallel()
	v := NewLocalTime(13, 14, 15, 500_000_000)
	if v.Kind() != KindLocalTime {
		t.Errorf("Kind()=%v", v.Kind())
	}
	if got := v.String(); got != "13:14:15.5" {
		t.Errorf("String()=%q; want 13:14:15.5", got)
	}
	if v.Hash() == 0 {
		t.Errorf("Hash returned zero, suspicious")
	}
	// Equal 3VL.
	if r := v.Equal(Null); r != Null {
		t.Errorf("Equal(Null) = %v; want Null", r)
	}
	v2 := NewLocalTime(13, 14, 15, 500_000_000)
	if r := v.Equal(v2); !IsTruthy(r) {
		t.Errorf("Equal(=) = %v; want true", r)
	}
	if r := v.Equal(IntegerValue(0)); r != BoolValue(false) {
		t.Errorf("Equal(Int) = %v; want false", r)
	}
	v3 := NewLocalTime(13, 14, 16, 0)
	if r := v.Equal(v3); r != BoolValue(false) {
		t.Errorf("Equal(!=) = %v; want false", r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// TimeValue
// ─────────────────────────────────────────────────────────────────────────────

func TestTimeValue_All(t *testing.T) {
	t.Parallel()
	v := NewTime(13, 14, 15, 0, 7200) // +02:00
	if v.Kind() != KindTime {
		t.Errorf("Kind()=%v", v.Kind())
	}
	if got := v.String(); got != "13:14:15+02:00" {
		t.Errorf("String()=%q", got)
	}
	if v.Hash() == 0 {
		t.Errorf("Hash returned zero, suspicious")
	}
	// Equal 3VL.
	if r := v.Equal(Null); r != Null {
		t.Errorf("Equal(Null) = %v; want Null", r)
	}
	v2 := NewTime(13, 14, 15, 0, 7200)
	if r := v.Equal(v2); !IsTruthy(r) {
		t.Errorf("Equal(=) = %v; want true", r)
	}
	if r := v.Equal(IntegerValue(0)); r != BoolValue(false) {
		t.Errorf("Equal(Int) = %v; want false", r)
	}
	// Same instant, different offset → false (per docstring).
	other := NewTime(12, 14, 15, 0, 3600)
	if r := v.Equal(other); r != BoolValue(false) {
		t.Errorf("Equal(other-offset) = %v; want false", r)
	}
	// UTC: "Z" suffix.
	utc := NewTime(0, 0, 0, 0, 0)
	if got := utc.String(); got != "00:00:00Z" {
		t.Errorf("utc String()=%q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DurationValue
// ─────────────────────────────────────────────────────────────────────────────

func TestDurationValue_All(t *testing.T) {
	t.Parallel()
	d := NewDuration(0, 0, 0, 0)
	if d.Kind() != KindDuration {
		t.Errorf("Kind()=%v", d.Kind())
	}
	if d.String() != "PT0S" {
		t.Errorf("zero String()=%q", d.String())
	}
	// Equal 3VL.
	if r := d.Equal(Null); r != Null {
		t.Errorf("Equal(Null) = %v; want Null", r)
	}
	d2 := NewDuration(0, 0, 0, 0)
	if r := d.Equal(d2); !IsTruthy(r) {
		t.Errorf("Equal(=) = %v; want true", r)
	}
	if r := d.Equal(IntegerValue(0)); r != BoolValue(false) {
		t.Errorf("Equal(Int) = %v; want false", r)
	}
	if r := d.Equal(NewDuration(1, 0, 0, 0)); r != BoolValue(false) {
		t.Errorf("Equal(!=) = %v; want false", r)
	}
	// Hash defined.
	d3 := NewDuration(1, 2, 3, 4)
	if d3.Hash() == 0 {
		t.Errorf("non-zero duration hashes to 0, suspicious")
	}
}

// TestNegateDuration_ExtraPaths exercises the negation helper, which is the
// inverse used by every Sub*From* arithmetic helper.
func TestNegateDuration_ExtraPaths(t *testing.T) {
	t.Parallel()
	d := NewDuration(1, 2, 3, 4)
	n := NegateDuration(d)
	if n.Months != -1 || n.Days != -2 {
		t.Errorf("NegateDuration months/days: %+v", n)
	}
	// Round-trip: negate twice → original.
	if r := NegateDuration(n); r != d {
		t.Errorf("double-negate: %+v vs %+v", r, d)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Formatting helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestFormatDuration_VariousShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    DurationValue
		want string
	}{
		// Zero → PT0S
		{NewDuration(0, 0, 0, 0), "PT0S"},
		// Pure month/year combination.
		{NewDuration(13, 0, 0, 0), "P1Y1M"},
		{NewDuration(-13, 0, 0, 0), "P-1Y-1M"},
		// Months only.
		{NewDuration(5, 0, 0, 0), "P5M"},
		// Days only.
		{NewDuration(0, 7, 0, 0), "P7D"},
		// Seconds with nanos → fractional seconds.
		{NewDuration(0, 0, 1, 500_000_000), "PT1.5S"},
		// Hours + minutes + seconds.
		{NewDuration(0, 0, 3*3600+5*60+7, 0), "PT3H5M7S"},
		// Negative duration: leading sign on first emitted component.
		{NewDuration(0, 0, -3, 0), "PT-3S"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			if got := formatDuration(c.d); got != c.want {
				t.Errorf("formatDuration(%+v)=%q; want %q", c.d, got, c.want)
			}
		})
	}
}

func TestFormatNanosToTime_NegativeClamps(t *testing.T) {
	t.Parallel()
	// Negative input is clamped to 0 before formatting.
	if got := formatNanosToTime(-1); got != "00:00:00" {
		t.Errorf("formatNanosToTime(-1)=%q; want 00:00:00", got)
	}
	// Zero output.
	if got := formatNanosToTime(0); got != "00:00:00" {
		t.Errorf("formatNanosToTime(0)=%q", got)
	}
	// Non-zero nanos render fraction.
	if got := formatNanosToTime(int64(time.Second) + int64(time.Hour)); got != "01:00:01" {
		t.Errorf("formatNanosToTime(1h1s)=%q", got)
	}
}

func TestFormatOffsetSec_AllBranches(t *testing.T) {
	t.Parallel()
	if got := formatOffsetSec(0); got != "Z" {
		t.Errorf("zero=%q", got)
	}
	if got := formatOffsetSec(2 * 3600); got != "+02:00" {
		t.Errorf("+2h=%q", got)
	}
	if got := formatOffsetSec(-2*3600 - 30*60); got != "-02:30" {
		t.Errorf("-2h30m=%q", got)
	}
}

func TestFormatLocalDateTime_NoZoneSuffix(t *testing.T) {
	t.Parallel()
	// Even when t.Location() is a non-UTC zone, the formatter must not emit
	// a zone suffix.
	loc := time.FixedZone("+02:00", 2*3600)
	tt := time.Date(2020, 1, 2, 3, 4, 5, 0, loc)
	got := formatLocalDateTime(tt)
	if got != "2020-01-02T03:04:05" {
		t.Errorf("formatLocalDateTime=%q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Parsers
// ─────────────────────────────────────────────────────────────────────────────

func TestParseLocalTime_All(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantH     int
		wantM     int
		wantS     int
		wantNS    int
		wantError bool
	}{
		{in: "13:14:15", wantH: 13, wantM: 14, wantS: 15},
		{in: "13:14:15.5", wantH: 13, wantM: 14, wantS: 15, wantNS: 500_000_000},
		{in: "131415", wantH: 13, wantM: 14, wantS: 15},
		{in: "1314", wantH: 13, wantM: 14},
		{in: "13", wantH: 13},
		{in: "13:14", wantH: 13, wantM: 14},
		// Fractional seconds beyond 9 digits → truncated to 9.
		{in: "13:14:15.123456789999", wantH: 13, wantM: 14, wantS: 15, wantNS: 123_456_789},
		// Empty string → error.
		{in: "", wantError: true},
		// Invalid HMS.
		{in: "ab:cd:ef", wantError: true},
		// Invalid compact length.
		{in: "12345", wantError: true},
		// Invalid fractional.
		{in: "13:14:15.abc", wantError: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLocalTime(c.in)
			if c.wantError {
				if err == nil {
					t.Fatalf("ParseLocalTime(%q) = %v; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLocalTime(%q): %v", c.in, err)
			}
			want := NewLocalTime(c.wantH, c.wantM, c.wantS, c.wantNS)
			if got != want {
				t.Errorf("ParseLocalTime(%q) = %+v; want %+v", c.in, got, want)
			}
		})
	}
}

func TestParseTime_All(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantH     int
		wantM     int
		wantS     int
		wantNS    int
		wantOff   int
		wantError bool
	}{
		{in: "13:14:15Z", wantH: 13, wantM: 14, wantS: 15, wantOff: 0},
		{in: "13:14:15+02:00", wantH: 13, wantM: 14, wantS: 15, wantOff: 7200},
		{in: "13:14:15-02:00", wantH: 13, wantM: 14, wantS: 15, wantOff: -7200},
		{in: "13:14:15+0200", wantH: 13, wantM: 14, wantS: 15, wantOff: 7200},
		{in: "13:14:15+02", wantH: 13, wantM: 14, wantS: 15, wantOff: 7200},
		{in: "131415-02", wantH: 13, wantM: 14, wantS: 15, wantOff: -7200},
		// Empty → error.
		{in: "", wantError: true},
		// Invalid offset sign.
		{in: "13:14:15?00", wantError: true},
		// Invalid offset body.
		{in: "13:14:15+02:0", wantError: true},
		// Invalid offset (length < 3 after sign omission).
		{in: "13:14:15+0", wantError: true},
		// Invalid HMS triggers parseTimeComponents error path.
		{in: "ab:cd:ef+00:00", wantError: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseTime(c.in)
			if c.wantError {
				if err == nil {
					t.Fatalf("ParseTime(%q) = %+v; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTime(%q): %v", c.in, err)
			}
			want := NewTime(c.wantH, c.wantM, c.wantS, c.wantNS, c.wantOff)
			if got != want {
				t.Errorf("ParseTime(%q) = %+v; want %+v", c.in, got, want)
			}
		})
	}
}

func TestParseLocalDateTime_All(t *testing.T) {
	t.Parallel()
	got, err := ParseLocalDateTime("2020-05-15T12:30:45.123")
	if err != nil {
		t.Fatalf("ParseLocalDateTime: %v", err)
	}
	want := NewLocalDateTime(2020, 5, 15, 12, 30, 45, 123_000_000)
	if got != want {
		t.Errorf("got %+v want %+v", got, want)
	}
	// Lowercase 't' separator.
	if _, err := ParseLocalDateTime("2020-05-15t12:30:45"); err != nil {
		t.Errorf("lowercase t: %v", err)
	}
	// Empty.
	if _, err := ParseLocalDateTime(""); err == nil {
		t.Errorf("empty should error")
	}
	// Whitespace-only → trimmed empty → error.
	if _, err := ParseLocalDateTime("   "); err == nil {
		t.Errorf("whitespace should error")
	}
	// Missing T.
	if _, err := ParseLocalDateTime("2020-05-15 12:30:45"); err == nil {
		t.Errorf("missing T should error")
	}
	// Invalid date part.
	if _, err := ParseLocalDateTime("zzzz-05-15T12:30:45"); err == nil {
		t.Errorf("bad date should error")
	}
	// Invalid time part.
	if _, err := ParseLocalDateTime("2020-05-15Tzz:zz:zz"); err == nil {
		t.Errorf("bad time should error")
	}
}

func TestParseDateTime_All(t *testing.T) {
	t.Parallel()
	got, err := ParseDateTime("2020-05-15T12:30:45Z")
	if err != nil {
		t.Fatalf("ParseDateTime: %v", err)
	}
	if got.T.Year() != 2020 || got.T.Hour() != 12 {
		t.Errorf("got %+v", got)
	}
	// Non-UTC zone.
	got2, err := ParseDateTime("2020-05-15T12:30:45+02:00")
	if err != nil {
		t.Fatalf("ParseDateTime: %v", err)
	}
	_, off := got2.T.Zone()
	if off != 7200 {
		t.Errorf("offset=%d; want 7200", off)
	}
	// Errors.
	if _, err := ParseDateTime(""); err == nil {
		t.Errorf("empty should error")
	}
	if _, err := ParseDateTime("2020-05-15 12:30:45Z"); err == nil {
		t.Errorf("missing T should error")
	}
	if _, err := ParseDateTime("zzzz-05-15T12:30:45Z"); err == nil {
		t.Errorf("bad date should error")
	}
	if _, err := ParseDateTime("2020-05-15Tzz:zz:zzZ"); err == nil {
		t.Errorf("bad time should error")
	}
}

func TestParseDate_OrdinalCompact(t *testing.T) {
	t.Parallel()
	// 2015 day 202 → 2015-07-21
	d, err := ParseDate("2015202")
	if err != nil {
		t.Fatalf("ParseDate ordinal compact: %v", err)
	}
	if d.String() != "2015-07-21" {
		t.Errorf("got %q", d.String())
	}
}

func TestParseDate_Errors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"   ",
		// Bad week date.
		"2015W",
		"2015-W30-x",
		"2015Wzz",
		// 6-char that is not pure digits.
		"20-005",
		// Length 4 but invalid.
		"abcd",
		// Bad ordinal day (>366).
		"2015-400",
		// Ordinal compact with bad year.
		"abcd202",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseDate(in); err == nil {
				t.Errorf("ParseDate(%q) should error", in)
			}
		})
	}
}

func TestParseDate_WeekDateBadWeek(t *testing.T) {
	t.Parallel()
	// Week 54 is invalid.
	if _, err := ParseDate("2015-W54"); err == nil {
		t.Errorf("week 54 should error")
	}
	// Day-of-week out of range.
	if _, err := ParseDate("2015-W30-9"); err == nil {
		t.Errorf("dow 9 should error")
	}
}

func TestParseDuration_FractionalAndErrors(t *testing.T) {
	t.Parallel()
	// Fractional year and month components, ISO comma decimal.
	if _, err := ParseDuration("P1,5Y"); err != nil {
		t.Errorf("comma decimal: %v", err)
	}
	if _, err := ParseDuration("P0.5M"); err != nil {
		t.Errorf("fractional month: %v", err)
	}
	if _, err := ParseDuration("P1W"); err != nil {
		t.Errorf("weeks: %v", err)
	}
	if _, err := ParseDuration("P1.5W"); err != nil {
		t.Errorf("fractional weeks: %v", err)
	}
	if _, err := ParseDuration("PT0.5H"); err != nil {
		t.Errorf("fractional hours: %v", err)
	}
	if _, err := ParseDuration("PT0.5M"); err != nil {
		t.Errorf("fractional minutes: %v", err)
	}
	// Lowercase 'p' accepted.
	if _, err := ParseDuration("p1Y"); err != nil {
		t.Errorf("lowercase p: %v", err)
	}
	// Lowercase 't' accepted.
	if _, err := ParseDuration("P1Yt1H"); err != nil {
		t.Errorf("lowercase t: %v", err)
	}
	// Negative components.
	if d, err := ParseDuration("P-1Y"); err != nil {
		t.Errorf("negative year: %v", err)
	} else if d.Months != -12 {
		t.Errorf("negative year months=%d; want -12", d.Months)
	}
	// "P" alone (just the prefix) is accepted: the loop body never executes,
	// so the result is a zero-valued duration. This is intentional per the
	// current implementation.
	if d, err := ParseDuration("P"); err != nil {
		t.Errorf("ParseDuration(P) should not error: %v", err)
	} else if d != (DurationValue{}) {
		t.Errorf("ParseDuration(P) = %+v; want zero", d)
	}
	// Errors.
	errCases := []string{
		"",         // empty
		"X1Y",      // bad prefix
		"P1X",      // unknown unit (non-time)
		"PT1X",     // unknown unit (in time)
		"P1.5",     // missing unit
		"PT1.0.0S", // bad float in token
		"P1Yblah",  // unknown unit chain
		"P-",       // sign without digits/unit
	}
	for _, in := range errCases {
		in := in
		t.Run("err-"+in, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseDuration(in); err == nil {
				t.Errorf("ParseDuration(%q) should error", in)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Arithmetic helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestSubDurationFromDate(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 1, 10)
	dur := NewDuration(0, 9, 0, 0)
	got := SubDurationFromDate(d, dur)
	if got != NewDate(2020, 1, 1) {
		t.Errorf("got %+v", got)
	}
}

func TestLocalDateTimeArithmetic(t *testing.T) {
	t.Parallel()
	v := NewLocalDateTime(2020, 1, 1, 0, 0, 0, 0)
	d := NewDuration(0, 1, 3600, 0) // +1 day, +1h
	got := AddDurationToLocalDateTime(v, d)
	if got.T.Day() != 2 || got.T.Hour() != 1 {
		t.Errorf("Add: %+v", got)
	}
	// Sub is inverse of Add.
	back := SubDurationFromLocalDateTime(got, d)
	if !back.T.Equal(v.T) {
		t.Errorf("round-trip: %+v vs %+v", back.T, v.T)
	}
}

func TestDateTimeArithmetic(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("+02:00", 2*3600)
	v := DateTimeValue{T: time.Date(2020, 1, 1, 0, 0, 0, 0, loc)}
	d := NewDuration(0, 1, 0, 0)
	got := AddDurationToDateTime(v, d)
	if got.T.Day() != 2 {
		t.Errorf("Add: %+v", got)
	}
	// Preserves zone.
	_, off := got.T.Zone()
	if off != 7200 {
		t.Errorf("zone lost: off=%d", off)
	}
	back := SubDurationFromDateTime(got, d)
	if !back.T.Equal(v.T) {
		t.Errorf("round-trip mismatch")
	}
}

func TestLocalTimeArithmetic_Wrap(t *testing.T) {
	t.Parallel()
	v := NewLocalTime(23, 59, 0, 0)
	d := NewDuration(0, 0, 120, 0) // +2 minutes → wraps to 00:01
	got := AddDurationToLocalTime(v, d)
	if want := NewLocalTime(0, 1, 0, 0); got != want {
		t.Errorf("wrap forward: %+v; want %+v", got, want)
	}
	// Inverse wraps backward.
	v2 := NewLocalTime(0, 1, 0, 0)
	d2 := NewDuration(0, 0, 120, 0)
	back := SubDurationFromLocalTime(v2, d2)
	if want := NewLocalTime(23, 59, 0, 0); back != want {
		t.Errorf("wrap backward: %+v; want %+v", back, want)
	}
}

func TestTimeArithmetic(t *testing.T) {
	t.Parallel()
	v := NewTime(13, 0, 0, 0, 3600)
	d := NewDuration(0, 0, 60, 0)
	got := AddDurationToTime(v, d)
	if want := NewTime(13, 1, 0, 0, 3600); got != want {
		t.Errorf("Add: %+v want %+v", got, want)
	}
	back := SubDurationFromTime(got, d)
	if back != v {
		t.Errorf("round-trip: %+v vs %+v", back, v)
	}
}

func TestMulDurationFloat(t *testing.T) {
	t.Parallel()
	d := NewDuration(2, 4, 6, 0)
	half := MulDurationFloat(d, 0.5)
	if half.Months != 1 {
		t.Errorf("half months=%d; want 1", half.Months)
	}
	// Multiplying by 0 yields zero duration (per code path — months/days/secs all 0).
	zero := MulDurationFloat(d, 0)
	if zero.Months != 0 || zero.Days != 0 || zero.Seconds != 0 {
		t.Errorf("zero=%+v", zero)
	}
}

func TestDivDurationFloat(t *testing.T) {
	t.Parallel()
	d := NewDuration(0, 0, 100, 0)
	half := DivDurationFloat(d, 2)
	if half.Seconds != 50 {
		t.Errorf("half=%+v", half)
	}
	// Division by zero → zero duration.
	z := DivDurationFloat(d, 0)
	if z != (DurationValue{}) {
		t.Errorf("div by zero: %+v", z)
	}
}

func TestSubTemporalsToDuration(t *testing.T) {
	t.Parallel()
	// LocalDateTime - LocalDateTime
	a := NewLocalDateTime(2020, 1, 2, 0, 0, 0, 0)
	b := NewLocalDateTime(2020, 1, 1, 0, 0, 0, 0)
	d := SubLocalDateTimes(a, b)
	if d.Seconds != 86400 {
		t.Errorf("SubLocalDateTimes seconds=%d; want 86400", d.Seconds)
	}
	// DateTime - DateTime
	loc := time.FixedZone("+02:00", 2*3600)
	at := DateTimeValue{T: time.Date(2020, 1, 1, 12, 0, 0, 0, loc)}
	bt := DateTimeValue{T: time.Date(2020, 1, 1, 10, 0, 0, 0, loc)}
	d2 := SubDateTimes(at, bt)
	if d2.Seconds != 2*3600 {
		t.Errorf("SubDateTimes seconds=%d; want 7200", d2.Seconds)
	}
	// LocalTime - LocalTime
	d3 := SubLocalTimes(NewLocalTime(13, 0, 0, 0), NewLocalTime(12, 0, 0, 0))
	if d3.Seconds != 3600 {
		t.Errorf("SubLocalTimes seconds=%d; want 3600", d3.Seconds)
	}
	// Time - Time
	d4 := SubTimes(NewTime(13, 0, 0, 0, 0), NewTime(12, 0, 0, 0, 0))
	if d4.Seconds != 3600 {
		t.Errorf("SubTimes seconds=%d; want 3600", d4.Seconds)
	}
}

func TestDurationFromGoDuration(t *testing.T) {
	t.Parallel()
	d := durationFromGoDuration(2*time.Hour + 30*time.Minute + 500*time.Millisecond)
	if d.Seconds != 2*3600+30*60 {
		t.Errorf("seconds=%d", d.Seconds)
	}
	if d.Nanos != 500_000_000 {
		t.Errorf("nanos=%d", d.Nanos)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Accessors
// ─────────────────────────────────────────────────────────────────────────────

func TestTemporalAccessor_Dispatch(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 5, 15)
	v, ok := temporalAccessor(d, "year")
	if !ok || v != IntegerValue(2020) {
		t.Errorf("Date.year via temporalAccessor: %v ok=%v", v, ok)
	}
	// LocalDateTime via dispatch.
	ld := NewLocalDateTime(2020, 5, 15, 12, 0, 0, 0)
	v, ok = temporalAccessor(ld, "hour")
	if !ok || v != IntegerValue(12) {
		t.Errorf("LocalDateTime.hour: %v ok=%v", v, ok)
	}
	// DateTime via dispatch.
	loc := time.FixedZone("+02:00", 2*3600)
	dt := DateTimeValue{T: time.Date(2020, 5, 15, 12, 0, 0, 0, loc)}
	v, ok = temporalAccessor(dt, "offset")
	if !ok || v != StringValue("+02:00") {
		t.Errorf("DateTime.offset: %v ok=%v", v, ok)
	}
	// LocalTime.
	lt := NewLocalTime(13, 14, 15, 0)
	v, ok = temporalAccessor(lt, "hour")
	if !ok || v != IntegerValue(13) {
		t.Errorf("LocalTime.hour: %v ok=%v", v, ok)
	}
	// Time.
	tv := NewTime(13, 14, 15, 0, 3600)
	v, ok = temporalAccessor(tv, "offsetSeconds")
	if !ok || v != IntegerValue(3600) {
		t.Errorf("Time.offsetSeconds: %v ok=%v", v, ok)
	}
	// Duration.
	du := NewDuration(13, 0, 0, 0)
	v, ok = temporalAccessor(du, "years")
	if !ok || v != IntegerValue(1) {
		t.Errorf("Duration.years: %v ok=%v", v, ok)
	}
	// Unrecognised value → (nil, false).
	if v, ok := temporalAccessor(IntegerValue(0), "year"); ok || v != nil {
		t.Errorf("non-temporal value: v=%v ok=%v", v, ok)
	}
}

func TestDateAccessor_MissingKey(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 5, 15)
	if v, ok := dateAccessor(d, "doesnotexist"); ok || v != nil {
		t.Errorf("unknown key: v=%v ok=%v", v, ok)
	}
	// weekYear / dayOfQuarter not in the earlier test list.
	if v, ok := dateAccessor(d, "weekYear"); !ok || v != IntegerValue(2020) {
		t.Errorf("weekYear: %v ok=%v", v, ok)
	}
	if v, ok := dateAccessor(d, "dayOfQuarter"); !ok || v.(IntegerValue) <= 0 {
		t.Errorf("dayOfQuarter: %v ok=%v", v, ok)
	}
	// dayOfWeek where t.Weekday()==Sunday → must be 7 (covered branch).
	dSun := NewDate(2020, 5, 17) // 2020-05-17 is Sunday
	if v, ok := dateAccessor(dSun, "dayOfWeek"); !ok || v != IntegerValue(7) {
		t.Errorf("Sunday dayOfWeek: %v ok=%v", v, ok)
	}
}

func TestLocalDateTimeAccessor_AllKeys(t *testing.T) {
	t.Parallel()
	v := NewLocalDateTime(2020, 5, 15, 13, 14, 15, 123_456_789)
	cases := []struct {
		key  string
		want IntegerValue
	}{
		{"hour", 13},
		{"minute", 14},
		{"second", 15},
		{"millisecond", 123},
		{"microsecond", 123_456},
		{"nanosecond", 123_456_789},
		{"year", 2020}, // delegates to dateAccessor
		{"month", 5},   // delegates to dateAccessor
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			got, ok := localDateTimeAccessor(v, c.key)
			if !ok || got != c.want {
				t.Errorf("%s: %v ok=%v want %v", c.key, got, ok, c.want)
			}
		})
	}
	// epochSeconds & epochMillis.
	if got, ok := localDateTimeAccessor(v, "epochSeconds"); !ok || got.(IntegerValue) <= 0 {
		t.Errorf("epochSeconds: %v ok=%v", got, ok)
	}
	if got, ok := localDateTimeAccessor(v, "epochMillis"); !ok || got.(IntegerValue) <= 0 {
		t.Errorf("epochMillis: %v ok=%v", got, ok)
	}
	// Unknown key.
	if got, ok := localDateTimeAccessor(v, "nope"); ok || got != nil {
		t.Errorf("unknown: %v ok=%v", got, ok)
	}
}

func TestDateTimeAccessor_ZoneKeys(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("+02:00", 2*3600)
	v := DateTimeValue{T: time.Date(2020, 5, 15, 12, 0, 0, 0, loc)}
	// Inherited from LocalDateTime.
	if got, ok := dateTimeAccessor(v, "hour"); !ok || got != IntegerValue(12) {
		t.Errorf("hour: %v ok=%v", got, ok)
	}
	// Zone-specific.
	if got, ok := dateTimeAccessor(v, "offset"); !ok || got != StringValue("+02:00") {
		t.Errorf("offset: %v ok=%v", got, ok)
	}
	if got, ok := dateTimeAccessor(v, "offsetSeconds"); !ok || got != IntegerValue(7200) {
		t.Errorf("offsetSeconds: %v ok=%v", got, ok)
	}
	if got, ok := dateTimeAccessor(v, "offsetMinutes"); !ok || got != IntegerValue(120) {
		t.Errorf("offsetMinutes: %v ok=%v", got, ok)
	}
	if got, ok := dateTimeAccessor(v, "timezone"); !ok || got != StringValue("+02:00") {
		t.Errorf("timezone: %v ok=%v", got, ok)
	}
	if got, ok := dateTimeAccessor(v, "no-such"); ok || got != nil {
		t.Errorf("unknown: %v ok=%v", got, ok)
	}
}

func TestTimeAccessor_ZoneKeys(t *testing.T) {
	t.Parallel()
	v := NewTime(13, 14, 15, 0, 3600)
	if got, ok := timeAccessor(v, "hour"); !ok || got != IntegerValue(13) {
		t.Errorf("hour: %v ok=%v", got, ok)
	}
	if got, ok := timeAccessor(v, "offset"); !ok || got != StringValue("+01:00") {
		t.Errorf("offset: %v ok=%v", got, ok)
	}
	if got, ok := timeAccessor(v, "offsetSeconds"); !ok || got != IntegerValue(3600) {
		t.Errorf("offsetSeconds: %v ok=%v", got, ok)
	}
	if got, ok := timeAccessor(v, "offsetMinutes"); !ok || got != IntegerValue(60) {
		t.Errorf("offsetMinutes: %v ok=%v", got, ok)
	}
	if got, ok := timeAccessor(v, "year"); ok || got != nil {
		t.Errorf("year on Time: %v ok=%v", got, ok)
	}
}

func TestLocalTimeAccessor_AllKeys(t *testing.T) {
	t.Parallel()
	v := NewLocalTime(13, 14, 15, 123_456_789)
	cases := []struct {
		key  string
		want IntegerValue
	}{
		{"hour", 13},
		{"minute", 14},
		{"second", 15},
		{"millisecond", 123},
		{"microsecond", 123_456},
		{"nanosecond", 123_456_789},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			got, ok := localTimeAccessor(v, c.key)
			if !ok || got != c.want {
				t.Errorf("%s: %v ok=%v want %v", c.key, got, ok, c.want)
			}
		})
	}
}

func TestDurationAccessor_AllKeys(t *testing.T) {
	t.Parallel()
	d := NewDuration(25, 10, 3*3600+5*60+7, 123_456_789)
	cases := []struct {
		key  string
		want IntegerValue
	}{
		{"years", 2},
		{"months", 25},
		{"weeks", 1},
		{"days", 10},
		{"hours", 3},
		{"minutes", 3*60 + 5},
		{"seconds", 3*3600 + 5*60 + 7},
		{"milliseconds", (3*3600+5*60+7)*1000 + 123},
		{"microseconds", (3*3600+5*60+7)*1_000_000 + 123_456},
		{"nanoseconds", (3*3600+5*60+7)*1_000_000_000 + 123_456_789},
		{"monthsOfYear", 1},
		{"monthsOfQuarter", 1},
		{"quartersOfYear", 0},
		{"quarters", 8},
		{"daysOfWeek", 3},
		{"minutesOfHour", 5},
		{"secondsOfMinute", 7},
		{"millisecondsOfSecond", 123},
		{"microsecondsOfSecond", 123_456},
		{"nanosecondsOfSecond", 123_456_789},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			got, ok := durationAccessor(d, c.key)
			if !ok {
				t.Fatalf("%s not recognised", c.key)
			}
			if got != c.want {
				t.Errorf("%s: got %v want %v", c.key, got, c.want)
			}
		})
	}
	// Unknown key → (nil, false).
	if v, ok := durationAccessor(d, "no-such-key"); ok || v != nil {
		t.Errorf("unknown: v=%v ok=%v", v, ok)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// evalTemporalArith — remaining branches
// ─────────────────────────────────────────────────────────────────────────────

func TestEvalTemporalArith_AllPaths(t *testing.T) {
	t.Parallel()
	d := NewDuration(0, 0, 60, 0)

	// Duration - Duration.
	if got, ok := evalTemporalArith("-", d, d); !ok || got.(DurationValue).Seconds != 0 {
		t.Errorf("Dur - Dur: %v ok=%v", got, ok)
	}
	// Duration * float.
	if got, ok := evalTemporalArith("*", d, FloatValue(0.5)); !ok || got.(DurationValue).Seconds != 30 {
		t.Errorf("Dur*float: %v ok=%v", got, ok)
	}
	// Duration / int.
	if got, ok := evalTemporalArith("/", d, IntegerValue(2)); !ok || got.(DurationValue).Seconds != 30 {
		t.Errorf("Dur/int: %v ok=%v", got, ok)
	}
	// Duration / float.
	if got, ok := evalTemporalArith("/", d, FloatValue(2.0)); !ok || got.(DurationValue).Seconds != 30 {
		t.Errorf("Dur/float: %v ok=%v", got, ok)
	}
	// scalar * Duration (Integer).
	if got, ok := evalTemporalArith("*", IntegerValue(2), d); !ok || got.(DurationValue).Seconds != 120 {
		t.Errorf("Int*Dur: %v ok=%v", got, ok)
	}
	// scalar * Duration (Float).
	if got, ok := evalTemporalArith("*", FloatValue(2.0), d); !ok || got.(DurationValue).Seconds != 120 {
		t.Errorf("Float*Dur: %v ok=%v", got, ok)
	}

	// LocalDateTime + Duration.
	ld := NewLocalDateTime(2020, 1, 1, 0, 0, 0, 0)
	if got, ok := evalTemporalArith("+", ld, d); !ok {
		t.Errorf("LDT+Dur: %v ok=%v", got, ok)
	} else if got.(LocalDateTimeValue).T.Minute() != 1 {
		t.Errorf("LDT+Dur minute=%d", got.(LocalDateTimeValue).T.Minute())
	}
	// LocalDateTime - Duration.
	if _, ok := evalTemporalArith("-", ld, d); !ok {
		t.Errorf("LDT-Dur ok=false")
	}
	// DateTime + Duration, DateTime - Duration.
	loc := time.FixedZone("+02:00", 2*3600)
	dt := DateTimeValue{T: time.Date(2020, 1, 1, 0, 0, 0, 0, loc)}
	if _, ok := evalTemporalArith("+", dt, d); !ok {
		t.Errorf("DT+Dur ok=false")
	}
	if _, ok := evalTemporalArith("-", dt, d); !ok {
		t.Errorf("DT-Dur ok=false")
	}
	// LocalTime + Duration, LocalTime - Duration.
	lt := NewLocalTime(13, 0, 0, 0)
	if _, ok := evalTemporalArith("+", lt, d); !ok {
		t.Errorf("LT+Dur ok=false")
	}
	if _, ok := evalTemporalArith("-", lt, d); !ok {
		t.Errorf("LT-Dur ok=false")
	}
	// Time + Duration, Time - Duration.
	tv := NewTime(13, 0, 0, 0, 3600)
	if _, ok := evalTemporalArith("+", tv, d); !ok {
		t.Errorf("T+Dur ok=false")
	}
	if _, ok := evalTemporalArith("-", tv, d); !ok {
		t.Errorf("T-Dur ok=false")
	}

	// Duration + Temporal (commutative; only +).
	if _, ok := evalTemporalArith("+", d, NewDate(2020, 1, 1)); !ok {
		t.Errorf("Dur+Date ok=false")
	}
	if _, ok := evalTemporalArith("+", d, ld); !ok {
		t.Errorf("Dur+LDT ok=false")
	}
	if _, ok := evalTemporalArith("+", d, dt); !ok {
		t.Errorf("Dur+DT ok=false")
	}
	if _, ok := evalTemporalArith("+", d, lt); !ok {
		t.Errorf("Dur+LT ok=false")
	}
	if _, ok := evalTemporalArith("+", d, tv); !ok {
		t.Errorf("Dur+Time ok=false")
	}

	// Temporal - Temporal → Duration.
	if _, ok := evalTemporalArith("-", ld, ld); !ok {
		t.Errorf("LDT-LDT ok=false")
	}
	if _, ok := evalTemporalArith("-", dt, dt); !ok {
		t.Errorf("DT-DT ok=false")
	}
	if _, ok := evalTemporalArith("-", lt, lt); !ok {
		t.Errorf("LT-LT ok=false")
	}
	if _, ok := evalTemporalArith("-", tv, tv); !ok {
		t.Errorf("Time-Time ok=false")
	}

	// No-match cases → (Null, false).
	// String * Integer is not temporal.
	if got, ok := evalTemporalArith("*", StringValue("a"), IntegerValue(1)); ok || got != Null {
		t.Errorf("str*int returned %v ok=%v; want Null,false", got, ok)
	}
	// Date * Date is undefined.
	if got, ok := evalTemporalArith("*", NewDate(2020, 1, 1), NewDate(2020, 1, 1)); ok || got != Null {
		t.Errorf("Date*Date returned %v ok=%v", got, ok)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal small helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestParseUint_EmptyAndError(t *testing.T) {
	t.Parallel()
	if _, ok := parseUint(""); ok {
		t.Errorf("empty should not parse")
	}
	if _, ok := parseUint("abc"); ok {
		t.Errorf("abc should not parse")
	}
	if v, ok := parseUint("42"); !ok || v != 42 {
		t.Errorf("42: %d ok=%v", v, ok)
	}
}

func TestParseInt_EmptyAndError(t *testing.T) {
	t.Parallel()
	if _, err := parseInt(""); err == nil {
		t.Errorf("empty should error")
	}
	if _, err := parseInt("abc"); err == nil {
		t.Errorf("abc should error")
	}
	if v, err := parseInt("42"); err != nil || v != 42 {
		t.Errorf("42: %d err=%v", v, err)
	}
}

func TestParseOffset_EdgeCases(t *testing.T) {
	t.Parallel()
	if v, err := parseOffset("Z"); err != nil || v != 0 {
		t.Errorf("Z: %d err=%v", v, err)
	}
	if v, err := parseOffset("+05"); err != nil || v != 5*3600 {
		t.Errorf("+05: %d err=%v", v, err)
	}
	if v, err := parseOffset("-0500"); err != nil || v != -5*3600 {
		t.Errorf("-0500: %d err=%v", v, err)
	}
	// Errors.
	if _, err := parseOffset("+"); err == nil {
		t.Errorf("+ alone should error")
	}
	if _, err := parseOffset("?05"); err == nil {
		t.Errorf("?05 should error")
	}
	if _, err := parseOffset("+abcd"); err == nil {
		t.Errorf("+abcd should error")
	}
	if _, err := parseOffset("+ab"); err == nil {
		t.Errorf("+ab should error")
	}
	if _, err := parseOffset("+05ab"); err == nil {
		t.Errorf("+05ab should error")
	}
	if _, err := parseOffset("+05000"); err == nil {
		t.Errorf("+05000 (length 5) should error")
	}
}

func TestIndexZoneStart(t *testing.T) {
	t.Parallel()
	if i := indexZoneStart("13:14:15Z"); i != 8 {
		t.Errorf("Z idx=%d", i)
	}
	if i := indexZoneStart("13:14:15+02:00"); i != 8 {
		t.Errorf("+ idx=%d", i)
	}
	if i := indexZoneStart("13:14:15-02:00"); i != 8 {
		t.Errorf("- idx=%d", i)
	}
	if i := indexZoneStart("13:14:15"); i != -1 {
		t.Errorf("none idx=%d; want -1", i)
	}
	// Leading '-' in first 2 chars must not be treated as a zone marker.
	if i := indexZoneStart("-1"); i != -1 {
		t.Errorf("short - idx=%d; want -1", i)
	}
}

func TestIsDigit(t *testing.T) {
	t.Parallel()
	if !isDigit('5') {
		t.Errorf("5 not digit")
	}
	if isDigit('a') {
		t.Errorf("a is digit")
	}
	if isDigit('/') || isDigit(':') {
		t.Errorf("boundary chars classified as digit")
	}
}

func TestScanDurationToken_AllPaths(t *testing.T) {
	t.Parallel()
	// Plain integer.
	num, unit, tail, err := scanDurationToken("1Y")
	if err != nil || num != 1 || unit != 'Y' || tail != "" {
		t.Errorf("1Y: num=%v unit=%c tail=%q err=%v", num, unit, tail, err)
	}
	// Negative.
	num, unit, _, err = scanDurationToken("-3M")
	if err != nil || num != -3 || unit != 'M' {
		t.Errorf("-3M: num=%v unit=%c err=%v", num, unit, err)
	}
	// Comma decimal.
	num, _, _, err = scanDurationToken("1,5Y")
	if err != nil || num != 1.5 {
		t.Errorf("1,5Y: num=%v err=%v", num, err)
	}
	// No unit (i == len).
	if _, _, _, err := scanDurationToken("1.5"); err == nil {
		t.Errorf("no unit should error")
	}
	// No number (sign without digits).
	if _, _, _, err := scanDurationToken("Y"); err == nil {
		t.Errorf("Y alone should error")
	}
	// Sign-only.
	if _, _, _, err := scanDurationToken("-"); err == nil {
		t.Errorf("- alone should error")
	}
	// Bad numeric body.
	if _, _, _, err := scanDurationToken("1.2.3Y"); err == nil {
		t.Errorf("bad float should error")
	}
}

func TestSplitFractional(t *testing.T) {
	t.Parallel()
	i, f := splitFractional(2.75)
	if i != 2 || f < 0.74 || f > 0.76 {
		t.Errorf("splitFractional(2.75) = %d, %f", i, f)
	}
}

func TestSplitFloat(t *testing.T) {
	t.Parallel()
	i, f := splitFloat(3.25)
	if i != 3 || f < 0.24 || f > 0.26 {
		t.Errorf("splitFloat(3.25) = %d, %f", i, f)
	}
}

func TestDaysPerMonthEstimate(t *testing.T) {
	t.Parallel()
	d := daysPerMonthEstimate(2)
	// 2 * 30.4375 = 60.875 → 60
	if d != 60 {
		t.Errorf("daysPerMonthEstimate(2)=%d; want 60", d)
	}
}

func TestSplitDaysToSeconds(t *testing.T) {
	t.Parallel()
	days, ns := splitDaysToSeconds(1.5)
	if days != 1 {
		t.Errorf("days=%d", days)
	}
	// Expected ns: half a day (43_200 seconds) expressed in nanoseconds.
	if ns != int64(43200)*1_000_000_000 {
		t.Errorf("ns=%d", ns)
	}
}

func TestOffsetLocation(t *testing.T) {
	t.Parallel()
	if l := offsetLocation(0); l != time.UTC {
		t.Errorf("zero offset should be UTC, got %v", l)
	}
	l := offsetLocation(7200)
	tt := time.Date(2020, 1, 1, 0, 0, 0, 0, l)
	_, off := tt.Zone()
	if off != 7200 {
		t.Errorf("offset=%d", off)
	}
}

func TestDateFromOrdinal_Errors(t *testing.T) {
	t.Parallel()
	if _, err := dateFromOrdinal(2020, 0); err == nil {
		t.Errorf("0 should error")
	}
	if _, err := dateFromOrdinal(2020, 367); err == nil {
		t.Errorf("367 should error")
	}
	if _, err := dateFromOrdinal(2020, 100); err != nil {
		t.Errorf("100 should succeed: %v", err)
	}
}

func TestDateFromIsoWeek_Errors(t *testing.T) {
	t.Parallel()
	// Bad week.
	if _, err := dateFromIsoWeek(2020, 0, 1); err == nil {
		t.Errorf("week 0 should error")
	}
	if _, err := dateFromIsoWeek(2020, 54, 1); err == nil {
		t.Errorf("week 54 should error")
	}
	if _, err := dateFromIsoWeek(2020, 1, 0); err == nil {
		t.Errorf("dow 0 should error")
	}
	if _, err := dateFromIsoWeek(2020, 1, 8); err == nil {
		t.Errorf("dow 8 should error")
	}
	// Force the Sunday → 7 branch in the helper. ISO year 2017 starts with
	// jan4 == 2017-01-04 (Wednesday), not Sunday — pick a year where 4 Jan
	// is on Sunday: 1981 has Jan 4 == Sunday.
	if d, err := dateFromIsoWeek(1981, 1, 1); err != nil {
		t.Errorf("Sunday jan4 path: %v", err)
	} else if d.Year != 1980 && d.Year != 1981 {
		// The Monday of ISO week 1 of 1981 is 1980-12-29; both are acceptable.
		t.Errorf("Sunday jan4 result: %+v", d)
	}
}
