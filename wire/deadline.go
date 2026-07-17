package wire

import "time"

// Deadlines travel as remaining-budget microseconds, never absolute
// times: the receiver re-anchors the budget against its own monotonic
// clock at receipt, so clock skew between nodes never corrupts a
// deadline; the only error is the one-way flight time, bounded
// same-AZ.

// RemainingUS converts a local deadline into the budget to put on the
// wire, clamped to zero when the deadline has passed and to the u32
// ceiling for callers without one (a real query budget is tens of
// milliseconds).
func RemainingUS(deadline, now time.Time) uint32 {
	us := deadline.Sub(now).Microseconds()
	if us <= 0 {
		return 0
	}
	return uint32(min(us, 1<<32-1))
}

// Reanchor turns a received budget into a deadline on the receiver's
// own clock.
func Reanchor(now time.Time, budgetUS uint32) time.Time {
	return now.Add(time.Duration(budgetUS) * time.Microsecond)
}
