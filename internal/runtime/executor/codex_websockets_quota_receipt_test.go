package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	runtimequota "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	domain "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quota"
)

func TestCodexQueuedQuotaKeepsReadReceipt(t *testing.T) {
	emit := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		<-emit
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100,"window_minutes":60,"reset_after_seconds":3600}}}`)); err != nil {
			t.Error(err)
		}
		<-release
	}))
	defer server.Close()
	defer close(release)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	received := time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)
	ref, _ := affinity.NewCredentialRef("codex", "fake", [32]byte{1})
	e := NewCodexWebsocketsExecutor(&config.Config{})
	e.now = func() time.Time { return received }
	sess := newEphemeralCodexWebsocketSession()
	sess.conn = conn
	sess.connCloser = newWebsocketConnectionCloser(conn)
	ch := sess.activate(conn)
	defer sess.clearActive(conn, ch)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); e.readUpstreamLoop(sess, conn) }()
	close(emit)
	// no consumer is admitted until the event is already queued
	for len(ch) == 0 {
		runtime.Gosched()
	}
	consumed := received.Add(6 * time.Minute)
	var snapshot domain.Snapshot
	count := 0
	ctx := exec.WithQuotaObserver(context.Background(), func(_ http.Header, payload []byte, at time.Time) {
		count++
		if !at.Equal(received) {
			t.Error("queue delay renewed receipt", at)
		}
		var err error
		snapshot, err = runtimequota.ParseCodexEvent(ref, payload, at)
		if err != nil {
			t.Error(err)
		}
	})
	_, payload, err := readCodexWebsocketMessage(ctx, sess, conn, ch)
	if err != nil {
		t.Fatal(err)
	}
	helps.AppendCodexAPIWebsocketResponse(ctx, &config.Config{}, payload)
	if count != 1 {
		t.Fatal("logging renewed the observation", count)
	}
	view := snapshot.View("", received, time.Hour)
	reset, ok := view.Windows()[0].ResetAt()
	if !ok || !reset.Equal(received.Add(time.Hour)) {
		t.Fatal("queued relative reset moved", reset)
	}
	if snapshot.View("", consumed, 5*time.Minute).Freshness() != domain.Stale {
		t.Fatal("queue delay made old quota fresh")
	}
	conn.Close()
	<-readerDone
}
