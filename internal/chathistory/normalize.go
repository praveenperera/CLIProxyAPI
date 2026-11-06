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
		conv.system = canonical(value)
	}

	if value, key := takeFirst(fields, toolKeys); key != "" {
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

// canonical encodes a decoded JSON value with sorted keys and without HTML escaping,
// after removing prompt cache markers. Clients move cache_control to the newest
// message on every turn, so keeping it would give the same message a new hash each
// time it is re-sent
func canonical(value any) []byte {
	stripCacheControl(value)

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return nil
	}

	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func stripCacheControl(value any) {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "cache_control")
		for _, child := range typed {
			stripCacheControl(child)
		}
	case []any:
		for _, child := range typed {
			stripCacheControl(child)
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
