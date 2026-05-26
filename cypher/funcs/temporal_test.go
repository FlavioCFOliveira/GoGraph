package funcs

import (
	"testing"

	"gograph/cypher/expr"
)

func TestFnDate_StringForm(t *testing.T) {
	t.Parallel()
	got, err := fnDate([]expr.Value{expr.StringValue("2020-01-02")})
	if err != nil {
		t.Fatalf("fnDate: %v", err)
	}
	dv, ok := got.(expr.DateValue)
	if !ok {
		t.Fatalf("fnDate returned %T", got)
	}
	if dv.Year != 2020 || dv.Month != 1 || dv.Day != 2 {
		t.Errorf("fnDate: %v", dv)
	}
}

func TestFnDate_NullInput(t *testing.T) {
	t.Parallel()
	got, err := fnDate([]expr.Value{expr.Null})
	if err != nil {
		t.Fatalf("fnDate(Null): %v", err)
	}
	if got != expr.Null {
		t.Errorf("fnDate(Null) = %v; want Null", got)
	}
}

func TestFnDate_MapForm_Calendar(t *testing.T) {
	t.Parallel()
	m := expr.MapValue{
		"year":  expr.IntegerValue(2021),
		"month": expr.IntegerValue(3),
		"day":   expr.IntegerValue(15),
	}
	got, err := fnDate([]expr.Value{m})
	if err != nil {
		t.Fatalf("fnDate(map): %v", err)
	}
	dv := got.(expr.DateValue)
	if dv.String() != "2021-03-15" {
		t.Errorf("dv = %q", dv.String())
	}
}

func TestFnDate_MapForm_Week(t *testing.T) {
	t.Parallel()
	m := expr.MapValue{
		"year":      expr.IntegerValue(2015),
		"week":      expr.IntegerValue(30),
		"dayOfWeek": expr.IntegerValue(2),
	}
	got, err := fnDate([]expr.Value{m})
	if err != nil {
		t.Fatalf("fnDate(week-map): %v", err)
	}
	dv := got.(expr.DateValue)
	if dv.String() != "2015-07-21" {
		t.Errorf("week-map: got %q want 2015-07-21", dv.String())
	}
}

func TestFnDuration_Map_Years(t *testing.T) {
	t.Parallel()
	m := expr.MapValue{
		"years":  expr.IntegerValue(1),
		"months": expr.IntegerValue(2),
		"days":   expr.IntegerValue(3),
	}
	got, err := fnDuration([]expr.Value{m})
	if err != nil {
		t.Fatalf("fnDuration(map): %v", err)
	}
	dv := got.(expr.DurationValue)
	if dv.Months != 14 || dv.Days != 3 {
		t.Errorf("dur: %+v", dv)
	}
}

func TestFnDuration_String(t *testing.T) {
	t.Parallel()
	got, err := fnDuration([]expr.Value{expr.StringValue("PT2H30M")})
	if err != nil {
		t.Fatalf("fnDuration: %v", err)
	}
	dv := got.(expr.DurationValue)
	if dv.Seconds != 2*3600+30*60 {
		t.Errorf("dur: %+v", dv)
	}
}

func TestFnDurationBetween_Dates(t *testing.T) {
	t.Parallel()
	a := expr.NewDate(2020, 1, 1)
	b := expr.NewDate(2020, 1, 10)
	got, err := fnDurationBetween([]expr.Value{a, b})
	if err != nil {
		t.Fatalf("fnDurationBetween: %v", err)
	}
	dv := got.(expr.DurationValue)
	if dv.Days != 9 {
		t.Errorf("between dates: %+v", dv)
	}
}

// TestFnDurationBetween_DateAndLocalTime pins the mixed (Date,
// LocalTime) contract: duration.between subtracts the time-of-day
// components only. The date side contributes nothing (midnight, by
// convention); the time side contributes its nanoseconds-since-midnight.
//
// This test used to assert NULL — the previous implementation refused
// every mixed-kind pair. The current implementation follows the
// openCypher rules: mixed pairs that share at least one comparable
// axis (date+date, or time+time) produce a duration along that axis,
// and time-only inputs paired with date-bearing inputs project to the
// time-of-day axis.
func TestFnDurationBetween_DateAndLocalTime(t *testing.T) {
	t.Parallel()
	a := expr.NewDate(2020, 1, 1)
	b := expr.NewLocalTime(12, 0, 0, 0)
	got, err := fnDurationBetween([]expr.Value{a, b})
	if err != nil {
		t.Fatalf("fnDurationBetween: %v", err)
	}
	d, ok := got.(expr.DurationValue)
	if !ok {
		t.Fatalf("expected DurationValue, got %T (%v)", got, got)
	}
	// Date is anchored at 00:00 → diff is +12h on the time-of-day axis.
	if d.Seconds != 12*3600 || d.Days != 0 || d.Months != 0 || d.Nanos != 0 {
		t.Errorf("date→localtime diff: got %+v, want 12h", d)
	}
}

func TestFnDateTime_StringWithZone(t *testing.T) {
	t.Parallel()
	got, err := fnDateTime([]expr.Value{expr.StringValue("2020-05-15T12:30:45+01:00")})
	if err != nil {
		t.Fatalf("fnDateTime: %v", err)
	}
	dt, ok := got.(expr.DateTimeValue)
	if !ok {
		t.Fatalf("expected DateTimeValue, got %T", got)
	}
	if dt.String() != "2020-05-15T12:30:45+01:00" {
		t.Errorf("dt: %q", dt.String())
	}
}

func TestFnLocalTime_Map(t *testing.T) {
	t.Parallel()
	m := expr.MapValue{
		"hour":   expr.IntegerValue(13),
		"minute": expr.IntegerValue(45),
	}
	got, err := fnLocalTime([]expr.Value{m})
	if err != nil {
		t.Fatalf("fnLocalTime: %v", err)
	}
	lt := got.(expr.LocalTimeValue)
	// Canonical openCypher textual form elides :00 seconds when both
	// seconds and nanos are zero.
	if lt.String() != "13:45" {
		t.Errorf("lt: %q", lt.String())
	}
}

func TestRegistry_RegistersTemporalFns(t *testing.T) {
	t.Parallel()
	r := DefaultRegistry
	for _, name := range []string{"date", "localdatetime", "datetime", "localtime", "time", "duration"} {
		if _, ok := r.Resolve(name); !ok {
			t.Errorf("registry missing %q", name)
		}
	}
}
