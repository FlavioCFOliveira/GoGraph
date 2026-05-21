package expr

import (
	"testing"
	"time"
)

func TestParseDate_Calendar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		y, m, d       int
		wantError     bool
		errorContains string
	}{
		{in: "2015-07-21", y: 2015, m: 7, d: 21},
		{in: "20150721", y: 2015, m: 7, d: 21},
		{in: "2015-07", y: 2015, m: 7, d: 1},
		{in: "201507", y: 2015, m: 7, d: 1},
		{in: "2015", y: 2015, m: 1, d: 1},
		{in: "abc", wantError: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDate(c.in)
			if c.wantError {
				if err == nil {
					t.Fatalf("ParseDate(%q) = %v; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDate(%q): %v", c.in, err)
			}
			if got.Year != c.y || got.Month != c.m || got.Day != c.d {
				t.Errorf("ParseDate(%q) = %v; want %d-%02d-%02d", c.in, got, c.y, c.m, c.d)
			}
		})
	}
}

func TestParseDate_Week(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		out   string
		isErr bool
	}{
		{in: "2015-W30-2", out: "2015-07-21"},
		{in: "2015W302", out: "2015-07-21"},
		{in: "2015-W30", out: "2015-07-20"},
		{in: "2015W30", out: "2015-07-20"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDate(c.in)
			if c.isErr {
				if err == nil {
					t.Fatalf("ParseDate(%q) = %v; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDate(%q): %v", c.in, err)
			}
			if got.String() != c.out {
				t.Errorf("ParseDate(%q).String() = %q; want %q", c.in, got.String(), c.out)
			}
		})
	}
}

func TestParseDate_Ordinal(t *testing.T) {
	t.Parallel()
	d, err := ParseDate("2015-202")
	if err != nil {
		t.Fatalf("ParseDate: %v", err)
	}
	if d.String() != "2015-07-21" {
		t.Errorf("ordinal: got %q want 2015-07-21", d.String())
	}
}

func TestDateEqual_NullSemantics(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 1, 1)
	// Equal with Null → Null.
	if r := d.Equal(Null); r != Null {
		t.Errorf("DateValue.Equal(Null) = %v; want Null", r)
	}
	// Equal with mismatched kind → false (not Null).
	if r := d.Equal(IntegerValue(42)); r != BoolValue(false) {
		t.Errorf("DateValue.Equal(Integer) = %v; want false", r)
	}
	// Equal with same value → true.
	d2 := NewDate(2020, 1, 1)
	if r := d.Equal(d2); !IsTruthy(r) {
		t.Errorf("DateValue.Equal(self) = %v; want true", r)
	}
	// Equal with different date → false.
	d3 := NewDate(2020, 1, 2)
	if r := d.Equal(d3); r != BoolValue(false) {
		t.Errorf("DateValue.Equal(other) = %v; want false", r)
	}
}

func TestDurationArithmetic_AddSub(t *testing.T) {
	t.Parallel()
	a := NewDuration(1, 2, 3, 4)
	b := NewDuration(10, 20, 30, 0)
	sum := AddDurations(a, b)
	if sum != (DurationValue{Months: 11, Days: 22, Seconds: 33, Nanos: 4}) {
		t.Errorf("AddDurations: %+v", sum)
	}
	diff := SubDurations(b, a)
	if diff != (DurationValue{Months: 9, Days: 18, Seconds: 26, Nanos: 999_999_996}) {
		t.Errorf("SubDurations: %+v", diff)
	}
}

func TestDateAddDuration(t *testing.T) {
	t.Parallel()
	d := NewDate(1984, 10, 11)
	dur := NewDuration(12*12+5, 14, 16*3600+12*60+70, 2) // years=12, months=5, days=14, hms=16:12:70
	got := AddDurationToDate(d, dur)
	if got.String() != "1997-03-25" {
		t.Errorf("AddDurationToDate = %q; want 1997-03-25", got.String())
	}
}

func TestParseDuration_BasicAndFraction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{in: "P1Y", want: "P1Y"},
		{in: "P2M", want: "P2M"},
		{in: "P3D", want: "P3D"},
		{in: "PT4H", want: "PT4H"},
		{in: "PT1H30M", want: "PT1H30M"},
		{in: "P1Y2M3DT4H5M6S", want: "P1Y2M3DT4H5M6S"},
		{in: "P1.5D", want: "P1DT12H"},
		{in: "PT0.5S", want: "PT0.5S"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			d, err := ParseDuration(c.in)
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", c.in, err)
			}
			if d.String() != c.want {
				t.Errorf("ParseDuration(%q).String() = %q; want %q", c.in, d.String(), c.want)
			}
		})
	}
}

func TestDateTimeFormatting(t *testing.T) {
	t.Parallel()
	utc := time.Date(2020, 5, 15, 12, 30, 45, 123_456_000, time.UTC)
	dt := DateTimeValue{T: utc}
	if got := dt.String(); got != "2020-05-15T12:30:45.123456Z" {
		t.Errorf("UTC DateTime: %q", got)
	}
	loc := time.FixedZone("+02:00", 2*3600)
	zoned := DateTimeValue{T: time.Date(2020, 5, 15, 12, 30, 45, 0, loc)}
	if got := zoned.String(); got != "2020-05-15T12:30:45+02:00" {
		t.Errorf("zoned DateTime: %q", got)
	}
}

func TestLocalTimeAccessor(t *testing.T) {
	t.Parallel()
	v := NewLocalTime(13, 14, 15, 500_000_000)
	if r, ok := localTimeAccessor(v, "hour"); !ok || r != IntegerValue(13) {
		t.Errorf("hour: %v ok=%v", r, ok)
	}
	if r, ok := localTimeAccessor(v, "minute"); !ok || r != IntegerValue(14) {
		t.Errorf("minute: %v ok=%v", r, ok)
	}
	if r, ok := localTimeAccessor(v, "millisecond"); !ok || r != IntegerValue(500) {
		t.Errorf("millisecond: %v ok=%v", r, ok)
	}
	if _, ok := localTimeAccessor(v, "year"); ok {
		t.Errorf("year on LocalTime should not be accessible")
	}
}

func TestDateAccessor_WeekFields(t *testing.T) {
	t.Parallel()
	// 2015-07-21 is a Tuesday in ISO week 30.
	d := NewDate(2015, 7, 21)
	cases := []struct {
		key  string
		want IntegerValue
	}{
		{"year", 2015},
		{"month", 7},
		{"day", 21},
		{"week", 30},
		{"dayOfWeek", 2},
		{"ordinalDay", 202},
		{"quarter", 3},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			got, ok := dateAccessor(d, c.key)
			if !ok {
				t.Fatalf("accessor %q not recognised", c.key)
			}
			if got != c.want {
				t.Errorf("%s: got %v, want %v", c.key, got, c.want)
			}
		})
	}
}

func TestEvalArith_TemporalDispatch(t *testing.T) {
	t.Parallel()
	d := NewDate(2020, 1, 1)
	dur := NewDuration(0, 0, 0, 0)
	dur.Days = 5
	got, _ := evalTemporalArith("+", d, dur)
	if dv, ok := got.(DateValue); !ok || dv != NewDate(2020, 1, 6) {
		t.Errorf("Date + Duration: got %v", got)
	}
	// Duration + Duration.
	a := NewDuration(1, 2, 3, 0)
	b := NewDuration(4, 5, 6, 0)
	if got, ok := evalTemporalArith("+", a, b); !ok || got.(DurationValue).Months != 5 {
		t.Errorf("Duration + Duration: %v", got)
	}
	// Duration * scalar.
	if got, ok := evalTemporalArith("*", a, IntegerValue(2)); !ok || got.(DurationValue).Months != 2 {
		t.Errorf("Duration * Integer: %v", got)
	}
	// Date - Date → Duration.
	a2 := NewDate(2020, 1, 10)
	b2 := NewDate(2020, 1, 1)
	if got, ok := evalTemporalArith("-", a2, b2); !ok || got.(DurationValue).Days != 9 {
		t.Errorf("Date - Date: %v", got)
	}
}

func TestCompare_TemporalKinds(t *testing.T) {
	t.Parallel()
	a := NewDate(2020, 1, 1)
	b := NewDate(2020, 1, 2)
	if Compare(a, b) >= 0 {
		t.Errorf("Compare(earlier, later) = %d; want -1", Compare(a, b))
	}
	if Compare(b, a) <= 0 {
		t.Errorf("Compare(later, earlier) = %d; want +1", Compare(b, a))
	}
	// Cross-kind ordering: Date(21) > Float(7).
	if Compare(a, FloatValue(1.0)) <= 0 {
		t.Errorf("Date should sort after Float per kindOrder")
	}
	// Null sorts last.
	if Compare(Null, a) <= 0 {
		t.Errorf("Null should sort after Date")
	}
}

func TestNewDuration_Normalisation(t *testing.T) {
	t.Parallel()
	// Negative nanos should be borrowed into seconds.
	d := NewDuration(0, 0, 0, -250_000_000)
	if d.Seconds != -1 || d.Nanos != 750_000_000 {
		t.Errorf("normalise -0.25s: %+v", d)
	}
	// Nanos overflow normalised.
	d2 := NewDuration(0, 0, 0, 1_500_000_000)
	if d2.Seconds != 1 || d2.Nanos != 500_000_000 {
		t.Errorf("normalise 1.5s: %+v", d2)
	}
}
