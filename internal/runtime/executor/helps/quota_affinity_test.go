package helps

import (
	"net/http"
	"strings"
	"testing"
)

func TestQuotaAffinityHeadersNeverEnterUpstreamLogs(t *testing.T) {
	headers := http.Header{"X-CLIProxy-Chat-ID": {"local-chat-secret"}, "X-CLIProxy-Chat-Handle": {"local-handle-secret"}, "Content-Type": {"application/json"}}
	var output strings.Builder
	writeHeaders(&output, headers)
	if strings.Contains(strings.ToLower(output.String()), "cliproxy-chat") || strings.Contains(output.String(), "local-") {
		t.Fatal("local identity entered upstream logs")
	}
	if headers["X-CLIProxy-Chat-Handle"][0] != "local-handle-secret" || !strings.Contains(output.String(), "application/json") {
		t.Fatal("header filtering changed input or ordinary headers")
	}
}
