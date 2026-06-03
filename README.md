# copilot-api

`copilot-api` exposes a small OpenAI-compatible HTTP API backed by the GitHub Copilot SDK. It is intended as a **single-user local proxy**: point OpenAI-compatible clients at the local base URL and choose a model available in your GitHub Copilot subscription.

The implementation follows the decisions in [`docs/implementation-plan.html`](docs/implementation-plan.html): replacement-mode prompts, Copilot SDK tools disabled by default, OpenAI-shaped errors, synthetic Chat history hydration, Responses continuity, client-owned function tool execution, SSE streaming, XDG storage, and manual purge.

Current SDK target: `github.com/github/copilot-sdk/go v1.0.0-beta.10`. As of the latest locally available module tags, a stable Go `v1.0.0` tag is not available yet.

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

If it is unset, `/v1/*` endpoints are unauthenticated and the server logs a warning. The server refuses to bind to non-loopback addresses without `COPILOT_API_KEY`; keep the default `127.0.0.1:8080` bind unless you enable auth.

## Endpoints

| Endpoint | Status | Notes |
| --- | --- | --- |
| `GET /healthz` | Implemented | Operational liveness JSON. |
| `GET /readyz` | Implemented | Checks SDK client state and model listing. |
| `GET /v1/models` | Implemented | Maps Copilot model metadata to OpenAI model objects. |
| `POST /v1/chat/completions` | Implemented | Non-streaming and SSE streaming; synthetic session hydration for history. |
| `POST /v1/responses` | Implemented | Text/image input, message-array input, function-call outputs, streaming. |
| `GET /v1/responses/{response_id}` | Implemented | Only for API-visible stored responses. Debug state is retained regardless. |
| `DELETE /v1/responses/{response_id}` | Implemented | Removes API-visible retrieval while retaining debug files. |

## Compatibility matrix

| Feature | Behavior |
| --- | --- |
| Models | `model` is required for every generation request. Unknown models are rejected as `model_not_found` after a forced model-cache refresh. Model metadata may include token limits such as `max_context_window_tokens`, `max_prompt_tokens`, and `max_output_tokens`, plus `supports_vision` and `vision` limits. |
| Reasoning effort | Send top-level `reasoning_effort` on Chat Completions or Responses requests. Responses `reasoning.effort` is also accepted in permissive mode for Codex CLI compatibility. The value is forwarded to Copilot when supplied; omitted request values can use `COPILOT_DEFAULT_REASONING_EFFORT`, adjusted to the closest model-supported effort when metadata is available. If the model does not support reasoning efforts, the default is omitted. `GET /v1/models` metadata may include `supported_reasoning_efforts` and `default_reasoning_effort`. Other Responses `reasoning` controls are ignored or rejected as unsafe. |
| Chat history | Leading `system`/`developer` messages become replacement system instructions. Prior non-final messages are converted to Copilot SDK `events.jsonl`; only the final user turn is sent. Mid-conversation `system`/`developer` messages are rejected. SDK infinite-session auto-compaction is disabled. |
| Prompt isolation | The SDK is always called with `SystemMessageConfig{Mode: "replace"}`. Empty caller instructions use a single-space replacement, then fall back to `You are a chat completion model.` if needed. This avoids SDK resume failures caused by persisted empty `system.message` events. |
| SDK tools | The SDK client runs in `ModeEmpty`. Built-in file/shell/MCP/memory/skill/repository tools are not exposed. `AvailableTools` is either request-scoped aliases or an impossible sentinel. Permissions reject everything except exact request-scoped custom tools. |
| OpenAI function tools | `type: "function"` tools are accepted using either Chat's nested `function` shape or Responses' top-level `name`/`parameters` shape. Public names are mapped to opaque SDK aliases. Tool calls are returned to clients; the proxy never executes business logic. Non-function Responses tools emitted by clients such as Codex CLI are ignored in permissive mode and rejected in strict mode. |
| Tool continuations | Chat clients append `role: "tool"` messages. Responses clients send `function_call_output` items with `previous_response_id`. The proxy validates one output per pending call before unblocking the parked SDK handlers. |
| `tool_choice` | Omitted/`auto` and `none` are supported. Forced function and `required` are rejected because the current SDK/runtime does not expose OpenAI-compatible enforcement for them. |
| `parallel_tool_calls` | Chat accepts omitted/`false` and rejects `true`. Responses accepts omitted/`true` and rejects `false`. Internal pending batches support multiple calls. |
| Streaming | SSE streams are OpenAI-shaped. SDK streaming deltas are forwarded as text deltas; tool calls are buffered and emitted complete; streams terminate with `[DONE]`. |
| Usage | SDK input/output/reasoning token events are mapped when available; unavailable fields are omitted. |
| Multimodal | User image inputs are supported for Chat `image_url` parts and Responses `input_image` parts. `http`, `https`, and base64 `data:` URLs are converted to Copilot blob attachments; remote image fetches reject loopback, private, link-local, multicast, and otherwise non-public hosts to avoid SSRF; selected models must advertise vision support. Image `file_id` inputs and binary/multimodal tool outputs are deferred. JSON object/array tool outputs are serialized to JSON text. |
| Unsupported fields | Strict compatibility defaults to disabled, so harmless unsupported client knobs such as `temperature` are ignored. For Codex CLI compatibility, Responses `include: ["reasoning.encrypted_content"]` and `text.verbosity` are accepted as no-ops in permissive mode. Unsupported semantics that would mislead clients still fail closed with OpenAI-shaped `invalid_request_error` responses. |

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `COPILOT_API_ADDR` | `127.0.0.1:8080` | HTTP listen address. |
| `COPILOT_API_KEY` | unset | Optional proxy bearer token. |
| `GITHUB_TOKEN` | unset | Optional process-wide GitHub token for Copilot SDK auth. |
| `COPILOT_CLI_PATH` | embedded matched CLI when bundled, else SDK fallback | Advanced override for the Copilot CLI binary. |
| `COPILOT_DEFAULT_REASONING_EFFORT` | unset | Optional reasoning effort to use when a Chat Completions or Responses request omits `reasoning_effort`. When model metadata advertises supported efforts, unsupported defaults are rounded to the closest supported level; models without reasoning-effort support omit it. |
| `COPILOT_MODELS_CACHE_TTL` | `10m` | Successful model-list cache TTL. |
| `COPILOT_TOOL_CALL_TTL` | `5m` | Liveness guard for parked tool-call continuations. |
| `COPILOT_REQUEST_TIMEOUT` | `0` | Optional generation timeout; `0` disables proxy-imposed timeouts. |
| `COPILOT_MAX_REQUEST_BODY_BYTES` | `104857600` | Optional HTTP body cap; `0` disables the proxy-specific cap. Default is 100 MiB to leave room for base64-encoded image data while bounding memory use. |
| `COPILOT_API_DATA_DIR` | `$XDG_DATA_HOME/copilot-api` | Retained SDK session files and synthetic Chat histories. |
| `COPILOT_API_STATE_DIR` | `$XDG_STATE_HOME/copilot-api` | Lock file, response records, and pending metadata. |
| `COPILOT_API_CACHE_DIR` | `$XDG_CACHE_HOME/copilot-api` | Model cache and transient cache files. |
| `COPILOT_API_CONFIG_DIR` | `$XDG_CONFIG_HOME/copilot-api` | Isolated Copilot SDK config dir. |
| `COPILOT_STRICT_COMPAT` | `false` | Reject harmless unsupported OpenAI fields that permissive mode normally ignores; useful for debugging client conformance. Unsafe unsupported semantics are always rejected. |
| `COPILOT_LOG_CONTENT` | `false` | Reserved for future guarded content logging. The current server still avoids logging prompts, responses, tool arguments, tool outputs, auth headers, and image data by default. |
| `COPILOT_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. Request metadata is logged at this level; 4xx responses log at warn and 5xx responses at error. Generation request logs include the requested `model` field and `reasoning_effort` when supplied. |

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

Reasoning effort is a top-level request field. Values are model-dependent; inspect `GET /v1/models` for `supported_reasoning_efforts` and `default_reasoning_effort` metadata when the Copilot SDK provides it. For clients that cannot send a request effort, set `COPILOT_DEFAULT_REASONING_EFFORT`; explicit request values still take precedence. For Responses requests, use `reasoning_effort` instead of a nested `reasoning` object.

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

const client = new OpenAI({ baseURL: "http://127.0.0.1:8080/v1", apiKey: "local-secret" });
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

If the model calls the tool, the response contains `message.tool_calls`. Execute the tool in your client and submit the output before `COPILOT_TOOL_CALL_TTL` expires:

```json
{
  "model": "gpt-5.5",
  "messages": [
    {"role":"user","content":"Use get_weather for Paris."},
    {"role":"assistant","tool_calls":[{"id":"call_...","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
    {"role":"tool","tool_call_id":"call_...","content":"{\"temperature\":\"18C\"}"}
  ]
}
```

## State, locking, and purge

The proxy stores Copilot SDK session state and `metadata.json` under the configured XDG directories. Files are retained forever by default for debugging, including when Responses `store:false` disables API-visible retrieval.

A server lock in the state directory prevents two servers from sharing the same store. Manual cleanup is explicit:

```sh
copilot-api purge --dry-run
copilot-api purge --yes
```

`purge` refuses while the server lock is active.

## Embedded Copilot CLI

This repository is configured with the Copilot SDK bundler tool, but generated `cmd/copilot-api/zcopilot_*` bundle artifacts are intentionally ignored and not committed. Run the bundler during release/package builds when you want the binary to include the SDK-matched Copilot CLI:

```sh
go tool bundler --output cmd/copilot-api
go build ./cmd/copilot-api
```

Docker builds run the bundler inside the build stage. Local development can skip bundling and use `COPILOT_CLI_PATH` or `copilot` on `PATH`, but that weakens SDK/CLI version matching.

Do not upgrade the SDK/CLI unless the hydration, prompt-isolation, tool disablement, tool-choice, and provider-shape tests/spikes have been re-run.

## Development

```sh
go test ./...
go vet ./...
```

Live Copilot integration checks should be gated by `COPILOT_API_LIVE_TESTS=1` and are not part of the default test suite.

The Deno + AI SDK integration suite is also gated by default. Start a local server, then enable it explicitly:

```sh
COPILOT_API_AI_SDK_DENO_TESTS=1 \
COPILOT_API_BASE_URL=http://127.0.0.1:8080/v1 \
COPILOT_API_KEY=local-secret \
deno task test:ai-sdk
```

The suite covers Chat Completions, Responses, reasoning effort, multi-turn history, MCP-backed client tools, and image inputs. See [`tests/ai-sdk-deno/README.md`](tests/ai-sdk-deno/README.md) for configuration options.
