package expr

import "testing"

// TestDateValue_StringParseDateRoundTrip asserts DateValue.String is the
// exact inverse of ParseDate across the whole year domain, including the
// ISO-8601 expanded ranges that an earlier %04d-only String broke: a
// five-digit year emitted without a leading '+' ("10000-01-01") matched
// none of ParseDate's forms and round-tripped as a plain string rather
// than a Date (rmp #1658).
func TestDateValue_StringParseDateRoundTrip(t *testing.T) {
	t.Parallel()
	years := []int{0, 1, -1, 9999, 10000, 99999, 100000, -9999, -10000, 12345, -12345, 2026}
	// Days chosen valid in every month/year so NewDate's time.Date
	// normalisation never rolls the date over.
	mds := [][2]int{{1, 1}, {6, 15}, {12, 28}}
	for _, y := range years {
		for _, md := range mds {
			want := NewDate(y, md[0], md[1])
			s := want.String()
			got, err := ParseDate(s)
			if err != nil {
				t.Fatalf("ParseDate(%q) [year %d]: %v", s, y, err)
			}
			if got != want {
				t.Fatalf("round-trip %d-%02d-%02d: String()=%q -> %+v, want %+v",
					y, md[0], md[1], s, got, want)
			}
		}
	}

	// The AC's named cases, asserting the canonical expanded-form output.
	canonical := []struct {
		v    DateValue
		want string
	}{
		{NewDate(10000, 1, 1), "+10000-01-01"},
		{NewDate(99999, 12, 31), "+99999-12-31"},
		{NewDate(-1, 1, 1), "-0001-01-01"},
		{NewDate(0, 1, 1), "0000-01-01"},
	}
	for _, c := range canonical {
		if got := c.v.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}
