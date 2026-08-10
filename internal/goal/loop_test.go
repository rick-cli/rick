package goal

import (
	"strings"
	"testing"
	"time"
)

func TestProgressShowsLoopRun(t *testing.T) {
	g := &Goal{
		Title:  "loop task",
		Status: "active",
		LoopRun: &LoopRun{
			MinRunSeconds: 3600,
			MaxRetries:    100,
			Retries:       7,
			StartedAt:     time.Now().Add(-10 * time.Minute),
		},
	}
	progress := Progress(g)
	if !strings.Contains(progress, "loop 7/100 retries") {
		t.Fatalf("progress = %q, want loop retry info", progress)
	}
	if !strings.Contains(progress, "50m") || !strings.Contains(progress, "1h00m") {
		t.Fatalf("progress = %q, want remaining/total durations", progress)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		secs int
		want string
	}{
		{45, "45s"},
		{90, "1m30s"},
		{3600, "1h00m"},
		{3630, "1h00m"}, // 1h0m30s renders as 1h00m, floor of minutes
		{0, "0s"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.secs); got != tt.want {
			t.Errorf("formatDuration(%d) = %q, want %q", tt.secs, got, tt.want)
		}
	}
}

func TestLoopGoalDefaultsToUnlimitedBudget(t *testing.T) {
	g := &Goal{Title: "t", Status: "active", LoopRun: &LoopRun{MinRunSeconds: 60, MaxRetries: 100}}
	if ok, remaining := CheckBudget(g); !ok || remaining != -1 {
		t.Fatalf("loop goal budget = ok:%v remaining:%d, want unlimited", ok, remaining)
	}
}
