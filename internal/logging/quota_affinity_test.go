package logging

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestQuotaAffinityHeadersNeverEnterFileLogs(t *testing.T) {
	headers := map[string][]string{"x-cliproxy-chat-id": {"local-chat-secret"}, "X-CLIProxy-Chat-Handle": {"local-handle-secret"}, "Content-Type": {"application/json"}}
	var request, response bytes.Buffer
	if err := writeRequestInfoWithBody(&request, "/v1/responses", "POST", headers, nil, "", time.Time{}, "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := writeResponseSection(&response, 200, true, headers, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	logger := &FileRequestLogger{}
	combined := request.String() + response.String() + logger.formatLogContent("/v1/responses", "POST", headers, nil, nil, nil, nil, nil, nil, 200, headers, nil)
	if strings.Contains(strings.ToLower(combined), "cliproxy-chat") || strings.Contains(combined, "local-chat-secret") || strings.Contains(combined, "local-handle-secret") {
		t.Fatal("local identity entered file logs")
	}
	if !strings.Contains(combined, "application/json") {
		t.Fatal("ordinary headers were removed")
	}
}
