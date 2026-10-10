package ui

import "testing"

// TestDetectColor verifies the NO_COLOR / TERM=dumb adaptive degradation.
func TestDetectColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	if detectColor() {
		t.Error("NO_COLOR should disable ANSI styling")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if detectColor() {
		t.Error("TERM=dumb should disable ANSI styling")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if !detectColor() {
		t.Error("styling should be enabled with color-capable TERM and no NO_COLOR")
	}
}
