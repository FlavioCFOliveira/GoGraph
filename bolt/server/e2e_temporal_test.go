package server_test

// e2e_temporal_test.go — task #1434: end-to-end driver round-trip for temporal
// values over the Bolt wire.
//
// Before the fix, exprValueToPackstream had no case for the six temporal
// expr.Value kinds, so they fell through to x.String() and reached the driver
// as plain PackStream strings. A real neo4j-go-driver therefore delivered a Go
// string where the caller expected neo4j.Date / neo4j.Duration / time.Time.
//
// After the fix, the server encodes each temporal value as the canonical
// PackStream temporal Struct, so the driver hydrates it into the proper Go
// type. These tests assert the hydrated type and value over a genuine driver
// connection (Bolt 5.x, UTC datetime mode).

import (
	"context"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// TestE2E_TemporalRoundTrip drives temporal Cypher functions through a real
// neo4j-go-driver session and asserts the driver hydrates each value into the
// canonical Go type (proving the wire encoding is the correct PackStream
// temporal Struct, not a string).
func TestE2E_TemporalRoundTrip(t *testing.T) {
	ctx := context.Background()
	driver, _ := newDriverForTest(t)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx) //nolint:errcheck

	t.Run("Date", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN date('2020-06-15') AS v`)
		d, ok := v.(neo4j.Date)
		if !ok {
			t.Fatalf("expected neo4j.Date, got %T (%v)", v, v)
		}
		got := d.Time()
		if got.Year() != 2020 || got.Month() != time.June || got.Day() != 15 {
			t.Errorf("date: got %v, want 2020-06-15", got.Format("2006-01-02"))
		}
	})

	t.Run("Duration", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN duration({months: 2, days: 3, seconds: 4}) AS v`)
		d, ok := v.(neo4j.Duration)
		if !ok {
			t.Fatalf("expected neo4j.Duration, got %T (%v)", v, v)
		}
		if d.Months != 2 || d.Days != 3 || d.Seconds != 4 {
			t.Errorf("duration: got %+v, want {Months:2 Days:3 Seconds:4}", d)
		}
	})

	t.Run("LocalTime", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN localtime('12:30:45') AS v`)
		lt, ok := v.(neo4j.LocalTime)
		if !ok {
			t.Fatalf("expected neo4j.LocalTime, got %T (%v)", v, v)
		}
		got := lt.Time()
		if got.Hour() != 12 || got.Minute() != 30 || got.Second() != 45 {
			t.Errorf("localtime: got %02d:%02d:%02d, want 12:30:45", got.Hour(), got.Minute(), got.Second())
		}
	})

	t.Run("Time", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN time('12:30:45+02:00') AS v`)
		// neo4j.Time and neo4j.OffsetTime are the same dbtype.Time alias.
		tm, ok := v.(neo4j.Time)
		if !ok {
			t.Fatalf("expected neo4j.Time, got %T (%v)", v, v)
		}
		got := tm.Time()
		if got.Hour() != 12 || got.Minute() != 30 || got.Second() != 45 {
			t.Errorf("time clock: got %02d:%02d:%02d, want 12:30:45", got.Hour(), got.Minute(), got.Second())
		}
		_, offset := got.Zone()
		if offset != 2*3600 {
			t.Errorf("time offset: got %d s, want %d s", offset, 2*3600)
		}
	})

	t.Run("LocalDateTime", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN localdatetime('2020-06-15T12:30:45') AS v`)
		ldt, ok := v.(neo4j.LocalDateTime)
		if !ok {
			t.Fatalf("expected neo4j.LocalDateTime, got %T (%v)", v, v)
		}
		got := ldt.Time()
		if got.Year() != 2020 || got.Month() != time.June || got.Day() != 15 ||
			got.Hour() != 12 || got.Minute() != 30 || got.Second() != 45 {
			t.Errorf("localdatetime: got %v, want 2020-06-15T12:30:45", got.Format("2006-01-02T15:04:05"))
		}
	})

	t.Run("DateTime_Offset", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN datetime('2020-06-15T12:30:45Z') AS v`)
		// A zoned datetime hydrates into a time.Time.
		got, ok := v.(time.Time)
		if !ok {
			t.Fatalf("expected time.Time, got %T (%v)", v, v)
		}
		want := time.Date(2020, time.June, 15, 12, 30, 45, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("datetime: got %v, want %v", got, want)
		}
	})

	t.Run("DateTime_NamedZone", func(t *testing.T) {
		v := single(ctx, t, session, `RETURN datetime('2020-06-15T12:30:45[Europe/Paris]') AS v`)
		got, ok := v.(time.Time)
		if !ok {
			t.Fatalf("expected time.Time, got %T (%v)", v, v)
		}
		// 12:30:45 in Europe/Paris (CEST, UTC+2 in June) == 10:30:45 UTC.
		wantUTC := time.Date(2020, time.June, 15, 10, 30, 45, 0, time.UTC)
		if !got.UTC().Equal(wantUTC) {
			t.Errorf("datetime named zone: got %v (UTC %v), want UTC %v", got, got.UTC(), wantUTC)
		}
		if loc := got.Location().String(); loc != "Europe/Paris" {
			t.Errorf("datetime named zone: got location %q, want Europe/Paris", loc)
		}
	})
}

// single runs query, asserts exactly one row with a column "v", and returns its
// value. It uses an autocommit read session so the lock-free read path is used.
func single(ctx context.Context, t *testing.T, session neo4j.SessionWithContext, query string) any {
	t.Helper()
	result, err := session.Run(ctx, query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	rec, err := result.Single(ctx)
	if err != nil {
		t.Fatalf("Single(%q): %v", query, err)
	}
	v, ok := rec.Get("v")
	if !ok {
		t.Fatalf("column 'v' missing in result of %q", query)
	}
	return v
}
