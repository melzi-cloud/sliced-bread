package guard

import (
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		status  string
		down    time.Duration
		resetAt time.Time
		want    Decision
	}{
		{"off", 10 * time.Second, time.Time{}, Wait},
		{"off", 40 * time.Second, time.Time{}, Replace},
		{"", 40 * time.Second, time.Time{}, Replace},
		{"running", 2 * time.Minute, time.Time{}, Wait},
		{"running", 6 * time.Minute, time.Time{}, Reset},
		{"running", 10 * time.Minute, now.Add(-3 * time.Minute), Wait},
		{"running", 16 * time.Minute, now.Add(-9 * time.Minute), Replace},
	}
	for i, c := range cases {
		if got := Decide(c.status, c.down, c.resetAt, now); got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestNodeViews(t *testing.T) {
	v := nodeViews([]map[string]any{{
		"metadata": map[string]any{"name": "sb-r-s2"},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "Unknown", "lastTransitionTime": "2026-11-07T03:00:00Z"}},
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": "10.0.1.3"}},
		},
	}, {"metadata": map[string]any{"name": "no-status"}}})
	if len(v) != 2 || v[1].Ready || v[1].InternalIP != "10.0.1.3" || v[1].Since.IsZero() {
		t.Fatalf("%+v", v)
	}
}
