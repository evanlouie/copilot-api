# copilot-api

`copilot-api` exposes a small OpenAI-compatible HTTP API backed by the GitHub
Copilot SDK. It is intended as a **single-user local proxy**: point
OpenAI-compatible clients at the local base URL and choose a model available in
your GitHub Copilot subscription.

The implementation follows the decisions in
[`docs/implementation-plan.html`](docs/implementation-plan.html):
replacement-mode prompts, Copilot SDK tools disabled by default, OpenAI-shaped
errors, synthetic Chat history hydration, Responses continuity, client-owned
function tool execution, SSE streaming, XDG storage, and manual purge.

Current SDK target: `github.com/github/copilot-sdk/go v1.0.0`.

## Quick start

```sh
# Build with the SDK-matched embedded Copilot CLI (recommended for releases).
go tool bundler --output cmd/copilot-api
go build ./cmd/copilot-api

# Run with logged-in GitHub/Copilot auth. Default bind is loopback.
./copilot-api serve

# Optional proxy bearer token.
COPILOT_API_KEY=local-secret ./copilot-api serve
```

If `COPILOT_API_KEY` is set, `/v1/*` endpoints require:

```http
Authorization: Bearer local-secret
```

If it is unset, `/v1/*` endpoints are unauthenticated and the server logs a
warning. The server refuses to bind to non-loopback addresses without
`COPILOT_API_KEY`; keep the default `127.0.0.1:8080` bind unless you enable
auth.

## Endpoints

| Endpoint                             | Status      | Notes                                                                      |
| ------------------------------------ | ----------- | -------------------------------------------------------------------------- |
| `GET /healthz`                       | Implemented | Operational liveness JSON.                                                 |
| `GET /readyz`                        | Implemented | Checks SDK client state and model listing.                                 |
| `GET /v1/models`                     | Implemented | Maps Copilot model metadata to OpenAI model objects.                       |
| `POST /v1/chat/completions`          | Implemented | Non-streaming and SSE streaming; synthetic session hydration for history.  |
| `POST /v1/responses`                 | Implemented | Text/image input, message-array input, function-call outputs, streaming.   |
| `GET /v1/responses`                  | Implemented | Responses WebSocket mode for streaming `response.create` events.           |
| `GET /v1/responses/{response_id}`    | Implemented | Only for API-visible stored responses. Debug state is retained regardless. |
| `DELETE /v1/responses/{response_id}` | Implemented | Removes API-visible retrieval while retaining debug files.                 |

## Compatibility matrix

| Feature                         | Behavior                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Models                          | `model` is required for every generation request. Unknown models are rejected as `model_not_found` after a forced model-cache refresh. Model metadata may include token limits such as `max_context_window_tokens`, `max_prompt_tokens`, and `max_output_tokens`, plus `supports_vision` and `vision` limits.                                                                                                                                                                                                                                                                                                                  |
| Reasoning effort                | Send top-level `reasoning_effort` on Chat Completions or Responses requests. Responses `reasoning.effort` is also accepted in permissive mode for Codex CLI compatibility. The value is forwarded to Copilot when supplied; omitted request values can use `COPILOT_DEFAULT_REASONING_EFFORT`, adjusted to the closest model-supported effort when metadata is available. If the model does not support reasoning efforts, the default is omitted. `GET /v1/models` metadata may include `supported_reasoning_efforts` and `default_reasoning_effort`. Other Responses `reasoning` controls are ignored or rejected as unsafe. |
| Reasoning output                | Model reasoning is surfaced on both surfaces. Chat Completions streams incremental `delta.reasoning` (+ `delta.reasoning_content`) chunks ahead of content/tool-call deltas and attaches `reasoning`, `reasoning_content`, and structured `reasoning_details` (Anthropic signed text plus any OpenAI-style encrypted blob) to the final assistant message; the streamed terminal carries a `reasoning_details` chunk for continuity. Responses emit ordered `reasoning` output items with `summary` text, bracketed by the full OpenAI summary lifecycle (`response.reasoning_summary_part.added`/`.done` around `response.reasoning_summary_text.delta`/`.done`) before the message item, plus `encrypted_content` when present (encrypted-only reasoning is reconciled into an announced item). Interleaved thinking is preserved automatically across the multi-request tool loop — each turn produces a fresh reasoning block. Inbound assistant `reasoning`/`reasoning_content`/`reasoning_details` are tolerated and replayed when rebuilding a cold session; opaque/encrypted reasoning is session-bound and stripped by the SDK on resume, so portable stateless `store:false` encrypted-reasoning round-trips are unsupported. The `COPILOT_REASONING_EMISSION` knob narrows or disables emission. |
| Chat history                    | Leading `system`/`developer` messages become replacement system instructions. Prior non-final messages are converted to Copilot SDK `events.jsonl`; only the final user turn is sent. A request may instead end with an assistant prefill (the trailing assistant content becomes prior context plus a `Continue.` continuation prompt) or a tool continuation. Mid-conversation `system`/`developer` messages are rejected. SDK infinite-session auto-compaction is disabled.                                                                                                                                                                                                                                                                                                                           |
| Prompt isolation                | The SDK is always called with `SystemMessageConfig{Mode: "replace"}`. Empty caller instructions use a single-space replacement, then fall back to `You are a chat completion model.` if needed. This avoids SDK resume failures caused by persisted empty `system.message` events.                                                                                                                                                                                                                                                                                                                                             |
| SDK tools                       | The SDK client runs in `ModeEmpty`. Built-in file/shell/MCP/memory/skill/repository tools are not exposed. `AvailableTools` is either request-scoped custom-tool filters or an impossible sentinel. Permissions reject everything except exact request-scoped custom tools.                                                                                                                                                                                                                                                                                                                                                     |
| Client-owned tools              | Chat Completions remains function-tool only. Responses accepts client-owned `function`, freeform `custom`, `namespace` child tools, and top-level `tool_search` specs. The proxy flattens them into request-scoped Copilot SDK custom tools, aliases SDK-unsafe names such as dotted function names, and rehydrates output items back to Responses shapes (`function_call`, `custom_tool_call`, and `tool_search_call`). Tool calls are returned to clients; the proxy never executes business logic. Hosted/proxy-executed tools such as provider MCP declarations, hosted `web_search`, and image generation are ignored in permissive mode so mixed Codex catalogs can proceed, and rejected in strict mode. |
| Tool continuations              | Chat clients append `role: "tool"` messages. Responses clients send `function_call_output`, `custom_tool_call_output`, or `tool_search_output` items with `previous_response_id`; mixed tool-output plus new message arrays are accepted for Responses compatibility. The proxy validates batch ownership, endpoint kind, requested model, pending call kind, and exactly one output per pending call before unblocking parked SDK handlers. `tool_search_output.tools` accepts only loadable client-owned `function` and `namespace` specs. Successful live `tool_search_output` items form an explicit turn boundary: returned tools are merged into the request-scoped catalog, persisted, the stale SDK runner is cut over, and the next Responses turn is configured with the merged catalog. If the live pending batch is no longer available (after a restart or TTL expiry), continuations fall back to replaying the supplied Chat transcript or the persisted previous Responses record and installed catalog so a model turn is still produced. Responses `input` may be omitted when `previous_response_id` is supplied. |
| `tool_choice`                   | Omitted/`auto` and `none` are supported. Forced function and `required` are rejected because the current SDK/runtime does not expose OpenAI-compatible enforcement for them.                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `parallel_tool_calls`           | Chat accepts omitted/`false`/`true`; Responses accepts omitted/`true` and rejects `false`. Internal pending batches capture and replay multiple tool calls per turn, so parallel tool calls round-trip on both surfaces. |
| Streaming                       | SSE streams are OpenAI-shaped. Chat streams emit reasoning deltas first (when present), then forward SDK text deltas, buffer tool calls, emit complete tool-call deltas, and terminate with `[DONE]`. Responses SSE streams emit lifecycle events (each carrying a monotonically increasing `sequence_number`) such as `response.created`, `response.in_progress`, reasoning summary events (`response.reasoning_summary_part.added`/`.done` and `response.reasoning_summary_text.delta`/`.done`), `response.output_item.added`, `response.content_part.added`, `response.output_text.delta`, `response.output_text.done`, `response.content_part.done`, `response.output_item.done`, function-call argument events, and `response.completed` or `response.failed`, then `[DONE]`. Extended Responses tool items (`custom_tool_call`, namespaced `function_call`, and `tool_search_call`) are announced with `response.output_item.added` before `response.output_item.done`; granular custom/tool-search deltas are deferred. Responses WebSocket mode emits the same JSON events and terminates failures with a top-level `error` for client compatibility. |
| Usage                           | SDK input/output/reasoning token events are mapped when available; unavailable fields are omitted. When Chat `stream_options.include_usage` is set, every streamed chunk carries `usage` (null until the terminal chunk) and a final empty-choices usage chunk is always emitted, using `usage: null` when upstream usage is unavailable.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| Multimodal                      | User image inputs are supported for Chat `image_url` parts and Responses `input_image` parts. `http`, `https`, and base64 `data:` URLs are converted to Copilot blob attachments; remote image fetches reject loopback, private, link-local, multicast, and otherwise non-public hosts to avoid SSRF; selected models must advertise vision support. Per-image size is capped at the model-advertised limit or 50 MiB, with a 100 MiB aggregate cap per request and a fallback limit of 20 images when model metadata omits a count. `function_call_output` content arrays are parsed: text parts become plain text, while image and file output parts are summarized as redacted text markers. Image `file_id` inputs and binary/image/file tool-output artifacts are deferred. JSON object/array tool outputs are serialized to JSON text.                                                                                                                         |
| Responses WebSocket differences | The OpenAI beta header is accepted but not required; instead of OpenAI's fixed 60-minute socket lifetime, connection limits are configurable via `COPILOT_WEBSOCKET_*` (an idle timeout, default 2m, that never closes a connection while a response is still generating; an optional hard max lifetime; and server ping keepalive); `response.create.response` nested payloads are accepted as an extension with nested fields taking precedence. `generate:false` creates a warmed Copilot SDK session and returns an empty completed response; tool-output continuations with `generate:false` fail clearly until warm post-search planning is supported. Warm sessions store and compare the normalized Responses tool catalog, including custom, namespace, unsafe-name aliases, and `tool_search`. Responses `store:false` is not API-visible but is still persisted locally for debugging and continuation, including dynamic tool catalogs until purge. |
| Unsupported fields              | Strict compatibility defaults to disabled, so harmless unsupported client knobs such as `temperature` are ignored. For Codex CLI compatibility, Responses `include: ["reasoning.encrypted_content"]` and `text.verbosity` are accepted as no-ops in permissive mode. Unsupported semantics that would mislead clients still fail closed with OpenAI-shaped `invalid_request_error` responses.                                                                                                                                                                                                                                  |

## Known Responses API limitations

`copilot-api` aims for practical OpenAI Responses compatibility on top of the
Copilot SDK; it is not a full OpenAI-hosted Responses runtime. Current known
limitations and intentional differences are:

- **Hosted/provider tools are unavailable.** The proxy does not implement hosted
  `web_search`, `file_search`, `image_generation`, `code_interpreter`,
  `computer_use`, or provider-hosted MCP execution. In permissive mode, hosted
  tool declarations are ignored when that is safe for mixed client catalogs; in
  strict mode they are rejected. Dynamically loaded `tool_search_output.tools`
  may install only client-owned `function` and `namespace` specs.
- **Client-owned tools are not executed by the proxy.** Function, custom,
  namespace, and dynamically loaded tools are exposed to the model as
  request-scoped synthetic SDK custom tools, then returned to the client as
  Responses tool-call items. The client must execute them and continue with the
  matching output item.
- **`tool_choice` supports only model-default planning and no-tools mode.**
  Omitted/`auto` and `none` are supported. Forced function choices and
  `required` are rejected because the Copilot SDK/runtime does not expose
  OpenAI-compatible enforcement.
- **Responses cannot enforce `parallel_tool_calls:false`.** Responses accepts
  omitted/`true` and rejects explicit `false`; the SDK Responses path exposes no
  public control to force serial tool planning.
- **Background Responses are unsupported.** Requests with `background` are
  rejected; WebSocket `background` fields are treated as transport-only no-ops
  when decoding `response.create` payloads.
- **Some generation controls are not forwarded.** `max_output_tokens` and
  `truncation` are rejected. `temperature`, `top_p`, `metadata`,
  `service_tier`, and `user` are not forwarded by this single-user proxy;
  permissive mode tolerates some of them as no-ops, while strict mode rejects
  them.
- **Structured text output is unsupported.** `text.verbosity` is accepted as a
  no-op for Codex compatibility, but `text.format` / JSON-schema-style
  structured output is rejected.
- **`include` is narrow.** Only `include: ["reasoning.encrypted_content"]` is
  accepted. Other include values are rejected.
- **Reasoning object controls are partial.** Top-level `reasoning_effort` and
  Responses `reasoning.effort` are supported. `reasoning.summary` is accepted
  for compatibility, but the proxy does not implement the full set of OpenAI
  reasoning controls or provider-specific summary behaviors.
- **Warm `generate:false` cannot include tool-output continuations.**
  `generate:false` can pre-warm a Responses session and return an empty
  completed response, but requests that combine `generate:false` with
  `function_call_output`, `custom_tool_call_output`, or `tool_search_output`
  fail clearly.
- **Image and file handling is partial.** User image inputs are supported only as
  `http`, `https`, or base64 `data:` URLs and only for models that advertise
  vision support. Image `file_id` inputs are unsupported. Tool-output image/file
  content parts are converted to redacted text summaries; binary artifacts are
  not retained or exposed as OpenAI files.
- **Cold fallback is synthetic, not exact provider-state replay.** When a live
  Copilot SDK session or pending tool batch is unavailable after restart or TTL
  expiry, the proxy reconstructs a continuation prompt from stored response
  records, tool outputs, and installed tool catalogs. This preserves practical
  continuity but is not byte-for-byte equivalent to resuming an opaque OpenAI
  provider session.
- **Usage fields are best-effort.** SDK usage events are mapped when available;
  unavailable token counts are omitted or emitted as `null` in streaming usage
  chunks where the OpenAI wire shape requires it.
- **Local debug retention differs from OpenAI.** `store:false` responses are not
  API-visible, but local continuation/debug records can still be retained until
  `copilot-api purge` is run, including prompts, tool outputs, and dynamic tool
  catalog metadata.

## Configuration

| Variable                           | Default                                              | Purpose                                                                                                                                                                                                                                                                                       |
| ---------------------------------- | ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `COPILOT_API_ADDR`                 | `127.0.0.1:8080`                                     | HTTP listen address.                                                                                                                                                                                                                                                                          |
| `COPILOT_API_KEY`                  | unset                                                | Optional proxy bearer token. Required when binding to a non-loopback address.                                                                                                                                                                                                                 |
| `GITHUB_TOKEN`                     | unset                                                | Optional process-wide GitHub token for Copilot SDK auth.                                                                                                                                                                                                                                      |
| `COPILOT_CLI_PATH`                 | embedded matched CLI when bundled, else SDK fallback | Advanced override for the Copilot CLI binary.                                                                                                                                                                                                                                                 |
| `COPILOT_DEFAULT_REASONING_EFFORT` | unset                                                | Optional reasoning effort to use when a Chat Completions or Responses request omits `reasoning_effort`. When model metadata advertises supported efforts, unsupported defaults are rounded to the closest supported level; models without reasoning-effort support omit it.                   |
| `COPILOT_MODELS_CACHE_TTL`         | `10m`                                                | Successful model-list cache TTL.                                                                                                                                                                                                                                                              |
| `COPILOT_TOOL_CALL_TTL`            | `5m`                                                 | Liveness guard for parked tool-call continuations.                                                                                                                                                                                                                                            |
| `COPILOT_REQUEST_TIMEOUT`          | `0`                                                  | Optional generation timeout; `0` disables proxy-imposed generation timeouts. Client disconnects and configured timeouts abort/disconnect the upstream SDK session.                                                                                                                            |
| `COPILOT_MAX_REQUEST_BODY_BYTES`   | `104857600`                                          | Optional HTTP body cap; `0` disables the proxy-specific cap. Default is 100 MiB to leave room for base64-encoded image data while bounding memory use.                                                                                                                                        |
| `COPILOT_WEBSOCKET_IDLE_TIMEOUT`   | `2m`                                                 | Idle timeout for Responses WebSocket connections. A connection closes only after the client has been silent this long while no response is generating; `0` disables it.                                                                                                                       |
| `COPILOT_WEBSOCKET_MAX_LIFETIME`   | `0`                                                  | Optional hard cap on total Responses WebSocket connection lifetime; `0` (default) disables it.                                                                                                                                                                                               |
| `COPILOT_WEBSOCKET_PING_INTERVAL`  | `30s`                                                | Server-side ping keepalive interval for Responses WebSocket connections; `0` disables pings.                                                                                                                                                                                                 |
| `COPILOT_API_DATA_DIR`             | `$XDG_DATA_HOME/copilot-api`                         | Retained SDK session files and synthetic Chat histories.                                                                                                                                                                                                                                      |
| `COPILOT_API_STATE_DIR`            | `$XDG_STATE_HOME/copilot-api`                        | Lock file, response records, and pending metadata.                                                                                                                                                                                                                                            |
| `COPILOT_API_CACHE_DIR`            | `$XDG_CACHE_HOME/copilot-api`                        | Model cache and transient cache files.                                                                                                                                                                                                                                                        |
| `COPILOT_API_CONFIG_DIR`           | `$XDG_CONFIG_HOME/copilot-api`                       | Isolated Copilot SDK config dir.                                                                                                                                                                                                                                                              |
| `COPILOT_STRICT_COMPAT`            | `false`                                              | Reject harmless unsupported OpenAI fields that permissive mode normally ignores; useful for debugging client conformance. Unsafe unsupported semantics are always rejected. |
| `COPILOT_REASONING_EMISSION`       | `both`                                               | Which reasoning fields the OpenAI-compatible surfaces emit: `both` (default; `reasoning` + `reasoning_content`), `reasoning`, `reasoning_content`, or `off`. `reasoning_details` and Responses reasoning items are emitted whenever the policy is not `off`. Narrow this for clients that render reasoning twice. |
| `COPILOT_LOG_CONTENT`              | `false`                                              | Opt-in request/response body logging. When `true`, completed request logs include up to 64 KiB each of `request_body` and `response_body`, plus truncation flags when capped, and debug logs include redacted Responses tool catalog counts/names. This can include prompts, responses, tool arguments, tool outputs, and image data; auth headers are not logged. |
| `COPILOT_LOG_LEVEL`                | `info`                                               | `debug`, `info`, `warn`, or `error`. Generation start metadata is logged at info; completed requests log at info/warn/error based on status and include accumulated generation fields. Generic request-received logs are debug-only.                                                          |

Durations accept Go duration strings like `5m`, or seconds as a number.

## Examples

### curl

```sh
curl http://127.0.0.1:8080/v1/models

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","messages":[{"role":"user","content":"Say hello in five words."}]}'

curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"Count to three."}]}'
```

Image input:

```sh
IMAGE_DATA_URL="data:image/png;base64,$(base64 -i screenshot.png | tr -d '\n')"

curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.5",
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "What is in this image?"},
        {"type": "image_url", "image_url": {"url": "'"$IMAGE_DATA_URL"'"}}
      ]
    }]
  }'
```

### Reasoning effort

Reasoning effort is a top-level request field. Values are model-dependent;
inspect `GET /v1/models` for `supported_reasoning_efforts` and
`default_reasoning_effort` metadata when the Copilot SDK provides it. For
clients that cannot send a request effort, set
`COPILOT_DEFAULT_REASONING_EFFORT`; explicit request values still take
precedence. For Responses requests, use `reasoning_effort` instead of a nested
`reasoning` object.

Chat Completions:

```sh
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.5",
    "reasoning_effort": "high",
    "messages": [{"role": "user", "content": "Solve this carefully: 19 * 37."}]
  }'
```

Responses API:

```sh
curl -s http://127.0.0.1:8080/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.5",
    "reasoning_effort": "medium",
    "input": "Give me a concise migration plan."
  }'
```

### Reasoning output

When a model emits extended thinking, the proxy surfaces it instead of dropping
it. Chat Completions streams `delta.reasoning` (and `delta.reasoning_content`)
chunks ahead of content and tool-call deltas, then attaches `reasoning`,
`reasoning_content`, and a structured `reasoning_details` array (Anthropic signed
text plus any OpenAI-style encrypted blob) to the final assistant message. The
Responses API emits ordered `reasoning` output items with `summary` text and
`response.reasoning_summary_text.*` stream events ahead of the message item.
Interleaved thinking is preserved automatically across the standard tool-call
loop because each follow-up request produces a fresh reasoning block.

Use `COPILOT_REASONING_EMISSION` to narrow what is emitted (`both` by default,
or `reasoning`, `reasoning_content`, or `off`) for clients that render reasoning
twice. Opaque/encrypted reasoning is session-bound and stripped by the Copilot
SDK on resume, so portable stateless `store:false` encrypted-reasoning
round-trips are not supported; rely on the live server-side session for
continuity.

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/v1", api_key="local-secret")
resp = client.chat.completions.create(
    model="gpt-5.5",
    messages=[{"role": "user", "content": "Say hello in five words."}],
)
print(resp.choices[0].message.content)

resp = client.chat.completions.create(
    model="gpt-5.5",
    reasoning_effort="high",
    messages=[{"role": "user", "content": "Solve this carefully: 19 * 37."}],
)
print(resp.choices[0].message.content)
```

### OpenAI JavaScript SDK

```js
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://127.0.0.1:8080/v1",
  apiKey: "local-secret",
});
const resp = await client.chat.completions.create({
  model: "gpt-5.5",
  messages: [{ role: "user", content: "Say hello in five words." }],
});
console.log(resp.choices[0].message.content);

const reasoned = await client.chat.completions.create({
  model: "gpt-5.5",
  reasoning_effort: "high",
  messages: [{ role: "user", content: "Solve this carefully: 19 * 37." }],
});
console.log(reasoned.choices[0].message.content);
```

### Client-owned function tools

```sh
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"gpt-5.5",
    "messages":[{"role":"user","content":"Use get_weather for Paris."}],
    "tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather by city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]
  }'
```

If the model calls the tool, the response contains `message.tool_calls`. Execute
the tool in your client and submit the output before `COPILOT_TOOL_CALL_TTL`
expires:

```json
{
  "model": "gpt-5.5",
  "messages": [
    { "role": "user", "content": "Use get_weather for Paris." },
    {
      "role": "assistant",
      "tool_calls": [
        {
          "id": "call_...",
          "type": "function",
          "function": {
            "name": "get_weather",
            "arguments": "{\"city\":\"Paris\"}"
          }
        }
      ]
    },
    {
      "role": "tool",
      "tool_call_id": "call_...",
      "content": "{\"temperature\":\"18C\"}"
    }
  ]
}
```

### Responses extended client-owned tools

Responses requests can supply the client-owned tool shapes used by Codex-style
agents:

```json
{
  "model": "gpt-5.5",
  "input": "Patch the file, then search for follow-up tools if needed.",
  "tools": [
    { "type": "function", "name": "multi_tool_use.parallel", "parameters": { "type": "object", "properties": {} } },
    { "type": "custom", "name": "apply_patch", "format": { "type": "grammar", "syntax": "lark", "definition": "start: /.+/" } },
    {
      "type": "namespace",
      "name": "mcp__grep_app",
      "tools": [
        { "name": "searchGitHub", "parameters": { "type": "object", "properties": { "query": { "type": "string" } } } }
      ]
    },
    { "type": "tool_search", "execution": "client", "parameters": { "type": "object", "properties": { "query": { "type": "string" } } } }
  ]
}
```

The SDK-facing names are implementation details. Responses output is rehydrated
back to public client shapes, for example a namespaced call becomes
`{"type":"function_call","namespace":"mcp__grep_app","name":"searchGitHub",...}`
and `apply_patch` becomes `custom_tool_call` with raw `input`. Execute tools in
the client and continue with the matching output item type. `tool_search_output`
may include loadable `function`/`namespace` specs. When a live successful
`tool_search_output` includes returned tools, the proxy installs them at the next
SDK turn boundary by merging them into the persisted request-scoped catalog and
starting a fresh/synthetic continuation configured with that merged catalog; the
proxy still never executes those client-owned tools itself.

## State, locking, and purge

The proxy stores Copilot SDK session state and `metadata.json` under the
configured XDG directories. Files are retained forever by default for debugging,
including when Responses `store:false` disables API-visible retrieval.

A server lock in the state directory prevents two servers from sharing the same
store. Manual cleanup is explicit:

```sh
copilot-api purge --dry-run
copilot-api purge --yes
```

`purge` refuses while the server lock is active.

## Embedded Copilot CLI

This repository is configured with the Copilot SDK bundler tool, but generated
`cmd/copilot-api/zcopilot_*` bundle artifacts are intentionally ignored and not
committed. Run the bundler during release/package builds when you want the
binary to include the SDK-matched Copilot CLI:

```sh
go tool bundler --output cmd/copilot-api
go build ./cmd/copilot-api
```

Docker builds run the bundler inside the build stage. Local development can skip
bundling and use `COPILOT_CLI_PATH` or `copilot` on `PATH`, but that weakens
SDK/CLI version matching.

Do not upgrade the SDK/CLI unless the hydration, prompt-isolation, tool
disablement, tool-choice, and provider-shape tests/spikes have been re-run.

## Development

```sh
go test ./...
go vet ./...
```

Live Copilot integration checks should be gated by `COPILOT_API_LIVE_TESTS=1`
and are not part of the default test suite.

The Deno + AI SDK integration suite is also gated by default. Start a local
server, then enable it explicitly:

```sh
COPILOT_API_AI_SDK_DENO_TESTS=1 \
COPILOT_API_BASE_URL=http://127.0.0.1:8080/v1 \
COPILOT_API_KEY=local-secret \
deno task test:ai-sdk
```

The suite covers Chat Completions, Responses over HTTP and WebSocket transports,
reasoning effort, multi-turn history, MCP-backed client tools, and image inputs.
See [`tests/ai-sdk-deno/README.md`](tests/ai-sdk-deno/README.md) for
configuration options.
