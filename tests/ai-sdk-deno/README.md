# AI SDK + Deno integration tests

This suite exercises `copilot-api` through the Vercel AI SDK running on Deno.
Its unique value over the Go unit tests is that it pairs the **real Vercel AI
SDK parser** with **real Copilot upstream**, so it catches wire-format
incompatibilities and behaviors that depend on a live model. Anything that can
be asserted with a fake gateway in Go belongs in a Go test, not here.

Coverage:

- `GET /healthz`, `GET /readyz`, and `GET /v1/models` discovery
- Chat Completions `generateText`
- Chat Completions `streamText` with assertions on `usage` (validates the
  `stream_options.include_usage` terminal chunk reaches the AI SDK) and the
  `stop` finish reason
- Responses API `generateText`
- Responses API `streamText` over HTTP/SSE and WebSocket transports, consumed
  via `fullStream` so the AI SDK Responses parser has to accept the full
  `response.created` → `response.in_progress` → `response.output_text.delta` →
  `response.output_text.done` → `response.completed` event sequence; asserts
  `usage` and `finishReason`
- Responses API reasoning effort via AI SDK provider options, with an assertion
  that the AI SDK surfaces reasoning output via `reasoningText`, reasoning
  parts, or `usage.reasoningTokens`
- Responses API `previous_response_id` continuation across two turns, including
  `store:false` continuation through the AI SDK WebSocket transport
- Multi-turn Chat Completions history
- MCP tools converted by the AI SDK MCP client into client-owned function tool
  calls for Chat and Responses, including Responses WebSocket streaming.
  Streaming variants iterate `fullStream` and assert the AI SDK reassembled
  streamed tool-call argument deltas into a structured `tool-call` part with the
  right `toolName` and parsed `input`
- `tool_choice: "none"` with a real model and registered tools (proves the proxy
  forwards the choice and the model honors it)
- Image inputs uploaded by the AI SDK as OpenAI-compatible image parts for Chat
  and Responses over HTTP/SSE and WebSocket, with assertions that the model
  mentions a color or shape from the fixture (not just "I see an image")
- WebSocket terminal error handling through the AI SDK transport, plus
  mid-stream abort followed by a fresh request (proves the proxy releases the
  cancelled upstream session and tool-call park, then serves new traffic)

The tests are gated so they are safe to run in normal development without a live
Copilot-backed server.

## Run

Start `copilot-api` in another shell first. The example uses an API key because
non-loopback binds require one and using a key also exercises the bearer-auth
path:

```sh
COPILOT_API_KEY=local-secret ./copilot-api serve
```

Then run the Deno integration suite:

```sh
COPILOT_API_AI_SDK_DENO_TESTS=1 \
COPILOT_API_BASE_URL=http://127.0.0.1:8080/v1 \
COPILOT_API_KEY=local-secret \
deno task test:ai-sdk
```

Optional environment variables:

| Variable                            | Default                                        | Purpose                                                                                                                |
| ----------------------------------- | ---------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `COPILOT_API_AI_SDK_DENO_TESTS`     | unset                                          | Must be `1` to enable live tests.                                                                                      |
| `COPILOT_API_BASE_URL`              | `http://127.0.0.1:8080/v1`                     | OpenAI-compatible base URL. The service root is also accepted; `/v1` is appended automatically.                        |
| `COPILOT_API_TEST_API_KEY`          | `COPILOT_API_KEY` or `not-needed`              | Bearer token sent by the AI SDK client.                                                                                |
| `COPILOT_API_TEST_MODEL`            | first model from `/v1/models`                  | Model ID to use for general text generation, multi-turn, and MCP tool tests.                                           |
| `COPILOT_API_TEST_REASONING_MODEL`  | first model advertising the requested effort   | Model ID to use for reasoning-effort tests. Set this if `/v1/models` does not advertise `supported_reasoning_efforts`. |
| `COPILOT_API_TEST_REASONING_EFFORT` | `low`                                          | Reasoning effort sent through AI SDK OpenAI provider options.                                                          |
| `COPILOT_API_TEST_VISION_MODEL`     | first model advertising `supports_vision=true` | Model ID to use for image-input tests. Set this if `/v1/models` does not advertise vision support.                     |
| `COPILOT_API_TEST_TIMEOUT_MS`       | `120000`                                       | Per-request timeout in milliseconds.                                                                                   |

`deno task test:ai-sdk` pins dependencies through the repository `deno.lock` and
grants the permissions needed by the AI SDK and local MCP test server on Deno
(`--allow-env`, `--allow-net`, `--allow-run=deno`, and `--allow-sys=hostname`).
