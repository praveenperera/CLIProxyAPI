package chathistory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A segment stays in the spool until ClickHouse's reply is read in full with no
// exception, since ClickHouse can report a failed insert after a 200
func TestUploadKeepsSegmentsClickHouseFailedAfter200(t *testing.T) {
	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait_end_of_query") != "1" {
			t.Errorf("query %s, want wait_end_of_query=1", r.URL.RawQuery)
		}

		if failing {
			w.Header().Set("X-ClickHouse-Exception-Code", "241")
			_, _ = w.Write([]byte("Code: 241. DB::Exception: memory limit exceeded"))
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "requests-1"+closedSuffix)
	if errWrite := os.WriteFile(path, []byte(`{"request_id":"r1"}`+"\n"), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}

	u := &uploader{endpoint: server.URL, database: "chat", client: server.Client()}
	var retryable *retryableError
	if errUpload := u.upload(context.Background(), path); !errors.As(errUpload, &retryable) {
		t.Fatalf("upload error %v, want a retryable error", errUpload)
	}

	if _, errStat := os.Stat(path); errStat != nil {
		t.Fatalf("segment removed after a failed insert: %v", errStat)
	}

	failing = false
	if errUpload := u.upload(context.Background(), path); errUpload != nil {
		t.Fatal(errUpload)
	}

	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("segment kept after a clean insert: %v", errStat)
	}
}
