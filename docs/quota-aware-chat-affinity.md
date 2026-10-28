# Quota-aware chat affinity

Use this optional profile to keep a chat on one exact local credential. A failed credential does not cause a switch to another account. Omit the profile to keep the existing HTTP routing behavior.

## Configure the server

Add this block under `routing`. Home and plugins must be disabled.

```yaml
routing:
  strategy: round-robin
  quota-aware-chat-affinity:
    namespace: local-chats
    state-dir: /var/lib/cliproxy/chat-affinity
    idle-retention: 168h
    hard-retention: 720h
    max-bindings: 65536
    max-quota-age: 5m
    poll-interval: 1m
    poll-concurrency: 4
    allow-legacy-requests: false
```

Use an absolute state path outside `auth-dir`. Only one server can use this directory. Keep the directory and its key across restarts. A lost directory or key invalidates saved chat handles. The directory contains private routing state and quota history, not provider tokens. Missing directory components are created with private permissions. Each parent entry is synchronized before the store can admit a request. A creation, synchronization, write, close, or rename error prevents dispatch. An uncertain write disables the open store until restart.

The file store supports local state on macOS and Linux where file and directory synchronization succeeds. Other platforms reject this profile. Keep state on a local durable volume, not a network or shared filesystem. Successful synchronization calls do not prove hardware behavior under power loss; universal crash durability and network filesystem durability are not claimed.

These values are the defaults, except for the required namespace and state path. A chat expires after the idle limit or the hard limit, whichever comes first. Live bindings are not removed to make room at the capacity limit. Use a new chat ID after expiry.

A reload can change other settings while it keeps the same validated profile. To enable, disable, or change the profile, stop and restart the server. An unsafe reload is rejected. Saved bindings and active turns stay in place.

Strict routing supports the local Codex and native Claude executors. Other providers and remote or plugin dispatch are not supported by this profile.

## Requests without chat headers

By default, every request must carry the chat headers below and is rejected with `chat_identity_required` otherwise. Clients such as Claude Code and the Codex CLI cannot save a chat handle between turns, so they cannot use strict chats.

Set `allow-legacy-requests: true` to let them share the server. A request with no `X-CLIProxy-Chat-*` header then uses session-sticky routing, priorities, and retries: each client session stays on one credential, so its prompt cache stays warm, and the `strategy` places new sessions and moves a session only when its credential fails or cools down. These session bindings are saved to `session-bindings.json` in the state directory every minute and at shutdown, so a restart keeps each live session on its credential until its `session-affinity-ttl` runs out. Bindings made in the last minute before a crash are lost. Sessions matched only by message history, without a session ID, are not saved. Any request that carries a chat header is still validated and pinned as a strict chat. Quota polling and history keep running for every credential. Changing this field needs a restart, like the rest of the profile.

## Retry brief upstream failures

`routing.transient-retries` defaults to `2`: up to three attempts on the same credential before normal failover. The waits are 1 second, then 2 seconds. Set it to `0` to disable these extra attempts. A config reload applies the new limit to subsequent credential/model attempts. Request cancellation stops a wait and returns the context error.

These retries cover HTTP 408 and every 5xx status, including 529, Anthropic `overloaded_error` (also when Claude sends it as the first stream event), transient transport failures, and streams that fail or close before the first data payload. No retry occurs after the first stream data payload. Auth, quota, model-support, invalid-request, and configured stop errors keep their existing behavior, and a configured `continue` action fails over at once. Intermediate failures do not change cooldowns or session bindings. Only the final result is recorded.

`session-affinity-rebind-on` applies after these retries are exhausted. With `any`, normal failover can move the session. With `quota`, brief failures do not change the saved session binding. Strict chats still cannot switch accounts. Home dispatch does not use these same-credential retries.

These retries do not consume `request-retry` rounds or the credential limit. Each credential/model attempt in an allowed round has one same-credential retry budget, shared with the existing one-time refresh-on-401 recovery. Executors rebuild each attempt from the original request, so configured output is not passed through payload rules a second time.

## Rank new sessions with a routing order

Set `strategy: ordered` and list criteria in `order` to choose where new sessions go:

```yaml
routing:
  strategy: ordered
  order: [weekly-reset, session-remaining, priority]
```

| Criterion | Prefers |
| --- | --- |
| `priority` | the higher `priority` field |
| `weekly-reset` | the weekly window that resets soonest, so allowance that would expire unused is spent first |
| `session-reset` | the short window, such as the Claude five-hour session, that resets soonest |
| `weekly-remaining` | the most weekly quota left, scaled by `quota_weight` |
| `session-remaining` | the most short-window quota left, scaled by `quota_weight` |

Earlier criteria decide and later ones break ties; full ties keep fill-first order. An empty `order` uses `[weekly-reset, session-remaining, priority]`. A credential whose fresh reading shows any window exhausted always ranks last, and one cooling down after a limit or failure is skipped. Credentials without data for a criterion come after those with it.

Unlike the other strategies, `ordered` ranks every priority tier, so `priority` is only as strong as its place in the list; leave it out to ignore it. Existing sessions never follow a change in ranking: they stay on their credential, keeping its prompt cache and account-bound thinking, until it fails or cools down.

A reset time stays usable after `max-quota-age` because it does not change before it passes, so ordering holds while usage polls back off. After a restart, or when a credential has no live reading yet, the weekly readings recorded in `quota-history.jsonl` are used under the same rules as live readings: the soonest reset until it passes, and the tightest remaining weekly quota. Remaining short-window quota and exhaustion use only fresh readings. The order applies to every provider, so Codex credentials are ranked the same way. For a strict chat, the strategy is the fallback selector used when quota ranking has no data.

Without this profile there is no quota data, so quota criteria tie and the server logs a warning. Change the order in the management panel under **Config → Network → Routing order**.

Point a client at the server with its usual base URL setting and a server API key. For Claude Code, set `ANTHROPIC_BASE_URL` to the server URL and `ANTHROPIC_AUTH_TOKEN` to the key. For Codex, add a model provider with `base_url` set to the server URL plus `/v1`, `wire_api = "responses"`, and an `env_key` naming the variable that holds the key.

## Start and continue a chat

Each request must have a valid server API credential. The server uses the authenticated caller to isolate chat IDs. Do not send a caller ID header.

Start a chat with two headers:

```http
X-CLIProxy-Chat-ID: my-chat-001
X-CLIProxy-Chat-Intent: start
```

Send the normal request body to `/v1/responses` with a Codex model that your server provides. For example, set `CPA_URL` to your server's LAN or Tailscale URL, then use:

```sh
curl -D response-headers.txt "$CPA_URL/v1/responses" \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'X-CLIProxy-Chat-ID: my-chat-001' \
  -H 'X-CLIProxy-Chat-Intent: start' \
  --data '{"model":"gpt-5","input":"Hello"}'
```

Save the returned `X-CLIProxy-Chat-Handle`. The handle is sent before a stream starts. It can also be returned when a saved dispatch fails. Keep it private. Do not send it to the provider.

For each next turn, use the same authenticated caller and chat ID:

```http
X-CLIProxy-Chat-ID: my-chat-001
X-CLIProxy-Chat-Intent: continue
X-CLIProxy-Chat-Handle: <saved handle>
```

If intent is absent, it means `continue`. Repeated `start` for a live chat keeps the saved account and returns the same handle. A start request must not include a handle. Duplicate, empty, unknown, or conflicting local headers are errors. The server does not infer a chat from prompt text or body fields.

Only one turn per chat can run at a time. The guard stays active until completion or cancellation. A model change must still be supported by the saved credential. A missing, expired, disabled, deleted, exhausted, or replaced credential does not create a new binding.

Token-count requests require a saved continuation. They cannot create a chat. For Responses WebSocket use, first start the chat over HTTP, then send the continuation headers on the WebSocket handshake. Reconnect with the same headers. A cold WebSocket start is rejected because its model is not available before the handshake.

## Quota and credential rules

Only new chats use quota ranking. The server keeps the highest available credential priority tier, excludes zero weights and fresh denials, and prefers the earliest positive future reset. Comparable remaining allowances break reset ties. Missing, stale, or unsupported quota data uses the configured fallback selector. Any applicable exhausted window blocks a saved turn.

Quota is observed from provider response headers and Codex events. The server also makes an initial read-only OAuth usage GET, then repeats it at the configured interval through the authenticated provider transport. Body size and concurrent fetches are bounded. The worker limit is shared across overlapping poll passes, and each credential has at most one fetch in flight. An established response has no new deadline. Shutdown cancellation stops and joins fetches. A failed poll does not renew old data. When the provider answers a usage read with HTTP 429, that credential is skipped by later passes until a backoff expires: the delay starts at three poll intervals (at least 15 minutes), doubles on each consecutive 429, follows `Retry-After` when that is longer, and is capped at 1 hour. The first successful read clears it. No quota reset calls are made.

Strict-chat ranking uses only the in-memory observations, which start empty after a restart. For charts, the server also records fractional remaining values per credential and window in `quota-history.jsonl` in the state directory. Points are added on the first observation, a change of at least 0.1 percentage points, a new reset time, or after 15 minutes without a stored point. Points older than 30 days are dropped when the server starts and when the log is rewritten, at most every 6 hours during polling. Each series keeps at most 8192 points. History writes are best effort: a write error is logged and does not affect dispatch or saved chats.

Read the history from `GET /v8/management/quota/history`. The optional `since` and `until` query parameters are unix seconds; the defaults are the last 14 days and now. The response lists current credentials with their `auth_index` and `quota_weight`, the account `email` when the auth file has one and its `plan_type` when known, such as a Codex plan from the file or its id token, and each window's points with `t`, `remaining` as a percent, and `reset_at` (0 when unknown):

```json
{
  "enabled": true,
  "retention_seconds": 2592000,
  "credentials": [
    {"auth_id": "...", "auth_index": "...", "provider": "claude", "quota_weight": 2,
     "email": "user@example.com",
     "windows": [
       {"id": "seven_day", "period_seconds": 604800,
        "points": [{"t": 1761400000, "remaining": 83.5, "reset_at": 1761900000}]}
     ]}
  ]
}
```

`quota_weight` comes from an optional top-level `quota_weight` number in the credential's auth file and defaults to 1. It tells pooled views how large one credential's quota is against others of the same provider. For Claude, count in Pro plans: 1 for Pro, 5 for Max 5x, and 10 for Max 20x, since a Max 20x weekly limit is only about twice a Max 5x's. The `weekly-remaining` and `session-remaining` routing criteria scale remaining quota by it; other routing ignores it. Set it in the management panel's account settings under **Quota weight**, or with `PATCH /v8/management/credentials/fields` and a body such as `{"name": "<file>", "quota_weight": 10}`. The value must be above 0 and at most 1000; `null` removes it.

Without this profile, the endpoint returns `"enabled": false` and no credentials.

Normal OAuth acquisition must prove the account for the exact token. Old files without this proof, synthetic identities, and setup tokens are not admitted. A normal refresh for the same verified account can continue. A persistent Codex socket also retains its actual handshake credential and caller/chat scope. A different scope or credential requires a new handshake; a required-socket request cannot use a socket with another owner. A different account cannot. API keys use a private HMAC of the exact key and origin; a key or origin replacement invalidates the binding.

## Errors

| Status | Typical local errors | Client action |
| --- | --- | --- |
| 400 | `chat_identity_required`, `chat_identity_invalid`, `chat_route_unsupported`, `chat_prefix_transform_unsupported` | Correct the request or use a supported route |
| 409 | `chat_binding_conflict`, `chat_busy` | Use the saved handle or wait for the active turn |
| 410 | `chat_binding_missing`, `chat_binding_expired` | Use a new explicit chat ID to start a new chat |
| 503 | Credential, quota, model, or store unavailable | Restore the saved credential or start a separate chat ID |

The local header family is removed from upstream requests and request logs. Normal upstream failures can retain their upstream status. They never select a second credential for a strict chat.

## Native Claude clients

Use `/v1/messages` with a native Claude request and response. Streaming uses native Claude events. Use `/v1/messages/count_tokens` only with a saved continuation. A count request cannot start a chat. OpenAI, Responses, Gemini, and other translated entrypoints cannot use strict Claude preservation.

Keep the caller's system, tools, and earlier messages unchanged. Send the full history, including opaque `thinking.signature`, thinking text, and `redacted_thinking.data`. The server does not sanitize these blocks, normalize signatures, insert replay blocks, rename tools, move system messages, or add billing and client-identity content. A model change keeps the same saved credential and the same caller history. It does not guarantee that the selected model can use every earlier block.

Caller beta headers and body `betas` are retained as transport betas. Supported `thinking.block_binding` controls are retained. For example:

```json
{
  "thinking": {
    "type": "adaptive",
    "block_binding": {"prefix_mismatch_behavior": "error"}
  },
  "betas": ["thinking-binding-controls-2026-08-01"]
}
```

Use `error` or `drop_block` for the mismatch control. Model-specific thinking validation still applies. Native Sonnet 5.5 `between_tools` uses only its `type` field and accepts omitted effort or `low`, `medium`, or `high`. Do not combine it with `block_binding`, a budget, or display controls. Forced tool choice with active thinking returns a local validation error; the server does not delete thinking to make the request pass.

Strict policy selects direct-native semantics even when the legacy default normally adds a Claude Code profile. You do not need to change that default. Forced cloaking, sensitive-word rules, system-message rebuilding, an explicit Claude Code fingerprint profile, configured client-identity or authorization headers, and matching payload rules that change protected content are not supported. These settings fail before network use and before a new binding is saved. Compatible non-prefix parameter rules still pass through model-specific thinking validation. Existing default and soft routing keep their previous behavior.

Native responses keep unknown fields and `input_transformations`, including start and final stream events. Usage telemetry records bounded drop counts for prefix, model, and organization mismatch, plus an unknown-reason count. It does not store transformation paths or opaque fields. Repeated start/final entries count once per attempt. Malformed entries do not stop the stream. These are provider reports, not proof of signature validity. Wire preservation does not establish provider cryptographic acceptance. See [Claude preserved thinking](https://platform.claude.com/docs/en/build-with-claude/preserved-thinking) for provider controls.

A provider prefix error is returned without body repair. Authentication, rate-limit, server, and stream failures do not move the chat to a second credential. Keep the saved handle when a turn fails.

## SDK hosts

Configure the manager with `ConfigureQuotaAffinity`. Use a trusted authenticated caller scope, `NewChatRequest`, and `executor.Options.Affinity` with `Execute`, `ExecuteStream`, or `ExecuteCount`. Use `AffinityBound` to obtain the saved handle before dispatch. Hosts can use `AuthenticatedCaller(provider, principal)` to create an isolated scope. Never obtain the principal from an untrusted client identity field.

A zero affinity option keeps the legacy SDK path. Selection-only APIs do not accept strict affinity: execution must pass through the manager's binding boundary. Run `RunQuotaPoller` with the service context and close the store with `CloseQuotaAffinity` after active work stops. The built-in service does this automatically.

For an explicit native SDK request, set `Options.NativeHistory` to `executor.NativeHistoryPreserve` and use Claude as both source and response format. The executor checks the same request and configuration policy even without a binding request. This flag alone does not create a durable chat pin; use `Options.Affinity` for that guarantee. Requests with native preservation or strict affinity reject request hooks before the hooks run. This rule also applies to a directly attached SDK host, even when plugins are disabled.
