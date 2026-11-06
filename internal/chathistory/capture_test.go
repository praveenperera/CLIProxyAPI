package chathistory

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// TestDuplexWebsocketTurnsKeepTheirRequests replays the order the duplex Codex
// websocket executor produces on one socket context: a steer written while the
// first response streams, and the next response.create written when the first
// completes but before its usage is published
func TestDuplexWebsocketTurnsKeepTheirRequests(t *testing.T) {
	recorder := &Recorder{queue: make(chan Event, 8)}
	SetRecorder(recorder)
	t.Cleanup(func() { SetRecorder(nil) })

	ctx := WithCapture(context.Background())
	frame := func(s string) { CaptureWebsocketChunk(ctx, []byte(s)) }

	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.create","n":1}`))
	frame(`{"type":"response.created","response":{"id":"r1"}}`)
	frame(`{"type":"response.output_text.delta","delta":"one"}`)
	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.steer","n":"steer"}`))
	frame(`{"type":"response.completed","response":{"id":"r1"}}`)
	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.create","n":3}`))
	Finish(ctx, usage.Record{RequestID: "r1"})

	frame(`{"type":"response.created","response":{"id":"r2"}}`)
	frame(`{"type":"response.completed","response":{"id":"r2"}}`)
	Finish(ctx, usage.Record{RequestID: "r2"})

	frame(`{"type":"response.created","response":{"id":"r3"}}`)
	frame(`{"type":"response.completed","response":{"id":"r3"}}`)
	Finish(ctx, usage.Record{RequestID: "r3"})

	want := []struct{ request, firstChunk string }{
		{`"n":1`, `"r1"`}, {`"n":"steer"`, `"r2"`}, {`"n":3`, `"r3"`},
	}
	for _, expected := range want {
		event := <-recorder.queue
		if !strings.Contains(string(event.RequestBody), expected.request) || len(event.Chunks) == 0 ||
			!strings.Contains(string(event.Chunks[0]), expected.firstChunk) || !strings.Contains(string(event.Chunks[len(event.Chunks)-1]), "completed") {
			t.Errorf("%s: request %s with %d chunks, want request %s and its own response", event.RequestID, event.RequestBody, len(event.Chunks), expected.request)
		}
	}
}

// A steer the upstream refuses gets no response, so the next response must still
// pair with the request written for it
func TestFailedSteerDoesNotShiftLaterTurns(t *testing.T) {
	recorder := &Recorder{queue: make(chan Event, 8)}
	SetRecorder(recorder)
	t.Cleanup(func() { SetRecorder(nil) })

	ctx := WithCapture(context.Background())
	frame := func(s string) { CaptureWebsocketChunk(ctx, []byte(s)) }

	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.create","n":1}`))
	frame(`{"type":"response.created","response":{"id":"r1"}}`)
	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.steer","n":"steer"}`))
	frame(`{"type":"response.steer.failed","steer":{"id":"s1"}}`)
	frame(`{"type":"response.completed","response":{"id":"r1"}}`)
	Finish(ctx, usage.Record{RequestID: "r1"})

	CaptureWebsocketRequest(ctx, []byte(`{"type":"response.create","n":3}`))
	frame(`{"type":"response.created","response":{"id":"r3"}}`)
	frame(`{"type":"response.completed","response":{"id":"r3"}}`)
	Finish(ctx, usage.Record{RequestID: "r3"})

	for _, want := range []string{`"n":1`, `"n":3`} {
		if event := <-recorder.queue; !strings.Contains(string(event.RequestBody), want) {
			t.Errorf("%s: request %s, want %s", event.RequestID, event.RequestBody, want)
		}
	}
}
