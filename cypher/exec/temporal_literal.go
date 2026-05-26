// temporal_literal.go — string-form temporal literal recogniser.
//
// The Cypher executor stores property-map literals as opaque strings in the
// CreateNode/CreateRelationship IR nodes; the executor re-parses them in
// [parsePropValue] when writing to storage. Temporal constructors such as
// `date('2020-01-01')` therefore arrive as their source-text representation
// rather than as evaluated expressions.
//
// To minimise the surface area of the persistence-layer change, temporal
// values are encoded as [lpg.PropString] with a single SOH (0x01..0x06)
// prefix byte that identifies the originating temporal kind. The body is the
// canonical openCypher textual form (the value returned by
// [expr.Value.String]). The bridge in cypher/api.go decodes the prefix back
// into the matching [expr.Value] when properties are read.
//
// This file is the *write* half of the bridge. The corresponding read half
// lives in [cypher.lpgPropToExpr]. The prefix bytes must stay aligned
// between both halves.

package exec

import (
	"fmt"
	"strings"

	"gograph/cypher/expr"
	"gograph/graph/lpg"
)

// Magic prefix bytes used by [parseTemporalLiteral] and the reverse decoder
// in cypher/api.go. They occupy the SOH..ACK range (0x01..0x06) so that any
// pre-existing PropString that starts with a printable character is safely
// distinguished.
const (
	tempPrefixDate          = "\x01"
	tempPrefixLocalDateTime = "\x02"
	tempPrefixDateTime      = "\x03"
	tempPrefixLocalTime     = "\x04"
	tempPrefixTime          = "\x05"
	tempPrefixDuration      = "\x06"
)

// parseTemporalLiteral attempts to recognise s as a temporal function-call
// literal (e.g. `date('2020-01-01')`). When the function name matches but the
// argument is invalid, the function returns (zero, true, err) so the caller
// can surface the parse error rather than fall through to the integer path.
// When s is not a temporal literal it returns (zero, false, nil).
func parseTemporalLiteral(s string) (lpg.PropertyValue, bool, error) {
	fn, body, ok := splitFunctionCall(s)
	if !ok {
		return lpg.PropertyValue{}, false, nil
	}
	switch strings.ToLower(fn) {
	case "date":
		v, err := evalTemporalArg(body, parseDateArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixDate, v.String()), true, nil
	case "localdatetime":
		v, err := evalTemporalArg(body, parseLocalDateTimeArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixLocalDateTime, v.String()), true, nil
	case "datetime":
		v, err := evalTemporalArg(body, parseDateTimeArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixDateTime, v.String()), true, nil
	case "localtime":
		v, err := evalTemporalArg(body, parseLocalTimeArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixLocalTime, v.String()), true, nil
	case "time":
		v, err := evalTemporalArg(body, parseTimeArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixTime, v.String()), true, nil
	case "duration":
		v, err := evalTemporalArg(body, parseDurationArg)
		if err != nil {
			return lpg.PropertyValue{}, true, err
		}
		return encodeTemporalProp(tempPrefixDuration, v.String()), true, nil
	}
	return lpg.PropertyValue{}, false, nil
}

// splitFunctionCall returns the function name and the argument body if s has
// the shape `name(body)`. Returns ok=false when s is not a single
// function-call expression at the top level.
func splitFunctionCall(s string) (name, body string, ok bool) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	name = strings.TrimSpace(s[:open])
	if name == "" {
		return "", "", false
	}
	// Reject names that contain non-identifier characters; this protects
	// against accidental matches like `(2+3)*5`.
	for _, r := range name {
		if r != '_' && r != '.' &&
			(r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
			(r < '0' || r > '9') {
			return "", "", false
		}
	}
	body = strings.TrimSpace(s[open+1 : len(s)-1])
	return name, body, true
}

// encodeTemporalProp builds an [lpg.PropertyValue] (kind PropString) carrying
// a temporal kind tag in its first byte followed by the canonical textual
// form.
func encodeTemporalProp(tag, body string) lpg.PropertyValue {
	return lpg.StringValue(tag + body)
}

// evalTemporalArg parses the argument of a temporal constructor. The accepted
// shapes are:
//
//   - 'literal'        — quoted string (the canonical case for TCK round-trip)
//   - {key: val, ...}  — map literal; only the keys recognised by the temporal
//     map constructors are honoured.
//
// fn is the kind-specific parser that turns the inner content into an
// expr.Value.
func evalTemporalArg(body string, fn func(string) (expr.Value, error)) (expr.Value, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		// Bare constructor: caller decides default (current instant) but we
		// short-circuit to a TYPE_ERROR-like NULL since the storage path
		// does not have a clock.
		return nil, errTemporalNoArg
	}
	return fn(body)
}

// errTemporalNoArg is returned when a temporal constructor literal has no
// argument. We surface it as an error so callers can decide to fall back to
// the legacy integer path.
var errTemporalNoArg = strErr("temporal literal: no argument")

// strErr is a lightweight error type for static messages, avoiding the
// fmt.Errorf allocation on the hot literal-parse path.
type strErr string

func (e strErr) Error() string { return string(e) }

// parseDateArg parses the inner argument of date(...). Accepts a quoted
// string literal or a map literal with year/month/day keys.
func parseDateArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDate(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToDateString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseDate(canon)
	}
	return nil, strErr("date(...): unsupported argument form")
}

// parseLocalDateTimeArg parses the inner argument of localdatetime(...).
func parseLocalDateTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseLocalDateTime(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToLocalDateTimeString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseLocalDateTime(canon)
	}
	return nil, strErr("localdatetime(...): unsupported argument form")
}

// parseDateTimeArg parses the inner argument of datetime(...).
func parseDateTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDateTime(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToDateTimeString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseDateTime(canon)
	}
	return nil, strErr("datetime(...): unsupported argument form")
}

// parseLocalTimeArg parses the inner argument of localtime(...).
func parseLocalTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseLocalTime(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToLocalTimeString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseLocalTime(canon)
	}
	return nil, strErr("localtime(...): unsupported argument form")
}

// parseTimeArg parses the inner argument of time(...).
func parseTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseTime(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToTimeString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseTime(canon)
	}
	return nil, strErr("time(...): unsupported argument form")
}

// parseDurationArg parses the inner argument of duration(...).
func parseDurationArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDuration(unquote(s))
	}
	if isMapLiteral(s) {
		fields, err := parseTemporalMapLiteral(s)
		if err != nil {
			return nil, err
		}
		canon, err := mapFieldsToDurationString(fields)
		if err != nil {
			return nil, err
		}
		return expr.ParseDuration(canon)
	}
	return nil, strErr("duration(...): unsupported argument form")
}

// parseTemporalMapLiteral splits a top-level map literal "{k1: v1, k2: v2}"
// into a key→value-string map. Values are returned with their surrounding
// whitespace trimmed but with their quotes (if any) intact, so callers can
// distinguish quoted strings from bare numbers.
//
// Limitations: only flat single-level maps with primitive scalar values
// (numbers, quoted strings) are supported, which is sufficient for the
// temporal constructors that motivate this helper. Nested maps and
// expression values yield an error.
func parseTemporalMapLiteral(s string) (map[string]string, error) {
	if !isMapLiteral(s) {
		return nil, strErr("temporal map literal: not a map literal")
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return map[string]string{}, nil
	}
	parts, err := splitTopLevelCommas(inner)
	if err != nil {
		return nil, err
	}
	fields := make(map[string]string, len(parts))
	for _, p := range parts {
		colon := strings.IndexByte(p, ':')
		if colon < 0 {
			return nil, strErr("temporal map literal: missing ':' in entry")
		}
		key := strings.TrimSpace(p[:colon])
		val := strings.TrimSpace(p[colon+1:])
		if key == "" || val == "" {
			return nil, strErr("temporal map literal: empty key or value")
		}
		fields[key] = val
	}
	return fields, nil
}

// splitTopLevelCommas splits s on commas that sit at brace-depth zero.
// This avoids breaking nested map/list literals (rare for temporals but
// retained for forward-compat).
func splitTopLevelCommas(s string) ([]string, error) {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
			if depth < 0 {
				return nil, strErr("temporal map literal: unbalanced closing bracket")
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		case '\'', '"':
			// Skip the entire quoted segment so quoted commas don't split.
			q := s[i]
			j := i + 1
			for j < len(s) && s[j] != q {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				j++
			}
			if j >= len(s) {
				return nil, strErr("temporal map literal: unterminated string")
			}
			i = j
		}
	}
	if depth != 0 {
		return nil, strErr("temporal map literal: unbalanced opening bracket")
	}
	tail := strings.TrimSpace(s[start:])
	if tail != "" {
		parts = append(parts, tail)
	}
	return parts, nil
}

// readIntField returns the integer value of key, or def when the key is
// absent. Returns an error when the key is present but the value is not
// an integer.
func readIntField(fields map[string]string, key string, def int) (int, error) {
	v, ok := fields[key]
	if !ok {
		return def, nil
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, fmt.Errorf("temporal map literal: %q: not an integer (%q)", key, v)
	}
	return n, nil
}

// readStringField returns the unquoted string value of key, or def when
// the key is absent. Quotes (single or double) are stripped if present.
func readStringField(fields map[string]string, key, def string) string {
	v, ok := fields[key]
	if !ok {
		return def
	}
	if isQuotedString(v) {
		return unquote(v)
	}
	return v
}

// mapFieldsToDateString converts {year, month, day} to "YYYY-MM-DD".
func mapFieldsToDateString(fields map[string]string) (string, error) {
	year, err := readIntField(fields, "year", 0)
	if err != nil {
		return "", err
	}
	month, err := readIntField(fields, "month", 1)
	if err != nil {
		return "", err
	}
	day, err := readIntField(fields, "day", 1)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), nil
}

// mapFieldsToLocalTimeString converts {hour, minute, second, nanosecond}
// to "HH:MM:SS[.fff]".
func mapFieldsToLocalTimeString(fields map[string]string) (string, error) {
	hour, err := readIntField(fields, "hour", 0)
	if err != nil {
		return "", err
	}
	minute, err := readIntField(fields, "minute", 0)
	if err != nil {
		return "", err
	}
	second, err := readIntField(fields, "second", 0)
	if err != nil {
		return "", err
	}
	nano, err := readIntField(fields, "nanosecond", 0)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
	if nano != 0 {
		base += fmt.Sprintf(".%09d", nano)
	}
	return base, nil
}

// mapFieldsToTimeString converts {hour, minute, second, nanosecond,
// timezone} to "HH:MM:SS[.fff]±HH:MM" (or "Z" for "UTC" / "+00:00").
func mapFieldsToTimeString(fields map[string]string) (string, error) {
	local, err := mapFieldsToLocalTimeString(fields)
	if err != nil {
		return "", err
	}
	tz := readStringField(fields, "timezone", "")
	if tz == "" {
		return local + "Z", nil
	}
	return local + tz, nil
}

// mapFieldsToLocalDateTimeString converts {year, month, day, hour,
// minute, second, nanosecond} to "YYYY-MM-DDTHH:MM:SS[.fff]".
func mapFieldsToLocalDateTimeString(fields map[string]string) (string, error) {
	d, err := mapFieldsToDateString(fields)
	if err != nil {
		return "", err
	}
	t, err := mapFieldsToLocalTimeString(fields)
	if err != nil {
		return "", err
	}
	return d + "T" + t, nil
}

// mapFieldsToDateTimeString converts the full zoned variant to
// "YYYY-MM-DDTHH:MM:SS[.fff]±HH:MM".
func mapFieldsToDateTimeString(fields map[string]string) (string, error) {
	d, err := mapFieldsToDateString(fields)
	if err != nil {
		return "", err
	}
	t, err := mapFieldsToTimeString(fields)
	if err != nil {
		return "", err
	}
	return d + "T" + t, nil
}

// durationComponents captures the eight integer fields recognised by the
// duration({…}) map constructor. Each field is 0 when absent.
type durationComponents struct {
	years, months, days     int
	hours, minutes, seconds int
	nanoseconds             int
}

// readDurationComponents extracts the eight recognised keys from fields.
func readDurationComponents(fields map[string]string) (durationComponents, error) {
	var c durationComponents
	for _, spec := range []struct {
		key string
		out *int
	}{
		{"years", &c.years},
		{"months", &c.months},
		{"days", &c.days},
		{"hours", &c.hours},
		{"minutes", &c.minutes},
		{"seconds", &c.seconds},
		{"nanoseconds", &c.nanoseconds},
	} {
		v, err := readIntField(fields, spec.key, 0)
		if err != nil {
			return durationComponents{}, err
		}
		*spec.out = v
	}
	return c, nil
}

// mapFieldsToDurationString converts {years, months, days, hours,
// minutes, seconds, nanoseconds} to the ISO-8601 PnYnMnDTnHnMnS form
// accepted by [expr.ParseDuration].
func mapFieldsToDurationString(fields map[string]string) (string, error) {
	c, err := readDurationComponents(fields)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteByte('P')
	writeDurationDateSegment(&sb, c)
	writeDurationTimeSegment(&sb, c)
	if sb.Len() == 1 { // bare "P" — empty duration
		sb.WriteString("T0S")
	}
	return sb.String(), nil
}

// writeDurationDateSegment emits the years/months/days portion of an
// ISO-8601 duration. Zero components are elided.
func writeDurationDateSegment(sb *strings.Builder, c durationComponents) {
	if c.years != 0 {
		fmt.Fprintf(sb, "%dY", c.years)
	}
	if c.months != 0 {
		fmt.Fprintf(sb, "%dM", c.months)
	}
	if c.days != 0 {
		fmt.Fprintf(sb, "%dD", c.days)
	}
}

// writeDurationTimeSegment emits the T-prefixed hours/minutes/seconds/
// fractional-seconds portion when at least one time component is non-
// zero. Otherwise nothing is written.
func writeDurationTimeSegment(sb *strings.Builder, c durationComponents) {
	if c.hours == 0 && c.minutes == 0 && c.seconds == 0 && c.nanoseconds == 0 {
		return
	}
	sb.WriteByte('T')
	if c.hours != 0 {
		fmt.Fprintf(sb, "%dH", c.hours)
	}
	if c.minutes != 0 {
		fmt.Fprintf(sb, "%dM", c.minutes)
	}
	if c.seconds != 0 || c.nanoseconds != 0 {
		if c.nanoseconds != 0 {
			fmt.Fprintf(sb, "%d.%09dS", c.seconds, c.nanoseconds)
		} else {
			fmt.Fprintf(sb, "%dS", c.seconds)
		}
	}
}

// isQuotedString reports whether s is enclosed in matching quotes.
func isQuotedString(s string) bool {
	if len(s) < 2 {
		return false
	}
	q := s[0]
	return (q == '\'' || q == '"') && s[len(s)-1] == q
}

// unquote returns the inner content of a quoted string. Caller must verify
// with [isQuotedString] first.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	return s[1 : len(s)-1]
}

// isMapLiteral reports whether s starts with '{' and ends with '}'.
func isMapLiteral(s string) bool {
	return len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}'
}
