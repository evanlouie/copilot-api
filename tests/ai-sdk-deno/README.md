# AI SDK + Deno integration tests

This test suite tests `copilot-api` through the Vercel AI SDK on Deno. It gives
more than the Go unit tests, because it uses the **true Vercel AI SDK parser**
with the **true Copilot upstream server**. Thus it finds incompatibilities in
the wire format. It also finds behavior that depends on a live model. Write a Go
test for each condition that a fake gateway in Go can test. Do not write that
test here.

Coverage:

- `GET /healthz`, `GET /readyz`, and `GET /v1/models` discovery
- Chat Completions `generateText`
- Chat Completions `streamText` with assertions on `usage` (this checks that the
  last chunk of `stream_options.include_usage` comes to the AI SDK) and on the
  `stop` finish reason
- Responses API `generateText`
- Responses API `streamText` on the HTTP/SSE transport and on the WebSocket
  transport. The test reads the result with `fullStream`. Thus the AI SDK
  Responses parser must accept the full event sequence `response.created` →
  `response.in_progress` → `response.output_text.delta` →
  `response.output_text.done` → `response.completed`. The test asserts `usage`
  and `finishReason`
- Responses API reasoning effort with the AI SDK provider options, and with the
  `model:effort` selector (with no `reasoningEffort` provider options). The test
  asserts that the AI SDK shows the reasoning output in `reasoningText`, in the
  reasoning parts, or in `usage.reasoningTokens`
- Responses API `previous_response_id` continuation on two turns. This includes
  the `store:false` continuation through the AI SDK WebSocket transport
- Multi-turn Chat Completions history
- MCP tools that the AI SDK MCP client changes into function tool calls that the
  client owns, for Chat and for Responses. This includes the Responses WebSocket
  streaming. The streaming tests read `fullStream`. They assert that the AI SDK
  assembled the streamed deltas of the tool-call arguments again into a
  structured `tool-call` part with the correct `toolName` and the parsed `input`
- `tool_choice: "none"` with a true model and registered tools (this shows that
  the proxy sends the choice, and that the model obeys it)
- Image inputs that the AI SDK sends as OpenAI-compatible image parts, for Chat
  and for Responses, on HTTP/SSE and on WebSocket. The test asserts that the
  model gives a color or a shape from the fixture. A reply such as "I see an
  image" is not sufficient
- The control of a terminal WebSocket error through the AI SDK transport. The
  test also stops a stream in the middle, and then sends a new request. This
  shows that the proxy releases the cancelled upstream session and the parked
  tool call. The proxy then processes new traffic.

The tests do not start if you do not enable them. Thus it is safe to run them in
usual development without a live server that connects to Copilot.

## Run

Start `copilot-api` in a different shell first. The example uses an API key,
because a bind to a non-loopback address needs a key. A key also tests the path
of the bearer authentication:

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

| Variable                            | Default                                     | Purpose                                                                                                        |
| ----------------------------------- | ------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `COPILOT_API_AI_SDK_DENO_TESTS`     | unset                                       | Must be `1` to enable the live tests.                                                                          |
| `COPILOT_API_BASE_URL`              | `http://127.0.0.1:8080/v1`                  | OpenAI-compatible base URL. The service root is also accepted. The server adds `/v1` automatically.            |
| `COPILOT_API_TEST_API_KEY`          | `COPILOT_API_KEY` or `not-needed`           | Bearer token that the AI SDK client sends.                                                                     |
| `COPILOT_API_TEST_MODEL`            | first model from `/v1/models`               | Model ID for general text generation, for multi-turn and for the MCP tool tests.                               |
| `COPILOT_API_TEST_REASONING_MODEL`  | first model that gives the requested effort | Model ID for the reasoning-effort tests. Set this if `/v1/models` does not give `supported_reasoning_efforts`. |
| `COPILOT_API_TEST_REASONING_EFFORT` | `low`                                       | Reasoning effort that goes through the AI SDK OpenAI provider options and the model-selector suffix case.      |
| `COPILOT_API_TEST_VISION_MODEL`     | first model with `supports_vision=true`     | Model ID for the image-input tests. Set this if `/v1/models` does not give the vision support.                 |
| `COPILOT_API_TEST_TIMEOUT_MS`       | `120000`                                    | Timeout of one request, in milliseconds.                                                                       |

`deno task test:ai-sdk` holds the dependencies at the versions in the repository
file `deno.lock`. It also gives the permissions that the AI SDK and the local
MCP test server need on Deno. The permissions are `--allow-env`, `--allow-net`,
`--allow-run=deno` and `--allow-sys=hostname`.
