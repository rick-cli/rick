package agent

import (
	"strings"
	"testing"
)

func TestCapSubagentReportTrimsLongReports(t *testing.T) {
	short := "a short report"
	if got := capSubagentReport(short); got != short {
		t.Fatalf("short report altered: %q", got)
	}
	long := strings.Repeat("payload-line\n", 4000) // ~52KB
	got := capSubagentReport(long)
	if len(got) > maxSubagentReportBytes {
		t.Fatalf("capped report still too large: %d bytes", len(got))
	}
	if !strings.Contains(got, "subagent report omitted") {
		t.Fatalf("capped report lacks omission marker")
	}
	// Head + tail preserved so the parent gets the conclusion.
	if !strings.HasPrefix(got, "payload-line\n") {
		t.Fatalf("capped report lost the head")
	}
	if !strings.HasSuffix(got, "payload-line\n") {
		t.Fatalf("capped report lost the tail")
	}
}
