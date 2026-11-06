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

// Signatures and encrypted reasoning are dropped from messages and responses, so
// the same turn hashes alike with or without them, while thinking text and
// images stay
func TestOpaqueProviderFieldsAreNotStored(t *testing.T) {
	body := func(signature string) []byte {
		return []byte(`{"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"` + signature + `"},{"type":"redacted_thinking","data":"` + signature + `"}]},
			{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo"}}]},
			{"type":"reasoning","summary":[],"encrypted_content":"` + signature + `"}]}`)
	}

	first, errFirst := splitConversation(body("c2lnLW9uZQ"))
	second, errSecond := splitConversation(body("c2lnLXR3bw"))
	if errFirst != nil || errSecond != nil {
		t.Fatal(errFirst, errSecond)
	}

	for i := range first.messages {
		if first.messages[i].hash != second.messages[i].hash {
			t.Fatalf("message %d hashes differ by its signature: %s", i, first.messages[i].content)
		}
	}

	stored := string(first.messages[0].content) + string(first.messages[1].content) + string(first.messages[2].content)
	if strings.Contains(stored, "c2lnLW9uZQ") || !strings.Contains(stored, `"plan"`) || !strings.Contains(stored, "iVBORw0KGgo") {
		t.Fatalf("stored %s, want the thinking text and image kept and every signature gone", stored)
	}

	response := assembleResponse([][]byte{[]byte(`{"type":"message","id":"m1","content":[{"type":"thinking","thinking":"plan","signature":"c2lnLW9uZQ"}]}`)})
	if strings.Contains(string(response.body), "c2lnLW9uZQ") || !strings.Contains(string(response.body), `"plan"`) {
		t.Fatalf("response %s, want the signature gone", response.body)
	}
}
