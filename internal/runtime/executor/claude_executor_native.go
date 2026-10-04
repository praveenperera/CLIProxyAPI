package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/affinity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// PreflightNativeHistory validates native preservation without network or state changes
func (e *ClaudeExecutor) PreflightNativeHistory(ctx context.Context, req exec.Request, opts exec.Options, credential exec.NativeHistoryCredential) error {
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: credential.Attributes, Metadata: credential.Metadata}
	_, _, err := e.prepareNativeHistory(ctx, auth, req, opts)
	return err
}

func (e *ClaudeExecutor) prepareNativeHistory(ctx context.Context, auth *cliproxyauth.Auth, req exec.Request, opts exec.Options) ([]byte, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts.Headers = e.nativeHistoryHeaders(ctx, opts)
	unsupported := affinity.NewError(affinity.PrefixTransformUnsupported, time.Time{})
	if opts.NativeHistory != exec.NativeHistoryPreserve || opts.SourceFormat.String() != "claude" || exec.ResponseFormatOrSource(opts).String() != "claude" || opts.Alt != "" {
		return nil, nil, unsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !gjson.ValidBytes(req.Payload) || !gjson.ParseBytes(req.Payload).IsObject() || !gjson.GetBytes(req.Payload, "messages").IsArray() || len(gjson.GetBytes(req.Payload, "messages").Array()) == 0 {
		return nil, nil, newClaudeTokenCountValidationError("native Claude preservation requires a JSON object with non-empty messages")
	}
	if value := gjson.GetBytes(req.Payload, "thinking"); value.Exists() && !value.IsObject() {
		return nil, nil, newClaudeTokenCountValidationError("native Claude thinking must be an object")
	}
	for _, message := range gjson.GetBytes(req.Payload, "messages").Array() {
		role, content := message.Get("role"), message.Get("content")
		if !message.IsObject() || role.Type != gjson.String || (role.String() != "user" && role.String() != "assistant" && role.String() != "system") || (content.Type != gjson.String && !content.IsArray()) {
			return nil, nil, newClaudeTokenCountValidationError("native Claude messages require a supported role and string or block-array content")
		}
		if content.IsArray() {
			for _, block := range content.Array() {
				if !block.IsObject() || block.Get("type").Type != gjson.String || block.Get("type").String() == "" {
					return nil, nil, newClaudeTokenCountValidationError("native Claude content blocks must be typed objects")
				}
			}
		}
	}
	apiKey, _ := claudeCreds(auth)
	if apiKey == "" {
		return nil, nil, newClaudeTokenCountValidationError("native Claude preservation requires a transport credential")
	}
	// legacy auto-cloak defaults do not constrain this explicit direct-native policy
	mode, strict, words, cache := getCloakConfigFromAuth(auth)
	cloak := resolveClaudeKeyCloakConfig(e.cfg, auth)
	if mode != "" && !strings.EqualFold(mode, "never") && !strings.EqualFold(mode, "auto") || strict || len(words) > 0 || cache || rebuildMidSystemMessageEnabled(e.cfg, auth) || claudeFingerprintProfileFromConfig(e.cfg, auth) != claudeFingerprintProfileDefault {
		return nil, nil, unsupported
	}
	if cloak != nil && ((cloak.Mode != "" && !strings.EqualFold(cloak.Mode, "never") && !strings.EqualFold(cloak.Mode, "auto")) || cloak.StrictMode || len(cloak.SensitiveWords) > 0 || cloak.CacheUserID != nil && *cloak.CacheUserID) {
		return nil, nil, unsupported
	}
	for key := range auth.Attributes {
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(key, "header:")))
		if strings.HasPrefix(key, "header:") && (name == "authorization" || name == "x-api-key" || name == "user-agent" || name == "x-app" || strings.HasPrefix(name, "x-stainless-") || strings.HasPrefix(name, "x-claude-code-") || name == "anthropic-dangerous-direct-browser-access") {
			return nil, nil, unsupported
		}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	body := bytes.Clone(req.Payload)
	// prefix rules are not a supported authority in this policy, even if they write the same value
	configured, touched := helps.ApplyPayloadConfigWithTrackedPaths(e.cfg, baseModel, "claude", "claude", "", body, body, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers, "system", "tools", "messages", "metadata", "context_management", "diagnostics", "thinking.block_binding", "betas")
	for _, hit := range touched {
		if hit {
			return nil, nil, unsupported
		}
	}
	for _, path := range []string{"system", "tools", "messages", "metadata", "context_management", "diagnostics", "thinking.block_binding", "betas"} {
		if gjson.GetBytes(configured, path).Raw != gjson.GetBytes(body, path).Raw {
			return nil, nil, unsupported
		}
	}
	body = configured
	var err error
	body, err = helps.ApplyRequestThinking(body, req, opts, "claude", "claude", e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	for _, path := range []string{"system", "tools", "messages", "thinking.block_binding", "betas"} {
		if gjson.GetBytes(body, path).Raw != gjson.GetBytes(req.Payload, path).Raw {
			return nil, nil, unsupported
		}
	}
	binding := gjson.GetBytes(body, "thinking.block_binding")
	if binding.Exists() {
		mismatch := binding.Get("prefix_mismatch_behavior")
		if !binding.IsObject() || len(binding.Map()) != 1 || mismatch.Type != gjson.String || (mismatch.String() != "error" && mismatch.String() != "drop_block") {
			return nil, nil, newClaudeTokenCountValidationError("thinking.block_binding requires prefix_mismatch_behavior error or drop_block")
		}
	}
	thinkingType := gjson.GetBytes(body, "thinking.type").String()
	if binding.Exists() && thinkingType != "enabled" && thinkingType != "adaptive" {
		return nil, nil, newClaudeTokenCountValidationError("thinking.block_binding requires enabled or adaptive thinking")
	}
	choice := gjson.GetBytes(body, "tool_choice.type").String()
	if (choice == "any" || choice == "tool") && thinkingType != "" && thinkingType != "disabled" {
		return nil, nil, newClaudeTokenCountValidationError("forced tool_choice is incompatible with active thinking; use auto tool_choice or disable thinking explicitly")
	}
	betas := gjson.GetBytes(body, "betas")
	if betas.Exists() {
		if !betas.IsArray() {
			return nil, nil, newClaudeTokenCountValidationError("native Claude betas must be an array of strings")
		}
		for _, beta := range betas.Array() {
			if beta.Type != gjson.String || strings.TrimSpace(beta.String()) == "" {
				return nil, nil, newClaudeTokenCountValidationError("native Claude betas must contain non-empty strings")
			}
		}
	}
	body = helps.SetStringIfDifferent(body, "model", e.upstreamModel(baseModel))
	extra, body := extractAndRemoveBetas(body)
	if binding.Exists() {
		extra = append(extra, claudeThinkingBindingBeta)
	}
	return body, extra, nil
}

func (e *ClaudeExecutor) nativeHistoryContext(ctx context.Context, opts exec.Options) context.Context {
	if opts.NativeHistory != exec.NativeHistoryPreserve {
		return ctx
	}
	return helps.WithNativeHistoryPreservation(ctx)
}

func (e *ClaudeExecutor) nativeHistoryHeaders(ctx context.Context, opts exec.Options) http.Header {
	if opts.Headers != nil {
		return opts.Headers
	}
	if ctx == nil {
		return nil
	}
	if headers := helps.IncomingHeadersFromContext(ctx); headers != nil {
		return headers
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.Request.Header
	}
	return nil
}
