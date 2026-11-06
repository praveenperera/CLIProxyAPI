package chathistory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Message is one conversation entry prepared for import, in the same canonical
// form and with the same hash the proxy's capture gives it
type Message struct {
	m message
}

// NewMessage canonicalizes a decoded conversation entry. It removes the entry's
// prompt cache markers and opaque provider fields from value in place, so hash
// value with ContentHash first
func NewMessage(value any) *Message {
	stripMessageMarkers(value)
	dropOpaque(value)
	content := canonical(value)
	return &Message{m: message{role: roleOf(value), content: content, hash: hashBytes(content)}}
}

// Role is the entry's role, or its item type for OpenAI Responses items
func (m *Message) Role() string {
	return m.m.role
}

// Decode parses JSON the way capture does, keeping numbers exact
func Decode(raw []byte) (any, error) {
	var value any
	errDecode := decodeJSON(raw, &value)
	return value, errDecode
}

// ContentHash is the full hex SHA-256 of a conversation entry's canonical
// encoding: sorted keys, no HTML escaping and no prompt cache markers. Claude Code
// names its transcript blobs this way, so the importer can match a request's
// message references against messages it rebuilds from the transcript
func ContentHash(value any) string {
	stripMessageMarkers(value)
	sum := sha256.Sum256(canonical(value))
	return hex.EncodeToString(sum[:])
}

// Call is one model call rebuilt from a client's local transcript rather than
// captured by the proxy. The embedded Event carries its metadata; RequestBody and
// Chunks are ignored, since the call arrives already split
type Call struct {
	Event
	// Format names the wire format of the request and response, such as claude or
	// openai-response
	Format string
	// System, Tools and Response are decoded JSON values, nil when absent
	System   any
	Tools    any
	Response any
	// Params holds the request's other top-level fields; it may be nil
	Params map[string]any
	// Messages is the request's whole conversation, oldest first
	Messages []*Message
}

// Import writes one call's rows on the caller's goroutine, skipping content and
// nodes this recorder already wrote, like Record. It is for a recorder that only
// imports: its dedup state is not shared safely with Record's worker
func (r *Recorder) Import(call Call) error {
	r.resetIfRejected()

	params := call.Params
	if params == nil {
		params = map[string]any{}
	}

	system := moveBillingHeader(call.System, params)
	stripBlockMarkers(system)
	stripToolMarkers(call.Tools)
	conv := conversation{
		system:   canonicalOrNil(system),
		tools:    canonicalOrNil(call.Tools),
		messages: make([]message, len(call.Messages)),
	}

	for i, msg := range call.Messages {
		conv.messages[i] = msg.m
	}

	conv.params = canonical(params)

	requestedAt := call.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}

	ts := requestedAt.UTC().Format(clickhouseTime)
	var response []byte
	if call.Response != nil {
		response = responseBody(call.Response)
	}

	request := newRequestRow(call.Event, ts, call.Format, response)
	request.Params = string(conv.params)
	rows := r.conversationRows(request, conv, call.SessionID, call.RequestID, ts, "", 0)
	if errWrite := r.spool.write(rows); errWrite != nil {
		return errWrite
	}

	r.remember(rows, "")
	return nil
}

// Spooled reports the bytes waiting for upload, so an importer can wait for the
// uploader instead of filling the disk. The uploader refreshes it after each pass
func (r *Recorder) Spooled() int64 {
	r.spool.mu.Lock()
	defer r.spool.mu.Unlock()
	return r.spool.spooled
}

// WaitUploaded closes open segments and waits until the uploader has tried every
// segment or ctx ends. It fails when ClickHouse rejected any segment, which stays
// in the spool directory with a .rejected suffix and is not retried. Segments
// still waiting when ctx ends upload on the next start
func (r *Recorder) WaitUploaded(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		r.spool.rotate(true)
		if len(r.spool.closed()) == 0 {
			if rejected := r.rejected.Load(); rejected > 0 {
				return fmt.Errorf("chathistory: ClickHouse rejected %d segments", rejected)
			}

			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func canonicalOrNil(value any) []byte {
	if value == nil {
		return nil
	}

	return canonical(value)
}
