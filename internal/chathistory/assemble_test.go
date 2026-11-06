package chathistory

import (
	"strings"
	"testing"
)

// Raw reads can split an event's JSON anywhere while every newline falls at the end
// of a read; joining them as lines would break the event and lose its text
func TestRawReadsSplitInsideAnEventKeepTheirText(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hel`),
		[]byte("lo\"}]}}]}\n\n"),
		[]byte(`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3}}` + "\n\n"),
	}

	got := assembleResponse(chunks)
	if !strings.Contains(string(got.body), `"hello"`) {
		t.Fatalf("assembled %s %s, want the text hello", got.format, got.body)
	}
}
