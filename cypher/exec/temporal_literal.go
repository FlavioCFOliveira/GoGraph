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
// string literal.
func parseDateArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDate(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("date(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("date(...): unsupported argument form")
}

// parseLocalDateTimeArg parses the inner argument of localdatetime(...).
func parseLocalDateTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseLocalDateTime(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("localdatetime(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("localdatetime(...): unsupported argument form")
}

// parseDateTimeArg parses the inner argument of datetime(...).
func parseDateTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDateTime(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("datetime(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("datetime(...): unsupported argument form")
}

// parseLocalTimeArg parses the inner argument of localtime(...).
func parseLocalTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseLocalTime(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("localtime(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("localtime(...): unsupported argument form")
}

// parseTimeArg parses the inner argument of time(...).
func parseTimeArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseTime(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("time(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("time(...): unsupported argument form")
}

// parseDurationArg parses the inner argument of duration(...).
func parseDurationArg(s string) (expr.Value, error) {
	if isQuotedString(s) {
		return expr.ParseDuration(unquote(s))
	}
	if isMapLiteral(s) {
		return nil, strErr("duration(map) literals are evaluated at query time, not persisted")
	}
	return nil, strErr("duration(...): unsupported argument form")
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
