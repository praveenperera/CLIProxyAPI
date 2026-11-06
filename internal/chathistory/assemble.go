package chathistory

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// assembled is a model response rebuilt from raw upstream chunks
type assembled struct {
	format string
	body   []byte
	// responseID is the upstream response id, used to continue OpenAI Responses chains
	responseID string
}

// assembleResponse rebuilds one response body from the raw chunks an executor
// received. A non-streaming response arrives as one JSON chunk and is kept as is.
// Streams are folded into the shape the provider uses for a non-streaming
// response, so a stored response reads the same either way. The format is read
// from the events themselves, because one provider can serve several formats
func assembleResponse(chunks [][]byte) assembled {
	events := parseEvents(chunks)
	if len(events) == 0 {
		return assembled{format: "raw", body: joinChunks(chunks)}
	}

	if len(events) == 1 && !isStreamEvent(events[0]) {
		return assembled{format: detectFormat(events[0]), body: canonical(events[0]), responseID: stringAt(events[0], "id")}
	}

	switch detectFormat(events[0]) {
	case "claude":
		return assembleClaude(events)
	case "openai-response":
		return assembleResponses(events)
	case "gemini":
		return assembleGemini(events)
	case "openai":
		return assembleChat(events)
	}

	return assembled{format: "raw", body: joinChunks(chunks)}
}

// parseEvents decodes the response's JSON events. A body that is one JSON value,
// including pretty-printed Google responses and Gemini's JSON array streams, is
// decoded whole; otherwise the body is read as SSE or JSON lines, skipping event
// names, comments and the [DONE] marker
func parseEvents(chunks [][]byte) []map[string]any {
	stream := joinChunks(chunks)
	var whole any
	if trimmed := bytes.TrimSpace(stream); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && decodeWhole(trimmed, &whole) {
		var events []map[string]any
		switch value := whole.(type) {
		case map[string]any:
			events = append(events, unwrapResponse(value))
		case []any:
			for _, element := range value {
				if event, ok := element.(map[string]any); ok {
					events = append(events, unwrapResponse(event))
				}
			}
		}

		return events
	}

	var events []map[string]any
	for _, line := range bytes.Split(stream, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var event map[string]any
		if decodeJSON(line, &event) == nil && event != nil {
			events = append(events, unwrapResponse(event))
		}
	}

	return events
}

// decodeWhole decodes data only when it is exactly one JSON value, so JSON lines
// are not mistaken for a single event
func decodeWhole(data []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(target) != nil {
		return false
	}

	_, errNext := decoder.Token()
	return errors.Is(errNext, io.EOF)
}

// unwrapResponse removes the {"response": {...}} envelope Gemini CLI style
// upstreams put around each Gemini chunk
func unwrapResponse(event map[string]any) map[string]any {
	if inner, ok := event["response"].(map[string]any); ok && len(event) <= 3 {
		if _, hasCandidates := inner["candidates"]; hasCandidates {
			return inner
		}
	}

	return event
}

func detectFormat(event map[string]any) string {
	kind := stringAt(event, "type")
	switch {
	case strings.HasPrefix(kind, "message") || strings.HasPrefix(kind, "content_block") || kind == "ping":
		return "claude"
	case strings.HasPrefix(kind, "response"):
		return "openai-response"
	case event["candidates"] != nil || event["usageMetadata"] != nil:
		return "gemini"
	case event["choices"] != nil:
		return "openai"
	}

	return "raw"
}

func isStreamEvent(event map[string]any) bool {
	kind := stringAt(event, "type")
	if strings.HasPrefix(kind, "message_") || strings.HasPrefix(kind, "content_block") || strings.HasPrefix(kind, "response.") {
		return true
	}

	return stringAt(event, "object") == "chat.completion.chunk"
}

func assembleClaude(events []map[string]any) assembled {
	message := map[string]any{"type": "message", "role": "assistant"}
	blocks := map[int]map[string]any{}
	partialJSON := map[int]*strings.Builder{}

	for _, event := range events {
		switch stringAt(event, "type") {
		case "message_start":
			if start, ok := event["message"].(map[string]any); ok {
				for key, value := range start {
					if key != "content" {
						message[key] = value
					}
				}
			}
		case "content_block_start":
			index := intAt(event, "index")
			if block, ok := event["content_block"].(map[string]any); ok {
				blocks[index] = block
			}
		case "content_block_delta":
			index := intAt(event, "index")
			block := blocks[index]
			if block == nil {
				block = map[string]any{}
				blocks[index] = block
			}

			delta, _ := event["delta"].(map[string]any)
			switch stringAt(delta, "type") {
			case "text_delta":
				block["text"] = stringAt(block, "text") + stringAt(delta, "text")
			case "thinking_delta":
				block["thinking"] = stringAt(block, "thinking") + stringAt(delta, "thinking")
			case "signature_delta":
				block["signature"] = stringAt(block, "signature") + stringAt(delta, "signature")
			case "input_json_delta":
				if partialJSON[index] == nil {
					partialJSON[index] = &strings.Builder{}
				}

				partialJSON[index].WriteString(stringAt(delta, "partial_json"))
			case "citations_delta":
				citations, _ := block["citations"].([]any)
				block["citations"] = append(citations, delta["citation"])
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				for key, value := range delta {
					message[key] = value
				}
			}

			if usage, ok := event["usage"].(map[string]any); ok {
				merged, _ := message["usage"].(map[string]any)
				if merged == nil {
					merged = map[string]any{}
				}

				for key, value := range usage {
					merged[key] = value
				}

				message["usage"] = merged
			}
		case "error":
			message["error"] = event["error"]
		}
	}

	for index, builder := range partialJSON {
		var input any
		if decodeJSON([]byte(builder.String()), &input) != nil {
			// keep a truncated tool call readable rather than dropping it
			input = builder.String()
		}

		if blocks[index] != nil {
			blocks[index]["input"] = input
		}
	}

	message["content"] = orderedBlocks(blocks)
	return assembled{format: "claude", body: canonical(message), responseID: stringAt(message, "id")}
}

func assembleResponses(events []map[string]any) assembled {
	var response map[string]any
	items := map[int]any{}
	// items still streaming when the stream ended, rebuilt from their deltas
	added := map[int]map[string]any{}
	deltas := map[int]*strings.Builder{}
	for _, event := range events {
		switch stringAt(event, "type") {
		case "response.created", "response.in_progress":
			if response == nil {
				response, _ = event["response"].(map[string]any)
			}
		case "response.output_item.added":
			if item, ok := event["item"].(map[string]any); ok {
				added[intAt(event, "output_index")] = item
			}
		case "response.output_text.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta":
			index := intAt(event, "output_index")
			if deltas[index] == nil {
				deltas[index] = &strings.Builder{}
			}

			deltas[index].WriteString(stringAt(event, "delta"))
		case "response.output_item.done":
			items[intAt(event, "output_index")] = event["item"]
		case "response.completed", "response.incomplete", "response.failed", "response.done":
			if final, ok := event["response"].(map[string]any); ok {
				response = final
			}
		case "error":
			if response == nil {
				response = map[string]any{}
			}

			response["error"] = event["error"]
		}
	}

	if response == nil {
		response = map[string]any{}
	}

	for index, item := range added {
		if _, done := items[index]; done {
			continue
		}

		text := ""
		if deltas[index] != nil {
			text = deltas[index].String()
		}

		switch stringAt(item, "type") {
		case "message":
			item["content"] = []any{map[string]any{"type": "output_text", "text": text}}
		case "function_call":
			item["arguments"] = text
		case "reasoning":
			item["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
		}

		item["status"] = "incomplete"
		items[index] = item
	}

	// some upstreams send the final response without its output and rely on the
	// streamed items, and an interrupted stream has no final response at all, so
	// fill the output from the items
	if output, _ := response["output"].([]any); len(output) == 0 && len(items) > 0 {
		indexes := make([]int, 0, len(items))
		for index := range items {
			indexes = append(indexes, index)
		}

		sort.Ints(indexes)
		output = make([]any, 0, len(indexes))
		for _, index := range indexes {
			output = append(output, items[index])
		}

		response["output"] = output
	}

	// instructions and tools repeat the request, which is already stored
	delete(response, "instructions")
	delete(response, "tools")
	return assembled{format: "openai-response", body: canonical(response), responseID: stringAt(response, "id")}
}

func assembleGemini(events []map[string]any) assembled {
	result := map[string]any{}
	var parts []any
	var finish any
	for _, event := range events {
		for _, key := range []string{"usageMetadata", "modelVersion", "responseId"} {
			if value, ok := event[key]; ok {
				result[key] = value
			}
		}

		candidates, _ := event["candidates"].([]any)
		if len(candidates) == 0 {
			continue
		}

		candidate, _ := candidates[0].(map[string]any)
		if reason, ok := candidate["finishReason"]; ok {
			finish = reason
		}

		content, _ := candidate["content"].(map[string]any)
		chunkParts, _ := content["parts"].([]any)
		for _, part := range chunkParts {
			parts = appendGeminiPart(parts, part)
		}
	}

	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": parts}}
	if finish != nil {
		candidate["finishReason"] = finish
	}

	result["candidates"] = []any{candidate}
	return assembled{format: "gemini", body: canonical(result), responseID: stringAt(result, "responseId")}
}

// appendGeminiPart merges consecutive text parts of the same kind, since a stream
// splits one paragraph of text or thought across many chunks
func appendGeminiPart(parts []any, part any) []any {
	next, ok := part.(map[string]any)
	if !ok || len(parts) == 0 {
		return append(parts, part)
	}

	last, ok := parts[len(parts)-1].(map[string]any)
	nextText, nextIsText := next["text"].(string)
	lastText, lastIsText := last["text"].(string)
	if !ok || !nextIsText || !lastIsText || last["thought"] != next["thought"] || !onlyTextKeys(next) {
		return append(parts, part)
	}

	last["text"] = lastText + nextText
	if signature, ok := next["thoughtSignature"]; ok {
		last["thoughtSignature"] = signature
	}

	return parts
}

// onlyTextKeys reports whether a part is plain text or thought text, as opposed to
// a function call or inline data that happens to carry text
func onlyTextKeys(part map[string]any) bool {
	for key := range part {
		if key != "text" && key != "thought" && key != "thoughtSignature" {
			return false
		}
	}

	return true
}

func assembleChat(events []map[string]any) assembled {
	result := map[string]any{"object": "chat.completion"}
	message := map[string]any{"role": "assistant"}
	var content, reasoning strings.Builder
	type call struct {
		id, kind, name string
		arguments      strings.Builder
	}

	calls := map[int]*call{}
	var finish any
	for _, event := range events {
		for _, key := range []string{"id", "model", "created", "usage", "system_fingerprint"} {
			if value, ok := event[key]; ok && value != nil {
				result[key] = value
			}
		}

		choices, _ := event["choices"].([]any)
		if len(choices) == 0 {
			continue
		}

		choice, _ := choices[0].(map[string]any)
		if reason, ok := choice["finish_reason"]; ok && reason != nil {
			finish = reason
		}

		delta, _ := choice["delta"].(map[string]any)
		content.WriteString(stringAt(delta, "content"))
		// some providers send the same text under both names
		reasoning.WriteString(cmp.Or(stringAt(delta, "reasoning_content"), stringAt(delta, "reasoning")))
		toolCalls, _ := delta["tool_calls"].([]any)
		for _, raw := range toolCalls {
			fragment, _ := raw.(map[string]any)
			index := intAt(fragment, "index")
			entry := calls[index]
			if entry == nil {
				entry = &call{}
				calls[index] = entry
			}

			if id := stringAt(fragment, "id"); id != "" {
				entry.id = id
			}

			if kind := stringAt(fragment, "type"); kind != "" {
				entry.kind = kind
			}

			function, _ := fragment["function"].(map[string]any)
			if name := stringAt(function, "name"); name != "" {
				entry.name = name
			}

			entry.arguments.WriteString(stringAt(function, "arguments"))
		}
	}

	message["content"] = content.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}

	if len(calls) > 0 {
		indexes := make([]int, 0, len(calls))
		for index := range calls {
			indexes = append(indexes, index)
		}

		sort.Ints(indexes)
		list := make([]any, 0, len(indexes))
		for _, index := range indexes {
			entry := calls[index]
			list = append(list, map[string]any{
				"id":       entry.id,
				"type":     cmp.Or(entry.kind, "function"),
				"function": map[string]any{"name": entry.name, "arguments": entry.arguments.String()},
			})
		}

		message["tool_calls"] = list
	}

	result["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}
	return assembled{format: "openai", body: canonical(result), responseID: stringAt(result, "id")}
}

func orderedBlocks(blocks map[int]map[string]any) []any {
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)
	ordered := make([]any, 0, len(indexes))
	for _, index := range indexes {
		ordered = append(ordered, blocks[index])
	}

	return ordered
}

// joinChunks rebuilds the response stream. Executors pass either one line per
// chunk or raw reads that split lines anywhere; raw reads already hold their
// newlines and must be joined as is, or a line spanning two reads breaks
func joinChunks(chunks [][]byte) []byte {
	for _, chunk := range chunks {
		if bytes.IndexByte(bytes.TrimRight(chunk, "\r\n"), '\n') >= 0 {
			return bytes.Join(chunks, nil)
		}
	}

	return bytes.Join(chunks, []byte("\n"))
}

func stringAt(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

func intAt(fields map[string]any, key string) int {
	switch value := fields[key].(type) {
	case json.Number:
		n, _ := value.Int64()
		return int(n)
	case float64:
		return int(value)
	}

	return 0
}
