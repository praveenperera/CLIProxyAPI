package test

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// fakeClickHouse keeps inserted rows by table and, like ClickHouse with
// non_replicated_deduplication_window, ignores a repeated deduplication token
type fakeClickHouse struct {
	mu     sync.Mutex
	rows   map[string][]map[string]any
	tokens map[string]bool
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if user, password, ok := r.BasicAuth(); !ok || user != "chat" || password != "secret" {
		http.Error(w, "auth", http.StatusUnauthorized)
		return
	}

	query := r.URL.Query().Get("query")
	var table string
	if _, errScan := fmt.Sscanf(query, "INSERT INTO chat.%s FORMAT JSONEachRow", &table); errScan != nil {
		http.Error(w, "bad query "+query, http.StatusBadRequest)
		return
	}

	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, errGzip := gzip.NewReader(r.Body)
		if errGzip != nil {
			http.Error(w, errGzip.Error(), http.StatusBadRequest)
			return
		}

		body = gz
	}

	var rows []map[string]any
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var row map[string]any
		if errRow := json.Unmarshal(scanner.Bytes(), &row); errRow != nil {
			http.Error(w, errRow.Error(), http.StatusBadRequest)
			return
		}

		rows = append(rows, row)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	token := r.URL.Query().Get("insert_deduplication_token")
	if token == "" || f.tokens[token] {
		return
	}

	f.tokens[token] = true
	f.rows[table] = append(f.rows[table], rows...)
}

const claudeChatHistoryStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-history-test","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"it"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello <b>"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" & bye"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}

event: message_stop
data: {"type":"message_stop"}

`

const codexChatHistoryStream = `data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-history-test","output":[]}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex says hi"}]}}

data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-history-test","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}

`

// TestChatHistoryStoresEachMessageOnce drives real executors through the auth
// manager, as the server does, and checks what reaches ClickHouse: a second turn
// adds only its new messages, moving cache_control does not duplicate a message,
// and streamed responses are assembled
func TestChatHistoryStoresEachMessageOnce(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "generateContent") {
			// Google APIs pretty-print non-streaming JSON
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\n  \"candidates\": [\n    {\n      \"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"gemini says hi\"}]},\n      \"finishReason\": \"STOP\"\n    }\n  ],\n  \"responseId\": \"gem_1\"\n}\n")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(r.URL.Path, "responses") {
			_, _ = io.WriteString(w, codexChatHistoryStream)
			return
		}

		_, _ = io.WriteString(w, claudeChatHistoryStream)
	}))
	defer upstream.Close()

	clickhouse := &fakeClickHouse{rows: map[string][]map[string]any{}, tokens: map[string]bool{}}
	clickhouseServer := httptest.NewServer(clickhouse)
	defer clickhouseServer.Close()

	recorder, errRecorder := chathistory.NewRecorder(chathistory.Options{
		SpoolDir: t.TempDir(), Endpoint: clickhouseServer.URL, User: "chat", Password: "secret",
	})
	if errRecorder != nil {
		t.Fatal(errRecorder)
	}

	chathistory.SetRecorder(recorder)
	t.Cleanup(func() { chathistory.SetRecorder(nil) })

	cfg := &config.Config{}
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewGeminiExecutor(cfg))
	for _, account := range []struct{ id, provider, model string }{
		{"history-claude", "claude", "claude-history-test"},
		{"history-codex", "codex", "gpt-history-test"},
		{"history-gemini", "gemini", "gemini-history-test"},
	} {
		registry.GetGlobalRegistry().RegisterClient(account.id, account.provider, []*registry.ModelInfo{{ID: account.model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(account.id) })
		if _, errRegister := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: account.id, Provider: account.provider, Status: cliproxyauth.StatusActive,
			Attributes: map[string]string{"base_url": upstream.URL, "api_key": "key-" + account.id},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	stream := func(session, provider, format, payload string) {
		t.Helper()
		ctx := logging.WithClientRequestMetadata(context.Background(), logging.ClientRequestMetadata{SessionID: session})
		var model struct {
			Model string `json:"model"`
		}

		_ = json.Unmarshal([]byte(payload), &model)
		result, errStream := manager.ExecuteStream(ctx, []string{provider}, cliproxyexecutor.Request{
			Model: model.Model, Payload: []byte(payload),
		}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString(format), OriginalRequest: []byte(payload)})
		if errStream != nil {
			t.Fatalf("%s stream: %v", provider, errStream)
		}

		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("%s chunk: %v", provider, chunk.Err)
			}
		}
	}

	// the billing header's cch changes with every request body, as Claude Code sends it
	system := func(cch string) string {
		return `"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220; cch=` + cch + `;"},{"type":"text","text":"You are terse."}],` +
			`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}]`
	}

	stream("sess-claude", "claude", "claude", `{"model":"claude-history-test","max_tokens":100,"stream":true,`+system("abcde")+`,
		"messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]}]}`)
	stream("sess-claude", "claude", "claude", `{"model":"claude-history-test","max_tokens":100,"stream":true,`+system("12345")+`,
		"messages":[{"role":"user","content":[{"type":"text","text":"first"}]},
		{"role":"assistant","content":[{"type":"text","text":"Hello"}]},
		{"role":"user","content":[{"type":"text","text":"second","cache_control":{"type":"ephemeral"}}]}]}`)
	stream("sess-codex", "codex", "openai-response", `{"model":"gpt-history-test","stream":true,"instructions":"be brief",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	geminiPayload := `{"contents":[{"role":"user","parts":[{"text":"hi gemini"}]}]}`
	if _, errExecute := manager.Execute(context.Background(), []string{"gemini"}, cliproxyexecutor.Request{
		Model: "gemini-history-test", Payload: []byte(geminiPayload),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, OriginalRequest: []byte(geminiPayload)}); errExecute != nil {
		t.Fatalf("gemini execute: %v", errExecute)
	}

	// Close drains the queue, closes segments and uploads them
	recorder.Close()

	clickhouse.mu.Lock()
	defer clickhouse.mu.Unlock()

	requests := clickhouse.rows["requests"]
	if len(requests) != 4 {
		t.Fatalf("requests = %d, want 4: %v", len(requests), requests)
	}

	first, second, codex := requests[0], requests[1], requests[2]
	// the conductor derives the session id itself; both Claude turns must share it
	session := first["session_id"]
	if session == "" || session != second["session_id"] || session == codex["session_id"] {
		t.Fatalf("session ids = %v, %v, %v", session, second["session_id"], codex["session_id"])
	}

	claudeNodes := map[float64]int{}
	for _, node := range clickhouse.rows["nodes"] {
		if node["session_id"] == session {
			claudeNodes[node["depth"].(float64)]++
		}
	}

	// depth 1 is shared by both turns and must be written once
	if len(claudeNodes) != 3 || claudeNodes[1] != 1 || claudeNodes[2] != 1 || claudeNodes[3] != 1 {
		t.Errorf("claude nodes by depth = %v, want one each at depths 1-3", claudeNodes)
	}

	kinds := map[string]int{}
	for _, blob := range clickhouse.rows["blobs"] {
		kinds[blob["kind"].(string)]++
	}

	// the Claude system prompt and tools are sent twice but stored once; the Codex
	// request adds its own instructions and the image tool its executor injects
	if kinds["message"] != 5 || kinds["system"] != 2 || kinds["tools"] != 2 {
		t.Errorf("blobs by kind = %v, want 5 messages (3 claude, 1 codex, 1 gemini), 2 system prompts and 2 tool lists", kinds)
	}

	if first["message_count"].(float64) != 1 || second["message_count"].(float64) != 3 {
		t.Errorf("claude request rows = %v / %v", first, second)
	}

	if first["system"] == "" || first["system"] != second["system"] || first["tools"] != second["tools"] {
		t.Errorf("system/tools hashes differ between turns: %v %v / %v %v", first["system"], first["tools"], second["system"], second["tools"])
	}

	var claudeResponse struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string         `json:"type"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			Input    map[string]any `json:"input"`
		} `json:"content"`
	}

	if errDecode := json.Unmarshal([]byte(first["response"].(string)), &claudeResponse); errDecode != nil {
		t.Fatalf("claude response: %v: %s", errDecode, first["response"])
	}

	content := claudeResponse.Content
	if first["format"] != "claude" || claudeResponse.StopReason != "tool_use" || len(content) != 3 ||
		content[0].Thinking != "plan it" || content[1].Text != "Hello <b> & bye" || content[2].Input["path"] != "a.go" {
		t.Errorf("claude response assembled wrong: format=%v %s", first["format"], first["response"])
	}

	if codex["format"] != "openai-response" || !strings.Contains(codex["response"].(string), "codex says hi") {
		t.Errorf("codex response assembled wrong: format=%v %s", codex["format"], codex["response"])
	}

	// the per-request billing hash is kept with the request, not in the shared prompt
	if !strings.Contains(first["params"].(string), "cch=abcde") || !strings.Contains(second["params"].(string), "cch=12345") {
		t.Errorf("billing header missing from params: %v / %v", first["params"], second["params"])
	}

	gemini := requests[3]
	if gemini["format"] != "gemini" || !strings.Contains(gemini["response"].(string), "gemini says hi") {
		t.Errorf("gemini response read wrong: format=%v %s", gemini["format"], gemini["response"])
	}
}
