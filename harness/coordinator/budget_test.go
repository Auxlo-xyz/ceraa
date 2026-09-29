package coordinator

import "testing"

func TestBudgetPointsAreGraduated(t *testing.T) {
	cases := []struct {
		name                          string
		soft, yield, grace            int
		wantSoft, wantYield, wantHard int
	}{
		// Unset means no limit, and every point must be zero so the guard is skipped
		// rather than compared against 0.
		{"unset", 0, 0, 0, 0, 0, 0},
		// The defaults: warn at the ceiling, yield at 1.5x, five requests of grace.
		{"defaults", 20, 0, 0, 20, 30, 35},
		{"odd soft", 7, 0, 0, 7, 10, 15},
		// An explicit yield point below the ceiling is nonsense and is corrected,
		// because a run must never be told to wrap up before it is warned.
		{"yield before soft", 20, 5, 0, 20, 30, 35},
		{"explicit", 10, 12, 3, 10, 12, 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{cfg: RunnerConfig{MaxRequests: tc.soft, YieldAt: tc.yield, HardGrace: tc.grace}}
			soft, yieldAt, hardAt := r.budgetPoints()
			if soft != tc.wantSoft || yieldAt != tc.wantYield || hardAt != tc.wantHard {
				t.Errorf("got (%d, %d, %d), want (%d, %d, %d)", soft, yieldAt, hardAt, tc.wantSoft, tc.wantYield, tc.wantHard)
			}
			if soft > 0 {
				if yieldAt <= soft {
					t.Errorf("yield at %d is not after the soft budget %d; a run would be stopped before being warned", yieldAt, soft)
				}
				if hardAt <= yieldAt {
					t.Errorf("hard stop at %d leaves no grace after the yield at %d, so a child told to wrap up is cut off mid-report", hardAt, yieldAt)
				}
			}
		})
	}
}
