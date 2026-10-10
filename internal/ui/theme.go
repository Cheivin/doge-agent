package ui

import (
	"fmt"
	"os"
)

// detectColor reports whether ANSI styling should be emitted, honoring the
// NO_COLOR spec (any value disables it) and TERM=dumb. Glamour (markdown)
// respects these too via its own termenv profile detection; this guards the
// hand-rolled helpers below.
func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

// colorEnabled is resolved once at startup.
var colorEnabled = detectColor()

// theme is the default dark palette:
// 24-bit colors for message backgrounds, tool states, and text accents.
const (
	colText        = "#d4d4d4"
	colGray        = "#808080"
	colDimGray     = "#666666"
	colDarkGray    = "#505050"
	colAccent      = "#8abeb7"
	colGreen       = "#b5bd68"
	colRed         = "#cc6666"
	colYellow      = "#ffff00"
	colBlue        = "#5f87ff"
	colCyan        = "#00d7ff"
	colUserMsgBg   = "#343541"
	colToolPending = "#282832"
	colToolSuccess = "#283228"
	colToolError   = "#3c2828"
	colThinking    = "#6e6e6e"
)

func fg(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\x1b[38;2;" + hexRGB(code) + "m" + s + "\x1b[0m"
}
func bg(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\x1b[48;2;" + hexRGB(code) + "m" + s + "\x1b[0m"
}
func bold(s string) string {
	if !colorEnabled {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}
func italic(s string) string {
	if !colorEnabled {
		return s
	}
	return "\x1b[3m" + s + "\x1b[0m"
}

// hexRGB converts "#rrggbb" to "r;g;b" for ANSI 24-bit color escapes.
func hexRGB(hex string) string {
	var r, g, b int
	fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return fmt.Sprintf("%d;%d;%d", r, g, b)
}

// themed helpers for the palette.
func accent(s string) string  { return fg(colAccent, s) }
func muted(s string) string   { return fg(colGray, s) }
func dimText(s string) string { return fg(colDimGray, s) }
func errText(s string) string { return fg(colRed, s) }
func warn(s string) string    { return fg(colYellow, s) }

// thinkingColor returns the accent for a thinking level, falling back to
// the muted thinking color.
func thinkingColor(s string) string { return fg(colThinking, s) }

// userMsgBg colors one line as the user message box.
func userMsgBg(s string) string { return bg(colUserMsgBg, " "+s+" ") }

// toolBg colors one line of a tool execution block by state.
func toolBg(state string, s string) string {
	switch state {
	case "success":
		return bg(colToolSuccess, s)
	case "error":
		return bg(colToolError, s)
	default:
		return bg(colToolPending, s)
	}
}
