package helps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestInputTransformationsSafeCountsStartFinal(t *testing.T) {
	reporter := &UsageReporter{}
	list := `[{"type":"thinking_dropped","reason":"prefix_binding_mismatch","path":"messages.1.content.0"},{"type":"thinking_dropped","reason":"model_binding_mismatch","path":"messages.1.content.1"},{"type":"thinking_dropped","reason":"organization_binding_mismatch","path":"messages.1.content.2"},{"type":"thinking_dropped","reason":"secret-future-reason","path":"secret-account-path"},{"type":"thinking_mismatch_allowed","reason":"prefix_binding_mismatch","path":"messages.1.content.3"},null,7,{}, {"type":"thinking_dropped","reason":7,"path":"bad"},{"type":"future_type","reason":"prefix_binding_mismatch","path":"future"}]`
	reporter.ObserveInputTransformations([]byte(`data: {"type":"message_start","message":{"input_transformations":` + list + `}}`))
	reporter.ObserveInputTransformations([]byte(`data: {"type":"message_delta","input_transformations":` + list + `}`))
	reporter.ObserveInputTransformations([]byte(`{"type":"message_delta","delta":{"input_transformations":[{"path":"messages.1.content.0","reason":"prefix_binding_mismatch","type":"thinking_dropped"}]}}`))
	for _, bad := range []string{`broken`, `{"input_transformations":null}`, `{"input_transformations":{}}`, `{"input_transformations":[{"type":"thinking_dropped","reason":"model_binding_mismatch"}]}`} {
		reporter.ObserveInputTransformations([]byte(bad))
	}
	want := usage.InputTransformationCounts{PrefixBindingMismatch: 1, ModelBindingMismatch: 1, OrganizationBindingMismatch: 1, Unknown: 1}
	got := reporter.buildRecord(usage.Detail{}, false).InputTransformations
	if got != want {
		t.Fatalf("counts=%+v want=%+v", got, want)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "messages") {
		t.Fatalf("unsafe telemetry: %s", encoded)
	}
	// new final entries are not lost when the start list also appears again
	reporter.ObserveInputTransformations([]byte(`{"input_transformations":[{"type":"thinking_dropped","reason":"prefix_binding_mismatch","path":"messages.4.content.0"}]}`))
	if reporter.InputTransformationCounts().PrefixBindingMismatch != 2 {
		t.Fatal("new final entry lost")
	}
}

func TestInputTransformationsBounded(t *testing.T) {
	reporter := &UsageReporter{}
	for i := 0; i < maxInputTransformationObservations+10; i++ {
		reporter.ObserveInputTransformations([]byte(fmt.Sprintf(`{"input_transformations":[{"type":"thinking_dropped","reason":"future","path":"messages.%d"}]}`, i)))
	}
	if len(reporter.transformationEntries) != maxInputTransformationObservations || reporter.InputTransformationCounts().Unknown != maxInputTransformationObservations {
		t.Fatal("counts not bounded")
	}
}
