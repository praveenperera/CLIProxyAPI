package claude

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestBetweenToolsCanonicalPipeline(t *testing.T) {
	model := &registry.ModelInfo{ID: "claude-sonnet-5-5", Type: "claude", Thinking: &registry.ThinkingSupport{BetweenToolsAllowed: true, ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}}}
	for _, effort := range []string{"", "low", "medium", "high"} {
		body := []byte(`{"thinking":{"type":"between_tools"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"opaque+/= unchanged"}]}]}`)
		if effort != "" {
			body = []byte(`{"thinking":{"type":"between_tools"},"output_config":{"effort":"` + effort + `"},"messages":[]}`)
		}
		result, err := thinking.ApplyThinkingWithModelInfo(body, nil, model.ID, "claude", "claude", "claude", model)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result, body) {
			t.Fatalf("native between-tools changed: %s", result)
		}
	}
	invalid := []string{
		`{"thinking":{"type":"between_tools","block_binding":{"prefix_mismatch_behavior":"error"}}}`,
		`{"thinking":{"type":"between_tools","display":"omitted"}}`,
		`{"thinking":{"type":"between_tools","budget_tokens":0}}`,
		`{"thinking":{"type":"between_tools"},"output_config":{"effort":"xhigh"}}`,
		`{"thinking":{"type":"between_tools"},"output_config":{"effort":"max"}}`,
		`{"thinking":{"type":"between_tools"},"output_config":{"effort":null}}`,
	}
	for _, input := range invalid {
		body := []byte(input)
		result, err := thinking.ApplyThinkingWithModelInfo(body, nil, model.ID, "claude", "claude", "claude", model)
		if err == nil || !bytes.Equal(result, body) {
			t.Fatal("invalid controls repaired or accepted", input)
		}
	}
	body := []byte(`{"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}}`)
	result, err := thinking.ApplyThinkingWithModelInfo(body, nil, model.ID+"(low)", "claude", "claude", "claude", model)
	if err != nil || gjson.GetBytes(result, "thinking.type").String() != "adaptive" || gjson.GetBytes(result, "output_config.effort").String() != "low" {
		t.Fatal("legacy suffix priority changed", err, string(result))
	}
	unsupported := *model
	unsupported.Thinking = &registry.ThinkingSupport{Levels: []string{"low", "high"}}
	if _, err := thinking.ApplyThinkingWithModelInfo(body, nil, model.ID, "claude", "claude", "claude", &unsupported); err == nil {
		t.Fatal("unsupported model accepted")
	}
	if _, err := thinking.ApplyThinkingWithModelInfo(body, nil, "unknown", "claude", "claude", "claude", nil); err == nil {
		t.Fatal("unknown capability accepted")
	}
	if _, err := thinking.ValidateConfig(thinking.ThinkingConfig{Mode: thinking.ModeBetweenTools}, model, "claude", "codex", false); err == nil {
		t.Fatal("unsupported provider accepted")
	}
}
