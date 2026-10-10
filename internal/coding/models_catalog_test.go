package coding

import (
	"os"
	"path/filepath"
	"testing"

	"doge/internal/ai"
)

// TestSaveCustomModelsRoundTrip 保存后原样读回；空列表落盘为 []
func TestSaveCustomModelsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	in := []*ai.Model{
		{ID: "glm-custom", Name: "GLM Custom", Provider: "zhipu", API: ai.APIOpenAICompletions,
			BaseURL: "https://example.com/v1", ContextWindow: 131072, MaxTokens: 8192,
			Input: []string{"text", "image"}, Reasoning: true},
	}
	if err := SaveCustomModels(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadCustomModels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != "glm-custom" || out[0].Provider != "zhipu" ||
		out[0].BaseURL != "https://example.com/v1" || out[0].ContextWindow != 131072 ||
		out[0].MaxTokens != 8192 || len(out[0].Input) != 2 || !out[0].Reasoning {
		t.Fatalf("round trip = %+v", out)
	}

	// 删光后写 []
	if err := SaveCustomModels(path, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("empty catalog = %q", string(data))
	}
	if custom, err := LoadCustomModels(path); err != nil || len(custom) != 0 {
		t.Fatalf("load empty = %v, %v", custom, err)
	}
}
