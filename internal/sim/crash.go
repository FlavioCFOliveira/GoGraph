package sim

// crashSeedMix derives the crash scheduler's independent sub-seed from the
// master seed, so the crash/restart timing never perturbs the workload draw
// stream: the actor/op/param sequence stays a pure function of cfg.Seed alone,
// exactly as the checker and disk sub-seeds do (see sim.go). The constant is an
// arbitrary odd 64-bit word distinct from checkerSeedMix and diskSeedMix.
const crashSeedMix uint64 = 0x2545f4914f6cdd1d

// Default crash-schedule parameters. They are deliberately conservative so a
// default run crashes often enough to exercise recovery on every reasonable
// tick budget, yet leaves a stability window long enough for the post-restart
// state to be re-validated before the next crash.
const (
	// defaultCrashProb is the per-eligible-tick probability of a crash. At 1/400
	// a default 100k-tick run sees on the order of 250 crash/recovery cycles,
	// densely exercising the durability path without dominating the run with
	// reopen cost.
	defaultCrashProb = 1.0 / 400.0
	// defaultStabilityWindow is the minimum number of ticks that must elapse
	// after a restart before another crash may be scheduled. It mirrors VOPR's
	// replica_stability: recovery must be given time to settle and be checked
	// before the next fault. It also guarantees forward progress (a run cannot
	// livelock crashing every tick).
	defaultStabilityWindow = 50
)

// CrashSchedule decides, deterministically from the seed, at which ticks a
// crash (a SIGKILL-equivalent: drop the in-memory engine, keep only the durable
// SimDisk bytes) occurs. After a crash the simulator reopens the store from the
// durable image via real recovery; CrashSchedule then enforces a stability
// window during which no further crash is scheduled, so recovery is given time
// to settle and be re-validated before the next fault (mirroring TigerBeetle
// VOPR's crash probability plus replica_stability).
//
// The decision is a pure function of (seed, tick, last-crash tick): given the
// same seed it produces the identical crash tick sequence on every run, which
// is what lets a failure be replayed bit-for-bit. CrashSchedule draws from its
// own sub-seed (derived from the master seed via [crashSeedMix]) so toggling
// crashes on or off never shifts the workload's op stream.
//
// # Concurrency contract
//
// CrashSchedule is NOT safe for concurrent use; it is consulted from the single
// simulation goroutine and its draw order is load-bearing for reproducibility.
type CrashSchedule struct {
	seed            *Seed
	crashProb       float64
	stabilityWindow int64
	// lastCrashTick is the tick of the most recent crash, or a sentinel low
	// value before the first crash so the opening stability window does not
	// suppress an early crash beyond the window itself.
	lastCrashTick int64
	// enabled is false when crashes are disabled (the safe default), in which
	// case ShouldCrash always returns false and never draws, keeping a
	// no-crash run byte-identical to the pre-crash simulator.
	enabled bool
}

// CrashConfig parameterises a [CrashSchedule]. The zero value disables crashes
// entirely (Enabled == false), which is the safe default: an existing run that
// does not opt in behaves exactly as before. When Enabled is true, non-positive
// fields fall back to their defaults.
type CrashConfig struct {
	// Enabled turns crash injection on. When false the schedule never fires and
	// never draws from the seed, so the workload stream is unperturbed.
	Enabled bool
	// CrashProb is the per-eligible-tick crash probability, clamped to [0,1].
	// A non-positive value falls back to [defaultCrashProb].
	CrashProb float64
	// StabilityWindow is the minimum tick gap enforced after a restart before
	// another crash may fire. A non-positive value falls back to
	// [defaultStabilityWindow].
	StabilityWindow int64
}

// NewCrashSchedule builds a crash schedule driven by seed and parameterised by
// cfg. When cfg.Enabled is false the returned schedule is inert: [ShouldCrash]
// always returns false and consumes no draws, so a run that does not opt into
// crashes is byte-identical to one built before crash support existed.
//
// The seed passed here must be the crash sub-seed (derived via [crashSeedMix]),
// never the master workload seed, so that enabling crashes does not shift the
// workload draw stream.
func NewCrashSchedule(seed *Seed, cfg CrashConfig) *CrashSchedule {
	prob := cfg.CrashProb
	if prob <= 0 {
		prob = defaultCrashProb
	}
	window := cfg.StabilityWindow
	if window <= 0 {
		window = defaultStabilityWindow
	}
	return &CrashSchedule{
		seed:            seed,
		crashProb:       prob,
		stabilityWindow: window,
		// Start the last-crash marker far enough below zero that the opening
		// stability window never suppresses a crash within the first window of
		// ticks: from tick 1 onward, tick-lastCrashTick already exceeds the
		// window.
		lastCrashTick: -1 - window,
		enabled:       cfg.Enabled,
	}
}

// Enabled reports whether crash injection is active for this schedule.
func (c *CrashSchedule) Enabled() bool { return c.enabled }

// ShouldCrash reports whether a crash should occur at the given tick. It returns
// false without drawing when crashes are disabled or when the tick is still
// inside the post-restart stability window (so the draw stream position depends
// only on the eligible ticks, keeping the crash sequence a pure function of the
// seed). On an eligible tick it draws exactly one Bool(crashProb); when that
// draw fires it records the tick as the most recent crash, opening a fresh
// stability window before the next eligible tick.
//
// tick must be non-decreasing across calls (the simulator advances it
// monotonically); calling out of order would corrupt the stability-window
// bookkeeping.
func (c *CrashSchedule) ShouldCrash(tick int64) bool {
	if !c.enabled {
		return false
	}
	// Suppress crashes inside the stability window without consuming a draw, so
	// the crash decision sequence is a pure function of the eligible ticks and
	// the seed — independent of how many ticks the window spans.
	if tick-c.lastCrashTick <= c.stabilityWindow {
		return false
	}
	if c.seed.Bool(c.crashProb) {
		c.lastCrashTick = tick
		return true
	}
	return false
}

// LastCrashTick returns the tick of the most recent crash this schedule fired,
// or a negative sentinel before the first crash. It is exposed for reports and
// tests asserting on crash timing.
func (c *CrashSchedule) LastCrashTick() int64 { return c.lastCrashTick }
