package tools

import "doge/internal/agent"

// AllTools returns the default coding tools: read, bash, edit, write.
func AllTools(ops *Operations) []*agent.AgentTool {
	return []*agent.AgentTool{
		ReadTool(ops),
		BashTool(ops),
		EditTool(ops),
		WriteTool(ops),
		CodeTool(ops),
	}
}

// ReadOnlyTools returns read/grep/find/ls for restricted modes.
func ReadOnlyTools(ops *Operations) []*agent.AgentTool {
	return []*agent.AgentTool{
		ReadTool(ops),
		GrepTool(ops),
		FindTool(ops),
		LSTool(ops),
	}
}
