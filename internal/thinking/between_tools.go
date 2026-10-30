package thinking

import (
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// ValidateBetweenToolsBody rejects fields forbidden by Claude between-tools mode
// It returns the original caller controls intact; it never repairs them by deletion
func ValidateBetweenToolsBody(body []byte) error {
	if !ojson.ValidBytes(body) {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking requires valid JSON")
	}
	object := ojson.GetBytes(body, "thinking")
	if !object.IsObject() {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking requires an object")
	}
	invalid := false
	object.ForEach(func(key, value ojson.Result) bool {
		if key.String() != "type" {
			invalid = true
			return false
		}
		return true
	})
	if invalid {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking accepts no other thinking fields")
	}
	effort := ojson.GetBytes(body, "output_config.effort")
	if effort.Exists() && (effort.Type != ojson.String || (effort.String() != "low" && effort.String() != "medium" && effort.String() != "high")) {
		return NewThinkingError(ErrLevelNotSupported, "between-tools thinking requires low, medium, or high effort")
	}
	return nil
}

// validateExplicitBetweenToolsDoc runs validateExplicitBetweenTools against the
// doc being applied; it serializes the doc only for an explicit between-tools
// request so ordinary requests pay one path lookup
func validateExplicitBetweenToolsDoc(doc *ojson.Doc, docValid bool, sourceBody []byte, fromFormat, providerFormat string, modelInfo *registry.ModelInfo, hasSuffix bool) error {
	if len(sourceBody) > 0 && fromFormat == "claude" {
		return validateExplicitBetweenTools(nil, sourceBody, fromFormat, providerFormat, modelInfo, hasSuffix)
	}
	if !docValid || doc.Get("thinking.type").String() != "between_tools" {
		return nil
	}
	return validateExplicitBetweenTools(doc.Bytes(), nil, fromFormat, providerFormat, modelInfo, hasSuffix)
}

// validateExplicitBetweenTools rejects an explicit Claude between-tools request
// that the route or its controls cannot carry; a model suffix takes priority
// over the body and skips the check
func validateExplicitBetweenTools(body, sourceBody []byte, fromFormat, providerFormat string, modelInfo *registry.ModelInfo, hasSuffix bool) error {
	if hasSuffix {
		return nil
	}
	modeBody := body
	if len(sourceBody) > 0 && fromFormat == "claude" {
		modeBody = sourceBody
	}
	explicit := extractClaudeConfig(modeBody)
	if explicit.Mode != ModeBetweenTools {
		return nil
	}
	if _, err := ValidateConfig(explicit, modelInfo, fromFormat, providerFormat, false); err != nil {
		return err
	}
	return ValidateBetweenToolsBody(modeBody)
}

// validateBetweenTools validates a between-tools config against the model
// capability and the claude-to-claude route
func validateBetweenTools(config ThinkingConfig, modelInfo *registry.ModelInfo, support *registry.ThinkingSupport, fromFormat, toFormat, model string) (*ThinkingConfig, error) {
	if fromFormat != "claude" || toFormat != "claude" || support == nil || !support.BetweenToolsAllowed || modelInfo.Type != "claude" {
		return nil, NewThinkingErrorWithModel(ErrThinkingNotSupported, "between-tools thinking is not supported for this route", model)
	}
	if config.Budget != 0 || (config.Level != "" && config.Level != LevelLow && config.Level != LevelMedium && config.Level != LevelHigh) {
		return nil, NewThinkingErrorWithModel(ErrLevelNotSupported, "between-tools thinking requires low, medium, or high effort", model)
	}
	return &config, nil
}
