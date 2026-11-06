package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
)

// Codex rebuilds the model calls in one Codex rollout file.
//
// A rollout lists the session's Responses API items in order: the inputs it sent
// and the outputs each call returned. Each call ends with a token_usage_record
// naming its response, or in older rollouts a token_count event, so a call's
// request is every item before its first output and its response is the outputs
// since the previous call. A compaction replaces the history with its summary
func Codex(path string) ([]chathistory.Call, Stats, error) {
	records, errScan := hasUsageRecords(path)
	if errScan != nil {
		return nil, Stats{}, errScan
	}

	r := &codexFile{useRecords: records, firstOutput: -1}
	if errRead := eachLine(path, r.read); errRead != nil {
		return nil, r.stats, errRead
	}

	r.stats.Calls = len(r.calls)
	return r.calls, r.stats, nil
}

// hasUsageRecords reports whether the rollout ends calls with token_usage_record
// lines; such rollouts also write token_count events, which would count each call
// twice
func hasUsageRecords(path string) (bool, error) {
	found := false
	errRead := eachLine(path, func(line []byte) {
		// the substring filters cheaply; decoding confirms it is the line's own type
		// rather than a nested object's
		if found || !bytes.Contains(line, []byte(`"token_usage_record"`)) {
			return
		}

		var head struct {
			Type string `json:"type"`
		}

		found = json.Unmarshal(line, &head) == nil && head.Type == "token_usage_record"
	})

	return found, errRead
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Ordinal   int64           `json:"ordinal"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexFile struct {
	stats      Stats
	useRecords bool
	calls      []chathistory.Call

	session      string
	parent       string
	instructions any
	tools        any
	meta         map[string]any
	model        string
	effort       string
	turnID       string
	cwd          string

	items []*chathistory.Message
	// outputs holds the current call's decoded outputs for its response
	outputs []any
	// firstOutput is where the current call's outputs start in items, or -1 before
	// the call has returned any
	firstOutput int
}

func (r *codexFile) read(raw []byte) {
	var line codexLine
	if json.Unmarshal(raw, &line) != nil {
		r.stats.BadLines++
		return
	}

	payload, errDecode := chathistory.Decode(line.Payload)
	if errDecode != nil {
		r.stats.BadLines++
		return
	}

	switch line.Type {
	case "session_meta":
		r.readMeta(payload)
	case "turn_context":
		r.model = text(payload, "model")
		r.effort = text(payload, "effort")
		r.turnID = text(payload, "turn_id")
		if cwd := text(payload, "cwd"); cwd != "" {
			r.cwd = cwd
		}
	case "response_item":
		r.readItem(payload)
	case "compacted":
		if history, ok := field(payload, "replacement_history").([]any); ok {
			r.items, r.outputs, r.firstOutput = nil, nil, -1
			for _, item := range history {
				r.appendItem(item)
			}
		}
	case "token_usage_record":
		if r.useRecords {
			r.finishCall(line, text(payload, "response_id"), field(payload, "usage"))
		}
	case "event_msg":
		if !r.useRecords && text(payload, "type") == "token_count" {
			r.finishCall(line, "", field(payload, "info", "last_token_usage"))
		}
	}
}

func (r *codexFile) readMeta(payload any) {
	id := text(payload, "session_id")
	if id == "" {
		id = text(payload, "id")
	}

	r.session = "codex:" + id
	if parent := text(payload, "source", "subagent", "thread_spawn", "parent_thread_id"); parent != "" {
		r.parent = "codex:" + parent
	}

	// base_instructions is {"text": ...} in current versions
	r.instructions = field(payload, "base_instructions")
	if instructions := text(payload, "base_instructions", "text"); instructions != "" {
		r.instructions = instructions
	}

	r.cwd = text(payload, "cwd")
	r.meta = map[string]any{
		"cli_version": text(payload, "cli_version"), "originator": text(payload, "originator"),
		"model_provider": text(payload, "model_provider"),
	}
}

func (r *codexFile) readItem(payload any) {
	if text(payload, "type") == "additional_tools" {
		r.tools = field(payload, "tools")
		return
	}

	r.appendItem(payload)
}

func (r *codexFile) appendItem(item any) {
	if r.firstOutput < 0 && isOutput(item) {
		r.firstOutput = len(r.items)
	}

	if r.firstOutput >= 0 {
		r.outputs = append(r.outputs, item)
	}

	r.items = append(r.items, chathistory.NewMessage(item))
}

// isOutput reports whether the model produced the item: reasoning, an assistant
// message, or a tool call. Tool outputs, user and developer messages and messages
// from other agents are inputs
func isOutput(item any) bool {
	kind := text(item, "type")
	switch {
	case kind == "message":
		return text(item, "role") == "assistant"
	case kind == "agent_message", kind == "configuration_update", strings.HasSuffix(kind, "_output"):
		return false
	}

	return true
}

func (r *codexFile) finishCall(line codexLine, responseID string, usage any) {
	end := r.firstOutput
	if end < 0 {
		end = len(r.items)
	}

	requestID := responseID
	if requestID == "" {
		requestID = fmt.Sprintf("%s:%d", r.session, line.Ordinal)
	}

	outputs := r.outputs
	if outputs == nil {
		outputs = []any{}
	}

	params := map[string]any{
		"model": r.model,
		"_codex": map[string]any{
			"turn_id": r.turnID, "cwd": r.cwd, "cli_version": r.meta["cli_version"],
			"originator": r.meta["originator"], "model_provider": r.meta["model_provider"],
		},
	}

	if r.effort != "" {
		params["reasoning"] = map[string]any{"effort": r.effort}
	}

	call := chathistory.Call{
		Event: chathistory.Event{
			RequestedAt: parseTime(line.Timestamp), RequestID: requestID, SessionID: r.session, ParentSessionID: r.parent,
			Source: "codex", Provider: "codex", Model: r.model, ResponseModel: r.model, ReasoningEffort: r.effort, Stream: true,
			InputTokens:      number(field(usage, "input_tokens")),
			CacheReadTokens:  number(field(usage, "cached_input_tokens")),
			CacheWriteTokens: number(field(usage, "cache_write_input_tokens")),
			OutputTokens:     number(field(usage, "output_tokens")),
			ReasoningTokens:  number(field(usage, "reasoning_output_tokens")),
		},
		Format: "openai-response",
		System: r.instructions,
		Tools:  r.tools,
		Params: params,
		// items only grow, and a compaction starts a new slice, so the prefix stays valid
		Messages: r.items[:end:end],
		Response: map[string]any{"id": responseID, "object": "response", "model": r.model, "output": outputs, "usage": usage},
	}

	r.calls = append(r.calls, call)
	r.firstOutput, r.outputs = -1, nil
}
