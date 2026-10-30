package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func dialClosingCodexWebsocket(t *testing.T) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		// an abrupt close with no close frame is the idle loss the read loop reports as EOF
		_ = conn.UnderlyingConn().Close()
	}))
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A duplex writer can take the next create after the read loop has retired a dead connection
// but before the loss reaches the turn. That write must fail as a transport loss, which the
// turn reports as upstream_websocket_lost, and not as a credential change that ends the socket
func TestCodexWebsocketWriteOnLostConnReportsLoss(t *testing.T) {
	conn := dialClosingCodexWebsocket(t)
	e := NewCodexWebsocketsExecutor(&config.Config{})
	sess := newEphemeralCodexWebsocketSession()
	sess.conn = conn
	sess.connCloser = newWebsocketConnectionCloser(conn)
	sess.recoverTransport = true
	e.readUpstreamLoop(sess, conn)
	if sess.conn != nil {
		t.Fatal("read loop kept the lost connection")
	}

	errWrite := writeCodexWebsocketMessageContext(context.Background(), sess, conn, []byte(`{"type":"response.create"}`))
	var affinityErr *affinity.Error
	if errors.As(errWrite, &affinityErr) {
		t.Fatalf("write on lost conn = %v, want the transport loss", errWrite)
	}
	if !cliproxyexecutor.IsUpstreamWebsocketTurnError(codexWebsocketTurnTransportError(errWrite)) {
		t.Fatalf("write on lost conn = %v, want an upstream websocket turn error", errWrite)
	}
}

// A connection detached for another reason, such as a target change, still refuses the write
func TestCodexWebsocketWriteOnReplacedConnReportsCredentialChanged(t *testing.T) {
	conn := dialClosingCodexWebsocket(t)
	sess := newEphemeralCodexWebsocketSession()

	errWrite := writeCodexWebsocketMessageContext(context.Background(), sess, conn, []byte(`{"type":"response.create"}`))
	var affinityErr *affinity.Error
	if !errors.As(errWrite, &affinityErr) || affinityErr.Reason() != affinity.CredentialChanged {
		t.Fatalf("write on detached conn = %v, want chat_credential_changed", errWrite)
	}
}
