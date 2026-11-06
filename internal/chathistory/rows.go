package chathistory

import (
	"time"
)

// clickhouseTime is the DateTime64(3) text form ClickHouse parses without settings
const clickhouseTime = "2006-01-02 15:04:05.000"

type blobRow struct {
	Hash      string `json:"hash"`
	Kind      string `json:"kind"`
	Format    string `json:"format"`
	Bytes     int    `json:"bytes"`
	Content   string `json:"content"`
	FirstSeen string `json:"first_seen"`
}

type nodeRow struct {
	SessionID string `json:"session_id"`
	Depth     uint32 `json:"depth"`
	Node      string `json:"node"`
	Parent    string `json:"parent"`
	Role      string `json:"role"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	TS        string `json:"ts"`
}

type requestRow struct {
	TS               string `json:"ts"`
	RequestID        string `json:"request_id"`
	Source           string `json:"source"`
	SessionID        string `json:"session_id"`
	ParentSessionID  string `json:"parent_session_id"`
	Format           string `json:"format"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	ResponseModel    string `json:"response_model"`
	AuthID           string `json:"auth_id"`
	ReasoningEffort  string `json:"reasoning_effort"`
	Stream           bool   `json:"stream"`
	Failed           bool   `json:"failed"`
	StatusCode       int    `json:"status_code"`
	Error            string `json:"error"`
	LatencyMS        int64  `json:"latency_ms"`
	TTFTMS           int64  `json:"ttft_ms"`
	InputTokens      int64  `json:"input_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	System           string `json:"system"`
	Tools            string `json:"tools"`
	Leaf             string `json:"leaf"`
	MessageCount     uint32 `json:"message_count"`
	Params           string `json:"params"`
	Response         string `json:"response"`
}

// rows turns one finished call into the rows it adds. Content and nodes already
// written by this process are skipped; anything repeated across restarts is
// collapsed by ClickHouse's ReplacingMergeTree
func (r *Recorder) rows(event Event) (map[string][]any, string) {
	requestedAt := event.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}

	ts := requestedAt.UTC().Format(clickhouseTime)
	response := assembleResponse(event.Chunks)
	conv, errSplit := splitConversation(event.RequestBody)
	request := newRequestRow(event, ts, response.format, response.body)
	if errSplit != nil {
		// keep a body this package cannot parse rather than lose the call
		request.Params = string(event.RequestBody)
		request.Error = joinError(request.Error, errSplit.Error())
		return map[string][]any{"requests": {request}}, response.responseID
	}

	request.Params = string(conv.params)
	if request.Format == "raw" {
		// a failed call has no response to read the format from
		request.Format = conv.format(event.Provider)
	}

	// an incremental Responses request continues the chain of the response it names
	parent, depth := "", uint32(0)
	if conv.previousResponseID != "" {
		if prior, ok := r.responses.get(conv.previousResponseID); ok {
			parent, depth = prior.node, prior.depth
		}
	}

	return r.conversationRows(request, conv, event.SessionID, event.RequestID, ts, parent, depth), response.responseID
}

// newRequestRow fills a request row's metadata from an event
func newRequestRow(event Event, ts, format string, response []byte) requestRow {
	source := event.Source
	if source == "" {
		source = "proxy"
	}

	return requestRow{
		TS:              ts,
		RequestID:       event.RequestID,
		Source:          source,
		SessionID:       event.SessionID,
		ParentSessionID: event.ParentSessionID,
		Format:          format,
		Provider:        event.Provider,
		Model:           event.Model,
		ResponseModel:   event.ResponseModel,
		AuthID:          event.AuthID,
		ReasoningEffort: event.ReasoningEffort,
		Stream:          event.Stream,
		Failed:          event.Failed,
		// ClickHouse rejects a whole segment over one out-of-range number, and
		// silently wraps others, so clamp to the column ranges here
		StatusCode:       min(max(event.StatusCode, 0), 65535),
		Error:            event.Error,
		LatencyMS:        clampUInt32(event.Latency.Milliseconds()),
		TTFTMS:           clampUInt32(event.TTFT.Milliseconds()),
		InputTokens:      max(event.InputTokens, 0),
		CacheReadTokens:  max(event.CacheReadTokens, 0),
		CacheWriteTokens: max(event.CacheWriteTokens, 0),
		OutputTokens:     max(event.OutputTokens, 0),
		ReasoningTokens:  max(event.ReasoningTokens, 0),
		Response:         string(response),
	}
}

// conversationRows adds the blobs and nodes of a split request, starting its chain
// at parent, and returns them with the request row
func (r *Recorder) conversationRows(request requestRow, conv conversation, sessionID, requestID, ts, parent string, depth uint32) map[string][]any {
	out := map[string][]any{}
	format := request.Format
	// stored gives the content to write, called only for a blob not yet written, since a
	// message's images are re-encoded there
	addBlob := func(kind string, content []byte, hash string, stored func() []byte) string {
		// an empty tool list or prompt is the same as none, so it gets no blob
		switch string(content) {
		case "", "null", "[]", "{}", `""`:
			return ""
		}

		if !r.blobs.has(hash) && !pendingBlob(out, hash) {
			kept := stored()
			out["blobs"] = append(out["blobs"], blobRow{
				Hash: hash, Kind: kind, Format: format, Bytes: len(kept), Content: string(kept), FirstSeen: ts,
			})
		}

		return hash
	}

	asIs := func(content []byte) func() []byte { return func() []byte { return content } }
	request.System = addBlob("system", conv.system, hashBytes(conv.system), asIs(conv.system))
	request.Tools = addBlob("tools", conv.tools, hashBytes(conv.tools), asIs(conv.tools))
	for _, msg := range conv.messages {
		addBlob("message", msg.content, msg.hash, msg.stored)
		node := nodeHash(parent, msg.hash)
		depth++
		if !r.nodes.has(sessionID + "|" + node) {
			out["nodes"] = append(out["nodes"], nodeRow{
				SessionID: sessionID, Depth: depth, Node: node, Parent: parent,
				Role: msg.role, Message: msg.hash, RequestID: requestID, TS: ts,
			})
		}

		parent = node
	}

	request.Leaf = parent
	request.MessageCount = depth
	out["requests"] = []any{request}
	return out
}

// pendingBlob reports whether this request already added the blob, for content
// that repeats inside one request such as identical tool results
func pendingBlob(out map[string][]any, hash string) bool {
	for _, row := range out["blobs"] {
		if row.(blobRow).Hash == hash {
			return true
		}
	}

	return false
}

func clampUInt32(value int64) int64 {
	return min(max(value, 0), 1<<32-1)
}

func joinError(existing, extra string) string {
	if existing == "" {
		return extra
	}

	return existing + "; " + extra
}

// seenSet remembers recent keys in two generations: when the current one fills,
// it becomes the previous one, so memory stays bounded and recent keys survive
type seenSet struct {
	limit    int
	current  map[string]struct{}
	previous map[string]struct{}
}

func newSeenSet(limit int) *seenSet {
	return &seenSet{limit: limit, current: map[string]struct{}{}, previous: map[string]struct{}{}}
}

func (s *seenSet) has(key string) bool {
	if _, ok := s.current[key]; ok {
		return true
	}

	_, ok := s.previous[key]
	return ok
}

func (s *seenSet) add(key string) {
	if len(s.current) >= s.limit {
		s.previous, s.current = s.current, map[string]struct{}{}
	}

	s.current[key] = struct{}{}
}

type leaf struct {
	node  string
	depth uint32
}

// leafMap remembers where recent responses left their conversation, in the same
// two-generation scheme as seenSet
type leafMap struct {
	limit    int
	current  map[string]leaf
	previous map[string]leaf
}

func newLeafMap(limit int) *leafMap {
	return &leafMap{limit: limit, current: map[string]leaf{}, previous: map[string]leaf{}}
}

func (m *leafMap) get(key string) (leaf, bool) {
	if value, ok := m.current[key]; ok {
		return value, true
	}

	value, ok := m.previous[key]
	return value, ok
}

func (m *leafMap) put(key string, value leaf) {
	if len(m.current) >= m.limit {
		m.previous, m.current = m.current, map[string]leaf{}
	}

	m.current[key] = value
}
