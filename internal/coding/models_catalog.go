package coding

import (
	"encoding/json"
	"os"

	"doge/internal/ai"
)

// BuiltinModels is the static model catalog: OpenAI + Anthropic via their
// native APIs, plus OpenAI-compatible services (Chat Completions).
// Costs are approximate $/M tokens.

var builtinModels = []*ai.Model{
	// OpenAI (Responses API)
	{ID: "gpt-5.2", Name: "GPT-5.2", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000, MaxTokens: 100000,
		Cost: ai.ModelCost{Input: 1.25, Output: 10}},
	{ID: "gpt-5.1", Name: "GPT-5.1", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000, MaxTokens: 100000,
		Cost: ai.ModelCost{Input: 1.25, Output: 10}},
	{ID: "gpt-5-mini", Name: "GPT-5 Mini", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 400000, MaxTokens: 100000,
		Cost: ai.ModelCost{Input: 0.25, Output: 2}},
	{ID: "gpt-4.1", Name: "GPT-4.1", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Input: []string{"text", "image"}, ContextWindow: 1000000, MaxTokens: 32768,
		Cost: ai.ModelCost{Input: 2, Output: 8}},
	{ID: "gpt-4.1-mini", Name: "GPT-4.1 Mini", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Input: []string{"text", "image"}, ContextWindow: 1000000, MaxTokens: 32768,
		Cost: ai.ModelCost{Input: 0.4, Output: 1.6}},
	{ID: "o4-mini", Name: "o4-mini", API: ai.APIOpenAIResponses, Provider: "openai", BaseURL: "https://api.openai.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 100000,
		Cost: ai.ModelCost{Input: 1.1, Output: 4.4}},

	// Anthropic (Messages API)
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", API: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 64000,
		ThinkingLevelMap: map[string]any{"minimal": 1024.0, "low": 2048.0, "medium": 4096.0, "high": 8192.0, "xhigh": 16384.0},
		Cost:             ai.ModelCost{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.3}},
	{ID: "claude-opus-4-1", Name: "Claude Opus 4.1", API: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 32000,
		Cost: ai.ModelCost{Input: 15, Output: 75, CacheWrite: 18.75, CacheRead: 1.5}},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", API: ai.APIAnthropicMessages, Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1",
		Reasoning: true, Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 64000,
		Cost: ai.ModelCost{Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1}},

	// DeepSeek (OpenAI-compatible)
	{ID: "deepseek-chat", Name: "DeepSeek Chat", API: ai.APIOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1",
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.27, Output: 1.1}},
	{ID: "deepseek-reasoner", Name: "DeepSeek Reasoner", API: ai.APIOpenAICompletions, Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.55, Output: 2.19}},

	// Qwen / DashScope (OpenAI-compatible)
	{ID: "qwen3-coder-plus", Name: "Qwen3 Coder Plus", API: ai.APIOpenAICompletions, Provider: "qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 262144, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.6, Output: 2.2}},
	{ID: "qwen-max", Name: "Qwen Max", API: ai.APIOpenAICompletions, Provider: "qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Input: []string{"text"}, ContextWindow: 32768, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 2.4, Output: 9.6}},
	{ID: "qwen-plus", Name: "Qwen Plus", API: ai.APIOpenAICompletions, Provider: "qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		Input: []string{"text"}, ContextWindow: 131072, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.8, Output: 2}},

	// Moonshot (OpenAI-compatible)
	{ID: "kimi-k2", Name: "Kimi K2", API: ai.APIOpenAICompletions, Provider: "moonshot", BaseURL: "https://api.moonshot.cn/v1",
		Reasoning: true, Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 16384,
		Cost: ai.ModelCost{Input: 0.6, Output: 2.5}},
	{ID: "moonshot-v1-32k", Name: "Moonshot v1 32k", API: ai.APIOpenAICompletions, Provider: "moonshot", BaseURL: "https://api.moonshot.cn/v1",
		Input: []string{"text"}, ContextWindow: 32768, MaxTokens: 4096,
		Cost: ai.ModelCost{Input: 0.6, Output: 2.5}},

	// Zhipu GLM (OpenAI-compatible)
	{ID: "glm-4.6", Name: "GLM-4.6", API: ai.APIOpenAICompletions, Provider: "zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 1, Output: 5}},
	{ID: "glm-4.5", Name: "GLM-4.5", API: ai.APIOpenAICompletions, Provider: "zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Input: []string{"text"}, ContextWindow: 128000, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.8, Output: 2}},

	// Z.ai (OpenAI-compatible)
	{ID: "zai-1", Name: "ZAI-1", API: ai.APIOpenAICompletions, Provider: "zai", BaseURL: "https://api.z.ai/api/paas/v4",
		Input: []string{"text"}, ContextWindow: 131072, MaxTokens: 8192,
		Cost: ai.ModelCost{Input: 0.7, Output: 2.8}},

	// OpenRouter (OpenAI-compatible, any model)
	{ID: "auto", Name: "OpenRouter Auto", API: ai.APIOpenAICompletions, Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
		Input: []string{"text", "image"}, ContextWindow: 200000, MaxTokens: 8192,
		Cost: ai.ModelCost{}},
}

// LoadCustomModels returns user-defined models from models.json. A missing
// file is not an error.
func LoadCustomModels(modelsFile string) ([]*ai.Model, error) {
	if modelsFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(modelsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var custom []*ai.Model
	if err := json.Unmarshal(data, &custom); err != nil {
		return nil, err
	}
	return custom, nil
}

// SaveCustomModels 写回自定义模型文件（扁平数组；空列表落盘为 []）
func SaveCustomModels(modelsFile string, models []*ai.Model) error {
	if models == nil {
		models = []*ai.Model{}
	}
	data, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(modelsFile, append(data, '\n'), 0o644)
}

// RegisterBuiltinProviders adds the providers for the builtin models.
func RegisterBuiltinProviders(models *ai.Models) {
	models.AddProvider(&ai.Provider{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1"})
	models.AddProvider(&ai.Provider{ID: "anthropic", Name: "Anthropic", BaseURL: "https://api.anthropic.com/v1"})
	models.AddProvider(&ai.Provider{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1"})
	models.AddProvider(&ai.Provider{ID: "qwen", Name: "Qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"})
	models.AddProvider(&ai.Provider{ID: "moonshot", Name: "Moonshot", BaseURL: "https://api.moonshot.cn/v1"})
	models.AddProvider(&ai.Provider{ID: "zhipu", Name: "Zhipu", BaseURL: "https://open.bigmodel.cn/api/paas/v4"})
	models.AddProvider(&ai.Provider{ID: "zai", Name: "Z.ai", BaseURL: "https://api.z.ai/api/paas/v4"})
	models.AddProvider(&ai.Provider{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1"})
	models.AddProvider(&ai.Provider{ID: "local", Name: "Local", RequiresKey: false})
}
