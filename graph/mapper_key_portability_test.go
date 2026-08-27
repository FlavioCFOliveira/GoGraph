package graph

// mapper_key_portability_test.go — regression gate for rmp #2528: a natural key
// whose value is an ADDRESS cannot be reproduced in another process, and the
// failure must be attributed to the key type rather than to the snapshot.

import (
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// pointerKey is the reported shape: comparable (so it is a legal Mapper key),
// persistable through txn.NewBinaryMarshalerCodec, and carrying a pointer field
// that formats under %v as an address.
type pointerKey struct {
	ID string
	P  *int
}

// interfaceKey hides the pointer behind an interface field, so only the VALUE
// says whether this key is portable — the static type cannot.
type interfaceKey struct {
	ID string
	V  any
}

// plainKey is the control: a custom struct that takes the same fmt.Fprintf
// fallback in mapperShardFor and is fully address-independent.
type plainKey struct {
	ID  string
	Seq int64
}

// TestMapperShardFor_AddressDependentKeyIsNotReproducible reproduces the defect
// deterministically and IN PROCESS, which is stronger than the subprocess reopen
// the report proposed: two allocations of the same logical key inside one process
// already hash to different shards, so no second process is needed to show the
// hash is not a function of the key's data. A subprocess run could also agree by
// coincidence; this cannot.
func TestMapperShardFor_AddressDependentKeyIsNotReproducible(t *testing.T) {
	t.Parallel()
	a, b := new(int), new(int)
	*a, *b = 7, 7

	k1, k2 := pointerKey{ID: "same", P: a}, pointerKey{ID: "same", P: b}
	if mapperShardFor(k1) == mapperShardFor(k2) {
		t.Error("two allocations of the same logical pointer-bearing key hashed to the SAME shard; the arm cannot show address dependence (allocator reuse?)")
	}
	// And the reason it can never be fixed by a better hash: Go equality on
	// such a key is address equality, so the two are not even the same key.
	if k1 == k2 {
		t.Error("pointer-bearing keys with different addresses compared EQUAL; the premise of this test is wrong")
	}

	// The control must be stable across allocations, or the fallback branch
	// itself would be broken for every custom struct key.
	if got, want := mapperShardFor(plainKey{ID: "same", Seq: 7}), mapperShardFor(plainKey{ID: "same", Seq: 7}); got != want {
		t.Errorf("an address-independent struct key hashed to shard %d then %d: the fmt fallback is not deterministic", got, want)
	}
}

// TestMapperLoadFrom_AttributesAnAddressDependentKey is the fix. LoadFrom
// already refused the shard disagreement; before #2528 it reported
// ErrMapperEntryCorrupted alone, which points diagnosis at the snapshot writer
// or the disk. It must now also name the key type, because no re-read of the
// file can fix a key that has no reproducible identity.
func TestMapperLoadFrom_AttributesAnAddressDependentKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		written  any // key as the writing process held it
		restored any // the same logical key, decoded into a fresh allocation
		wantPath string
	}{
		{"pointer_field", pointerKey{ID: "k", P: new(int)}, pointerKey{ID: "k", P: new(int)}, "key.P"},
		{"interface_holding_pointer", interfaceKey{ID: "k", V: new(int)}, interfaceKey{ID: "k", V: new(int)}, "key.V"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			switch w := tc.written.(type) {
			case pointerKey:
				r := tc.restored.(pointerKey)
				m := NewMapper[pointerKey]()
				err = m.LoadFrom([]MapperEntry[pointerKey]{{Key: r, ID: packNodeID(mapperShardFor(w), 0)}})
			case interfaceKey:
				r := tc.restored.(interfaceKey)
				m := NewMapper[interfaceKey]()
				err = m.LoadFrom([]MapperEntry[interfaceKey]{{Key: r, ID: packNodeID(mapperShardFor(w), 0)}})
			default:
				t.Fatalf("unhandled case type %T", w)
			}
			if err == nil {
				t.Fatal("LoadFrom ACCEPTED an entry whose key cannot hash to the recorded shard")
			}
			if !errors.Is(err, ErrMapperEntryCorrupted) {
				t.Errorf("error does not wrap ErrMapperEntryCorrupted, breaking existing callers: %v", err)
			}
			if !errors.Is(err, ErrMapperKeyNotPortable) {
				t.Errorf("error blames the SNAPSHOT for what is a key-type defect; want ErrMapperKeyNotPortable too: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error does not name the offending field %q, so the operator cannot find it: %v", tc.wantPath, err)
			}
		})
	}
}

// TestMapperLoadFrom_GenuineCorruptionIsStillReportedAsCorruption is the other
// direction, and it is what keeps the new attribution honest: a portable key
// whose recorded shard is simply wrong is a damaged snapshot, and must NOT be
// mislabelled as a key-type problem.
func TestMapperLoadFrom_GenuineCorruptionIsStillReportedAsCorruption(t *testing.T) {
	t.Parallel()
	m := NewMapper[plainKey]()
	k := plainKey{ID: "k", Seq: 1}
	wrongShard := (mapperShardFor(k) + 1) & mapperShardMask
	err := m.LoadFrom([]MapperEntry[plainKey]{{Key: k, ID: packNodeID(wrongShard, 0)}})
	if !errors.Is(err, ErrMapperEntryCorrupted) {
		t.Fatalf("a wrong recorded shard must be reported as corruption: %v", err)
	}
	if errors.Is(err, ErrMapperKeyNotPortable) {
		t.Errorf("an address-INDEPENDENT key was blamed for a corrupted snapshot: %v", err)
	}
}

// TestAddressDependentKeyPath covers the detector directly, including the shapes
// the LoadFrom arms above cannot reach.
func TestAddressDependentKeyPath(t *testing.T) {
	t.Parallel()
	type nested struct {
		Inner pointerKey
	}
	type arrayOfPtr struct {
		A [2]*int
	}
	type chanKey struct {
		C chan int
	}
	type unsafeKey struct {
		U unsafe.Pointer
	}

	if got := addressDependentKeyPath("plain string"); got != "" {
		t.Errorf("string reported as address-dependent: %q", got)
	}
	if got := addressDependentKeyPath(int64(5)); got != "" {
		t.Errorf("int64 reported as address-dependent: %q", got)
	}
	if got := addressDependentKeyPath(plainKey{ID: "a", Seq: 1}); got != "" {
		t.Errorf("address-independent struct reported as address-dependent: %q", got)
	}
	if got := addressDependentKeyPath([16]byte{1}); got != "" {
		t.Errorf("byte array reported as address-dependent: %q", got)
	}
	// A nil interface field carries no address, so it is portable as it stands.
	if got := addressDependentKeyPath(interfaceKey{ID: "a"}); got != "" {
		t.Errorf("nil interface field reported as address-dependent: %q", got)
	}
	// A nil POINTER field is still address-dependent as a type: the next value
	// of this key type may well carry a live address, and reporting it only
	// when non-nil would make the diagnosis depend on the sample.
	if got := addressDependentKeyPath(pointerKey{ID: "a"}); !strings.Contains(got, "key.P") {
		t.Errorf("nil pointer field not reported: %q", got)
	}
	if got := addressDependentKeyPath(nested{}); !strings.Contains(got, "key.Inner.P") {
		t.Errorf("nested pointer not reported at its full path: %q", got)
	}
	if got := addressDependentKeyPath(arrayOfPtr{}); !strings.Contains(got, "key.A[0]") {
		t.Errorf("pointer inside an array not reported: %q", got)
	}
	if got := addressDependentKeyPath(chanKey{}); !strings.Contains(got, "key.C") {
		t.Errorf("channel field not reported: %q", got)
	}
	if got := addressDependentKeyPath(unsafeKey{}); !strings.Contains(got, "key.U") {
		t.Errorf("unsafe.Pointer field not reported: %q", got)
	}
	// A bare pointer key is the most direct case of all.
	if got := addressDependentKeyPath(new(int)); !strings.Contains(got, "key (*int)") {
		t.Errorf("bare pointer key not reported: %q", got)
	}
}
