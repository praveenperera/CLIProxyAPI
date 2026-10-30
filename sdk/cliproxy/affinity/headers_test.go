package affinity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestLocalHTTPAffinityIdentity(t *testing.T) {
	caller, _ := AuthenticatedCaller("local", "fake-client-key")
	other, _ := AuthenticatedCaller("different", "fake-client-key")
	if caller == other {
		t.Fatal("access providers share caller scope")
	}
	headers := http.Header{}
	headers.Set(ChatIDHeader, "chat")
	headers.Set(ChatIntentHeader, "start")
	start, err := ParseHeaders("namespace", caller, headers)
	if err != nil || start.Intent() != StartIntent {
		t.Fatalf("start parse %v", err)
	}
	handle, _ := NewHandle()
	headers.Set(ChatHandleHeader, handle.Encode())
	headers.Del(ChatIntentHeader)
	next, err := ParseHeaders("namespace", caller, headers)
	if err != nil || next.Intent() != ContinueIntent || next.Key() != start.Key() {
		t.Fatalf("continue parse %v", err)
	}
	isolated, err := ParseHeaders("namespace", other, headers)
	if err != nil || isolated.Key() == next.Key() {
		t.Fatal("caller collision")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if text := fmt.Sprintf(format, next); strings.Contains(text, handle.Encode()) || strings.Contains(text, "value:") {
			t.Fatal("handle printed")
		}
	}
	for _, mutation := range []func(http.Header){
		func(h http.Header) { h.Del(ChatIDHeader) },
		func(h http.Header) { h[ChatIDHeader] = []string{"chat", "other"} },
		func(h http.Header) { h["x-cliproxy-chat-id"] = []string{"other"} },
		func(h http.Header) { h.Set(ChatIDHeader, " chat") },
		func(h http.Header) { h.Del(ChatHandleHeader) },
		func(h http.Header) { h.Set(ChatIntentHeader, "start") },
		func(h http.Header) { h.Set("X-CLIProxy-Chat-Caller", "fake-claimed-caller") },
		func(h http.Header) { h.Set(ChatIntentHeader, "invalid") },
	} {
		copy := headers.Clone()
		mutation(copy)
		if _, err := ParseHeaders("namespace", caller, copy); err == nil {
			t.Fatal("conflicting or missing identity accepted")
		}
	}
	if _, err := ParseHeaders("namespace", CallerScope{}, headers); err == nil {
		t.Fatal("unauthenticated accepted")
	}
	if _, err := ParseHeaders("namespace", caller, nil); err == nil {
		t.Fatal("strict missing input accepted")
	}
	if _, err := ParseHeaders("", caller, headers); err == nil {
		t.Fatal("disabled profile accepted local input")
	}
	if r, err := ParseHeaders("", CallerScope{}, nil); err != nil || r.Intent() != Legacy {
		t.Fatal("legacy changed")
	}
	stripped := StripHeaders(headers)
	if len(stripped) != 0 || headers.Get(ChatIDHeader) == "" {
		t.Fatal("strip mutated input or leaked")
	}
}

func TestAcquiredOAuthProvenanceCannotAuthorizeReplacementOrSyntheticIdentity(t *testing.T) {
	provenance, err := AcquiredOAuthProvenance("codex", "real-account", "fake-acquired-token")
	if err != nil {
		t.Fatal(err)
	}
	if !provenance.Matches("codex", "real-account", "fake-acquired-token") || provenance.Matches("codex", "synthetic-account", "fake-acquired-token") || provenance.Matches("codex", "real-account", "fake-replacement-token") {
		t.Fatal("provenance accepted replacement")
	}
	data, err := json.Marshal(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fake-acquired-token") {
		t.Fatal("serialized token")
	}
	var decoded any
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := ParseOAuthProvenance(decoded)
	if err != nil || restored != provenance {
		t.Fatal("provenance not durable")
	}
	for _, data := range []string{`{"version":2}`, `{"version":1,"provider":"claude","principal":"account","token_digest":"bad"}`, `{"version":1,"provider":"codex","principal":"account","token_digest":"` + strings.Repeat("0", 64) + `"}`, `{"extra":true}`} {
		var value OAuthProvenance
		if err = json.Unmarshal([]byte(data), &value); err == nil {
			t.Fatal("invalid provenance accepted")
		}
	}
}
