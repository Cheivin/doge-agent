package coding

import (
	"encoding/json"
	"strings"

	"doge/internal/ai"
)

// CompactionSettings mirror the defaults: reserve tokens for the response,
// keep the most recent tokens in context.
type CompactionSettings struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
}

// DefaultCompactionSettings returns the defaults.
func DefaultCompactionSettings() CompactionSettings {
	return CompactionSettings{
		Enabled:          true,
		ReserveTokens:    16384,
		KeepRecentTokens: 20000,
	}
}

// ShouldCompact reports whether the context should be compacted, mirroring
// shouldCompact: tokens > contextWindow - reserveTokens.
func ShouldCompact(contextTokens, contextWindow, reserveTokens int) bool {
	return contextTokens > contextWindow-reserveTokens
}

// FindCutPoint finds the cut index in entries (root-first) such that roughly
// keepRecentTokens remain in context. Entries before the cut are summarized.
// The cut never falls on a tool result ; the returned index is
// the first entry to KEEP.
func FindCutPoint(entries []*SessionEntry, keepRecentTokens int) (int, bool) {
	tokens := 0
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		tokens += estimateEntryTokens(e)
		if tokens < keepRecentTokens {
			continue
		}
		// Candidate cut: index i is the first entry to keep.
		cut := i
		// Backtrack to the nearest legal cut point (never a tool result).
		for cut < len(entries) && !isLegalCut(entries[cut]) {
			cut++
		}
		if cut >= len(entries) {
			return -1, false
		}
		return cut, true
	}
	return -1, false
}

func isLegalCut(e *SessionEntry) bool {
	if e.Type != EntryMessage || e.Message == nil {
		// Compaction/branch-summary/custom entries are legal cuts.
		return e.Type == EntryCompaction || e.Type == EntryBranchSummary ||
			e.Type == EntryCustomMessage || e.Type == EntryCustom
	}
	role := e.Message.Role
	return role == ai.RoleUser || role == ai.RoleAssistant
}

func estimateEntryTokens(e *SessionEntry) int {
	if e.Message != nil {
		return ai.EstimateMessageTokens(e.Message)
	}
	// Non-message entries are cheap.
	return 10
}

// SummarizationPrompt is the fixed-format summarization prompt .
const SummarizationPrompt = `## Goal
Analyze the conversation and provide a concise summary that captures all important information from the conversation. If there are requirements, constraints, bugs, errors, or anything else that the next conversation should know, include it in the summary.

## Constraints & Preferences
- Keep the summary concise but comprehensive. Include all important information such as requirements, constraints, bugs, errors, and any other important details.
- Avoid using placeholders like "the user asked..." or "the assistant did..." or similar. Instead, provide concrete details.
- Only include information that is relevant to the current task.
- Do not include information about the conversation itself, like "the user asked a question" or "the assistant responded".
- Structure the summary in the following sections:

## Progress
- Done: A bullet list of what has been accomplished so far.
- In Progress: A bullet list of what is currently in progress.
- Blocked: A bullet list of any blockers.

## Key Decisions
- A bullet list of key decisions made so far.

## Next Steps
- A bullet list of what should be done next.

## Critical Context
- A bullet list of any critical context that the next conversation should know.`

// UpdateSummarizationPrompt is used when a previous summary exists.
const UpdateSummarizationPrompt = `## Goal
Analyze the conversation and provide an updated summary of the previous summary and the new conversation. If there are requirements, constraints, bugs, errors, or anything else that the next conversation should know, include it in the summary.

## Constraints & Preferences
- Keep the summary concise but comprehensive. Include all important information such as requirements, constraints, bugs, errors, and any other important details.
- Avoid using placeholders like "the user asked..." or "the assistant did..." or similar. Instead, provide concrete details.
- Only include information that is relevant to the current task.
- Do not include information about the conversation itself, like "the user asked a question" or "the assistant responded".
- Structure the summary in the following sections:

## Previous Summary
%s

## Progress
- Done: A bullet list of what has been accomplished so far.
- In Progress: A bullet list of what is currently in progress.
- Blocked: A bullet list of any blockers.

## Key Decisions
- A bullet list of key decisions made so far.

## Next Steps
- A bullet list of what should be done next.

## Critical Context
- A bullet list of any critical context that the next conversation should know.`

// SummarizationSystemPrompt guides the summarization model.
const SummarizationSystemPrompt = `You are an expert conversation summarizer. You produce concise, information-dense summaries of coding sessions.`

// SerializeConversation renders entries as plain text (tool results
// truncated), so the model summarizes instead of continuing the dialogue.
func SerializeConversation(entries []*SessionEntry) string {
	var out strings.Builder
	out.WriteString("<conversation>\n")
	for _, e := range entries {
		if e.Message == nil {
			continue
		}
		m := e.Message
		switch m.Role {
		case ai.RoleUser:
			out.WriteString("USER: " + m.Text() + "\n")
		case ai.RoleAssistant:
			text := m.Text()
			if text != "" {
				out.WriteString("ASSISTANT: " + text + "\n")
			}
			for _, b := range m.Content {
				if b.Type == ai.ContentTypeToolCall {
					out.WriteString("TOOL CALL: " + b.ToolName + " " + strings.TrimSpace(marshalArgs(b.Arguments)) + "\n")
				}
			}
		case ai.RoleToolResult:
			text := m.Text()
			if len(text) > 2000 {
				text = text[:2000] + "…"
			}
			out.WriteString("TOOL RESULT: " + text + "\n")
		}
	}
	out.WriteString("</conversation>")
	return out.String()
}

func marshalArgs(args map[string]any) string {
	data, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(data)
}

// BuildSummarizationPrompt builds the summarization prompt for the entries
// before the cut point.
func BuildSummarizationPrompt(entries []*SessionEntry, previousSummary string) string {
	conversation := SerializeConversation(entries)
	if previousSummary != "" {
		prompt := UpdateSummarizationPrompt
		prompt = strings.ReplaceAll(prompt, "%s", previousSummary)
		return prompt + "\n\n<conversation>\n" + conversation + "\n</conversation>"
	}
	return SummarizationPrompt + "\n\n<conversation>\n" + conversation + "\n</conversation>"
}
