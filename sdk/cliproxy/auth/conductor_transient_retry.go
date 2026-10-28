package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type transientRetry struct {
	manager *Manager
	limit   int
	used    int
}

func (m *Manager) newTransientRetry() *transientRetry {
	limit := int(m.transientRetries.Load())
	if m.HomeEnabled() {
		limit = 0
	}
	return &transientRetry{manager: m, limit: limit}
}

func (r *transientRetry) allows(auth *Auth, opts cliproxyexecutor.Options, err error) bool {
	var bootstrap *streamBootstrapError
	isBootstrap := errors.As(err, &bootstrap)
	invalidRequest := isRequestInvalidError(err) && !(isBootstrap && isStreamTruncatedError(err))
	if err == nil || r.used >= r.limit || isRequestStopError(err) || invalidRequest || isCredentialScopedError(err) || isResponsesCompactRequestFaultError(opts, err) {
		return false
	}
	if errors.Is(err, context.Canceled) || (errors.Is(err, context.DeadlineExceeded) && !isConfiguredTLSHandshakeTimeoutError(err)) {
		return false
	}
	action, matched := matchRequestScopedErrorAction(auth, err, r.manager.runtimeConfigSnapshot())
	// a configured continue means try the next credential now
	if isRequestScopedStop(action, matched) || (matched && (action == RequestScopedActionContinue || action == RequestScopedActionContinueAndCooldown)) {
		return false
	}
	resultErr := resultErrorFromError(err)
	if shouldRebindSessionAffinityOnError(resultErr) || isModelSupportResultError(resultErr) || isInvalidGrantErrorMessage(resultErr.Code) || isInvalidGrantErrorMessage(resultErr.Message) || resultErr.Code == ErrorCodeForceCooldown {
		return false
	}
	status := statusCodeFromError(err)
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout {
		return false
	}
	if isTransientTransportError(err) || resultErr.Code == "empty_stream" || resultErr.Code == "overloaded_error" {
		return true
	}
	// any 5xx left after the exclusions above is brief, including 529 and proxy codes like 520-524
	if status == http.StatusRequestTimeout || (status >= 500 && status <= 599) {
		return true
	}
	return isBootstrap || strings.Contains(strings.ToLower(resultErr.Message), "overloaded_error")
}

func waitForTransientRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func retryTransient[T any](r *transientRetry, ctx *context.Context, seq *uint64, auth *Auth, opts cliproxyexecutor.Options, call func(context.Context) (T, error)) (T, error) {
	for {
		value, err := call(*ctx)
		err = markUpstreamExecutionAttemptFromContext(*ctx, err)
		if (*ctx).Err() != nil || !r.allows(auth, opts, err) {
			return value, err
		}

		r.used++
		logEntryWithRequestID(*ctx).WithFields(map[string]any{
			"status": statusCodeFromError(err), "attempt": r.used + 1,
		}).Info("retrying brief upstream failure on the same credential")
		wait := r.manager.transientRetryWait
		if wait == nil {
			wait = waitForTransientRetry
		}
		if errWait := wait(*ctx, time.Duration(r.used)*time.Second); errWait != nil {
			return value, errWait
		}
		if errCtx := (*ctx).Err(); errCtx != nil {
			return value, errCtx
		}

		*ctx = newUpstreamAttemptContext(*ctx)
		*seq = r.manager.nextExecutionSeq()
	}
}

func cloneTransientRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	// rebuild from the original input, never from a previous attempt's configured payload
	req.Payload = append([]byte(nil), req.Payload...)
	opts.OriginalRequest = append([]byte(nil), opts.OriginalRequest...)
	opts.Headers = opts.Headers.Clone()
	opts.Metadata = cloneRequestMetadata(opts.Metadata)
	return req, opts
}

func (r *transientRetry) response(ctx *context.Context, seq *uint64, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, call func(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)) (cliproxyexecutor.Response, error) {
	return retryTransient(r, ctx, seq, auth, opts, func(attemptCtx context.Context) (cliproxyexecutor.Response, error) {
		attemptReq, attemptOpts := cloneTransientRequest(req, opts)
		return call(attemptCtx, auth, attemptReq, attemptOpts)
	})
}

type transientStreamAttempt struct {
	result       *cliproxyexecutor.StreamResult
	buffered     []cliproxyexecutor.StreamChunk
	closed       bool
	bootstrapErr error
}

func (r *transientRetry) stream(ctx *context.Context, seq *uint64, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, executor ProviderExecutor) (transientStreamAttempt, error) {
	attempt, err := retryTransient(r, ctx, seq, auth, opts, func(attemptCtx context.Context) (transientStreamAttempt, error) {
		attemptReq, attemptOpts := cloneTransientRequest(req, opts)
		result, errStream := executor.ExecuteStream(attemptCtx, auth, attemptReq, attemptOpts)
		result, errStream = validateStreamResult(result, errStream)
		attempt := transientStreamAttempt{result: result}
		if errStream != nil {
			return attempt, errStream
		}

		attempt.buffered, attempt.closed, attempt.bootstrapErr = readStreamBootstrap(attemptCtx, result.Chunks)
		if attempt.closed && (r.limit > 0 || len(attempt.buffered) == 0) {
			// empty chunks are not data and must not commit an otherwise empty stream
			attempt.bootstrapErr = &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
		}
		attempt.bootstrapErr = markUpstreamExecutionAttemptFromContext(attemptCtx, attempt.bootstrapErr)
		if attempt.bootstrapErr != nil {
			discardStreamChunks(result.Chunks)
		}
		return attempt, newStreamBootstrapError(attempt.bootstrapErr, result.Headers)
	})
	// a final bootstrap failure keeps the existing bootstrap result path, including headers and
	// 401 refresh; error identity is not compared because attempt marking rewraps it, and only a
	// backoff wait cancelled by the request returns directly
	if errCtx := (*ctx).Err(); attempt.bootstrapErr != nil && (errCtx == nil || !errors.Is(err, errCtx)) {
		return attempt, nil
	}
	return attempt, err
}
