# AI SDK + Deno integration tests

This suite exercises `copilot-api` through the Vercel AI SDK running on Deno. It
covers:

- `/v1/models` discovery
- Chat Completions `generateText`
- Chat Completions `streamText`
- Responses API `generateText`
- Responses API `streamText`
- Responses API reasoning effort via AI SDK provider options
- Multi-turn Chat Completions history
- MCP tools converted by the AI SDK MCP client into client-owned function tool
  calls for Chat and Responses, both non-streaming and streaming
- Image inputs uploaded by the AI SDK as OpenAI-compatible image parts for Chat
  and Responses

The tests are gated so they are safe to run in normal development without a live
Copilot-backed server.

## Run

Start `copilot-api` in another shell first:

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
