package redisqueue

import coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"

// inputTransformationsPayload returns nil when no transformation was observed so the
// field is omitted from the queued payload
func inputTransformationsPayload(record coreusage.Record) *coreusage.InputTransformationCounts {
	if record.InputTransformations == (coreusage.InputTransformationCounts{}) {
		return nil
	}
	counts := record.InputTransformations
	return &counts
}
