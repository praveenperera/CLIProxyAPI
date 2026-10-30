package claude

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
)

// applyBetweenTools validates a between-tools config and writes it in Claude
// format, leaving every other caller control untouched
func applyBetweenTools(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) ([]byte, error) {
	doc := ojson.NewDoc(body)
	if err := applyBetweenToolsDoc(doc, config, modelInfo); err != nil {
		return body, err
	}
	return doc.Bytes(), nil
}

// applyBetweenToolsDoc is applyBetweenTools for a doc edited in place; values
// that already match are not rewritten so a native request passes through
// byte-identical
func applyBetweenToolsDoc(doc *ojson.Doc, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) error {
	if _, err := thinking.ValidateConfig(config, modelInfo, "claude", "claude", false); err != nil {
		return err
	}
	if err := thinking.ValidateBetweenToolsBody(doc.Bytes()); err != nil {
		return err
	}
	if doc.Get("thinking.type").String() != "between_tools" {
		if err := doc.Set("thinking.type", "between_tools"); err != nil {
			return err
		}
	}
	if config.Level != "" && doc.Get("output_config.effort").String() != string(config.Level) {
		return doc.Set("output_config.effort", string(config.Level))
	}
	return nil
}
