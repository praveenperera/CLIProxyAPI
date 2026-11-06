# Chat history

With `observability.chat-history.enabled`, the proxy records every upstream model call in ClickHouse: the request it sent, the response it received, and the call's session, account, token usage, latency and outcome. Capture works for every executor that reports through the shared logging helpers, including Claude, Codex (SSE and websocket), Gemini and OpenAI-compatible upstreams, and it does not depend on `request-log`.

## Configuration

```yaml
observability:
  chat-history:
    enabled: true
    endpoint: "http://code.local:8123"
    database: "chat"            # default
    user: "chat"
    password-file: "~/.config/chat-history/password"
    spool-dir: "~/.local/share/cliproxyapi/chat-history"
    max-spool-mb: 5120          # default
    flush-seconds: 30           # default
```

The password is read from `password-file` so it stays out of the config file and the management API's config view. An invalid block is rejected when the config loads. A missing password file or an unwritable spool directory is logged and leaves capture off without stopping the proxy. Changing the block on a running server logs a warning and takes effect after a restart.

The ClickHouse schema lives with the server that runs it; for this deployment see `ai5090/chat-history/` in the homelab repository. The tables are `chat.blobs`, `chat.nodes` and `chat.requests`, described below.

## What is stored

Each request re-sends the whole conversation, so storing request bodies would grow with the square of conversation length. The proxy splits each request instead:

| Part | Stored as |
| --- | --- |
| System prompt or instructions | One `blobs` row per distinct content, referenced from `requests.system` |
| Tool list | One `blobs` row per distinct content, referenced from `requests.tools` |
| Each message or Responses input item | One `blobs` row per distinct content, plus one `nodes` row per position in the session |
| Other request fields (model, max tokens, thinking) | `requests.params` |
| The response | `requests.response`, assembled from the stream |

A node's id hashes its parent node and its message, so two requests that share a prefix share its nodes, and a conversation that is rewound or edited branches from the shared part. Each `requests` row points at its last node (`leaf`) and records `message_count`. A turn that adds two messages writes two nodes however long the conversation already is.

Before hashing, messages are re-encoded with sorted keys and without prompt cache markers, because clients move `cache_control` to the newest message on every turn and the same message would otherwise hash differently each time it is re-sent. Markers are removed only where the Claude API defines them: on a message, its content blocks, the blocks inside a tool result, system blocks and tools. A `cache_control` key inside a tool's input or schema is kept. For the same reason a leading Claude `x-anthropic-billing-header` system block, whose `cch` value hashes the whole request body, is moved out of the system prompt into `requests.params` as `_billing_header`.

The request is the body sent upstream, after translation and any payload rules, so it is what the model saw. When an executor retries inside one attempt, such as falling back to another base URL, the last request and its response are kept.

Responses are folded into the provider's non-streaming response shape: Claude content blocks with thinking, signatures and parsed tool input; the Responses API `response` object with its output items, including items an interrupted stream left unfinished; Gemini candidates with merged text parts; and Chat Completions choices with joined content and tool calls. A body that is one JSON value, such as a pretty-printed Gemini response, is kept whole. Unknown formats keep the raw bytes. A response larger than 64 MiB keeps its first 64 MiB and the row's `error` says it was truncated. `requests.format` comes from the response, or from the request's layout when a failed call has none.

A websocket can carry many turns on one attempt, and duplex Codex sessions write the next `response.create` or a `response.steer` while the current response is still streaming. Websocket requests therefore queue, each response takes the oldest queued request when its first frame arrives, and a steer is recorded as the request of the response that consumes it. A steer the upstream refuses with `response.steer.failed` leaves the queue, since no response will take it. A websocket upgrade the upstream rejects sent no model request and is not recorded. Websocket turns have no HTTP status, so a successful one records `status_code` 0.

An OpenAI Responses request that sends only new input with `previous_response_id` continues the chain of the request that produced that response, while this process remembers it; the earlier response's output stays in that request's `response` column rather than becoming a node.

## Delivery

Capture never blocks a request. The executor hands the captured buffers to a bounded queue of 64 calls when it publishes the call's usage; if the queue is full the call is dropped and counted, and the uploader logs the count. A background worker parses the call, skips content and nodes it has already written, and appends rows to per-table NDJSON segments in `spool-dir`, flushed after every call. Numbers are clamped to their column ranges, since one out-of-range value would get a whole segment rejected. A failed write cuts the segment back to its last complete row and starts a new one.

Segments are closed after `flush-seconds` or 32 MiB and uploaded with gzip over ClickHouse's HTTP interface. Each segment's file name is its insert deduplication token, so a retry after a lost response does not insert rows twice while ClickHouse still remembers the token, the last 10,000 inserts. Past that, the tables' `ReplacingMergeTree` keys collapse the repeated rows when parts merge. Uploads stop at the first failure and retry with backoff up to five minutes, keeping table order. A segment ClickHouse rejects as malformed is renamed with a `.rejected` suffix and skipped, and the worker forgets what it has written so later calls send their blobs and nodes again instead of pointing at rows that never arrived. Rejected segments do not count toward `max-spool-mb`. Segments left open by a crash are trimmed to their last complete line and uploaded on the next start. Shutdown spools the queue, closes segments and makes one last upload attempt.

While `max-spool-mb` is waiting for upload, new calls are dropped rather than filling the disk. Errors other than a rejection, such as a missing table (404) or a server error, are retried indefinitely, so a misconfigured server fills the spool and then stops recording until it is fixed; the log names the failing segment.

## Importing local transcripts

`cmd/import_chat_history` loads conversations from before the proxy recorded them: Claude Code transcripts under `~/.claude/projects` (main sessions and `subagents/` files) and Codex rollouts under `~/.codex/sessions`. It writes through the same rows, spool and upload as the proxy, so imported content shares blobs with captured content, and `requests.source` says where each row came from: `proxy`, `claude-code` or `codex`.

```bash
go build -o import_chat_history ./cmd/import_chat_history
./import_chat_history -dry-run                     # parse everything, report the spooled size, upload nothing
./import_chat_history -endpoint http://code.local:8123 -password-file ~/.config/chat-history/password
./import_chat_history -until 2026-11-06T00:00:00-06:00   # skip calls from when the proxy started recording
```

Re-running it is safe: `chat.requests` is a `ReplacingMergeTree` keyed by session, time and request id, so a second import of the same call collapses into the first when parts merge, and reads with `FINAL` see one row before then. An interrupted run leaves segments in `-spool-dir`, which the next run uploads first.

How each source becomes calls:

- **Claude Code** writes an `api-request` line per API call, holding its params and its messages as the first `keep` messages of a `base` request plus a `tail` of message hashes, and an `api-request-shape` line holding the system prompt and tools. A message the transcript does not hold elsewhere is an `api-request-blob` line; the rest are rebuilt from the transcript's `user` and `assistant` lines, merging consecutive user lines such as parallel tool results into one message as the API received them. The response is the assistant message whose lines name the request in `requestRef`; messages Claude Code writes itself as `<synthetic>` stay in the history but never answer a request, unless they record an API error. Sessions are `claude-code:<sessionId>`, and a subagent is `claude-code:<sessionId>/agent-<agentId>` with the main session as its parent.
- **Older Claude Code transcripts** without `api-request` lines are rebuilt from each line's `parentUuid` chain, one call per assistant message, with no system prompt or tools.
- **Codex** rollouts list the Responses API items in order. Each call ends with a `token_usage_record` naming its response, or a `token_count` event in rollouts without records. A call's request is every item before its first output (reasoning, an assistant message or a tool call), and its response is the outputs after it; inputs that arrive while the model works, such as another agent's message, join the next request instead. A `turn_aborted` event ends a call that has no usage, and its items stay in the history. A `compacted` line replaces the history with its `replacement_history`, all of it input. Sessions are `codex:<id>`, and a subagent's parent is the thread that spawned it.

Imported rows have no latency, status or account, since transcripts do not record them. A message a Claude Code request references but the transcript does not hold is stored as `{"role":"unknown","_missing_claude_code_hash":...}`, or `_missing_claude_code_base` with its position when the whole base request is missing, so the conversation keeps its length; the importer prints the count. A full import of one machine's late-2026 transcripts left 340 such messages across 144,659 calls.

## Limits

- Calls served by plugin executors that do not use the shared logging helpers are not captured. Antigravity passes its request body to those helpers only with `request-log` on, so its calls are recorded only then.
- Streamed responses are assembled from `data:` lines that each hold one JSON event, as Claude and Codex send them; an event split across several `data:` lines is skipped. Chat Completions and Gemini responses keep only their first choice or candidate.
- An executor's internal retry, such as Codex resending a plain body after the upstream refuses a compressed one, is recorded as its final call only.
- Session titles are not stored; the first user message of a session is in `chat.session_messages(session = ...)`.
- Duplicate-skipping covers what this process has written recently. Content written again after a restart is collapsed by ClickHouse's `ReplacingMergeTree` during merges, so exact reads use `FINAL`, and the kept copy's `first_seen` may be the later sighting.
- Calls still queued in memory when the process crashes are lost; a clean shutdown spools them.
- `previous_response_id` chains are remembered in memory only. After a restart an incremental Responses turn starts a new chain; its `params` keep `previous_response_id` for joining later.
- Transcripts imported after the proxy starts recording describe the same calls again under different session ids; pass `-until` with the time capture started.
- Nothing is masked. Tool output routinely includes file contents and credentials, so keep the ClickHouse server private.
