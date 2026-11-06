package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
)

func writeLines(t *testing.T, name string, lines ...any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	var b strings.Builder
	for _, line := range lines {
		encoded, errMarshal := json.Marshal(line)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}

		b.Write(encoded)
		b.WriteByte('\n')
	}

	if errWrite := os.WriteFile(path, []byte(b.String()), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	return path
}

type object = map[string]any

func hashOf(t *testing.T, value any) string {
	t.Helper()
	encoded, _ := json.Marshal(value)
	decoded, errDecode := chathistory.Decode(encoded)
	if errDecode != nil {
		t.Fatal(errDecode)
	}

	return chathistory.ContentHash(decoded)
}

// spooled reads every row a dry-run recorder left in dir, by table
func spooled(t *testing.T, dir string) map[string][]object {
	t.Helper()
	rows := map[string][]object{}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	for _, path := range paths {
		table := strings.SplitN(filepath.Base(path), "-", 2)[0]
		file, errOpen := os.Open(path)
		if errOpen != nil {
			t.Fatal(errOpen)
		}

		scanner := bufio.NewScanner(file)
		scanner.Buffer(nil, 1<<24)
		for scanner.Scan() {
			var row object
			if errUnmarshal := json.Unmarshal(scanner.Bytes(), &row); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}

			rows[table] = append(rows[table], row)
		}

		_ = file.Close()
	}

	return rows
}

// A Claude Code transcript rebuilds each request from its base, its kept prefix
// and its tail: one message from a blob, one from the assistant's lines, and one
// merged from two parallel tool results, as the API received them
func TestClaudeCodeRebuildsRequestsAndStoresThemOnce(t *testing.T) {
	session := "s1"
	hello := object{"role": "user", "content": "hello"}
	answer := []any{
		object{"type": "text", "text": "running it"},
		object{"type": "tool_use", "id": "t1", "name": "Bash", "input": object{"command": "ls"}},
	}
	results := []any{
		object{"type": "tool_result", "tool_use_id": "t1", "content": "a.txt"},
		object{"type": "tool_result", "tool_use_id": "t2", "content": "b.txt"},
	}

	path := writeLines(t, session+".jsonl",
		object{"type": "api-request-shape", "sessionId": session, "shapeHash": "S", "shape": object{
			"system": []any{
				object{"type": "text", "text": "x-anthropic-billing-header: cc_version=2; cch=abcde;"},
				object{"type": "text", "text": "You are Claude Code."},
			},
			"tools": []any{object{"name": "Bash"}},
		}},
		object{"type": "api-request-blob", "sessionId": session, "hash": hashOf(t, hello), "message": hello},
		object{"type": "user", "sessionId": session, "uuid": "u1", "message": hello},
		object{"type": "api-request", "sessionId": session, "id": "r1", "timestamp": "2026-11-01T10:00:00Z", "shapeHash": "S",
			"params": object{"model": "claude-opus-5-5", "output_config": object{"effort": "high"}}, "keep": 0, "tail": []any{hashOf(t, hello)}},
		object{"type": "assistant", "sessionId": session, "uuid": "a1", "parentUuid": "u1", "requestRef": "r1", "apiBlockIndex": 1,
			"message": object{"id": "m1", "model": "claude-opus-5-5", "role": "assistant", "content": []any{answer[1]},
				"usage": object{"input_tokens": 10, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 2, "output_tokens": 7}}},
		// written out of order on purpose: the block index decides the order
		object{"type": "assistant", "sessionId": session, "uuid": "a0", "parentUuid": "u1", "requestRef": "r1", "apiBlockIndex": 0,
			"message": object{"id": "m1", "model": "claude-opus-5-5", "role": "assistant", "content": []any{answer[0]}}},
		object{"type": "user", "sessionId": session, "uuid": "u2", "parentUuid": "a1", "message": object{"role": "user", "content": []any{results[0]}}},
		object{"type": "user", "sessionId": session, "uuid": "u3", "parentUuid": "u2", "message": object{"role": "user", "content": []any{results[1]}}},
		object{"type": "api-request", "sessionId": session, "id": "r2", "timestamp": "2026-11-01T10:00:05Z", "shapeHash": "S",
			"params": object{"model": "claude-opus-5-5"}, "base": "r1", "keep": 1,
			"tail": []any{hashOf(t, object{"role": "assistant", "content": answer}), hashOf(t, object{"role": "user", "content": results}), "0000"}},
	)

	calls, stats, errParse := ClaudeCode(path)
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 2 || stats.Unresolved != 1 || stats.Chained != 0 {
		t.Fatalf("calls %d stats %+v, want 2 calls and one unresolved message", len(calls), stats)
	}

	first, second := calls[0], calls[1]
	if first.SessionID != "claude-code:s1" || first.Model != "claude-opus-5-5" || first.ReasoningEffort != "high" {
		t.Fatalf("first call metadata: %+v", first.Event)
	}

	if first.InputTokens != 10 || first.CacheReadTokens != 5 || first.CacheWriteTokens != 2 || first.OutputTokens != 7 || first.Failed {
		t.Fatalf("first call usage: %+v", first.Event)
	}

	roles := []string{}
	for _, msg := range second.Messages {
		roles = append(roles, msg.Role())
	}

	if strings.Join(roles, ",") != "user,assistant,user,unknown" || !second.Failed {
		t.Fatalf("second call roles %v failed %v", roles, second.Failed)
	}

	dir := t.TempDir()
	recorder, errRecorder := chathistory.NewRecorder(chathistory.Options{SpoolDir: dir})
	if errRecorder != nil {
		t.Fatal(errRecorder)
	}

	for _, call := range calls {
		if errImport := recorder.Import(call); errImport != nil {
			t.Fatal(errImport)
		}
	}

	recorder.Close()
	rows := spooled(t, dir)

	kinds := map[string]int{}
	for _, blob := range rows["blobs"] {
		kinds[blob["kind"].(string)]++
		if blob["kind"] == "system" && strings.Contains(blob["content"].(string), "billing-header") {
			t.Fatalf("system blob kept the billing header: %s", blob["content"])
		}
	}

	if kinds["system"] != 1 || kinds["tools"] != 1 || kinds["message"] != 4 {
		t.Fatalf("blob kinds %v, want one system, one tools and four messages", kinds)
	}

	if len(rows["nodes"]) != 4 || len(rows["requests"]) != 2 {
		t.Fatalf("%d nodes and %d requests, want 4 and 2", len(rows["nodes"]), len(rows["requests"]))
	}

	request := rows["requests"][0]
	if request["source"] != "claude-code" || !strings.Contains(request["params"].(string), "cch=abcde") {
		t.Fatalf("first request row: source %v params %v", request["source"], request["params"])
	}

	var response object
	_ = json.Unmarshal([]byte(request["response"].(string)), &response)
	content, _ := response["content"].([]any)
	if len(content) != 2 || content[0].(object)["type"] != "text" {
		t.Fatalf("response content %v, want text then tool_use", response["content"])
	}

	if rows["requests"][1]["message_count"].(float64) != 4 {
		t.Fatalf("second request message_count %v, want 4", rows["requests"][1]["message_count"])
	}
}

// A file without request records, as older Claude Code versions wrote, still gives
// one call per assistant message, rebuilt from the parentUuid chain
func TestClaudeCodeRebuildsOldTranscriptsFromTheChain(t *testing.T) {
	path := writeLines(t, "old.jsonl",
		object{"type": "user", "sessionId": "s2", "uuid": "u1", "message": object{"role": "user", "content": "first"}},
		object{"type": "assistant", "sessionId": "s2", "uuid": "a1", "parentUuid": "u1", "requestId": "req_1",
			"message": object{"id": "m1", "model": "claude-old", "content": []any{object{"type": "text", "text": "one"}}}},
		object{"type": "system", "sessionId": "s2", "uuid": "x1", "parentUuid": "a1"},
		object{"type": "user", "sessionId": "s2", "uuid": "u2", "parentUuid": "x1", "message": object{"role": "user", "content": "second"}},
		object{"type": "assistant", "sessionId": "s2", "uuid": "a2", "parentUuid": "u2", "requestId": "req_2",
			"message": object{"id": "m2", "model": "claude-old", "content": []any{object{"type": "text", "text": "two"}}}},
	)

	calls, stats, errParse := ClaudeCode(path)
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 2 || stats.Chained != 2 {
		t.Fatalf("calls %d stats %+v, want two chained calls", len(calls), stats)
	}

	if calls[1].RequestID != "req_2" || len(calls[1].Messages) != 3 || calls[1].Messages[1].Role() != "assistant" {
		t.Fatalf("second call: id %s, %d messages", calls[1].RequestID, len(calls[1].Messages))
	}
}

// A Codex rollout gives one call per usage record: the request is every item
// before the call's first output, and a compaction starts the history again
func TestCodexSplitsCallsAtUsageRecords(t *testing.T) {
	usage := func(input int) object {
		return object{"input_tokens": input, "cached_input_tokens": 4, "output_tokens": 3, "reasoning_output_tokens": 1}
	}

	item := func(payload object) object { return object{"type": "response_item", "payload": payload} }
	path := writeLines(t, "rollout-1.jsonl",
		object{"type": "session_meta", "payload": object{"id": "c1", "base_instructions": object{"text": "You are Codex."}, "cli_version": "0.1"}},
		object{"type": "turn_context", "payload": object{"model": "gpt-6", "effort": "high", "turn_id": "t1"}},
		item(object{"type": "additional_tools", "role": "developer", "tools": []any{object{"name": "exec"}}}),
		item(object{"type": "message", "role": "developer", "content": []any{object{"type": "input_text", "text": "rules"}}}),
		item(object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "list files"}}}),
		item(object{"type": "reasoning", "summary": []any{}}),
		item(object{"type": "function_call", "call_id": "f1", "name": "exec", "arguments": "{}"}),
		object{"type": "token_usage_record", "ordinal": 7, "timestamp": "2026-11-01T10:00:00Z", "payload": object{"response_id": "resp_1", "usage": usage(20)}},
		// the token_count event repeats the record, so it must not add a call
		object{"type": "event_msg", "payload": object{"type": "token_count", "info": object{"last_token_usage": usage(20)}}},
		item(object{"type": "function_call_output", "call_id": "f1", "output": "a.txt"}),
		item(object{"type": "message", "role": "assistant", "content": []any{object{"type": "output_text", "text": "a.txt"}}}),
		object{"type": "token_usage_record", "ordinal": 10, "payload": object{"response_id": "resp_2", "usage": usage(30)}},
		object{"type": "compacted", "payload": object{"replacement_history": []any{
			object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "summary"}}},
		}}},
		item(object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "more"}}}),
		item(object{"type": "message", "role": "assistant", "content": []any{object{"type": "output_text", "text": "done"}}}),
		object{"type": "token_usage_record", "ordinal": 14, "payload": object{"response_id": "resp_3", "usage": usage(40)}},
	)

	calls, _, errParse := Codex(path)
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}

	want := [][]string{
		{"developer", "user"},
		{"developer", "user", "reasoning", "function_call", "function_call_output"},
		{"user", "user"},
	}

	for i, call := range calls {
		roles := []string{}
		for _, msg := range call.Messages {
			roles = append(roles, msg.Role())
		}

		if strings.Join(roles, ",") != strings.Join(want[i], ",") {
			t.Fatalf("call %d roles %v, want %v", i, roles, want[i])
		}
	}

	first := calls[0]
	if first.RequestID != "resp_1" || first.SessionID != "codex:c1" || first.Model != "gpt-6" || first.InputTokens != 20 || first.CacheReadTokens != 4 {
		t.Fatalf("first call metadata: %+v", first.Event)
	}

	output, _ := first.Response.(map[string]any)["output"].([]any)
	if len(output) != 2 || first.System != "You are Codex." || first.Tools == nil {
		t.Fatalf("first call output %v system %v tools %v", output, first.System, first.Tools)
	}
}

// A message Claude Code writes itself after an answer keeps the model's answer as
// the response, and a request whose base the file lacks keeps its real depth
func TestClaudeCodeKeepsResponsesAndDepthAcrossGaps(t *testing.T) {
	session := "s3"
	hello := object{"role": "user", "content": "hello"}
	path := writeLines(t, session+".jsonl",
		object{"type": "api-request-blob", "sessionId": session, "hash": hashOf(t, hello), "message": hello},
		object{"type": "api-request", "sessionId": session, "id": "r1", "params": object{"model": "claude-opus-5-5"}, "tail": []any{hashOf(t, hello)}},
		object{"type": "assistant", "sessionId": session, "uuid": "a1", "requestRef": "r1",
			"message": object{"id": "m1", "model": "claude-opus-5-5", "content": []any{object{"type": "text", "text": "hi"}}}},
		object{"type": "assistant", "sessionId": session, "uuid": "a2", "parentUuid": "a1",
			"message": object{"id": "m2", "model": "<synthetic>", "content": []any{object{"type": "text", "text": "No response requested."}}}},
		// the base was written to a file that no longer exists
		object{"type": "api-request", "sessionId": session, "id": "r2", "params": object{"model": "claude-opus-5-5"},
			"base": "gone", "keep": 2, "tail": []any{hashOf(t, hello)}},
	)

	calls, stats, errParse := ClaudeCode(path)
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 2 || stats.Unresolved != 2 {
		t.Fatalf("calls %d stats %+v, want 2 calls and two unresolved messages", len(calls), stats)
	}

	encoded, _ := json.Marshal(calls[0].Response)
	if !strings.Contains(string(encoded), `"hi"`) || calls[0].Failed {
		t.Fatalf("first response %s failed %v, want the model's answer", encoded, calls[0].Failed)
	}

	roles := []string{}
	for _, msg := range calls[1].Messages {
		roles = append(roles, msg.Role())
	}

	if strings.Join(roles, ",") != "unknown,unknown,user" {
		t.Fatalf("second call roles %v, want two placeholders before the tail", roles)
	}
}

// An interrupted turn, a message from another agent while the model works and a
// compaction item all stay in the history without becoming a call's output
func TestCodexKeepsInputsOutOfResponses(t *testing.T) {
	usage := object{"input_tokens": 10, "output_tokens": 2}
	item := func(payload object) object { return object{"type": "response_item", "payload": payload} }
	user := func(text string) object {
		return item(object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": text}}})
	}
	assistant := func(text string) object {
		return item(object{"type": "message", "role": "assistant", "content": []any{object{"type": "output_text", "text": text}}})
	}

	lines := []any{
		object{"type": "session_meta", "payload": object{"id": "c2"}},
		user("first"),
		item(object{"type": "reasoning", "summary": []any{}}),
		object{"type": "event_msg", "payload": object{"type": "turn_aborted"}},
		user("second"),
		assistant("working"),
		item(object{"type": "agent_message", "content": "from another agent"}),
		assistant("done"),
		object{"type": "token_usage_record", "payload": object{"response_id": "resp_1", "usage": usage}},
		object{"type": "compacted", "payload": object{"replacement_history": []any{
			object{"type": "message", "role": "user", "content": []any{object{"type": "input_text", "text": "summary"}}},
			object{"type": "compaction", "encrypted_content": "x"},
		}}},
		user("third"),
		assistant("ok"),
		object{"type": "token_usage_record", "payload": object{"response_id": "resp_2", "usage": usage}},
	}

	calls, _, errParse := Codex(writeLines(t, "rollout-2.jsonl", lines...))
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}

	want := []struct{ request, output string }{
		{"user,reasoning,user", "message,message"},
		{"user,compaction,user", "message"},
	}

	for i, call := range calls {
		roles := []string{}
		for _, msg := range call.Messages {
			roles = append(roles, msg.Role())
		}

		output, _ := call.Response.(map[string]any)["output"].([]any)
		types := []string{}
		for _, entry := range output {
			types = append(types, entry.(object)["type"].(string))
		}

		if strings.Join(roles, ",") != want[i].request || strings.Join(types, ",") != want[i].output {
			t.Fatalf("call %d request %v output %v, want %+v", i, roles, types, want[i])
		}
	}

	// older rollouts have no usage records, and send token_count for rate limit
	// updates too, with no usage and no call behind it
	legacy := []any{
		object{"type": "session_meta", "payload": object{"id": "c3"}},
		object{"type": "event_msg", "payload": object{"type": "token_count", "info": nil}},
		user("hi"),
		object{"type": "event_msg", "payload": object{"type": "token_count", "info": object{"last_token_usage": usage}}},
		assistant("hello"),
		object{"type": "event_msg", "payload": object{"type": "token_count", "info": object{"last_token_usage": usage}}},
	}

	calls, _, errParse = Codex(writeLines(t, "rollout-3.jsonl", legacy...))
	if errParse != nil {
		t.Fatal(errParse)
	}

	if len(calls) != 1 || len(calls[0].Messages) != 1 {
		t.Fatalf("%d legacy calls, want one call answering the user", len(calls))
	}
}
