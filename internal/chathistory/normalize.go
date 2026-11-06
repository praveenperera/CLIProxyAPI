// Package chathistory records proxied model conversations for long-term storage in
// ClickHouse. Each distinct message, system prompt and tool list is stored once, and
// each conversation is a chain of nodes, so storage grows with new content rather
// than with the full history every request re-sends. See docs/chat-history.md
package chathistory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// conversation is a request body split into the parts that are stored separately
type conversation struct {
	// system is the canonical system prompt or instructions, nil when absent
	system []byte
	// tools is the canonical tool list, nil when absent
	tools []byte
	// messages holds each conversation entry in canonical form, oldest first
	messages []message
	// params holds every other top-level request field as canonical JSON
	params []byte
	// previousResponseID links an incremental OpenAI Responses request to the
	// response it continues, empty for requests that carry their full history
	previousResponseID string
	// key is the field that held the conversation
	key string
}

// format names the request's wire format from its layout, for calls whose
// response does not show it. Claude and Chat Completions both use "messages", so
// the provider decides between them
func (c conversation) format(provider string) string {
	switch {
	case c.key == "contents":
		return "gemini"
	case c.key == "input":
		return "openai-response"
	case c.key == "messages" && provider == "claude":
		return "claude"
	case c.key == "messages":
		return "openai"
	}

	return "raw"
}

type message struct {
	role    string
	content []byte
	hash    string
}

// keys tried in order for each part, covering Claude, OpenAI Chat Completions,
// OpenAI Responses (including Codex) and Gemini request bodies
var (
	conversationKeys = []string{"messages", "input", "contents"}
	systemKeys       = []string{"system", "instructions", "systemInstruction", "system_instruction"}
	toolKeys         = []string{"tools"}
)

var errNotJSONObject = errors.New("chathistory: request body is not a JSON object")

// splitConversation parses a request body into its stored parts. Gemini CLI style
// bodies that wrap the real request in a "request" field are unwrapped, and the
// wrapper's other fields are kept in params
func splitConversation(body []byte) (conversation, error) {
	var top map[string]any
	if errDecode := decodeJSON(body, &top); errDecode != nil {
		return conversation{}, errDecode
	}

	if top == nil {
		return conversation{}, errNotJSONObject
	}

	fields := top
	if inner, ok := top["request"].(map[string]any); ok && !hasAny(top, conversationKeys) {
		delete(top, "request")
		for key, value := range top {
			inner["_wrapper_"+key] = value
		}

		fields = inner
	}

	var conv conversation
	if value, key := takeFirst(fields, systemKeys); key != "" {
		value = moveBillingHeader(value, fields)
		stripBlockMarkers(value)
		conv.system = canonical(value)
	}

	if value, key := takeFirst(fields, toolKeys); key != "" {
		stripToolMarkers(value)
		conv.tools = canonical(value)
	}

	if id, ok := fields["previous_response_id"].(string); ok {
		conv.previousResponseID = id
	}

	if value, key := takeFirst(fields, conversationKeys); key != "" {
		conv.key = key
		conv.messages = splitMessages(value)
	}

	conv.params = canonical(fields)
	return conv, nil
}

// claudeBillingPrefix starts the system block Claude Code, and the proxy for Claude
// OAuth credentials, put first in the system prompt
const claudeBillingPrefix = "x-anthropic-billing-header:"

// moveBillingHeader moves a leading Claude billing header block out of the system
// prompt and into the request's params. The block carries a hash of the whole
// request body, so leaving it in would store the full system prompt again for
// every request
func moveBillingHeader(system any, fields map[string]any) any {
	blocks, ok := system.([]any)
	if !ok || len(blocks) == 0 {
		return system
	}

	first, ok := blocks[0].(map[string]any)
	if !ok {
		return system
	}

	if text, _ := first["text"].(string); !strings.HasPrefix(text, claudeBillingPrefix) {
		return system
	}

	fields["_billing_header"] = first
	return blocks[1:]
}

// splitMessages turns a conversation field into canonical messages. A bare string,
// which OpenAI Responses accepts as input, becomes one user message
func splitMessages(value any) []message {
	if text, ok := value.(string); ok {
		value = []any{map[string]any{"role": "user", "content": text}}
	}

	items, ok := value.([]any)
	if !ok {
		return nil
	}

	messages := make([]message, 0, len(items))
	for _, item := range items {
		stripMessageMarkers(item)
		dropOpaque(item)
		content := canonical(item)
		messages = append(messages, message{
			role:    roleOf(item),
			content: content,
			hash:    hashBytes(content),
		})
	}

	return messages
}

// roleOf names a conversation entry: its role for chat messages, or its item type
// for OpenAI Responses items such as function_call and reasoning
func roleOf(item any) string {
	fields, ok := item.(map[string]any)
	if !ok {
		return ""
	}

	if role, ok := fields["role"].(string); ok && role != "" {
		return role
	}

	if kind, ok := fields["type"].(string); ok {
		return kind
	}

	return ""
}

func takeFirst(fields map[string]any, keys []string) (any, string) {
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			delete(fields, key)
			return value, key
		}
	}

	return nil, ""
}

func hasAny(fields map[string]any, keys []string) bool {
	for _, key := range keys {
		if _, ok := fields[key]; ok {
			return true
		}
	}

	return false
}

func decodeJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	// keep integers such as token limits and ids exact
	decoder.UseNumber()
	return decoder.Decode(target)
}

// canonical encodes a decoded JSON value with sorted keys and without HTML escaping
func canonical(value any) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return nil
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Prompt cache markers are removed before hashing, because clients move
// cache_control to the newest message on every turn and the same message would
// otherwise hash differently each time it is re-sent. Only the places the Claude
// API defines markers are cleaned: a message, its content blocks, the blocks of a
// tool result, system blocks and tools. A key of the same name inside a tool's
// input or schema is the caller's data and stays

// dropOpaque removes, in place, the opaque base64 fields providers attach so a
// later call can be verified or resume hidden reasoning: Claude thinking
// signatures, redacted thinking, OpenAI's encrypted reasoning and Gemini thought
// signatures. Nobody but the provider can read them, and they were most of the
// stored bytes because random data does not compress. Images stay on purpose as
// real content; if storage grows too large they are the first thing to drop
// here, which changes their messages' hashes and so needs a fresh import. It runs after
// hashing for Claude Code's own references and before the stored hash, so the
// proxy and the importer store the same content under the same hash
func dropOpaque(value any) {
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			dropOpaque(item)
		}
	case map[string]any:
		switch value["type"] {
		case "thinking":
			delete(value, "signature")
		case "redacted_thinking":
			delete(value, "data")
		case "reasoning":
			delete(value, "encrypted_content")
		}

		delete(value, "thoughtSignature")
		for _, field := range value {
			dropOpaque(field)
		}
	}
}

// responseBody encodes a response for storage without its opaque fields
func responseBody(value any) []byte {
	dropOpaque(value)
	return canonical(value)
}

// stripMessageMarkers removes cache markers from one conversation entry in place
func stripMessageMarkers(item any) {
	fields, ok := item.(map[string]any)
	if !ok {
		return
	}

	delete(fields, "cache_control")
	stripBlockMarkers(fields["content"])
}

// stripBlockMarkers removes cache markers from a list of content blocks in place,
// including the blocks nested in a tool result
func stripBlockMarkers(value any) {
	blocks, ok := value.([]any)
	if !ok {
		return
	}

	for _, block := range blocks {
		fields, ok := block.(map[string]any)
		if !ok {
			continue
		}

		delete(fields, "cache_control")
		if fields["type"] == "tool_result" {
			stripBlockMarkers(fields["content"])
		}
	}
}

// stripToolMarkers removes the cache marker from each tool definition in place
func stripToolMarkers(value any) {
	tools, ok := value.([]any)
	if !ok {
		return
	}

	for _, tool := range tools {
		if fields, ok := tool.(map[string]any); ok {
			delete(fields, "cache_control")
		}
	}
}

// hashBytes is the stored content key: the first 16 bytes of SHA-256, hex encoded
func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:16])
}

// nodeHash keys a message by its position: equal conversation prefixes give equal
// nodes, and an edited or rewound conversation branches from the shared prefix
func nodeHash(parent, messageHash string) string {
	sum := sha256.Sum256([]byte(parent + ":" + messageHash))
	return hex.EncodeToString(sum[:16])
}
