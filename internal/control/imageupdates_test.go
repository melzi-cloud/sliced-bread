package control

import "testing"

func TestLatestVersion(t *testing.T) {
	tags := []string{"latest", "1.37.4", "1.37.5", "1.37.6", "1.38.0", "1.38.0-alpine", "20240101", "testing", "2.0.0", "1.4.10"}
	if got := latestVersion("1.37.5", tags); got != "2.0.0" {
		t.Fatalf("got %q", got)
	}
	if got := latestVersion("1.38.0", []string{"1.37.9", "1.38.0"}); got != "" {
		t.Fatalf("no newer: %q", got)
	}
	if got := latestVersion("v3.4.1", []string{"v3.4.2", "3.9.9", "v3.5.0"}); got != "v3.5.0" {
		t.Fatalf("v prefix: %q", got)
	}
	if got := latestVersion("latest", tags); got != "" {
		t.Fatalf("non-version current: %q", got)
	}
	if got := latestVersion("3.4.1", []string{"20250101.1.1", "3.4.2"}); got != "3.4.2" {
		t.Fatalf("date-like: %q", got)
	}
}
