package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type chatBoundaryKey struct{}
type chatBoundary struct {
	request affinity.Request
	err     error
	bound   func(affinity.Handle)
}

func (h *BaseAPIHandler) captureChatBoundary(ctx context.Context) context.Context {
	if _, ok := ctx.Value(chatBoundaryKey{}).(chatBoundary); ok {
		return ctx
	}
	var b chatBoundary
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx == nil || ginCtx.Request == nil {
		return ctx
	}
	if saved, ok := ginCtx.Request.Context().Value(chatBoundaryKey{}).(chatBoundary); ok {
		return context.WithValue(ctx, chatBoundaryKey{}, saved)
	}
	caller := affinity.CallerFromContext(ginCtx.Request.Context())
	if caller == (affinity.CallerScope{}) {
		caller, _ = affinity.NewCallerScope(requestCallerScope(ginCtx))
	}
	namespace := ""
	if h.AuthManager != nil {
		namespace = h.AuthManager.QuotaAffinityNamespace()
		// an empty namespace makes a header-less request legacy, while any chat header stays strict
		if namespace != "" && h.AuthManager.QuotaAffinityAllowsLegacy() && !hasLocalChatHeader(ginCtx.Request.Header) {
			namespace = ""
		}
	}
	b.request, b.err = affinity.ParseHeaders(namespace, caller, ginCtx.Request.Header)
	b.bound = func(handle affinity.Handle) { ginCtx.Writer.Header().Set(affinity.ChatHandleHeader, handle.Encode()) }
	// remove local identity before any plugin or upstream sees inbound headers
	ginCtx.Request = ginCtx.Request.Clone(ginCtx.Request.Context())
	ginCtx.Request.Header = affinity.StripHeaders(ginCtx.Request.Header)
	return context.WithValue(ctx, chatBoundaryKey{}, b)
}

func hasLocalChatHeader(headers http.Header) bool {
	for name := range headers {
		if affinity.IsLocalHeader(name) {
			return true
		}
	}
	return false
}

func (h *BaseAPIHandler) prepareChatAffinity(ctx context.Context) (context.Context, *interfaces.ErrorMessage) {
	ctx = h.captureChatBoundary(ctx)
	b, _ := ctx.Value(chatBoundaryKey{}).(chatBoundary)
	if b.err != nil {
		return ctx, executionErrorMessage(b.err)
	}
	if b.request.Intent() != affinity.Legacy && (h.AuthManager == nil || h.AuthManager.HomeEnabled()) {
		return ctx, executionErrorMessage(affinity.NewError(affinity.RouteUnsupported, time.Time{}))
	}
	if b.request.Intent() != affinity.Legacy {
		if err := h.AuthManager.ValidateChatRequest(ctx, b.request); err != nil {
			return ctx, executionErrorMessage(err)
		}
	}
	return ctx, nil
}

func chatOptions(ctx context.Context, opts exec.Options) exec.Options {
	b, _ := ctx.Value(chatBoundaryKey{}).(chatBoundary)
	opts.Affinity = b.request
	opts.AffinityBound = b.bound
	opts.Headers = affinity.StripHeaders(opts.Headers)
	return opts
}

func strictChat(ctx context.Context) bool {
	b, _ := ctx.Value(chatBoundaryKey{}).(chatBoundary)
	return b.request.Intent() != affinity.Legacy
}
func strictRouteError(ctx context.Context, plugin string) *interfaces.ErrorMessage {
	if strictChat(ctx) && plugin != "" {
		return executionErrorMessage(affinity.NewError(affinity.RouteUnsupported, time.Time{}))
	}
	return nil
}

func localChatHeaders(headers http.Header, ctx context.Context) http.Header {
	if !strictChat(ctx) {
		return headers
	}
	ginCtx, _ := ctx.Value("gin").(*gin.Context)
	if ginCtx == nil {
		return headers
	}
	handle := ginCtx.Writer.Header().Get(affinity.ChatHandleHeader)
	if handle == "" {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	out.Set(affinity.ChatHandleHeader, handle)
	return out
}

// PrepareChatWebsocket validates continuation before the websocket upgrade
// Start must use HTTP because the model is not available until after the upgrade
func (h *BaseAPIHandler) PrepareChatWebsocket(c *gin.Context) *interfaces.ErrorMessage {
	ctx := context.WithValue(c.Request.Context(), "gin", c)
	ctx, errMsg := h.prepareChatAffinity(ctx)
	if errMsg != nil {
		return errMsg
	}
	b, _ := ctx.Value(chatBoundaryKey{}).(chatBoundary)
	if b.request.Intent() == affinity.Legacy {
		return nil
	}
	if b.request.Intent() != affinity.ContinueIntent {
		return executionErrorMessage(affinity.NewError(affinity.RouteUnsupported, time.Time{}))
	}
	if err := h.AuthManager.ValidateChatContinuation(ctx, b.request); err != nil {
		return executionErrorMessage(err)
	}
	c.Request = c.Request.WithContext(ctx)
	c.Writer.Header().Set(affinity.ChatHandleHeader, b.request.Handle().Encode())
	return nil
}

// chatRequestHookError rejects request interceptors when a strict chat request
// forbids prefix transforms
func (h *BaseAPIHandler) chatRequestHookError(ctx context.Context, host PluginInterceptorHost, opts exec.Options) *interfaces.ErrorMessage {
	policy := opts
	if strictChat(ctx) {
		policy = chatOptions(ctx, opts)
	}
	if requestInterceptorsEnabled(host) && !policy.RequestHooksAllowed() {
		return executionErrorMessage(affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{}))
	}
	return nil
}

// prepareChatStream applies chat affinity for a streaming execution and, on
// failure, returns an error channel holding the rejection
func (h *BaseAPIHandler) prepareChatStream(ctx context.Context) (context.Context, <-chan *interfaces.ErrorMessage) {
	ctx, chatErr := h.prepareChatAffinity(ctx)
	if chatErr != nil {
		return ctx, singleErrorChan(chatErr)
	}
	return ctx, nil
}

// strictRouteStream rejects plugin executor routes for strict chat requests on
// streaming executions
func strictRouteStream(ctx context.Context, plugin string) <-chan *interfaces.ErrorMessage {
	if chatErr := strictRouteError(ctx, plugin); chatErr != nil {
		return singleErrorChan(chatErr)
	}
	return nil
}

func singleErrorChan(errMsg *interfaces.ErrorMessage) <-chan *interfaces.ErrorMessage {
	errChan := make(chan *interfaces.ErrorMessage, 1)
	errChan <- errMsg
	close(errChan)
	return errChan
}

// settleCancelRace downgrades a successful completion to canceled when the
// context was cancelled while the final read saw a closed source
func settleCancelRace(ctx context.Context, outcome pluginapi.RequestCompletionOutcome, status int, err error) (pluginapi.RequestCompletionOutcome, int, error) {
	if outcome == pluginapi.RequestCompletionSucceeded && ctx != nil && ctx.Err() != nil {
		return pluginapi.RequestCompletionCanceled, 0, ctx.Err()
	}
	return outcome, status, err
}
