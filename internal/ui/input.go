package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// inputBox is a multi-line text input with cursor editing, replacing the
// external textarea dependency. Enter submits (handled by the caller);
// Shift+Enter inserts a newline. The cursor is a rune index into value.
type inputBox struct {
	value       string
	cursor      int
	focused     bool
	width       int
	placeholder string
}

func newInputBox() *inputBox {
	return &inputBox{placeholder: "Ask doge…"}
}

func (b *inputBox) SetWidth(w int) { b.width = w }

func (b *inputBox) SetValue(v string) {
	b.value = v
	b.cursor = len([]rune(v))
}

func (b *inputBox) Value() string { return b.value }

func (b *inputBox) Reset() {
	b.value = ""
	b.cursor = 0
}

func (b *inputBox) Focus() { b.focused = true }

func (b *inputBox) Blur() { b.focused = false }

func (b *inputBox) Focused() bool { return b.focused }

// Update handles a key press message; returns false when the key was not
// consumed (submit/quit are handled by the caller).
func (b *inputBox) Update(msg tea.KeyPressMsg) bool {
	switch msg.String() {
	case "enter", "ctrl+c", "ctrl+d":
		return false
	case "shift+enter":
		// Newline inside the input; plain Enter still submits.
		b.insert("\n")
		return true
	case "backspace":
		if b.cursor > 0 {
			r := []rune(b.value)
			r = append(r[:b.cursor-1], r[b.cursor:]...)
			b.value = string(r)
			b.cursor--
		}
		return true
	case "delete":
		if b.cursor < len([]rune(b.value)) {
			r := []rune(b.value)
			r = append(r[:b.cursor], r[b.cursor+1:]...)
			b.value = string(r)
		}
		return true
	case "left":
		if b.cursor > 0 {
			b.cursor--
		}
		return true
	case "right":
		if b.cursor < len([]rune(b.value)) {
			b.cursor++
		}
		return true
	case "up", "down":
		// Vertical cursor movement between lines of a multi-line input.
		if msg.String() == "up" {
			b.moveLine(-1)
		} else {
			b.moveLine(1)
		}
		return true
	case "home":
		b.cursor = 0
		return true
	case "end":
		b.cursor = len([]rune(b.value))
		return true
	case "ctrl+w":
		// Delete the word before the cursor.
		r := []rune(b.value)
		i := b.cursor
		for i > 0 && r[i-1] == ' ' {
			i--
		}
		for i > 0 && r[i-1] != ' ' {
			i--
		}
		b.value = string(append(append([]rune{}, r[:i]...), r[b.cursor:]...))
		b.cursor = i
		return true
	case "ctrl+u":
		// Delete from the start of the current line to the cursor.
		r := []rune(b.value)
		lineStart := 0
		for i := b.cursor - 1; i >= 0; i-- {
			if r[i] == '\n' {
				lineStart = i + 1
				break
			}
		}
		b.value = string(append(append([]rune{}, r[:lineStart]...), r[b.cursor:]...))
		b.cursor = lineStart
		return true
	case "space":
		b.insert(" ")
		return true
	}
	// Printable characters arrive with a populated Text (v2 merges what v1
	// called "runes" into it).
	if len(msg.Text) > 0 {
		b.insert(msg.Text)
		return true
	}
	return false
}

// Paste inserts pasted text at the cursor, preserving line breaks so
// multi-line pastes land as multi-line input.
func (b *inputBox) Paste(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	b.insert(s)
}

// insert inserts text at the cursor.
func (b *inputBox) insert(s string) {
	r := []rune(b.value)
	head := string(r[:b.cursor])
	tail := string(r[b.cursor:])
	b.value = head + s + tail
	b.cursor += len([]rune(s))
}

// moveLine moves the cursor vertically by delta lines, keeping its column
// (clamped to the target line's length). No-op at the first/last line.
func (b *inputBox) moveLine(delta int) {
	lines := strings.Split(b.value, "\n")
	// Locate the cursor's line and its rune offset within that line.
	cur := b.cursor
	lineIdx := 0
	for i, l := range lines {
		n := len([]rune(l))
		if cur <= n {
			lineIdx = i
			break
		}
		cur -= n + 1
	}
	target := lineIdx + delta
	if target < 0 || target >= len(lines) {
		return
	}
	if cur > len([]rune(lines[target])) {
		cur = len([]rune(lines[target]))
	}
	idx := 0
	for i := 0; i < target; i++ {
		idx += len([]rune(lines[i])) + 1
	}
	b.cursor = idx + cur
}

// View renders the input with a cursor marker. Single-line input keeps the
// original "❯ " prompt; multi-line input renders each line, the first with the
// prompt and continuation lines indented to align, with the cursor marker on
// its line.
func (b *inputBox) View() string {
	const prefix = "❯ "
	const indent = "  "
	value := b.value
	if value == "" && !b.focused {
		return truncateLine(prefix+b.placeholder, b.width)
	}
	if !b.focused {
		return truncateLine(prefix+value, b.width)
	}
	lines := strings.Split(value, "\n")
	// Cursor → (line, column) with column a rune offset into the line.
	cur := b.cursor
	lineIdx := 0
	for i, l := range lines {
		n := len([]rune(l))
		if cur <= n {
			lineIdx = i
			break
		}
		cur -= n + 1
	}
	r := []rune(lines[lineIdx])
	if cur > len(r) {
		cur = len(r)
	}
	lines[lineIdx] = string(r[:cur]) + "▏" + string(r[cur:])
	for i, l := range lines {
		if i == 0 {
			lines[i] = truncateLine(prefix+l, b.width)
		} else {
			lines[i] = truncateLine(indent+l, b.width)
		}
	}
	return strings.Join(lines, "\n")
}
