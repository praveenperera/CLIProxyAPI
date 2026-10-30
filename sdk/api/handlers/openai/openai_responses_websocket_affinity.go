package openai

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
)

// prepareChatWebsocketUpgrade validates chat continuation before the websocket
// upgrade and returns the upgrade response headers, including the bound chat
// handle when one was set; it writes the error response and returns false when
// the request must not upgrade
func (h *OpenAIResponsesAPIHandler) prepareChatWebsocketUpgrade(c *gin.Context) (http.Header, bool) {
	// a handler without a base has no auth manager to bind chats to, so the
	// upgrade proceeds as upstream would
	if h.BaseAPIHandler == nil {
		return websocketUpgradeHeaders(c.Request), true
	}
	if errChat := h.PrepareChatWebsocket(c); errChat != nil {
		h.WriteErrorResponse(c, errChat)
		return nil, false
	}
	upgradeHeaders := websocketUpgradeHeaders(c.Request)
	if handle := c.Writer.Header().Get(affinity.ChatHandleHeader); handle != "" {
		upgradeHeaders.Set(affinity.ChatHandleHeader, handle)
	}
	return upgradeHeaders, true
}
