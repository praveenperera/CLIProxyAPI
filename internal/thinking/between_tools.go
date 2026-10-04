package thinking

import "github.com/tidwall/gjson"

// ValidateBetweenToolsBody rejects fields forbidden by Claude between-tools mode
// It returns the original caller controls intact; it never repairs them by deletion
func ValidateBetweenToolsBody(body []byte) error {
	if !gjson.ValidBytes(body) {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking requires valid JSON")
	}
	object := gjson.GetBytes(body, "thinking")
	if !object.IsObject() {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking requires an object")
	}
	invalid := false
	object.ForEach(func(key, value gjson.Result) bool {
		if key.String() != "type" {
			invalid = true
			return false
		}
		return true
	})
	if invalid {
		return NewThinkingError(ErrThinkingNotSupported, "between-tools thinking accepts no other thinking fields")
	}
	effort := gjson.GetBytes(body, "output_config.effort")
	if effort.Exists() && (effort.Type != gjson.String || (effort.String() != "low" && effort.String() != "medium" && effort.String() != "high")) {
		return NewThinkingError(ErrLevelNotSupported, "between-tools thinking requires low, medium, or high effort")
	}
	return nil
}
