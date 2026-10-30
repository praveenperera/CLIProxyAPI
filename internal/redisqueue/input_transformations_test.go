package redisqueue

import (
	"context"
	"encoding/json"
	"testing"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageQueuePluginEmitsSafeTransformationCounts(t *testing.T) {
	withEnabledQueue(t, func() {
		counts := coreusage.InputTransformationCounts{PrefixBindingMismatch: 2, ModelBindingMismatch: 1, OrganizationBindingMismatch: 3, Unknown: 1}
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{Provider: "claude", InputTransformations: counts})
		payload := popSinglePayload(t)
		raw := payload["input_transformations"]
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		var got coreusage.InputTransformationCounts
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if got != counts {
			t.Fatalf("counts=%+v want=%+v", got, counts)
		}
		(&usageQueuePlugin{}).HandleUsage(context.Background(), coreusage.Record{Provider: "claude"})
		requireMissingField(t, popSinglePayload(t), "input_transformations")
	})
}
