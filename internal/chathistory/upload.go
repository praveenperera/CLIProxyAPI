package chathistory

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// uploader inserts closed spool segments into ClickHouse over its HTTP interface
// and deletes each segment once ClickHouse has accepted it
type uploader struct {
	endpoint string
	database string
	user     string
	password string
	client   *http.Client
}

// upload sends one segment. The segment's file name is the insert deduplication
// token, so retrying a segment whose earlier insert succeeded without a response
// does not store its rows twice
func (u *uploader) upload(ctx context.Context, path string) error {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		return fmt.Errorf("chathistory: read segment: %w", errRead)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return os.Remove(path)
	}

	name := filepath.Base(path)
	table := name[:strings.IndexByte(name, '-')]

	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	_, _ = gz.Write(data)
	_ = gz.Close()

	query := url.Values{}
	query.Set("query", fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow", u.database, table))
	query.Set("insert_deduplication_token", strings.TrimSuffix(name, closedSuffix))
	// rows written by a newer proxy may carry columns this server does not have yet
	query.Set("input_format_skip_unknown_fields", "1")
	query.Set("date_time_input_format", "best_effort")
	// buffer the reply until the insert ends, so its status reports the outcome
	// instead of a 200 sent before a failure
	query.Set("wait_end_of_query", "1")

	request, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint+"/?"+query.Encode(), &body)
	if errRequest != nil {
		return errRequest
	}

	request.Header.Set("Content-Encoding", "gzip")
	request.SetBasicAuth(u.user, u.password)

	response, errDo := u.client.Do(request)
	if errDo != nil {
		return &retryableError{fmt.Errorf("chathistory: upload %s: %w", name, errDo)}
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		errUpload := fmt.Errorf("chathistory: upload %s: %s: %s", name, response.Status, strings.TrimSpace(string(detail)))

		// a malformed segment would block every segment behind it; set it aside for
		// inspection and keep going. Auth, throttling and server errors are retried
		if response.StatusCode == http.StatusBadRequest {
			_ = os.Rename(path, path+rejectedSuffix)
			return fmt.Errorf("%w (kept as %s%s)", errUpload, name, rejectedSuffix)
		}

		return &retryableError{errUpload}
	}

	// ClickHouse can still report a failure after a 200, in this header or at the
	// end of the body; the segment is the only copy of its rows, so it stays until
	// the reply is read in full and carries no exception
	detail, errBody := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errBody != nil {
		return &retryableError{fmt.Errorf("chathistory: upload %s: read reply: %w", name, errBody)}
	}

	if code := response.Header.Get("X-ClickHouse-Exception-Code"); code != "" || bytes.Contains(detail, []byte("__exception__")) {
		return &retryableError{fmt.Errorf("chathistory: upload %s: exception %s after 200: %s", name, code, strings.TrimSpace(string(detail[:min(len(detail), 2048)])))}
	}

	return os.Remove(path)
}

const rejectedSuffix = ".rejected"

// retryableError marks an upload failure that should stop the current pass and be
// retried with backoff, as opposed to a rejected segment that was set aside
type retryableError struct{ error }

func (e *retryableError) Unwrap() error { return e.error }

// backoff doubles the wait after each failed pass, up to five minutes
func backoff(failures int) time.Duration {
	wait := 5 * time.Second
	for i := 1; i < failures && wait < 5*time.Minute; i++ {
		wait *= 2
	}

	return min(wait, 5*time.Minute)
}
