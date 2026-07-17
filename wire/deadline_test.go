package wire

import (
	"testing"
	"time"
)

func TestDeadlineBudget(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(8 * time.Millisecond)
	if got := RemainingUS(deadline, now); got != 8000 {
		t.Fatalf("RemainingUS = %d", got)
	}
	if got := RemainingUS(now, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("passed deadline: %d", got)
	}
	if got := RemainingUS(now.Add(3*time.Hour), now); got != 1<<32-1 {
		t.Fatalf("clamp: %d", got)
	}
	// A budget re-anchored on a skewed clock keeps its duration.
	skewed := now.Add(-5 * time.Minute)
	if d := Reanchor(skewed, 8000).Sub(skewed); d != 8*time.Millisecond {
		t.Fatalf("reanchor duration = %v", d)
	}
}
