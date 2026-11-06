package chathistory

import (
	"strings"
	"testing"
)

// Cache markers go, but a key of the same name in a tool's input or schema is
// data: two calls that differ only there must hash differently
func TestCacheMarkersStripOnlyWhereTheAPIDefinesThem(t *testing.T) {
	body := func(flag string) []byte {
		return []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
			"tools":[{"name":"t","cache_control":{"type":"ephemeral"},"input_schema":{"properties":{"cache_control":{"type":"boolean"}}}}],
			"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"u","name":"t","input":{"cache_control":` + flag + `},"cache_control":{"type":"ephemeral"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]}]}]}`)
	}

	on, errOn := splitConversation(body("true"))
	off, errOff := splitConversation(body("false"))
	if errOn != nil || errOff != nil {
		t.Fatal(errOn, errOff)
	}

	if on.messages[0].hash == off.messages[0].hash {
		t.Fatal("tool inputs that differ only in a cache_control key hashed the same")
	}

	if strings.Contains(string(on.system), "ephemeral") || strings.Contains(string(on.messages[1].content), "ephemeral") {
		t.Fatalf("markers left in system %s or tool result %s", on.system, on.messages[1].content)
	}

	if !strings.Contains(string(on.tools), `"properties":{"cache_control"`) || strings.Contains(string(on.tools), "ephemeral") {
		t.Fatalf("tools %s, want the schema property kept and the marker gone", on.tools)
	}
}
