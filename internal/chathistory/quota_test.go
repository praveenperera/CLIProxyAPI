package chathistory

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// TestQuotaReadingsReachTheSpool runs calls and a usage poll through the real
// recorder and checks the rows it spools: a Claude call keeps only its quota
// headers, a Codex stream keeps its codex.rate_limits event, and the poll keeps
// its reply whole
func TestQuotaReadingsReachTheSpool(t *testing.T) {
	dir := t.TempDir()
	recorder, errNew := NewRecorder(Options{SpoolDir: dir})
	if errNew != nil {
		t.Fatal(errNew)
	}

	SetRecorder(recorder)
	t.Cleanup(func() { SetRecorder(nil) })

	claude := WithCapture(context.Background())
	CaptureRequest(claude, []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`))
	CaptureHeaders(claude, http.Header{
		"Content-Type": {"application/json"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":    {"0.69"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.31"},
		"Retry-After":        {"12"},
		"X-Codex-Turn-State": {"gAAAAABq8pg7"},
	})
	CaptureChunk(claude, []byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"hello"}]}`))
	Finish(claude, usage.Record{RequestID: "claude-call", AuthID: "claude-a", Provider: "claude"})

	codex := WithCapture(context.Background())
	CaptureRequest(codex, []byte(`{"model":"gpt-6-sol","input":[{"role":"user","content":"hi"}]}`))
	CaptureChunk(codex, []byte("event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":12}}}\n\n"))
	// the completed response quotes the event name in its text, and must not be taken for the event
	CaptureChunk(codex, []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"codex.rate_limits\"}]}]}}\n\n"))
	Finish(codex, usage.Record{RequestID: "codex-call", AuthID: "codex-a", Provider: "codex"})

	RecordQuotaPoll(QuotaPoll{At: time.Unix(1794277756, 0), Provider: "claude", AuthID: "claude-a", StatusCode: 200,
		Body: []byte(`{"five_hour":{"utilization":8,"resets_at":"2026-11-10T03:00:00Z"},"seven_day_fable":null}`)})
	recorder.Close()

	requests := map[string]map[string]any{}
	for _, row := range spooledRows(t, dir, "requests") {
		requests[row["request_id"].(string)] = row
	}

	claudeLimits, _ := requests["claude-call"]["rate_limits"].(map[string]any)
	want := map[string]any{
		"anthropic-ratelimit-unified-7d-utilization":    "0.69",
		"anthropic-ratelimit-unified-7d_oi-utilization": "0.31",
		"retry-after": "12",
	}
	if len(claudeLimits) != len(want) {
		t.Errorf("claude rate_limits = %v, want %v", claudeLimits, want)
	}

	for name, value := range want {
		if claudeLimits[name] != value {
			t.Errorf("claude rate_limits[%s] = %v, want %v", name, claudeLimits[name], value)
		}
	}

	codexLimits, _ := requests["codex-call"]["rate_limits"].(map[string]any)
	if event, _ := codexLimits[codexRateLimitsKey].(string); event != `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":12}}}` {
		t.Errorf("codex rate_limits = %v, want the codex.rate_limits event as JSON", codexLimits)
	}

	quota := spooledRows(t, dir, "quota")
	if len(quota) != 1 || quota[0]["auth_id"] != "claude-a" || quota[0]["source"] != "poll" ||
		quota[0]["ts"] != "2026-11-10 02:29:16.000" || quota[0]["body"] != `{"five_hour":{"utilization":8,"resets_at":"2026-11-10T03:00:00Z"},"seven_day_fable":null}` {
		t.Errorf("quota rows = %v, want the poll with its reply whole", quota)
	}
}

func spooledRows(t *testing.T, dir, table string) []map[string]any {
	t.Helper()

	paths, _ := filepath.Glob(filepath.Join(dir, table+"-*"+closedSuffix))
	var rows []map[string]any
	for _, path := range paths {
		file, errOpen := os.Open(path)
		if errOpen != nil {
			t.Fatal(errOpen)
		}

		scanner := bufio.NewScanner(file)
		scanner.Buffer(nil, 1<<20)
		for scanner.Scan() {
			var row map[string]any
			if errDecode := json.Unmarshal(scanner.Bytes(), &row); errDecode != nil {
				t.Fatal(errDecode)
			}

			rows = append(rows, row)
		}

		_ = file.Close()
	}

	return rows
}
