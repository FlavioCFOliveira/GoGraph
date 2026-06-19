package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// scaleConfig captures the opt-in scale knobs of the seed subcommand.
// Its zero value means "canonical fixture only" — exactly the behaviour
// the example shipped with — so the deterministic golden output is
// untouched unless the operator explicitly asks for a larger graph.
//
// When users > 0 the seed layers a seeded, reproducible synthetic
// population on top of the canonical fixture: users extra :User nodes,
// each given an out-degree of friends :FOLLOWS edges to distinct other
// synthetic users. Fixing seed fixes the data shape exactly, so a scaled
// run is reproducible across machines (the principle behind the
// "realistic, reproducible data" rubric in docs/examples-standard.md).
type scaleConfig struct {
	users   int   // number of extra synthetic :User nodes (0 = fixture only)
	friends int   // FOLLOWS out-degree per synthetic user
	seed    int64 // RNG seed; fixes the deterministic synthetic data shape
}

// defaultScaleConfig returns the off-by-default scale knobs: zero extra
// users, so seed reproduces the canonical fixture byte-for-byte and the
// existing golden tests keep passing.
func defaultScaleConfig() scaleConfig {
	return scaleConfig{
		users:   0,
		friends: 8,
		seed:    1,
	}
}

// enabled reports whether the scale knobs ask for a synthetic population
// beyond the canonical fixture.
func (c scaleConfig) enabled() bool { return c.users > 0 }

// validate rejects a configuration that cannot produce the requested
// shape — for instance more friends than there are synthetic users to
// befriend. It is checked once, at the boundary, before any work. A
// disabled config (users == 0) is always valid.
func (c scaleConfig) validate() error {
	if c.users < 0 {
		return fmt.Errorf("scale: users must be >= 0, got %d", c.users)
	}
	if !c.enabled() {
		return nil
	}
	if c.friends < 0 {
		return fmt.Errorf("scale: friends must be >= 0, got %d", c.friends)
	}
	if c.friends >= c.users {
		return fmt.Errorf("scale: friends (%d) must be < users (%d): not enough distinct synthetic users to befriend", c.friends, c.users)
	}
	return nil
}

// scaleStats reports the realised shape and wall-clock cost of the
// synthetic population so the seed subcommand can surface throughput
// telemetry. Counts are facts (reproducible for a fixed seed); elapsed is
// volatile telemetry.
type scaleStats struct {
	users   int
	follows int
	elapsed time.Duration
}

// scaleKeyPrefix namespaces every synthetic user key so the generated
// population can never collide with the canonical fixture's natural keys
// (alice..erin) nor with the Cypher CREATE synthetic keys (__cx_*). The
// 24-char hex suffix is drawn from the seeded RNG.
const scaleKeyPrefix = "u_"

// appendSyntheticPopulation adds cfg.users synthetic :User nodes and their
// :FOLLOWS edges to tx, drawing every random choice from a math/rand seeded
// by cfg.seed so the result is reproducible. It honours ctx cancellation on
// a coarse interval. The returned scaleStats records the realised counts and
// the elapsed wall-clock time.
//
// The synthetic users mirror the fixture's :User shape (username,
// display_name, created_at) so they are indistinguishable from the
// hand-written fixture to every downstream query — the stats battery counts
// them, FOLLOWS traversals walk them — which is what lets a scaled run
// actually exercise the engine at size.
func appendSyntheticPopulation(ctx context.Context, tx *txn.Tx[string, float64], cfg scaleConfig) (scaleStats, error) {
	//nolint:gosec // G404: a seeded math/rand is intentional — the synthetic
	// population must reproduce a fixed shape for a given -seed; crypto/rand
	// would defeat the reproducibility the examples standard requires.
	rng := rand.New(rand.NewSource(cfg.seed))
	start := time.Now()

	keys := make([]string, cfg.users)
	seen := make(map[string]struct{}, cfg.users)
	for i := range keys {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return scaleStats{}, err
			}
		}
		key := uniqueSyntheticKey(rng, seen)
		keys[i] = key
		if err := addSyntheticUser(tx, rng, key, i); err != nil {
			return scaleStats{}, err
		}
	}

	follows := 0
	targets := make(map[int]struct{}, cfg.friends)
	for i, src := range keys {
		if i%scaleCheckEvery == 0 {
			if err := ctx.Err(); err != nil {
				return scaleStats{}, err
			}
		}
		clear(targets)
		for len(targets) < cfg.friends {
			j := rng.Intn(cfg.users)
			if j == i {
				continue
			}
			targets[j] = struct{}{}
		}
		for j := range targets {
			if err := addLabelledEdge(tx, src, keys[j], relFollows); err != nil {
				return scaleStats{}, err
			}
			follows++
		}
	}

	return scaleStats{
		users:   cfg.users,
		follows: follows,
		elapsed: time.Since(start),
	}, nil
}

// scaleCheckEvery bounds how often the synthetic build polls ctx for
// cancellation: often enough that a cancelled large run stops promptly,
// rare enough that the check is free relative to the surrounding work.
const scaleCheckEvery = 4096

// addSyntheticUser adds one synthetic :User node carrying the same three
// properties the fixture users carry, so the synthetic and fixture
// populations are uniform to every query. seq makes display_name and
// created_at deterministic and distinct without consuming RNG draws on the
// hot path.
func addSyntheticUser(tx *txn.Tx[string, float64], rng *rand.Rand, key string, seq int) error {
	if err := tx.AddNode(key); err != nil {
		return fmt.Errorf("synthetic user %s: add node: %w", key, err)
	}
	if err := tx.SetNodeLabel(key, labelUser); err != nil {
		return fmt.Errorf("synthetic user %s: label: %w", key, err)
	}
	if err := tx.SetNodeProperty(key, "username", lpg.StringValue(key)); err != nil {
		return fmt.Errorf("synthetic user %s: username: %w", key, err)
	}
	if err := tx.SetNodeProperty(key, "display_name", lpg.StringValue(syntheticName(rng))); err != nil {
		return fmt.Errorf("synthetic user %s: display_name: %w", key, err)
	}
	if err := tx.SetNodeProperty(key, "created_at", lpg.StringValue(syntheticCreatedAt(seq))); err != nil {
		return fmt.Errorf("synthetic user %s: created_at: %w", key, err)
	}
	return nil
}

// uniqueSyntheticKey returns a namespaced 24-char hex user key that has not
// been handed out before, recording it in seen. The random bytes are drawn
// from rng via Read (not Intn) so no int<->uint64 narrowing is involved.
func uniqueSyntheticKey(rng *rand.Rand, seen map[string]struct{}) string {
	var b [12]byte
	for {
		_, _ = rng.Read(b[:]) // math/rand.Read never returns an error
		key := scaleKeyPrefix + hex.EncodeToString(b[:])
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		return key
	}
}

// syntheticName assembles a plausible "First Last" name from the fixed word
// lists. Names are allowed to repeat — the unique key is the hex id, which
// mirrors reality and matches example 26's convention.
func syntheticName(rng *rand.Rand) string {
	return scaleFirstNames[rng.Intn(len(scaleFirstNames))] + " " + scaleLastNames[rng.Intn(len(scaleLastNames))]
}

// scaleCreatedAtRef is the fixed reference date the synthetic created_at
// timestamps count forward from. Anchoring to a constant — never the wall
// clock — keeps the dataset reproducible for a given -seed.
var scaleCreatedAtRef = time.Date(2026, time.January, 6, 0, 0, 0, 0, time.UTC)

// syntheticCreatedAt returns a deterministic RFC 3339 UTC timestamp for the
// seq-th synthetic user, one day apart, anchored to scaleCreatedAtRef and
// after the canonical fixture's last created_at (2026-01-05). Encoding it as
// an ISO-8601 string matches the fixture so a downstream ORDER BY created_at
// sees one uniform ordering.
func syntheticCreatedAt(seq int) string {
	return scaleCreatedAtRef.AddDate(0, 0, seq).Format(time.RFC3339)
}

// scaleFirstNames / scaleLastNames are the fixed word lists the synthetic
// generator draws display names from. Fixed so the dataset is reproducible.
var scaleFirstNames = []string{
	"Olivia", "Liam", "Emma", "Noah", "Ava", "Oliver", "Sophia", "Elijah",
	"Isabella", "James", "Mia", "Lucas", "Charlotte", "Mateo", "Amelia",
	"Ethan", "Harper", "Leo", "Evelyn", "Sebastian", "Abigail", "Daniel",
	"Emily", "Henry", "Ella", "Alexander", "Scarlett", "Jack", "Aria",
	"Benjamin", "Camila", "Theodore", "Luna", "Samuel", "Chloe", "David",
}

var scaleLastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller",
	"Davis", "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez",
	"Wilson", "Anderson", "Thomas", "Taylor", "Moore", "Jackson", "Martin",
	"Lee", "Perez", "Thompson", "White", "Harris", "Sanchez", "Clark",
}
