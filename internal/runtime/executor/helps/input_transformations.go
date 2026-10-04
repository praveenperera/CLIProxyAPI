package helps

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const maxInputTransformationObservations = 4096

// ObserveInputTransformations records bounded provider-reported drops without retaining paths
// Duplicate entries across start and final events count once for the upstream attempt
func (r *UsageReporter) ObserveInputTransformations(payload []byte) {
	if r == nil {
		return
	}
	payload = bytes.TrimSpace(payload)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[5:])
	}
	if !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	lists := []gjson.Result{root.Get("input_transformations")}
	if root.Get("type").String() == "message_start" {
		lists = append(lists, root.Get("message.input_transformations"))
	}
	if root.Get("type").String() == "message_delta" {
		lists = append(lists, root.Get("delta.input_transformations"))
	}
	r.transformationsMu.Lock()
	defer r.transformationsMu.Unlock()
	for _, list := range lists {
		if !list.IsArray() {
			continue
		}
		list.ForEach(func(_, entry gjson.Result) bool {
			if len(r.transformationEntries) >= maxInputTransformationObservations {
				return false
			}
			kind, reason, path := entry.Get("type"), entry.Get("reason"), entry.Get("path")
			if !entry.IsObject() || kind.Type != gjson.String || kind.String() != "thinking_dropped" || reason.Type != gjson.String || reason.String() == "" || path.Type != gjson.String || path.String() == "" {
				return true
			}
			if len(reason.String()) > 256 || len(path.String()) > 1024 {
				return true
			}
			// a hash permits deduplication without retaining opaque provider path strings
			identity, _ := json.Marshal([3]string{kind.String(), reason.String(), path.String()})
			hash := sha256.Sum256(identity)
			if r.transformationEntries == nil {
				r.transformationEntries = make(map[[32]byte]struct{})
			}
			if _, exists := r.transformationEntries[hash]; exists {
				return true
			}
			r.transformationEntries[hash] = struct{}{}
			switch reason.String() {
			case "prefix_binding_mismatch":
				r.transformations.PrefixBindingMismatch++
			case "model_binding_mismatch":
				r.transformations.ModelBindingMismatch++
			case "organization_binding_mismatch":
				r.transformations.OrganizationBindingMismatch++
			default:
				r.transformations.Unknown++
			}
			return true
		})
	}
}

// InputTransformationCounts returns safe provider-reported drop counts for this attempt
func (r *UsageReporter) InputTransformationCounts() usage.InputTransformationCounts {
	if r == nil {
		return usage.InputTransformationCounts{}
	}
	r.transformationsMu.Lock()
	defer r.transformationsMu.Unlock()
	return r.transformations
}
