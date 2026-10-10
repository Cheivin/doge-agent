package coding

import (
	"fmt"
	"html"
	"os"
	"strings"

	"doge/internal/ai"
)

// ExportToHTML renders a session transcript as a standalone HTML file,
// following the export functionality in a simplified form.
func ExportToHTML(session *Session, outPath string) error {
	var body strings.Builder
	for _, entry := range session.Entries {
		if entry.Message == nil {
			continue
		}
		body.WriteString(renderHTMLMessage(entry.Message))
	}

	template := `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>doge session %s</title>
<style>
body { font-family: -apple-system, sans-serif; max-width: 900px; margin: 2rem auto; padding: 0 1rem; color: #222; }
.msg { margin: 1rem 0; padding: 0.75rem 1rem; border-radius: 8px; }
.user { background: #eef3fb; }
.assistant { background: #f6f6f6; }
.tool { background: #fbfbf0; font-family: monospace; font-size: 0.9em; }
.error { background: #fdecec; color: #a33; }
pre { background: #f0f0f0; padding: 0.75rem; border-radius: 6px; overflow-x: auto; }
.role { font-size: 0.8em; color: #888; margin-bottom: 0.25rem; }
</style>
</head>
<body>
<h1>doge session %s</h1>
<p>cwd: %s</p>
%s
</body>
</html>`
	out := fmt.Sprintf(template, html.EscapeString(session.Header.ID),
		html.EscapeString(session.Header.ID), html.EscapeString(session.Header.Cwd), body.String())
	return os.WriteFile(outPath, []byte(out), 0o644)
}

func renderHTMLMessage(m *ai.Message) string {
	var content strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case ai.ContentTypeText:
			content.WriteString("<pre>" + html.EscapeString(b.Text) + "</pre>")
		case ai.ContentTypeThinking:
			content.WriteString("<p style='color:#888'><i>" + html.EscapeString(b.Thinking) + "</i></p>")
		case ai.ContentTypeToolCall:
			content.WriteString("<p>→ <b>" + html.EscapeString(b.ToolName) + "</b> " +
				html.EscapeString(fmt.Sprintf("%v", b.Arguments)) + "</p>")
		}
	}
	cls := "assistant"
	switch m.Role {
	case ai.RoleUser:
		cls = "user"
	case ai.RoleToolResult:
		cls = "tool"
	}
	if m.IsError {
		cls = "error"
	}
	return fmt.Sprintf("<div class='msg %s'><div class='role'>%s</div>%s</div>\n",
		cls, html.EscapeString(m.Role), content.String())
}
