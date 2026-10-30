package executor

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// checkDialFence verifies the credential fence for a websocket dial and returns
// the headers with affinity routing headers removed
func checkDialFence(ctx context.Context, wsURL string, headers http.Header) (http.Header, error) {
	requestFence, errRequestFence := http.NewRequestWithContext(ctx, http.MethodGet, wsURL, nil)
	if errRequestFence != nil {
		return nil, errRequestFence
	}
	requestFence.Header = headers
	if errFence := cliproxyexecutor.CheckCredentialRequest(ctx, requestFence); errFence != nil {
		return nil, errFence
	}
	return affinity.StripHeaders(headers), nil
}

func writeCodexWebsocketMessageContext(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if err := cliproxyexecutor.CheckCredentialFence(ctx); err != nil {
		return err
	}
	if sess != nil {
		sess.connMu.Lock()
		binding := sess.transport
		current := sess.conn
		sess.connMu.Unlock()
		if current != conn {
			return affinity.NewError(affinity.CredentialChanged, time.Time{})
		}
		if err := binding.Check(ctx); err != nil {
			return err
		}
	}
	return writeWebsocketPayloadMessage("codex", sess, conn, payload)
}

// observeQuotaRead reports a buffered text frame to the quota observer using its original receipt time
func observeQuotaRead(ctx context.Context, ev codexWebsocketRead) {
	if ev.msgType == websocket.TextMessage {
		cliproxyexecutor.ObserveQuotaTimedEvent(ctx, cliproxyexecutor.TimedEvent{Payload: ev.payload, ReceivedAt: ev.receivedAt})
	}
}

// newTransportBinding captures the credential transport state a websocket connection is dialed under
func (e *CodexWebsocketsExecutor) newTransportBinding(ctx context.Context, wsURL string, headers http.Header) (cliproxyexecutor.TransportBinding, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, wsURL, nil)
	if err != nil {
		return cliproxyexecutor.TransportBinding{}, err
	}
	request.Header = headers
	return cliproxyexecutor.NewTransportBinding(ctx, request)
}

// receiveTime returns the receipt timestamp for a frame, honoring the injectable test clock
func (e *CodexWebsocketsExecutor) receiveTime() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now().UTC()
}
