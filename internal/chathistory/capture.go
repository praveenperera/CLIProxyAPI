package chathistory

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// maxCaptureBytes bounds the raw response kept for one call. Past it the response
// is recorded as truncated, so one runaway stream cannot hold unbounded memory
const maxCaptureBytes = 64 << 20

var active atomic.Pointer[Recorder]

// SetRecorder installs the process-wide recorder, or disables capture when nil, and
// returns the recorder it replaced so the caller can close it. A call that finishes
// after the swap goes to the recorder installed at that moment
func SetRecorder(r *Recorder) *Recorder {
	return active.Swap(r)
}

type captureKey struct{}

// capture collects one upstream attempt's requests and raw responses as the
// executor sees them.
//
// An HTTP attempt sends one request and reads one response, but an executor may
// retry inside the attempt, so a new HTTP request replaces the turn in progress.
// A websocket attempt can keep one context for a whole socket: duplex sessions
// write the next response.create, or a response.steer, while the current response
// is still streaming. Websocket requests therefore queue, and a turn takes the
// oldest queued request when its response starts. Finish hands off the current
// turn and leaves the queue for the turns after it
type capture struct {
	mu      sync.Mutex
	pending []pendingRequest
	// steerIDs holds every steer id upstream acknowledged on this socket
	steerIDs map[string]bool
	current  turn
}

// pendingRequest is a queued request frame. A steer learns its upstream id from
// the steer event that acknowledges it, so a later failure removes that steer and
// not another one still queued
type pendingRequest struct {
	body    []byte
	steer   bool
	steerID string
}

type turn struct {
	// started is set by the turn's first response chunk
	started bool
	// sawCreated marks that a websocket response.created frame belongs to this turn
	sawCreated bool
	request    []byte
	chunks     [][]byte
	bytes      int
	truncated  bool
	status     int
	err        string
}

// WithCapture gives an upstream attempt its own capture. It returns ctx unchanged
// when recording is off, so the hooks below cost one context lookup
func WithCapture(ctx context.Context) context.Context {
	if active.Load() == nil {
		return ctx
	}

	return context.WithValue(ctx, captureKey{}, &capture{})
}

func captureFrom(ctx context.Context) *capture {
	if ctx == nil {
		return nil
	}

	c, _ := ctx.Value(captureKey{}).(*capture)
	return c
}

// CaptureRequest records an HTTP request body sent upstream. A retry inside the
// same attempt replaces the earlier request and anything read for it
func CaptureRequest(ctx context.Context, body []byte) {
	c := captureFrom(ctx)
	if c == nil {
		return
	}

	// a websocket upgrade is logged as a bodyless HTTP request; it is not a model
	// request and must not replace the one already queued
	if len(body) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// copied because callers may reuse the buffer
	c.pending = []pendingRequest{{body: bytes.Clone(body)}}
	c.current = turn{}
}

// CaptureWebsocketRequest queues a websocket request frame for the turn whose
// response starts next
func CaptureWebsocketRequest(ctx context.Context, body []byte) {
	c := captureFrom(ctx)
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.pending = append(c.pending, pendingRequest{body: bytes.Clone(body), steer: isSteer(body)})
}

// CaptureChunk records one raw upstream HTTP response chunk
func CaptureChunk(ctx context.Context, chunk []byte) {
	if c := captureFrom(ctx); c != nil && len(chunk) > 0 {
		c.mu.Lock()
		c.appendChunk(chunk)
		c.mu.Unlock()
	}
}

// CaptureWebsocketChunk records one upstream websocket frame. A second
// response.created in one turn means the previous response ended without
// reaching Finish, so it starts a new turn rather than merging two responses
func CaptureWebsocketChunk(ctx context.Context, frame []byte) {
	c := captureFrom(ctx)
	if c == nil || len(frame) == 0 {
		return
	}

	created := isResponseCreated(frame)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.trackSteer(frame)

	if created && c.current.sawCreated {
		c.current = turn{}
	}

	c.appendChunk(frame)
	if created {
		c.current.sawCreated = true
		// a frame that arrived between turns may have started this turn before
		// its request was queued
		if c.current.request == nil {
			c.current.request = c.popPending()
		}
	}
}

// appendChunk adds a chunk to the current turn; the caller holds mu
func (c *capture) appendChunk(chunk []byte) {
	if !c.current.started {
		c.current.started = true
		c.current.request = c.popPending()
	}

	if c.current.truncated {
		return
	}

	// keep a clean prefix: the chunk that crosses the limit keeps what fits, and
	// later ones are skipped, since they would leave a gap
	if room := maxCaptureBytes - c.current.bytes; len(chunk) > room {
		c.current.truncated = true
		chunk = chunk[:room]
		if len(chunk) == 0 {
			return
		}
	}

	c.current.chunks = append(c.current.chunks, bytes.Clone(chunk))
	c.current.bytes += len(chunk)
}

// popPending takes the oldest queued request; the caller holds mu
func (c *capture) popPending() []byte {
	if len(c.pending) == 0 {
		return nil
	}

	request := c.pending[0].body
	c.pending[0] = pendingRequest{}
	c.pending = c.pending[1:]
	return request
}

// trackSteer follows the upstream's steer events; the caller holds mu.
// Upstream acknowledges steers in the order they were sent, so an acknowledgement
// names the oldest steer without an id yet. A refused steer never gets a response
// of its own, so it leaves the queue; left there, it would pair every later
// response with the request before it
func (c *capture) trackSteer(frame []byte) {
	if !bytes.Contains(frame[:min(len(frame), 256)], []byte(`"response.steer.`)) {
		return
	}

	kind := ojson.GetBytes(frame, "type").String()
	if !strings.HasPrefix(kind, "response.steer.") {
		return
	}

	id := ojson.GetBytes(frame, "steer.id").String()
	index := c.steerIndex(id)
	if index < 0 {
		return
	}

	if kind == "response.steer.failed" {
		c.pending = append(c.pending[:index], c.pending[index+1:]...)
		return
	}

	c.pending[index].steerID = id
	if c.steerIDs == nil {
		c.steerIDs = map[string]bool{}
	}

	c.steerIDs[id] = true
}

// steerIndex finds the queued steer with this id, or else the oldest steer not yet
// acknowledged. An id seen before whose steer already left the queue, as when its
// successor response started, matches nothing; the caller holds mu
func (c *capture) steerIndex(id string) int {
	if id != "" {
		for i, request := range c.pending {
			if request.steer && request.steerID == id {
				return i
			}
		}

		if c.steerIDs[id] {
			return -1
		}
	}

	for i, request := range c.pending {
		if request.steer && request.steerID == "" {
			return i
		}
	}

	return -1
}

func isSteer(body []byte) bool {
	return ojson.GetBytes(body, "type").String() == "response.steer"
}

func isResponseCreated(frame []byte) bool {
	// websocket frames are small JSON objects that name their type near the start
	head := frame[:min(len(frame), 256)]
	return bytes.Contains(head, []byte(`"response.created"`))
}

// CaptureStatus records the upstream HTTP status
func CaptureStatus(ctx context.Context, status int) {
	if c := captureFrom(ctx); c != nil {
		c.mu.Lock()
		c.current.status = status
		c.mu.Unlock()
	}
}

// CaptureError records an upstream error that has no response body
func CaptureError(ctx context.Context, err error) {
	if c := captureFrom(ctx); c != nil && err != nil {
		c.mu.Lock()
		c.current.err = err.Error()
		c.mu.Unlock()
	}
}

// Finish pairs the attempt's usage record with its captured request and response
// and queues them. It runs on the executor's goroutine, so it only hands off
// buffers; parsing happens on the recorder's worker
func Finish(ctx context.Context, record usage.Record) {
	recorder := active.Load()
	c := captureFrom(ctx)
	if recorder == nil || c == nil {
		return
	}

	c.mu.Lock()
	done := c.current
	c.current = turn{}
	if done.request == nil {
		// a call that failed before any response chunk still sent its request
		done.request = c.popPending()
	}
	c.mu.Unlock()

	if len(done.request) == 0 {
		return
	}

	status, errText := done.status, done.err
	if record.Fail.StatusCode != 0 {
		status = record.Fail.StatusCode
	}

	if errText == "" {
		errText = record.Fail.Body
	}

	if done.truncated {
		errText = joinError(errText, "response truncated at 64 MiB")
	}

	detail := record.Detail
	recorder.Record(Event{
		RequestedAt:      record.RequestedAt,
		RequestID:        record.RequestID,
		SessionID:        record.SessionID,
		ParentSessionID:  record.ParentSessionID,
		Provider:         record.Provider,
		Model:            record.Model,
		ResponseModel:    record.ResponseModel,
		AuthID:           record.AuthID,
		ReasoningEffort:  record.ReasoningEffort,
		Stream:           record.Stream,
		Failed:           record.Failed,
		StatusCode:       status,
		Error:            errText,
		Latency:          record.Latency,
		TTFT:             record.TTFT,
		InputTokens:      detail.InputTokens,
		CacheReadTokens:  max(detail.CacheReadTokens, detail.CachedTokens),
		CacheWriteTokens: detail.CacheCreationTokens,
		OutputTokens:     detail.OutputTokens,
		ReasoningTokens:  detail.ReasoningTokens,
		RequestBody:      done.request,
		Chunks:           done.chunks,
	})
}
