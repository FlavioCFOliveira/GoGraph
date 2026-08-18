package sim

// metrics_required.go — per-scenario REQUIRED-COUNTERS declarations (rmp #2479).
//
// # What this file is for
//
// A fault scenario that asserts the EFFECT of a fault without confirming the
// faulted code path was entered proves less than it appears to. rmp #2465 had to
// establish that the mid-publish crash window was genuinely entered before its
// durability verdict meant anything; rmp #2471 gated group-commit coalescing on a
// counter and then found the counter itself could be satisfied by an unrelated
// path; rmp #2478 found the in-memory backend silently skips madvise, so a green
// matrix would have been evidence of nothing.
//
// [MetricsOracle] reads exactly four counters, all from the Cypher layer. Every
// storage- and Bolt-layer metric emitted across the module is unasserted — so the
// counter that would PROVE a fault fired is the one nothing reads. This file
// closes that: each fault scenario DECLARES the counters its faults must move,
// and failing to move a declared counter is a violation. It is a COVERAGE
// PRECONDITION in the shape rmp #2470/#2471 established — kept apart from the
// scenario's own verdict, because a declaration that did not fire means the RUN
// proved nothing, not that the ENGINE is broken.
//
// # The trap this file is built around
//
// A declaration that names a counter which some OTHER path also increments is
// satisfiable without the fault firing. That is not hypothetical here: it was
// MEASURED. store/wal/format.go increments "store.wal.Decode.errors" on EVERY
// decode failure class, including a benign torn tail — the ordinary shape of a
// WAL whose writer was killed mid-write, with no corruption anywhere. Declaring
// it alone for the WAL-corruption arm would be satisfied by a crash tail.
//
// So [RequiredCounter] carries the uniqueness judgement explicitly. A counter
// declared [SharedWithOtherPaths] must be accompanied by a discriminator: either
// a second counter unique to the path, or a CONTROL arm that runs the same
// scenario with the fault disabled and shows the counter does NOT move. The
// control is the standing guard rmp #2471 kept for the same reason.
//
// # Why the names are not read off a list
//
// docs/metrics.md carries an inventory of wired metric names. Nothing here was
// taken from it. Every name below was observed by DRIVING the scenario with a
// recording sink installed and reading what arrived — the same discipline the
// scenario oracles apply to the durable image. A name that a scenario stopped
// emitting would fail the declaration rather than quietly documenting a fiction.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// MetricsObservation is the immutable set of metric names a recording sink saw
// across one bracketed run: counter totals and per-name latency-observation
// counts. It is what a [ScenarioCounterDecl] is adjudicated against.
//
// # Concurrency contract
//
// A MetricsObservation is produced by copying out of the sink under its mutex
// and is not mutated afterwards; it is safe to read from many goroutines.
type MetricsObservation struct {
	// Counters maps counter name to the total delta the sink accumulated.
	Counters map[string]uint64
	// Latencies maps latency name to how many observations arrived under it.
	Latencies map[string]uint64
}

// Counter returns a counter's total (0 when it never moved).
func (o MetricsObservation) Counter(name string) uint64 { return o.Counters[name] }

// LatencyCount returns how many latency observations a name received.
func (o MetricsObservation) LatencyCount(name string) uint64 { return o.Latencies[name] }

// Names returns every metric name observed — counters and latency names alike —
// in sorted order. It is what the metrics-blind assertion reads, and what a
// witness log prints.
func (o MetricsObservation) Names() []string {
	names := make([]string, 0, len(o.Counters)+len(o.Latencies))
	for n := range o.Counters {
		names = append(names, n)
	}
	for n := range o.Latencies {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// String renders the observed counters (latency names are omitted: they are
// per-invocation and would swamp a failure message).
func (o MetricsObservation) String() string {
	keys := make([]string, 0, len(o.Counters))
	for n := range o.Counters {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("counters{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s=%d", k, o.Counters[k])
	}
	b.WriteString("}")
	return b.String()
}

// snapshot copies the sink's current totals into an immutable observation.
func (b *recordingBackend) snapshot() MetricsObservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	obs := MetricsObservation{
		Counters:  make(map[string]uint64, len(b.counters)),
		Latencies: make(map[string]uint64, len(b.latencyN)),
	}
	for k, v := range b.counters {
		obs.Counters[k] = v
	}
	for k, v := range b.latencyN {
		obs.Latencies[k] = v
	}
	return obs
}

// -----------------------------------------------------------------------------
// The declaration
// -----------------------------------------------------------------------------

// CounterUniqueness records how many code paths in the module emit a declared
// counter. It is the property that decides whether the counter, on its own,
// identifies the path the fault took.
type CounterUniqueness int

const (
	// CounterUniqueToPath means every emission site for the name lives on the one
	// code path the declaration is about, so an increment IS that path running.
	CounterUniqueToPath CounterUniqueness = iota
	// CounterSharedWithOtherPaths means the module increments the name from more
	// than one path, so the counter moving does NOT by itself say the declared
	// fault fired. Such a counter is only admissible with a
	// [RequiredCounter.Discriminator].
	CounterSharedWithOtherPaths
)

// String renders a uniqueness classification for a report.
func (u CounterUniqueness) String() string {
	if u == CounterSharedWithOtherPaths {
		return "shared"
	}
	return "unique"
}

// RequiredCounter is one counter a fault scenario declares its faults must move,
// with the minimum it must reach and the reason the path emits it.
type RequiredCounter struct {
	// Name is the exact metric name, as OBSERVED by driving the scenario with a
	// recording sink installed — never copied from an inventory.
	Name string
	// Why states what running the name proves about the fault, in one line.
	Why string
	// Discriminator is how the declaration stays falsifiable when Uniqueness is
	// [CounterSharedWithOtherPaths]: the second signal, or the control arm, that
	// separates this fault from the other paths that move the same name. It is
	// REQUIRED for a shared counter and the shape gate rejects a declaration that
	// omits it.
	Discriminator string
	// Min is the floor the counter must reach. It is a structural number (one per
	// window, one per component) wherever the scenario fixes one, not the value a
	// single run happened to produce.
	Min uint64
	// Uniqueness is how many paths emit Name; see [CounterUniqueness].
	Uniqueness CounterUniqueness
}

// String renders a required counter for a violation message.
func (r RequiredCounter) String() string {
	return fmt.Sprintf("%s>=%d (%s)", r.Name, r.Min, r.Uniqueness)
}

// ScenarioCounterDecl is one fault scenario's required-counters declaration: the
// counters its faults must move, or an explicit statement that the module emits
// NO counter capable of witnessing them.
//
// A declaration is adjudicated by [ScenarioCounterDecl.Check] against a
// [MetricsObservation] taken across the scenario run. Its own well-formedness is
// adjudicated separately by [CheckCounterDeclShape]: an empty or self-
// contradictory declaration would otherwise pass by saying nothing, which is the
// vacuity this file exists to prevent.
type ScenarioCounterDecl struct {
	// Scenario names the scenario, and the arm when the scenario has more than
	// one fault shape (for example "db-teardown[fault-on-close]").
	Scenario string
	// BlindReason, when non-empty, declares that NOTHING in the module emits a
	// counter for this scenario's fault. Required must then be empty. The claim is
	// not taken on trust: BlindPrefix names the metric namespace whose silence is
	// ASSERTED, so wiring a counter into that namespace fails the declaration and
	// forces it to be updated rather than leaving a stale blindness on record.
	BlindReason string
	// BlindPrefix is the namespace asserted to stay silent. It is meaningful only
	// alongside BlindReason.
	BlindPrefix string
	// Required is every counter the scenario's faults must move.
	Required []RequiredCounter
}

// String renders the declaration for a test log.
func (d ScenarioCounterDecl) String() string {
	if d.BlindReason != "" {
		return fmt.Sprintf("%s: metrics-blind (%q silent) — %s", d.Scenario, d.BlindPrefix, d.BlindReason)
	}
	parts := make([]string, 0, len(d.Required))
	for _, r := range d.Required {
		parts = append(parts, r.String())
	}
	return d.Scenario + ": " + strings.Join(parts, " ")
}

// Check adjudicates the declaration against what one scenario run emitted. Every
// declared counter that did not reach its floor is a violation, and so is a
// metrics-blind declaration whose namespace turned out to be noisy.
//
// A violation here is a COVERAGE failure: it says the run did not demonstrate the
// fault path was entered, so the scenario's own verdict rests on nothing. It is
// therefore reported separately from that verdict.
func (d ScenarioCounterDecl) Check(obs MetricsObservation) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{
			Kind:    ViolationOracleDeviation,
			Op:      "<required-counters:" + d.Scenario + ">",
			Message: msg,
		})
	}

	if d.BlindReason != "" {
		for _, name := range obs.Names() {
			if strings.HasPrefix(name, d.BlindPrefix) {
				add(fmt.Sprintf("declared metrics-blind on %q, but %q was emitted: the observability gap this declaration pins has changed and the declaration is now stale — replace BlindReason with the counters the path emits",
					d.BlindPrefix, name))
			}
		}
		return v
	}

	for _, r := range d.Required {
		if got := obs.Counter(r.Name); got < r.Min {
			add(fmt.Sprintf("required counter %s did not move enough: got %d, want >= %d — %s (observed %s)",
				r.Name, got, r.Min, r.Why, obs))
		}
	}
	return v
}

// CheckCounterDeclShape is the SEPARATE, shape-only non-vacuity gate over a
// declaration. It reads no run: it asks only whether the declaration could ever
// have failed. A declaration that names nothing, that claims blindness AND names
// counters, that sets a floor of zero, or that admits a shared counter with no
// discriminator, is satisfied by any run at all — the exact failure mode rmp
// #2471 found when a coalescing metric turned out to be reachable by an unrelated
// path.
//
// It is kept apart from [ScenarioCounterDecl.Check] for the reason rmp #2470
// established: an uninformative declaration must not read as a failing fault.
func CheckCounterDeclShape(d ScenarioCounterDecl) []Violation {
	var v []Violation
	add := func(msg string) {
		v = append(v, Violation{
			Kind:    ViolationOracleDeviation,
			Op:      "<required-counters-shape:" + d.Scenario + ">",
			Message: msg,
		})
	}

	if d.Scenario == "" {
		add("declaration names no scenario")
	}
	switch {
	case d.BlindReason != "" && len(d.Required) > 0:
		add("declaration both claims metrics-blindness and names required counters; it cannot mean both")
	case d.BlindReason != "" && d.BlindPrefix == "":
		add("metrics-blind declaration names no BlindPrefix, so its silence is asserted over nothing and can never fail")
	case d.BlindReason == "" && len(d.Required) == 0:
		add("declaration names no required counter and claims no blindness: it is satisfied by every run, including one in which no fault fired")
	}

	seen := make(map[string]struct{}, len(d.Required))
	for _, r := range d.Required {
		switch {
		case r.Name == "":
			add("a required counter has no name")
		case r.Min == 0:
			add(fmt.Sprintf("required counter %q has a floor of 0, which every run satisfies", r.Name))
		case r.Why == "":
			add(fmt.Sprintf("required counter %q states no reason the fault path emits it", r.Name))
		}
		if r.Uniqueness == CounterSharedWithOtherPaths && r.Discriminator == "" {
			add(fmt.Sprintf("required counter %q is emitted from more than one path and carries no discriminator: it is satisfiable without the fault firing", r.Name))
		}
		if _, dup := seen[r.Name]; dup {
			add(fmt.Sprintf("required counter %q is declared twice", r.Name))
		}
		seen[r.Name] = struct{}{}
	}
	return v
}

// ObserveMetrics installs the recording sink, runs fn, restores the no-op default
// and returns everything the sink saw. It is the bracket for a scenario that does
// NOT install a sink of its own; a scenario that does (see [RunDBTeardown])
// publishes its own [MetricsObservation] on its evidence instead, and must not be
// wrapped in this.
//
// Because the metrics sink is GLOBAL (see [MetricsOracle]) the bracket must run
// SERIALLY: the caller must not run concurrent metrics-emitting work, and must
// not call t.Parallel in a test that uses it.
func ObserveMetrics(fn func() error) (MetricsObservation, error) {
	rb := newRecordingBackend()
	metrics.SetBackend(rb)
	defer metrics.SetBackend(nil)
	err := fn()
	return rb.snapshot(), err
}

// -----------------------------------------------------------------------------
// The declarations
// -----------------------------------------------------------------------------

// Arm keys for the declarations whose scenario has more than one fault shape.
const (
	// DeclDBTeardownFaultOnClose is the db-teardown arm that arms a one-shot
	// fsync fault on the WAL close's own fsync (step 3 of the composed teardown).
	DeclDBTeardownFaultOnClose = "db-teardown[fault-on-close]"
	// DeclCheckpointCadenceTransientFault is the cadence arm whose one periodic
	// fire fails inside its snapshot publish and whose next fire must recover.
	DeclCheckpointCadenceTransientFault = "checkpoint-cadence[transient-fault]"
)

// ScenarioCounterDecls returns every per-scenario required-counters declaration,
// in a fixed order.
//
// Every name below was obtained by DRIVING the scenario with a recording sink
// installed and reading what arrived, across the scenario's own spread of seeds;
// the floors are the structural counts the scenario fixes (one per publish
// window, one per corrupted component) rather than the value one run produced.
// Nothing here was copied out of docs/metrics.md.
func ScenarioCounterDecls() []ScenarioCounterDecl {
	return []ScenarioCounterDecl{
		csrFilePublishFaultDecl(),
		walCorruptionDecl(),
		checkpointDirFsyncDecl(),
		checkpointCrashStormDecl(),
		snapshotCorruptionDecl(),
		dbTeardownFaultDecl(),
		checkpointCadenceTransientDecl(),
	}
}

// csrFilePublishFaultDecl declares the ST4 csrfile-publish arm METRICS-BLIND.
//
// This is the honest reading of a measurement, not a gap in the sweep. store/
// csrfile contains no reference to internal/metrics at all, and a full driven run
// of the scenario across its six fixture seeds emitted ZERO metric names of any
// kind — counters and latency observations alike. The atomic publish protocol
// (tmp write -> fsync -> rename -> parent-dir fsync) is entirely unobserved, so
// there is no counter that could prove the ENOSPC bound or the armed Sync fault
// was reached, and inventing one from a neighbouring layer would be exactly the
// non-unique declaration rmp #2471 warns against.
//
// What IS asserted is the blindness itself: no name under "store." may be emitted
// during the arm. That turns "we declared nothing" from a vacuous pass into a
// falsifiable claim — the day csrfile gains a counter, this declaration fails and
// must be replaced by the real name.
func csrFilePublishFaultDecl() ScenarioCounterDecl {
	return ScenarioCounterDecl{
		Scenario:    ScenarioCSRFilePublishFault,
		BlindReason: "store/csrfile emits no metric: the atomic publish protocol is unobserved, so no counter can witness the ENOSPC bound or the armed Sync fault",
		BlindPrefix: "store.",
	}
}

// walCorruptionDecl declares the ST5 WAL-corruption arm.
//
// The counter it must move is "store.wal.Decode.errors", and that counter is the
// textbook case of the trap this file is built around. store/wal/format.go
// increments it on EVERY decode failure class — including the io.EOF /
// io.ErrUnexpectedEOF path that yields [wal.ErrTornFrame], which is the ordinary
// shape of a WAL whose writer was killed mid-write and involves no corruption at
// all. Declaring it alone would be satisfied by a benign crash tail.
//
// The discriminator is a CONTROL, in the shape rmp #2471 kept as a standing
// guard: [runWALCorruptionFailStopWith] runs the identical scenario with the
// interior byte flip WITHHELD, and the control requires the counter to stay at
// zero. The clean replay, the prefix replay and the clean reopen the scenario
// performs before the flip all end at a frame boundary, so nothing but the
// corruption can move it.
func walCorruptionDecl() ScenarioCounterDecl {
	return ScenarioCounterDecl{
		Scenario: ScenarioWALCorruptionFailStop,
		Required: []RequiredCounter{{
			Name: "store.wal.Decode.errors",
			Min:  2,
			Why:  "the corrupt interior frame fails its CRC once on the measured replay and once again on the fail-stop reopen, so a run that detected the corruption moves it at least twice",
			// MEASURED: 2 on every seed of the scenario's fixed spread.
			Uniqueness:    CounterSharedWithOtherPaths,
			Discriminator: "store/wal/format.go raises it for a BENIGN torn tail too; the control arm runs the same scenario with the byte flip withheld and requires it to stay at 0 (TestMetricsRequiredCounters_WALCorruptionControlWithdrawsTheFault)",
		}},
	}
}

// checkpointDirFsyncDecl declares the ST6 post-rename dir-fsync poison arm.
//
// The fault lands on the parent-directory fsync the WAL prefix truncation issues
// after its rename, which poisons the writer. The declaration reads BOTH halves
// of that: the truncate that failed, and the poison it left behind.
func checkpointDirFsyncDecl() ScenarioCounterDecl {
	return ScenarioCounterDecl{
		Scenario: ScenarioCheckpointDirFsyncFault,
		Required: []RequiredCounter{
			{
				Name:       "store.wal.TruncatePrefix.errors",
				Min:        1,
				Why:        "the injected post-rename parent-dir fsync fault fails the prefix truncation itself",
				Uniqueness: CounterUniqueToPath,
			},
			{
				Name:       "store.checkpoint.RunCheckpoint.errors",
				Min:        1,
				Why:        "the failed truncation propagates out of the checkpoint that requested it",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "any checkpoint failure moves it; store.wal.TruncatePrefix.errors above is unique to the truncate " +
					"and pins WHICH step of the checkpoint failed",
			},
			{
				Name:       "store.wal.Append.errors",
				Min:        1,
				Why:        "after the failed truncate the writer is POISONED and refuses a subsequent append",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "an append can fail for other reasons; it is required alongside store.wal.Sync.errors and " +
					"store.wal.Close.errors as the three-way poison signature, all downstream of the unique truncate failure",
			},
			{
				Name:          "store.wal.Sync.errors",
				Min:           1,
				Why:           "the poisoned writer refuses a subsequent sync rather than acknowledging a commit it cannot make durable",
				Uniqueness:    CounterSharedWithOtherPaths,
				Discriminator: "see store.wal.Append.errors — the poison is declared as a signature, not as one counter",
			},
			{
				Name:       "store.wal.Close.errors",
				Min:        1,
				Why:        "the poisoned writer surfaces the poison on close rather than swallowing it",
				Uniqueness: CounterUniqueToPath,
			},
		},
	}
}

// checkpointCrashStormDecl declares the checkpoint-crash-storm scenario.
//
// The storm drives one cycle per publish window ([checkpointStormWindows]), each
// with a one-shot, path-keyed fault inside the snapshot publish, then crashes.
// The floors are therefore the window count, not a run's observed total.
//
// The load-bearing name is "store.recovery.snapshot.promoteParentFsync": it has
// exactly ONE emission site in the module, on the branch that promotes a stranded
// backup onto the live snapshot name, so an increment IS the interrupted-publish
// repair running. It is the same observable the pre-existing
// TestCheckpointCrashStorm_PromoteBranchEnteredMetric pins for one window; the
// declaration makes it a standing requirement of the whole storm.
func checkpointCrashStormDecl() ScenarioCounterDecl {
	windows := uint64(len(checkpointStormWindows()))
	return ScenarioCounterDecl{
		Scenario: ScenarioCheckpointCrashStorm,
		Required: []RequiredCounter{
			{
				Name:       "store.recovery.snapshot.promoteParentFsync",
				Min:        1,
				Why:        "recovery promoted a stranded backup onto the live snapshot name — the interrupted-publish repair branch, reached only by the stranded-backup window",
				Uniqueness: CounterUniqueToPath,
			},
			{
				Name:       "store.checkpoint.RunCheckpoint.errors",
				Min:        windows,
				Why:        "every publish window aborts the checkpoint that entered it, so one cycle per window must fail",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "any checkpoint failure moves it; store.recovery.snapshot.promoteParentFsync above is unique to the repair branch " +
					"and store.snapshot.WriteSnapshotFullCtx.errors below pins the failure inside the PUBLISH rather than before it",
			},
			{
				Name:       "store.snapshot.WriteSnapshotFullCtx.errors",
				Min:        windows,
				Why:        "each window's fault fires inside the full-snapshot publish, so the publish — not something upstream of it — is what failed",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "any snapshot write failure moves it; it is required at the WINDOW COUNT alongside " +
					"store.checkpoint.RunCheckpoint.errors, so a single incidental write failure cannot satisfy it",
			},
		},
	}
}

// snapshotCorruptionDecl declares the snapshot-corruption battery.
//
// The battery corrupts each published component in turn and requires recovery to
// fail-stop with that component's typed sentinel. The aggregates
// ("store.recovery.OpenCtxFS.errors", "store.recovery.openCodec.errors",
// "store.snapshot.LoadSnapshotFull.errors") move for ANY reopen failure, WAL-side
// included, so none of them can say the damage was seen where it was done. The
// per-component reader counters can, and they are what discriminates: each is
// emitted only by its own component's decoder.
//
// One component has no such counter. The MAPPER arm's damage is caught before
// [snapshot.ReadMapperString] is reached, so "store.snapshot.ReadMapperString
// .errors" never moves and the mapper arm is witnessed only by the aggregates.
// That is recorded here rather than papered over: eight of the nine components
// carry a unique per-component witness, the mapper does not.
func snapshotCorruptionDecl() ScenarioCounterDecl {
	components := uint64(len(snapshotCorruptionComponents()))
	perComponent := []struct{ name, comp string }{
		{"store.snapshot.ReadCSR.errors", "csr.bin"},
		{"store.snapshot.ReadLabels.errors", "labels.bin"},
		{"store.snapshot.ReadProperties.errors", "properties.bin"},
		{"store.snapshot.ReadTombstones.errors", "tombstones.bin"},
		{"store.snapshot.ReadEdgeHandles.errors", "edgehandles.bin"},
		{"store.snapshot.ReadConstraints.errors", "constraints.bin"},
		{"store.snapshot.ReadIndexDefs.errors", "indexdefs.bin"},
		{"store.snapshot.ReadManifestFile.errors", "manifest.json"},
	}
	req := make([]RequiredCounter, 0, 3+len(perComponent))
	req = append(req,
		RequiredCounter{
			Name:       "store.snapshot.LoadSnapshotFull.errors",
			Min:        components,
			Why:        "every corrupted component must abort the snapshot load rather than yielding a half-graph, so the count is at least one per component arm",
			Uniqueness: CounterSharedWithOtherPaths,
			Discriminator: "it moves for any snapshot-load failure; the per-component reader counters below are unique to their own decoder " +
				"and are what say the damage was detected where it was done",
		},
		RequiredCounter{
			Name:       "store.recovery.openCodec.errors",
			Min:        components,
			Why:        "recovery must refuse to open over each corrupted image rather than proceeding",
			Uniqueness: CounterSharedWithOtherPaths,
			Discriminator: "it moves for any recovery-open failure including a WAL-side one; the per-component reader counters below discriminate, " +
				"and the scenario's own sentinel assertions pin the typed error",
		},
		RequiredCounter{
			Name:       "store.recovery.OpenCtxFS.errors",
			Min:        components,
			Why:        "the refusal reached the recovery entry point the store actually opens through, so no arm was adjudicated on an inner call the store never makes",
			Uniqueness: CounterSharedWithOtherPaths,
			Discriminator: "it is the outermost aggregate and moves for ANY reopen failure; the per-component reader counters below are what " +
				"say the damage was seen at the component the arm damaged",
		},
	)
	for _, pc := range perComponent {
		req = append(req, RequiredCounter{
			Name:       pc.name,
			Min:        1,
			Why:        "the " + pc.comp + " arm's damage was detected by that component's own decoder, not merely by an aggregate",
			Uniqueness: CounterUniqueToPath,
		})
	}
	return ScenarioCounterDecl{Scenario: ScenarioSnapshotCorruptionFailStop, Required: req}
}

// dbTeardownFaultDecl declares the db-teardown fault arm.
//
// The arm makes step 3 of the composed teardown fail deterministically by arming
// a one-shot Sync fault on the WAL close's own fsync. "store.DB.Close.errors" has
// exactly one emission site in store/db.go, and "store.wal.Close.errors" is
// emitted only from the WAL's Close — together they say the teardown failed AND
// that it failed at the step the fault was armed on, not somewhere else.
//
// The control is free and already an arm of the same suite: the identical
// configuration with FaultOnClose withheld moves NEITHER counter (MEASURED).
func dbTeardownFaultDecl() ScenarioCounterDecl {
	return ScenarioCounterDecl{
		Scenario: DeclDBTeardownFaultOnClose,
		Required: []RequiredCounter{
			{
				Name:       "store.DB.Close.errors",
				Min:        1,
				Why:        "the composed teardown published a non-nil error, which is what the identity clause pins across every caller",
				Uniqueness: CounterUniqueToPath,
			},
			{
				Name:       "store.wal.Close.errors",
				Min:        1,
				Why:        "the failure came from step 3 — the WAL close's own fsync, where the fault was armed — and not from the final checkpoint or the goroutine join",
				Uniqueness: CounterUniqueToPath,
			},
		},
	}
}

// checkpointCadenceTransientDecl declares the cadence transient-failure arm.
//
// The arm arms a one-shot fsync fault immediately before one periodic fire, so
// exactly one cadence fire fails INSIDE its snapshot publish — before the WAL is
// synced and long before the prefix truncate — and the next fire must recover.
// The cadence env drives the checkpointer through its own fold callback rather
// than store/checkpoint's RunCheckpoint, so "store.checkpoint.RunCheckpoint
// .errors" never moves here: the counters that do are the snapshot writer's, as
// MEASURED. That is precisely why the names are driven out rather than guessed.
//
// The control is free and already an arm of the same suite: the clean arm runs the
// same geometry and the same commits with no fault and moves NEITHER counter.
func checkpointCadenceTransientDecl() ScenarioCounterDecl {
	return ScenarioCounterDecl{
		Scenario: DeclCheckpointCadenceTransientFault,
		Required: []RequiredCounter{
			{
				Name:       "store.snapshot.WriteSnapshotFullCtx.errors",
				Min:        1,
				Why:        "exactly one periodic fire failed inside the full-snapshot publish",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "any snapshot write failure moves it; the CLEAN cadence arm is the standing control and moves it 0 times " +
					"(TestMetricsRequiredCounters_CadenceCleanArmWithdrawsTheFault)",
			},
			{
				Name:       "store.snapshot.WriteCapture.errors",
				Min:        1,
				Why:        "the failure landed in the capture/write phase of the publish, which is where the fault was armed — before the WAL sync",
				Uniqueness: CounterSharedWithOtherPaths,
				Discriminator: "see store.snapshot.WriteSnapshotFullCtx.errors — the clean arm is the control for both, and the pair " +
					"together locates the failure inside the publish rather than at its edges",
			},
		},
	}
}
