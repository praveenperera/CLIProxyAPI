package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// executeNative sends a preserved native Messages request. prepareNativeHistory has already
// finalized the body, user payload rules included, so nothing here rewrites business fields;
// native preservation only admits Claude source and response formats, so the upstream reply
// is the caller's JSON response and needs no translation
func (e *ClaudeExecutor) executeNative(ctx context.Context, auth *cliproxyauth.Auth, req exec.Request, opts exec.Options) (resp exec.Response, err error) {
	body, betas, err := e.prepareNativeHistory(ctx, auth, req, opts)
	if err != nil {
		return resp, err
	}
	ctx = e.nativeHistoryContext(ctx, opts)
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	// compaction items fail or dispatch exactly as they do on the Messages path
	if errExpand := expandClaudeResponsesCompaction(&req, &opts); errExpand != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
	}
	if claudeResponsesCompactionRequested(req, opts) {
		return e.executeClaudeCompaction(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := e.nativeHistoryReporter(ctx, auth, baseModel)
	defer reporter.TrackFailure(ctx, &err)

	body = helps.SetBoolIfDifferent(body, "stream", false)
	httpResp, decodedBody, fastRequest, err := e.sendNativeHistory(ctx, auth, reporter, nativeHistoryCall{
		path:    "/v1/messages?beta=true",
		body:    body,
		betas:   betas,
		headers: e.nativeHistoryHeaders(ctx, opts),
	})
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, err)
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	// a native turn records that it used no OAuth tool aliases, as the Messages path does
	e.rememberClaudeOAuthToolAliases(body, nil, claudeMessageIDFromResponse(data))
	reporter.ObserveResponseModel(data)
	reporter.ObserveInputTransformations(data)
	data = e.restoreResponseModel(data, req.Model)
	reporter.Publish(ctx, helps.ParseClaudeUsage(data))
	return exec.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

// executeStreamNative forwards a preserved native Messages stream as complete SSE events
func (e *ClaudeExecutor) executeStreamNative(ctx context.Context, auth *cliproxyauth.Auth, req exec.Request, opts exec.Options) (_ *exec.StreamResult, err error) {
	body, betas, err := e.prepareNativeHistory(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	ctx = e.nativeHistoryContext(ctx, opts)
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if errExpand := expandClaudeResponsesCompaction(&req, &opts); errExpand != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
	}
	if claudeResponsesCompactionRequested(req, opts) {
		return e.executeClaudeCompactionStream(ctx, auth, req, opts)
	}
	// native callers always get the OAuth cancellation contract, whatever the credential type
	defer func() {
		if cancelErr := newClaudeOAuthCancellationError(ctx, true, err); cancelErr != nil {
			err = cancelErr
		}
	}()
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := e.nativeHistoryReporter(ctx, auth, baseModel)
	defer reporter.TrackFailure(ctx, &err)

	body = helps.SetBoolIfDifferent(body, "stream", true)
	httpResp, decodedBody, fastRequest, err := e.sendNativeHistory(ctx, auth, reporter, nativeHistoryCall{
		path:    "/v1/messages?beta=true",
		body:    body,
		betas:   betas,
		headers: e.nativeHistoryHeaders(ctx, opts),
		stream:  true,
	})
	if err != nil {
		return nil, err
	}
	out := make(chan exec.StreamChunk, 1)
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)
		emitCancellation := func(cause error) bool {
			cancelErr := newClaudeOAuthCancellationError(ctx, true, cause)
			if cancelErr == nil {
				return false
			}
			helps.RecordAPIResponseError(ctx, e.cfg, cancelErr)
			streamUsage.PublishFailure(ctx, reporter, cancelErr)
			select {
			case out <- exec.StreamChunk{Err: cancelErr}:
			default:
				// cancellation replaces queued data so a full buffer cannot hide the terminal error
				select {
				case <-out:
				default:
				}
				select {
				case out <- exec.StreamChunk{Err: cancelErr}:
				default:
				}
			}
			return true
		}

		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var event bytes.Buffer
		var upstreamMessageID string
		var upstreamStreamErr error
		upstreamCompleted := false
		var overload helps.ClaudeLeadingOverloadGuard
		flushEvent := func() bool {
			if event.Len() == 0 {
				return true
			}
			observeClaudeSSEEvent(event.Bytes(), &upstreamMessageID, &upstreamCompleted, &upstreamStreamErr)
			if overload.Drop(event.Bytes(), upstreamStreamErr) {
				event.Reset()
				return true
			}
			cloned := bytes.Clone(event.Bytes())
			event.Reset()
			select {
			case out <- exec.StreamChunk{Payload: cloned}:
				overload.Sent()
				return true
			case <-ctx.Done():
				return false
			}
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			reporter.ObserveResponseModel(line)
			reporter.ObserveInputTransformations(line)
			streamUsage.ObserveClaudeStream(line)
			line = e.restoreResponseModel(line, req.Model)
			event.Write(line)
			event.WriteByte('\n')
			if len(bytes.TrimSpace(line)) == 0 {
				if !flushEvent() {
					emitCancellation(ctx.Err())
					return
				}
				if upstreamCompleted || upstreamStreamErr != nil {
					break
				}
			}
		}
		if !flushEvent() {
			emitCancellation(ctx.Err())
			return
		}
		if !upstreamCompleted {
			if emitCancellation(scanner.Err()) {
				return
			}
			errScan := scanner.Err()
			if errScan == nil {
				errScan = upstreamStreamErr
			}
			if errScan == nil && ctx.Err() == nil {
				errScan = statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before message_stop"}
			}
			if errScan != nil {
				errScan = wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errScan)
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				streamUsage.PublishFailure(ctx, reporter, errScan)
				select {
				case out <- exec.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
				return
			}
		}
		if upstreamCompleted {
			e.rememberClaudeOAuthToolAliases(body, nil, upstreamMessageID)
		}
	}()
	return &exec.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// countTokensNative forwards a preserved native body to Anthropic's count_tokens endpoint and
// returns its response untranslated
func (e *ClaudeExecutor) countTokensNative(ctx context.Context, auth *cliproxyauth.Auth, req exec.Request, opts exec.Options) (_ exec.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	// a token count is accounted as a request but never as generated output
	ctx = usage.WithGenerate(ctx, false)
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	body, betas, err := e.prepareNativeHistory(ctx, auth, req, opts)
	if err != nil {
		return exec.Response{}, err
	}
	// Claude Code 2.1.220's beta.messages.countTokens() always appends this beta
	betas = append(betas, claudeTokenCountingBeta)
	ctx = e.nativeHistoryContext(ctx, opts)

	httpResp, decodedBody, _, err := e.sendNativeHistory(ctx, auth, reporter, nativeHistoryCall{
		path:        "/v1/messages/count_tokens?beta=true",
		body:        body,
		betas:       betas,
		headers:     e.nativeHistoryHeaders(ctx, opts),
		countTokens: true,
	})
	if err != nil {
		return exec.Response{}, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return exec.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.ObserveInputTransformations(data)
	reporter.EnsurePublished(ctx)
	return exec.Response{Payload: data, Headers: httpResp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) nativeHistoryReporter(ctx context.Context, auth *cliproxyauth.Auth, baseModel string) *helps.UsageReporter {
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	if upstreamModel := e.upstreamModel(baseModel); upstreamModel != baseModel {
		reporter.SetUpstreamModel(upstreamModel)
	}
	return reporter
}

// nativeHistoryCall is one preserved native request with a finalized body
type nativeHistoryCall struct {
	path    string
	body    []byte
	betas   []string
	headers http.Header
	stream  bool
	// countTokens follows the count_tokens contract, which has no reasoning effort, fast mode,
	// HTTP usage tracking or upstream error debug log
	countTokens bool
}

// sendNativeHistory sends a native call and returns the decoded success body. An upstream error
// status is returned as the direct upstream response so the caller sees Anthropic's own error
func (e *ClaudeExecutor) sendNativeHistory(ctx context.Context, auth *cliproxyauth.Auth, reporter *helps.UsageReporter, call nativeHistoryCall) (*http.Response, io.ReadCloser, bool, error) {
	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	url := fmt.Sprintf("%s%s", baseURL, call.path)
	if !call.countTokens {
		reporter.SetTranslatedReasoningEffort(call.body, "claude")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(call.body))
	if err != nil {
		return nil, nil, false, err
	}
	if errHeaders := applyClaudeHeadersWithNativeProfile(httpReq, auth, apiKey, call.stream, call.betas, call.body, e.cfg, call.headers, false, false, ""); errHeaders != nil {
		return nil, nil, false, errHeaders
	}
	fastRequest := !call.countTokens && isAnthropicUpstreamBase(baseURL) && claudeRequestIsFast(httpReq, call.body)
	authID, authLabel, authType, authValue := claudeAuthLogIdentity(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      call.body,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	if !call.countTokens {
		httpClient = reporter.TrackHTTPClient(httpClient)
	}
	httpResp, err := doClaudeUpstreamRequest(httpClient, httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, nil, fastRequest, wrapClaudeFastRequestError(fastRequest, 0, err)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, decErr := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			errClassified := classifyClaudeUpstreamErrorWithCooling(httpResp.StatusCode, httpResp.Header, []byte(msg), e.modelLevelCooling())
			return nil, nil, fastRequest, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, errClassified)
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if !call.countTokens {
			helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		}
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, nil, fastRequest, newClaudeFastDirectResponseError(httpResp, b)
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, claudeResponseContentEncoding(httpResp.Header))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, nil, fastRequest, wrapClaudeFastRequestError(fastRequest, httpResp.StatusCode, err)
	}
	return httpResp, decodedBody, fastRequest, nil
}
