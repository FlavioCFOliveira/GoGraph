package sim

import "time"

// VirtualClock is the simulation's logical clock. It models the passage of time
// as a monotonically-increasing tick counter rather than reading the wall
// clock, so the simulation never observes real time and stays fully
// deterministic. One tick represents one unit of simulated time whose duration
// is fixed at construction (1 tick == 1ms by convention).
//
// VirtualClock deliberately exposes no way to read [time.Now]: the entire sim
// package is free of wall-clock reads, which is what lets a given seed replay
// identically.
//
// # Concurrency contract
//
// VirtualClock is NOT safe for concurrent use. It is advanced and read from the
// single simulation goroutine only.
type VirtualClock struct {
	ticks    int64
	tickSize time.Duration
}

// NewVirtualClock returns a clock at tick zero whose every tick advances
// simulated time by tickSize. A non-positive tickSize is normalised to 1ms so
// [VirtualClock.SimulatedTime] always advances.
func NewVirtualClock(tickSize time.Duration) *VirtualClock {
	if tickSize <= 0 {
		tickSize = time.Millisecond
	}
	return &VirtualClock{tickSize: tickSize}
}

// Tick advances the clock by one tick and returns the new tick count.
func (c *VirtualClock) Tick() int64 {
	c.ticks++
	return c.ticks
}

// Now returns the current tick count (the number of ticks elapsed since
// construction).
func (c *VirtualClock) Now() int64 { return c.ticks }

// SimulatedTime returns the simulated elapsed time, computed as the tick count
// multiplied by the per-tick duration.
func (c *VirtualClock) SimulatedTime() time.Duration {
	return time.Duration(c.ticks) * c.tickSize
}
