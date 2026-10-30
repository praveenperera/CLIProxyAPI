package helps

import "testing"

func TestStreamUsageBufferObserveClaudeStream_KeepsOneHourCacheWrites(t *testing.T) {
	var buffer StreamUsageBuffer
	buffer.ObserveClaudeStream([]byte(`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-5-5","usage":{"input_tokens":20,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_5m_input_tokens":400,"ephemeral_1h_input_tokens":600},"output_tokens":1}}}`))
	buffer.ObserveClaudeStream([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":15}}`))
	detail, ok := buffer.Detail()
	if !ok || detail.CacheCreationTokens != 1000 || detail.CacheWrite1hTokens != 600 {
		t.Fatalf("detail = %+v", detail)
	}
}
