package transcript

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
)

// ClaudeCode rebuilds the model calls in one Claude Code transcript, a main session
// file or a subagent's file.
//
// Recent Claude Code versions record each API request as an api-request line: its
// params, a shape hash naming the system prompt and tools, and its messages as the
// first keep messages of a base request plus a tail of message hashes. A message is
// stored as an api-request-blob line unless the transcript's user and assistant
// lines already hold it, and one API message can merge several consecutive user
// lines, such as parallel tool results. Calls the file has no api-request line for,
// which older versions never wrote, are rebuilt by following each line's
// parentUuid chain; those carry no system prompt or tools
func ClaudeCode(path string) ([]chathistory.Call, Stats, error) {
	f := &ccFile{
		blobs:      map[string]json.RawMessage{},
		candidates: map[string]any{},
		shapes:     map[string]ccShape{},
		byID:       map[string]*ccRequest{},
		groups:     map[string]*ccGroup{},
		messages:   map[string]*chathistory.Message{},
	}

	errRead := eachLine(path, f.read)
	if errRead != nil {
		return nil, f.stats, errRead
	}

	return f.calls(), f.stats, nil
}

type ccLine struct {
	Type          string          `json:"type"`
	SessionID     string          `json:"sessionId"`
	AgentID       string          `json:"agentId"`
	UUID          string          `json:"uuid"`
	ParentUUID    string          `json:"parentUuid"`
	Timestamp     string          `json:"timestamp"`
	ID            string          `json:"id"`
	Base          string          `json:"base"`
	Keep          int             `json:"keep"`
	Tail          []string        `json:"tail"`
	ShapeHash     string          `json:"shapeHash"`
	Params        json.RawMessage `json:"params"`
	QuerySource   string          `json:"querySource"`
	Version       string          `json:"version"`
	RequestDigest string          `json:"requestDigest"`
	Hash          string          `json:"hash"`
	Message       json.RawMessage `json:"message"`
	Shape         json.RawMessage `json:"shape"`
	RequestRef    string          `json:"requestRef"`
	RequestID     string          `json:"requestId"`
	APIBlockIndex *int            `json:"apiBlockIndex"`
	APIError      bool            `json:"isApiErrorMessage"`
}

type ccShape struct {
	system any
	tools  any
}

type ccRequest struct {
	line     ccLine
	resolved bool
	messages []*chathistory.Message
	response *ccGroup
}

// ccGroup is one assistant message, which Claude Code writes as one line per
// content block
type ccGroup struct {
	id        string
	request   string
	sessionID string
	agentID   string
	timestamp string
	requestID string
	apiError  bool
	model     string
	stop      any
	stopSeq   any
	usage     any
	blocks    []any
	// indexes maps an apiBlockIndex to its position in blocks, so a block written
	// again replaces the earlier copy
	indexes map[int]int
	message *chathistory.Message
	// before is the conversation the message answered, set when the file has no
	// request record for it
	before *ccState
}

// ccState is the conversation as of one transcript line, for rebuilding calls from
// the parentUuid chain
type ccState struct {
	messages []*chathistory.Message
	// role and id name the last message, so a following line of the same run
	// merges into it
	role      string
	id        string
	runBlocks []any
}

type ccChainLine struct {
	uuid    string
	parent  string
	kind    string
	content any
	group   *ccGroup
}

type ccFile struct {
	stats Stats

	blobs map[string]json.RawMessage
	// candidates are messages rebuilt from user and assistant lines, keyed by
	// Claude Code's hash of them
	candidates map[string]any
	shapes     map[string]ccShape
	requests   []*ccRequest
	byID       map[string]*ccRequest
	groups     map[string]*ccGroup
	groupOrder []*ccGroup
	chain      []ccChainLine
	messages   map[string]*chathistory.Message

	// run holds the content blocks of the user lines since the last request or
	// assistant line, which Claude Code sends as one message
	run         []any
	lastRequest string
}

func (f *ccFile) read(raw []byte) {
	var line ccLine
	if json.Unmarshal(raw, &line) != nil {
		f.stats.BadLines++
		return
	}

	switch line.Type {
	case "api-request":
		request := &ccRequest{line: line}
		request.line.Params = rawObject(line.Params)
		f.requests = append(f.requests, request)
		f.byID[line.ID] = request
		f.lastRequest = line.ID
		f.run = nil
	case "api-request-blob":
		f.blobs[line.Hash] = rawObject(line.Message)
	case "api-request-shape":
		var shape struct {
			System json.RawMessage `json:"system"`
			Tools  json.RawMessage `json:"tools"`
		}

		if json.Unmarshal(rawObject(line.Shape), &shape) == nil {
			f.shapes[line.ShapeHash] = ccShape{system: decodeOrNil(shape.System), tools: decodeOrNil(shape.Tools)}
		}
	case "user":
		f.readUser(line)
	case "assistant":
		f.readAssistant(line)
	default:
		if line.UUID != "" {
			f.chain = append(f.chain, ccChainLine{uuid: line.UUID, parent: line.ParentUUID})
		}
	}
}

func (f *ccFile) readUser(line ccLine) {
	decoded, errDecode := chathistory.Decode(rawObject(line.Message))
	if errDecode != nil {
		f.stats.BadLines++
		return
	}

	content := field(decoded, "content")
	f.candidate(map[string]any{"role": "user", "content": content})
	if _, isText := content.(string); isText {
		f.candidate(map[string]any{"role": "user", "content": blocksOf(content)})
	}

	f.run = append(f.run, blocksOf(content)...)
	if len(f.run) > len(blocksOf(content)) {
		f.candidate(map[string]any{"role": "user", "content": slices.Clone(f.run)})
	}

	f.chain = append(f.chain, ccChainLine{uuid: line.UUID, parent: line.ParentUUID, kind: "user", content: content})
}

func (f *ccFile) readAssistant(line ccLine) {
	f.run = nil

	var msg struct {
		ID           string          `json:"id"`
		Model        string          `json:"model"`
		Content      json.RawMessage `json:"content"`
		StopReason   json.RawMessage `json:"stop_reason"`
		StopSequence json.RawMessage `json:"stop_sequence"`
		Usage        json.RawMessage `json:"usage"`
	}

	if json.Unmarshal(rawObject(line.Message), &msg) != nil {
		f.stats.BadLines++
		return
	}

	id := msg.ID
	if id == "" {
		id = line.UUID
	}

	group := f.groups[id]
	if group == nil {
		request := line.RequestRef
		if request == "" {
			request = f.lastRequest
		}

		group = &ccGroup{
			id: id, request: request, sessionID: line.SessionID, agentID: line.AgentID,
			timestamp: line.Timestamp, requestID: line.RequestID, indexes: map[int]int{},
		}

		f.groups[id] = group
		f.groupOrder = append(f.groupOrder, group)
	}

	group.apiError = group.apiError || line.APIError
	group.model = msg.Model
	group.stop = decodeOrNil(msg.StopReason)
	group.stopSeq = decodeOrNil(msg.StopSequence)
	if usage := decodeOrNil(msg.Usage); usage != nil {
		group.usage = usage
	}

	blocks, _ := decodeOrNil(msg.Content).([]any)
	for i, block := range blocks {
		if line.APIBlockIndex == nil {
			group.blocks = append(group.blocks, block)
			continue
		}

		index := *line.APIBlockIndex + i
		if position, ok := group.indexes[index]; ok {
			group.blocks[position] = block
			continue
		}

		group.indexes[index] = len(group.blocks)
		group.blocks = append(group.blocks, block)
	}

	f.chain = append(f.chain, ccChainLine{uuid: line.UUID, parent: line.ParentUUID, kind: "assistant", group: group})
}

func (f *ccFile) candidate(value map[string]any) {
	f.candidates[chathistory.ContentHash(value)] = value
}

func (f *ccFile) calls() []chathistory.Call {
	for _, group := range f.groupOrder {
		group.sortBlocks()
		value := map[string]any{"role": "assistant", "content": group.blocks}
		f.candidates[chathistory.ContentHash(value)] = value
		if request := f.byID[group.request]; request != nil {
			// a retried request keeps its last answer
			request.response = group
		}
	}

	calls := make([]chathistory.Call, 0, len(f.requests))
	for _, request := range f.requests {
		calls = append(calls, f.requestCall(request))
	}

	chained := f.unpaired()
	if len(chained) > 0 {
		f.walkChain()
		for _, group := range chained {
			if group.before != nil {
				calls = append(calls, f.chainCall(group))
			}
		}
	}

	f.stats.Calls = len(calls)
	return calls
}

// unpaired lists real model answers that no api-request line produced
func (f *ccFile) unpaired() []*ccGroup {
	var groups []*ccGroup
	for _, group := range f.groupOrder {
		// <synthetic> messages are written by Claude Code itself, not a model
		if group.model == "<synthetic>" && !group.apiError {
			continue
		}

		if f.byID[group.request] == nil {
			groups = append(groups, group)
		}
	}

	return groups
}

func (f *ccFile) requestCall(request *ccRequest) chathistory.Call {
	line := request.line
	params, _ := decodeOrNil(line.Params).(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	params["_claude_code"] = map[string]any{
		"request": line.ID, "version": line.Version, "query_source": line.QuerySource, "digest": line.RequestDigest,
	}

	shape := f.shapes[line.ShapeHash]
	session, parent := claudeSession(line.SessionID, line.AgentID)
	call := chathistory.Call{
		Event: chathistory.Event{
			RequestedAt: parseTime(line.Timestamp), RequestID: line.ID, SessionID: session, ParentSessionID: parent,
			Source: "claude-code", Provider: "claude", Model: text(params, "model"),
			ReasoningEffort: text(params, "output_config", "effort"), Stream: true,
		},
		Format:   "claude",
		System:   shape.system,
		Tools:    shape.tools,
		Params:   params,
		Messages: f.resolve(request),
	}

	if request.response == nil {
		call.Failed = true
		call.Error = "no response in the transcript"
		return call
	}

	request.response.fill(&call)
	return call
}

func (f *ccFile) chainCall(group *ccGroup) chathistory.Call {
	session, parent := claudeSession(group.sessionID, group.agentID)
	requestID := group.requestID
	if requestID == "" {
		requestID = group.id
	}

	call := chathistory.Call{
		Event: chathistory.Event{
			RequestedAt: parseTime(group.timestamp), RequestID: requestID, SessionID: session, ParentSessionID: parent,
			Source: "claude-code", Provider: "claude", Model: group.model, Stream: true,
		},
		Format:   "claude",
		Params:   map[string]any{"model": group.model, "_claude_code": map[string]any{"rebuilt_from": "message chain"}},
		Messages: group.before.messages,
	}

	group.fill(&call)
	f.stats.Chained++
	return call
}

// fill adds the assistant message as the call's response
func (g *ccGroup) fill(call *chathistory.Call) {
	call.ResponseModel = g.model
	call.Response = map[string]any{
		"id": g.id, "type": "message", "role": "assistant", "model": g.model,
		"content": g.blocks, "stop_reason": g.stop, "stop_sequence": g.stopSeq, "usage": g.usage,
	}

	call.InputTokens = number(field(g.usage, "input_tokens"))
	call.CacheReadTokens = number(field(g.usage, "cache_read_input_tokens"))
	call.CacheWriteTokens = number(field(g.usage, "cache_creation_input_tokens"))
	call.OutputTokens = number(field(g.usage, "output_tokens"))
	call.ReasoningTokens = number(field(g.usage, "output_tokens_details", "thinking_tokens"))
	if g.apiError {
		call.Failed = true
		for _, block := range g.blocks {
			call.Error = joinText(call.Error, text(block, "text"))
		}
	}
}

func (g *ccGroup) sortBlocks() {
	if len(g.indexes) != len(g.blocks) {
		// some lines had no block index, so file order is all there is
		return
	}

	order := make([]int, 0, len(g.indexes))
	for index := range g.indexes {
		order = append(order, index)
	}

	sort.Ints(order)
	sorted := make([]any, len(order))
	for i, index := range order {
		sorted[i] = g.blocks[g.indexes[index]]
		g.indexes[index] = i
	}

	g.blocks = sorted
}

// resolve rebuilds a request's messages from its base and tail
func (f *ccFile) resolve(request *ccRequest) []*chathistory.Message {
	if request.resolved {
		return request.messages
	}

	// set first, so a base cycle in a corrupt file ends instead of recursing
	request.resolved = true

	var prefix []*chathistory.Message
	if request.line.Base != "" {
		if base := f.byID[request.line.Base]; base != nil {
			prefix = f.resolve(base)
		}
	}

	keep := min(max(request.line.Keep, 0), len(prefix))
	if missing := request.line.Keep - keep; missing > 0 {
		f.stats.Unresolved += missing
	}

	messages := make([]*chathistory.Message, 0, keep+len(request.line.Tail))
	messages = append(messages, prefix[:keep]...)
	for _, hash := range request.line.Tail {
		messages = append(messages, f.message(hash))
	}

	request.messages = messages
	return messages
}

func (f *ccFile) message(hash string) *chathistory.Message {
	if msg, ok := f.messages[hash]; ok {
		return msg
	}

	var value any
	if raw, ok := f.blobs[hash]; ok {
		value, _ = chathistory.Decode(raw)
	}

	if value == nil {
		value = f.candidates[hash]
	}

	if value == nil {
		f.stats.Unresolved++
		value = map[string]any{"role": "unknown", "content": []any{}, "_missing_claude_code_hash": hash}
	}

	msg := chathistory.NewMessage(value)
	f.messages[hash] = msg
	return msg
}

// walkChain rebuilds the conversation at every line from its parentUuid, merging
// consecutive user lines and the lines of one assistant message as the API saw them
func (f *ccFile) walkChain() {
	states := map[string]*ccState{}
	empty := &ccState{}
	for _, line := range f.chain {
		base := states[line.parent]
		if base == nil {
			base = empty
		}

		state := base
		switch line.kind {
		case "user":
			if base.role == "user" {
				blocks := append(slices.Clone(base.runBlocks), blocksOf(line.content)...)
				state = base.replaceLast("user", "", blocks, map[string]any{"role": "user", "content": blocks})
			} else {
				state = base.push("user", "", blocksOf(line.content), map[string]any{"role": "user", "content": line.content})
			}
		case "assistant":
			group := line.group
			if base.role == "assistant" && base.id == group.id {
				break
			}

			if group.before == nil {
				group.before = base
			}

			if group.message == nil {
				group.message = chathistory.NewMessage(map[string]any{"role": "assistant", "content": group.blocks})
			}

			state = &ccState{messages: append(slices.Clip(base.messages), group.message), role: "assistant", id: group.id}
		}

		if line.uuid != "" {
			states[line.uuid] = state
		}
	}
}

func (s *ccState) push(role, id string, runBlocks []any, value any) *ccState {
	return &ccState{
		messages: append(slices.Clip(s.messages), chathistory.NewMessage(value)),
		role:     role, id: id, runBlocks: runBlocks,
	}
}

func (s *ccState) replaceLast(role, id string, runBlocks []any, value any) *ccState {
	kept := s.messages[:len(s.messages)-1]
	return &ccState{
		messages: append(slices.Clip(kept), chathistory.NewMessage(value)),
		role:     role, id: id, runBlocks: runBlocks,
	}
}

// claudeSession names a transcript's session; a subagent's file is its own session
// under the main one
func claudeSession(sessionID, agentID string) (string, string) {
	if agentID == "" {
		return "claude-code:" + sessionID, ""
	}

	return "claude-code:" + sessionID + "/agent-" + agentID, "claude-code:" + sessionID
}

// blocksOf turns message content into content blocks; plain text is one text block
func blocksOf(content any) []any {
	switch typed := content.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		return typed
	}

	return nil
}

func decodeOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	value, errDecode := chathistory.Decode(raw)
	if errDecode != nil {
		return nil
	}

	return value
}

func joinText(existing, extra string) string {
	switch {
	case extra == "":
		return existing
	case existing == "":
		return extra
	}

	return existing + "\n" + extra
}
