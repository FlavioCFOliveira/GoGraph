package funcs

// completeness.go — commonly-needed openCypher built-ins added for functional
// completeness (audit finding F5, #1832): elementId, timestamp, randomUUID,
// isNaN, and the toStringList / toIntegerList / toFloatList / toBooleanList
// family. Each is registered in buildDefaultRegistry. All are TCK-neutral (no
// openCypher TCK scenario references these names), so they extend the surface a
// real workload needs without disturbing the 3897-scenario baseline.

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// fnElementID returns a stable string identifier for a node or relationship —
// the openCypher/Neo4j-recommended replacement for the deprecated id(). GoGraph
// backs it with the durable entity id rendered in decimal, which is stable for
// the lifetime of the entity in this database. Null in → null out; a
// non-entity argument is a typed error, matching id().
func fnElementID(args []expr.Value) (expr.Value, error) {
	if err := requireArity("elementId", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.NodeValue:
		return expr.StringValue(strconv.FormatInt(int64(v.ID), 10)), nil
	case expr.RelationshipValue:
		return expr.StringValue(strconv.FormatInt(int64(v.ID), 10)), nil
	default:
		return nil, &TypeError{Function: "elementId", ArgIndex: 0, Got: args[0].Kind(), Want: "Node or Relationship"}
	}
}

// fnTimestamp returns the milliseconds since the Unix epoch at the statement's
// frozen instant. Like the temporal "now" constructors it reads
// [StatementNow], so every call within one statement observes the same value;
// the engine additionally overrides it per query via its now-aware registry so
// concurrent queries each see their own instant. Zero-argument only.
func fnTimestamp(args []expr.Value) (expr.Value, error) {
	if err := requireArity("timestamp", args, 0); err != nil {
		return nil, err
	}
	return expr.IntegerValue(StatementNow().UnixMilli()), nil
}

// fnRandomUUID returns a randomly generated RFC 4122 version-4 UUID string.
// Like rand() it is intentionally non-deterministic. Zero-argument only.
func fnRandomUUID(args []expr.Value) (expr.Value, error) {
	if err := requireArity("randomUUID", args, 0); err != nil {
		return nil, err
	}
	s, err := randomUUIDv4()
	if err != nil {
		return nil, &expr.EvalError{Msg: "randomUUID: entropy source unavailable: " + err.Error()}
	}
	return expr.StringValue(s), nil
}

// randomUUIDv4 builds a version-4 (random) UUID in canonical 8-4-4-4-12 form
// from 16 cryptographically-random bytes, setting the version (0x4x) and
// variant (0x8..0xB) bits per RFC 4122 §4.4.
func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:]), nil
}

// fnIsNaN reports whether a numeric value is the IEEE-754 not-a-number value.
// An integer is never NaN. Null in → null out; a non-numeric argument is a
// typed error, per openCypher isNaN(NUMBER) -> BOOLEAN.
func fnIsNaN(args []expr.Value) (expr.Value, error) {
	if err := requireArity("isNaN", args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	switch v := args[0].(type) {
	case expr.FloatValue:
		return expr.BoolValue(math.IsNaN(float64(v))), nil
	case expr.IntegerValue:
		return expr.BoolValue(false), nil
	default:
		return nil, &TypeError{Function: "isNaN", ArgIndex: 0, Got: args[0].Kind(), Want: "Float or Integer"}
	}
}

// listConvert applies the scalar converter conv to every element of a list,
// substituting null for any element that conv cannot convert (an error or a
// null result), per the openCypher 9 toXList semantics: "values not convertible
// ... will be null in the list returned". Null in → null out; a non-list
// argument is a typed error.
func listConvert(fnName string, args []expr.Value, conv expr.BuiltinFn) (expr.Value, error) {
	if err := requireArity(fnName, args, 1); err != nil {
		return nil, err
	}
	if expr.IsNull(args[0]) {
		return expr.Null, nil
	}
	lst, ok := args[0].(expr.ListValue)
	if !ok {
		return nil, &TypeError{Function: fnName, ArgIndex: 0, Got: args[0].Kind(), Want: "List"}
	}
	out := make(expr.ListValue, len(lst))
	scratch := make([]expr.Value, 1)
	for i, e := range lst {
		scratch[0] = e
		v, err := conv(scratch)
		if err != nil || expr.IsNull(v) {
			out[i] = expr.Null
			continue
		}
		out[i] = v
	}
	return out, nil
}

func fnToStringList(args []expr.Value) (expr.Value, error) {
	return listConvert("toStringList", args, fnToString)
}

func fnToIntegerList(args []expr.Value) (expr.Value, error) {
	return listConvert("toIntegerList", args, fnToInteger)
}

func fnToFloatList(args []expr.Value) (expr.Value, error) {
	return listConvert("toFloatList", args, fnToFloat)
}

func fnToBooleanList(args []expr.Value) (expr.Value, error) {
	return listConvert("toBooleanList", args, fnToBoolean)
}
