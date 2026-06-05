import { createMCPClient } from "@ai-sdk/mcp";
import { Experimental_StdioMCPTransport } from "@ai-sdk/mcp/mcp-stdio";
import { createOpenAI } from "@ai-sdk/openai";
import { assert, assertEquals } from "@std/assert";
import { createWebSocketFetch } from "@vercel/ai-sdk-openai-websocket-fetch";
import {
  generateText,
  stepCountIs,
  streamText,
  type TextStreamPart,
  type ToolSet,
} from "ai";

const ENABLED = env("COPILOT_API_AI_SDK_DENO_TESTS") === "1";
const DEFAULT_BASE_URL = "http://127.0.0.1:8080/v1";
const DEFAULT_TIMEOUT_MS = 120_000;
const DEFAULT_REASONING_EFFORT = "low";
// A 128x128 RGB PNG with colored quadrants and black diagonals. Avoid using a
// 1x1 fixture here: Copilot's upstream image processor rejects images that are
// valid PNGs but too small to process as vision input.
const TEST_PNG_BASE64 =
  "iVBORw0KGgoAAAANSUhEUgAAAIAAAACACAIAAABMXPacAAADXElEQVR42u3cy5ETQRBF0WfEGCEjMAJzMAxLWI8b7BEoGESr1aquys+7BBm5qG3esy+9/Zr3ywW6ly/v0L2V19vv+Q9QX/8nwPUGtAG6/vWt2xlcA3T9PwBcA3T9vwCgBuj6WwCiAbr+DgDOAF1/H4BlgK7/FABkgK5/BEAxQNd/AYAwQNd/DeBvgK4/BGBugK4/CuBsgK5/AsDWAF3/HICnAbr+aQBDA3T9GQA3A3T9SQArA3T9eQAfA3T9JQATA3T9VQAHA3T9AIB2A3T9GIBeA3T9MIBGA3T9SIAuA3T9YIAWA3T9eIB6A3T9FIBiA3T9LIBKA3T9RIAyA3T9XIAaA3T9dIACA3T9CoBsA3T9IoBUA3T9OoA8A3T9UoAkA3T9aoAMA3T9BoBwA3T9HoBYA3T9NoBAA3T9ToAoA3T9ZoAQA3T9foB1A3R9C4BFA3R9F4AVA3R9I4BpA3R9L4A5A3R9O4AJA3R9R4CzBuj6pgCnDND1fQHGDdD1rQEGDdD13QFGDND1AQAvDdD1GQDHBuj6GIADA3R9EsAzA3R9GMCuAbo+D+DRAF0fCbAxQNenAtwboOuDATYG3E/sdfn6Gbr3AN+/fYKu/oH6aAOh61/f1xvQBkLXvwGgDYSu/wHANRC6/j0A1EDo+hsAooHQ9R8BcAZC198FYBkIXf8ZAMhA6PoHABQDoesfAyAMhK7/EsDfQOj6IwDmBkLXHwRwNhC6/jiArYHQ9U8BeBoIXf8sgKGB0PUnANwMhK4/B2BlIHT9aQAfA6HrrwCYGAhdfxHAwUDo+usA7QZC1w8B6DUQun4UQKOB0PUDAboMhK4fC9BiIHT9cIB6A6HrZwAUGwhdPwmg0kDo+nkAZQZC108FqDEQun42QIGB0PULALINhK5fA5BqIHT9MoA8A6HrVwIkGQhdvxggw0Do+vUA4QZC128BiDUQun4XQKCB0PUbAaIMhK7fCxBiIHT9doB1A6HrOwAsGghd3wRgxUDo+j4A0wZC17cCmDMQur4bwISB0PUNAc4aCF3fE+CUgdD1bQHGDYSu7wwwaCB0fXOAEQOh6/sDvDQQuj4C4NhA6PoUgAMDoeuDAJ4ZCF2fBbBrIHR9HMCjgdD1iQAbA6HrQwHuDYSuzwX4MPgBQm3Rxp2JZFMAAAAASUVORK5CYII=";

type ModelInfo = {
  id?: unknown;
  metadata?: Record<string, unknown>;
};

type ModelList = {
  object?: string;
  data?: ModelInfo[];
};

type TestConfig = {
  apiKey: string;
  model?: string;
  openai: ReturnType<typeof createOpenAI>;
  reasoningModel?: string;
  serviceBaseURL: string;
  timeoutMs: number;
  v1BaseURL: string;
  visionModel?: string;
};

function env(name: string): string | undefined {
  try {
    return Deno.env.get(name) ?? undefined;
  } catch {
    return undefined;
  }
}

function loadConfig(): TestConfig {
  const v1BaseURL = normalizeV1BaseURL(
    env("COPILOT_API_BASE_URL") ?? DEFAULT_BASE_URL,
  );
  const apiKey = env("COPILOT_API_TEST_API_KEY") ?? env("COPILOT_API_KEY") ??
    "not-needed";
  return {
    apiKey,
    model: env("COPILOT_API_TEST_MODEL"),
    openai: createOpenAI({
      apiKey,
      baseURL: v1BaseURL,
      name: "copilot-api",
    }),
    reasoningModel: env("COPILOT_API_TEST_REASONING_MODEL"),
    serviceBaseURL: serviceBaseURLFrom(v1BaseURL),
    timeoutMs: parseTimeout(env("COPILOT_API_TEST_TIMEOUT_MS")),
    v1BaseURL,
    visionModel: env("COPILOT_API_TEST_VISION_MODEL"),
  };
}

function normalizeV1BaseURL(raw: string): string {
  const url = new URL(raw);
  const path = url.pathname.replace(/\/+$/, "");
  url.pathname = path.endsWith("/v1") ? path : `${path}/v1`;
  return url.toString().replace(/\/$/, "");
}

function serviceBaseURLFrom(v1BaseURL: string): string {
  const url = new URL(v1BaseURL);
  const path = url.pathname.replace(/\/+$/, "").replace(/\/v1$/, "");
  url.pathname = path === "" ? "/" : path;
  return url.toString().replace(/\/$/, "");
}

function websocketResponsesURLFrom(v1BaseURL: string): string {
  const url = new URL(v1BaseURL);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = `${url.pathname.replace(/\/+$/, "")}/responses`;
  return url.toString();
}

function openAIWithWebSocketFetch(config: TestConfig): {
  openai: ReturnType<typeof createOpenAI>;
  close: () => void;
} {
  const wsFetch = createWebSocketFetch({
    url: websocketResponsesURLFrom(config.v1BaseURL),
  });
  return {
    openai: createOpenAI({
      apiKey: config.apiKey,
      baseURL: config.v1BaseURL,
      fetch: wsFetch as unknown as typeof fetch,
      name: "copilot-api-ws",
    }),
    close: () => wsFetch.close(),
  };
}

function parseTimeout(raw: string | undefined): number {
  if (raw == null || raw.trim() === "") {
    return DEFAULT_TIMEOUT_MS;
  }
  const parsed = Number(raw);
  if (!Number.isFinite(parsed) || parsed <= 0) {
    throw new Error(
      `COPILOT_API_TEST_TIMEOUT_MS must be a positive number: ${raw}`,
    );
  }
  return parsed;
}

function authHeaders(config: TestConfig): Headers {
  const headers = new Headers();
  headers.set("Authorization", `Bearer ${config.apiKey}`);
  headers.set("Accept", "application/json");
  return headers;
}

async function fetchJSON<T>(
  config: TestConfig,
  url: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  for (const [key, value] of authHeaders(config)) {
    headers.set(key, value);
  }
  const response = await fetch(url, {
    ...init,
    headers,
    signal: AbortSignal.timeout(config.timeoutMs),
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(
      `${init.method ?? "GET"} ${url} failed with ${response.status}: ${body}`,
    );
  }
  return (body === "" ? undefined : JSON.parse(body)) as T;
}

let modelListPromise: Promise<ModelList> | undefined;
let selectedModelPromise: Promise<string> | undefined;

async function listedModels(config: TestConfig): Promise<ModelList> {
  modelListPromise ??= fetchJSON<ModelList>(
    config,
    `${config.v1BaseURL}/models`,
  );
  return await modelListPromise;
}

async function selectedModel(config: TestConfig): Promise<string> {
  if (config.model != null && config.model !== "") {
    return config.model;
  }
  if (selectedModelPromise == null) {
    selectedModelPromise = (async () => {
      const models = await listedModels(config);
      const id = models.data?.find(
        (item) => typeof item.id === "string" && item.id.length > 0,
      )?.id;
      assert(
        typeof id === "string" && id.length > 0,
        "GET /v1/models returned no usable model ids",
      );
      return id;
    })();
  }
  return await selectedModelPromise;
}

async function selectedReasoningModel(
  config: TestConfig,
  effort: string,
): Promise<string | undefined> {
  if (config.reasoningModel != null && config.reasoningModel !== "") {
    return config.reasoningModel;
  }
  if (config.model != null && config.model !== "") {
    return config.model;
  }
  const models = await listedModels(config);
  const model = models.data?.find((item) => {
    const efforts = item.metadata?.supported_reasoning_efforts;
    return (
      typeof item.id === "string" &&
      Array.isArray(efforts) &&
      efforts.includes(effort)
    );
  });
  return typeof model?.id === "string" ? model.id : undefined;
}

async function selectedVisionModel(
  config: TestConfig,
): Promise<string | undefined> {
  if (config.visionModel != null && config.visionModel !== "") {
    return config.visionModel;
  }
  if (config.model != null && config.model !== "") {
    return config.model;
  }
  const models = await listedModels(config);
  const model = models.data?.find((item) => {
    const meta = item.metadata;
    const supports = meta?.supports_vision === true ||
      (meta?.capabilities as { supports?: { vision?: unknown } } | undefined)
          ?.supports?.vision === true;
    return typeof item.id === "string" && supports;
  });
  return typeof model?.id === "string" ? model.id : undefined;
}

function assertNonEmptyText(text: string, label: string) {
  assert(text.trim().length > 0, `${label} returned empty text`);
}

type UsageShape = {
  inputTokens?: number | undefined;
  outputTokens?: number | undefined;
  totalTokens?: number | undefined;
};

function assertUsage(usage: UsageShape | undefined, label: string) {
  assert(usage != null, `${label} did not return a usage object`);
  const input = usage.inputTokens ?? 0;
  const output = usage.outputTokens ?? 0;
  const total = usage.totalTokens ?? 0;
  assert(
    total > 0 || input > 0 || output > 0,
    `${label} usage had no positive token counts: ${JSON.stringify(usage)}`,
  );
}

function responseIdFromMetadata(meta: unknown): string | undefined {
  if (meta == null || typeof meta !== "object") return undefined;
  const openai = (meta as Record<string, unknown>).openai;
  if (openai == null || typeof openai !== "object") return undefined;
  const id = (openai as Record<string, unknown>).responseId;
  return typeof id === "string" && id.length > 0 ? id : undefined;
}

async function responseIdFromStreamResult(
  result: unknown,
): Promise<string | undefined> {
  const stream = result as {
    providerMetadata?: Promise<unknown>;
    response?: Promise<{ id?: string }>;
  };
  const meta = stream.providerMetadata == null
    ? undefined
    : await stream.providerMetadata;
  return responseIdFromMetadata(meta) ?? (await stream.response)?.id;
}

function mentionsAny(text: string, needles: string[]): boolean {
  const lower = text.toLowerCase();
  return needles.some((needle) => lower.includes(needle.toLowerCase()));
}

type CollectedStream = {
  text: string;
  toolCalls: Array<{
    toolCallId: string;
    toolName: string;
    input: unknown;
  }>;
  toolInputStarts: Array<{ id: string; toolName: string }>;
  toolInputDeltas: number;
  finishParts: number;
};

async function collectFullStream(
  stream: AsyncIterable<TextStreamPart<ToolSet>>,
): Promise<CollectedStream> {
  const collected: CollectedStream = {
    text: "",
    toolCalls: [],
    toolInputStarts: [],
    toolInputDeltas: 0,
    finishParts: 0,
  };
  for await (const part of stream) {
    switch (part.type) {
      case "text-delta":
        collected.text += part.text;
        break;
      case "tool-call":
        collected.toolCalls.push({
          toolCallId: part.toolCallId,
          toolName: part.toolName,
          input: part.input,
        });
        break;
      case "tool-input-start":
        collected.toolInputStarts.push({
          id: part.id,
          toolName: part.toolName,
        });
        break;
      case "tool-input-delta":
        collected.toolInputDeltas += 1;
        break;
      case "finish":
        collected.finishParts += 1;
        break;
    }
  }
  return collected;
}

function testPngBytes(): Uint8Array {
  return Uint8Array.from(atob(TEST_PNG_BASE64), (char) => char.charCodeAt(0));
}

function uniqueCodeword(prefix: string): string {
  return `${prefix}-${crypto.randomUUID().slice(0, 8)}`;
}

type MCPTools = Awaited<
  ReturnType<Awaited<ReturnType<typeof createMCPClient>>["tools"]>
>;

async function withMCPTools<T>(
  fn: (tools: MCPTools) => Promise<T>,
): Promise<T> {
  const serverPath = decodeURIComponent(
    new URL("./mcp_server.ts", import.meta.url).pathname,
  );
  const mcpClient = await createMCPClient({
    transport: new Experimental_StdioMCPTransport({
      command: "deno",
      args: [
        "run",
        "--quiet",
        "--allow-env",
        "--allow-sys=hostname",
        serverPath,
      ],
    }),
  });

  try {
    const tools = await mcpClient.tools();
    assert(
      "echo_codeword" in tools,
      "MCP client did not expose the echo_codeword tool",
    );
    return await fn(tools);
  } finally {
    await mcpClient.close();
  }
}

function integrationTest(
  name: string,
  fn: (config: TestConfig) => Promise<void>,
) {
  Deno.test({
    name,
    ignore: !ENABLED,
    async fn() {
      await fn(loadConfig());
    },
  });
}

integrationTest(
  "copilot-api exposes health, readiness, and models endpoints",
  async (config) => {
    const health = await fetchJSON<Record<string, unknown>>(
      config,
      `${config.serviceBaseURL}/healthz`,
    );
    assertEquals(health.status, "ok");

    const ready = await fetchJSON<Record<string, unknown>>(
      config,
      `${config.serviceBaseURL}/readyz`,
    );
    assertEquals(
      ready.status,
      "ready",
      `GET /readyz returned ${JSON.stringify(ready)}`,
    );

    const models = await listedModels(config);
    assertEquals(models.object, "list");
    const model = await selectedModel(config);
    assert(
      models.data?.some((item) => item.id === model),
      `selected model ${model} was not present in GET /v1/models`,
    );
  },
);

integrationTest(
  "AI SDK chat generateText works against /v1/chat/completions",
  async (config) => {
    const model = await selectedModel(config);
    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.chat(model),
      prompt:
        "Reply with one short sentence confirming that chat completions work.",
    });

    assertNonEmptyText(result.text, "chat generateText");
    assert(
      result.finishReason != null,
      "chat generateText did not report a finish reason",
    );
  },
);

integrationTest(
  "AI SDK chat streamText emits usage and a stop finish reason",
  async (config) => {
    const model = await selectedModel(config);
    const result = streamText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.chat(model),
      prompt:
        "Reply with one short sentence confirming that chat streaming works.",
    });

    let text = "";
    for await (const delta of result.textStream) {
      text += delta;
    }

    assertNonEmptyText(text, "chat streamText");
    const finishReason = await result.finishReason;
    assertEquals(
      finishReason,
      "stop",
      `chat streamText finishReason = ${finishReason}, want stop`,
    );
    // Verifies the AI SDK can parse the proxy's stream_options.include_usage
    // terminal chunk shape; only a real client + real upstream can catch this.
    assertUsage(await result.usage, "chat streamText");
  },
);

integrationTest(
  "AI SDK responses generateText works against /v1/responses",
  async (config) => {
    const model = await selectedModel(config);
    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        "Reply with one short sentence confirming that the Responses API works.",
    });

    assertNonEmptyText(result.text, "responses generateText");
    assert(
      result.finishReason != null,
      "responses generateText did not report a finish reason",
    );
  },
);

integrationTest(
  "AI SDK responses streamText emits usage, finish reason, and SSE events",
  async (config) => {
    const model = await selectedModel(config);
    const result = streamText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        "Reply with one short sentence confirming that Responses streaming works.",
    });

    // Consuming fullStream forces the AI SDK Responses parser to accept the
    // entire response.created -> response.output_text.delta ->
    // response.completed event sequence, not just the text payload.
    const collected = await collectFullStream(result.fullStream);
    assertNonEmptyText(collected.text, "responses streamText");
    assert(
      collected.finishParts >= 1,
      "responses streamText did not surface a finish part",
    );
    const finishReason = await result.finishReason;
    assertEquals(
      finishReason,
      "stop",
      `responses streamText finishReason = ${finishReason}, want stop`,
    );
    assertUsage(await result.usage, "responses streamText");
  },
);

integrationTest(
  "AI SDK WebSocket transport streams Responses text",
  async (config) => {
    const model = await selectedModel(config);
    const ws = openAIWithWebSocketFetch(config);
    try {
      const result = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: ws.openai.responses(model),
        prompt:
          "Reply with one short sentence confirming that Responses WebSocket streaming works.",
      });

      const collected = await collectFullStream(result.fullStream);
      assertNonEmptyText(collected.text, "Responses WebSocket streamText");
      assert(
        collected.finishParts >= 1,
        "Responses WebSocket streamText did not surface a finish part",
      );
      assertEquals(await result.finishReason, "stop");
      assertUsage(await result.usage, "Responses WebSocket streamText");
    } finally {
      ws.close();
    }
  },
);

integrationTest(
  "AI SDK WebSocket transport accepts store:false Responses continuation on the same socket",
  async (config) => {
    const model = await selectedModel(config);
    const ws = openAIWithWebSocketFetch(config);
    try {
      const first = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: ws.openai.responses(model),
        prompt:
          "Reply with one short sentence acknowledging this first store:false WebSocket turn.",
        providerOptions: { openai: { store: false } },
      });
      const firstCollected = await collectFullStream(first.fullStream);
      assertNonEmptyText(firstCollected.text, "Responses WebSocket first turn");
      const responseId = await responseIdFromStreamResult(first);
      assert(
        typeof responseId === "string" && responseId.length > 0,
        "Responses WebSocket first turn did not surface a response id",
      );

      const second = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: ws.openai.responses(model),
        prompt:
          "Reply with one short sentence confirming this store:false continuation was accepted.",
        providerOptions: {
          openai: { previousResponseId: responseId, store: false },
        },
      });
      const secondCollected = await collectFullStream(second.fullStream);
      assert(
        secondCollected.finishParts >= 1,
        `Responses WebSocket store:false continuation did not finish: ${
          JSON.stringify(secondCollected)
        }`,
      );
      const secondResponseId = await responseIdFromStreamResult(second);
      assert(
        typeof secondResponseId === "string" && secondResponseId.length > 0,
        "Responses WebSocket continuation did not surface a response id",
      );
    } finally {
      ws.close();
    }
  },
);

integrationTest(
  "AI SDK WebSocket transport terminates cleanly on Responses errors",
  async (config) => {
    const model = await selectedModel(config);
    const ws = openAIWithWebSocketFetch(config);
    try {
      const result = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: ws.openai.responses(model),
        prompt: "This request should fail before generation.",
        providerOptions: {
          openai: {
            previousResponseId: `resp_missing_${crypto.randomUUID()}`,
            store: false,
          },
        },
      });
      // The WebSocket fetch shim treats top-level error frames as terminal and
      // may either emit an error part or reject while the stream is drained. A
      // clean close without an observed error must fail this test.
      let observedError = false;
      try {
        for await (const part of result.fullStream) {
          if ((part as { type?: string }).type === "error") {
            observedError = true;
          }
        }
        await result.finishReason;
      } catch (_err) {
        observedError = true;
      }
      assert(
        observedError,
        "Responses WebSocket missing previous_response_id closed without an observed terminal error",
      );
    } finally {
      ws.close();
    }
  },
);

integrationTest(
  "AI SDK WebSocket transport executes streaming Responses MCP tools",
  async (config) => {
    const model = await selectedModel(config);
    const ws = openAIWithWebSocketFetch(config);
    try {
      await withMCPTools(async (tools) => {
        const codeword = uniqueCodeword("ws-mcp-responses-stream");
        const result = streamText({
          abortSignal: AbortSignal.timeout(config.timeoutMs),
          model: ws.openai.responses(model),
          tools,
          stopWhen: stepCountIs(3),
          prompt:
            `Use the echo_codeword tool with codeword ${codeword}. After the tool runs, reply with only the tool result text.`,
        });
        const collected = await collectFullStream(result.fullStream);
        assert(
          collected.toolCalls.some((call) => call.toolName === "echo_codeword"),
          `WebSocket Responses MCP did not call echo_codeword: ${
            JSON.stringify(
              collected.toolCalls,
            )
          }`,
        );
        // Some live models stop after the tool result without emitting a final
        // assistant text delta. The important compatibility contract is that the
        // AI SDK WebSocket transport saw and executed the tool call.
      });
    } finally {
      ws.close();
    }
  },
);

integrationTest(
  "AI SDK WebSocket transport streams Responses image inputs",
  async (config) => {
    const model = await selectedVisionModel(config);
    if (model == null) {
      console.warn(
        "No model advertised supports_vision=true; set COPILOT_API_TEST_VISION_MODEL to force this test.",
      );
      return;
    }
    const ws = openAIWithWebSocketFetch(config);
    try {
      const result = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: ws.openai.responses(model),
        messages: [
          {
            role: "user",
            content: [
              {
                type: "text",
                text:
                  "The attached 128x128 PNG has four colored quadrants and two black diagonal lines forming an X. In one short sentence, name at least one color you see or mention the diagonal lines.",
              },
              {
                type: "image",
                image: testPngBytes(),
                mediaType: "image/png",
                providerOptions: { openai: { imageDetail: "low" } },
              },
            ],
          },
        ],
      });
      const collected = await collectFullStream(result.fullStream);
      assertNonEmptyText(collected.text, "Responses WebSocket image input");
      assert(
        mentionsAny(collected.text, [
          "red",
          "green",
          "blue",
          "yellow",
          "black",
          "diagonal",
          "cross",
          "quadrant",
          "x ",
          "x-shaped",
          "lines",
        ]),
        `Responses WebSocket vision response did not mention fixture details: ${
          JSON.stringify(
            collected.text,
          )
        }`,
      );
    } finally {
      ws.close();
    }
  },
);

integrationTest(
  "AI SDK forwards reasoning effort through the Responses API",
  async (config) => {
    const effort = env("COPILOT_API_TEST_REASONING_EFFORT") ??
      DEFAULT_REASONING_EFFORT;
    const model = await selectedReasoningModel(config, effort);
    if (model == null) {
      console.warn(
        `No model advertised supported_reasoning_efforts containing ${effort}; set COPILOT_API_TEST_REASONING_MODEL to force this test.`,
      );
      return;
    }

    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        "Use the requested reasoning effort and reply with one concise sentence about why 19 * 37 = 703.",
      providerOptions: {
        openai: {
          forceReasoning: true,
          reasoningEffort: effort,
          store: false,
        },
      },
    });

    assertNonEmptyText(result.text, "reasoning effort generateText");
    assert(
      result.finishReason != null,
      "reasoning effort generateText did not report a finish reason",
    );
    // Reasoning models should expose reasoning output through one of:
    // (a) AI SDK reasoning parts, (b) reasoningText, or (c) usage.reasoningTokens.
    // Failing all three indicates the proxy dropped the upstream reasoning signal.
    const reasoningText = (result as { reasoningText?: string }).reasoningText;
    const reasoningParts =
      (result as { reasoning?: Array<unknown> }).reasoning ?? [];
    const reasoningTokens =
      (result.usage as { reasoningTokens?: number } | undefined)
        ?.reasoningTokens ?? 0;
    assert(
      (reasoningText != null && reasoningText.length > 0) ||
        reasoningParts.length > 0 ||
        reasoningTokens > 0,
      `reasoning effort generateText surfaced no reasoning signal (reasoningText=${
        JSON.stringify(
          reasoningText,
        )
      }, reasoningParts=${reasoningParts.length}, reasoningTokens=${reasoningTokens}, usage=${
        JSON.stringify(
          result.usage,
        )
      })`,
    );
  },
);

integrationTest(
  "AI SDK Responses API continues conversations through previous_response_id",
  async (config) => {
    const model = await selectedModel(config);
    const codeword = uniqueCodeword("resp-prev");

    const first = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        `Remember this exact codeword for the next turn: ${codeword}. Reply with one short sentence acknowledging it.`,
      providerOptions: { openai: { store: true } },
    });
    assertNonEmptyText(first.text, "previous_response_id first turn");

    const responseId = responseIdFromMetadata(first.providerMetadata) ??
      first.response?.id;
    assert(
      typeof responseId === "string" && responseId.length > 0,
      `first turn did not surface a Responses API id (providerMetadata=${
        JSON.stringify(
          first.providerMetadata,
        )
      }, response=${JSON.stringify(first.response)})`,
    );

    const second = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        "Reply with only the exact codeword from my previous message, no other text.",
      providerOptions: {
        openai: { previousResponseId: responseId, store: false },
      },
    });

    assert(
      second.text.includes(codeword),
      `previous_response_id continuation lost the codeword: text=${
        JSON.stringify(
          second.text,
        )
      }, providerMetadata=${JSON.stringify(second.providerMetadata)}`,
    );
  },
);

integrationTest(
  "AI SDK multi-turn chat preserves prior conversation messages",
  async (config) => {
    const model = await selectedModel(config);
    const codeword = uniqueCodeword("turn");
    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.chat(model),
      messages: [
        {
          role: "user",
          content:
            `Remember this exact codeword for the next turn: ${codeword}`,
        },
        {
          role: "assistant",
          content: `I will remember the exact codeword ${codeword}.`,
        },
        {
          role: "user",
          content:
            "Reply with only the exact codeword from the previous assistant message.",
        },
      ],
    });

    assert(
      result.text.includes(codeword),
      `multi-turn response ${
        JSON.stringify(
          result.text,
        )
      } did not include ${codeword}`,
    );
  },
);

integrationTest(
  "AI SDK MCP tools execute through chat completions",
  async (config) => {
    const model = await selectedModel(config);
    await withMCPTools(async (tools) => {
      const codeword = uniqueCodeword("mcp-chat");
      const result = await generateText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: config.openai.chat(model),
        tools,
        stopWhen: stepCountIs(3),
        prompt:
          `Use the echo_codeword tool with codeword ${codeword}. After the tool runs, reply with only the tool result text.`,
      });

      const serializedSteps = JSON.stringify(result.steps);
      assert(
        serializedSteps.includes(`MCP_OK:${codeword}`),
        `MCP tool result was not observed in AI SDK steps: ${serializedSteps}`,
      );
      // Some live models stop after the tool result without emitting final text;
      // the serialized steps assertion above proves tool execution completed.
    });
  },
);

integrationTest(
  "AI SDK streaming MCP tools execute through chat completions",
  async (config) => {
    const model = await selectedModel(config);
    await withMCPTools(async (tools) => {
      const codeword = uniqueCodeword("mcp-chat-stream");
      const result = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: config.openai.chat(model),
        tools,
        stopWhen: stepCountIs(3),
        prompt:
          `Use the echo_codeword tool with codeword ${codeword}. After the tool runs, reply with only the tool result text.`,
      });

      // Consume fullStream so the AI SDK has to reassemble streamed tool-call
      // argument deltas into a complete tool-call part; the proxy's chat
      // tool-call delta format is only exercised on this path.
      const collected = await collectFullStream(result.fullStream);
      assert(
        collected.toolCalls.length >= 1,
        `streaming chat MCP did not surface any reassembled tool calls: ${
          JSON.stringify(
            collected,
          )
        }`,
      );
      const echoCall = collected.toolCalls.find(
        (call) => call.toolName === "echo_codeword",
      );
      assert(
        echoCall != null,
        `streaming chat MCP did not call echo_codeword: ${
          JSON.stringify(
            collected.toolCalls,
          )
        }`,
      );
      const args = echoCall.input as { codeword?: unknown } | undefined;
      assertEquals(
        args?.codeword,
        codeword,
        `reassembled tool call args were wrong: ${JSON.stringify(echoCall)}`,
      );
      // Some live models stop after the tool result without emitting final text;
      // the tool-call assertions above prove streaming tool execution completed.
    });
  },
);

integrationTest(
  "AI SDK MCP tools execute through the Responses API",
  async (config) => {
    const model = await selectedModel(config);
    await withMCPTools(async (tools) => {
      const codeword = uniqueCodeword("mcp-responses");
      const result = await generateText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: config.openai.responses(model),
        tools,
        stopWhen: stepCountIs(3),
        prompt:
          `Use the echo_codeword tool with codeword ${codeword}. After the tool runs, reply with only the tool result text.`,
      });

      const serializedSteps = JSON.stringify(result.steps);
      assert(
        serializedSteps.includes(`MCP_OK:${codeword}`),
        `Responses MCP tool result was not observed in AI SDK steps: ${serializedSteps}`,
      );
      // Some live models stop after the tool result without emitting final text;
      // the serialized steps assertion above proves tool execution completed.
    });
  },
);

integrationTest(
  "AI SDK streaming MCP tools execute through the Responses API",
  async (config) => {
    const model = await selectedModel(config);
    await withMCPTools(async (tools) => {
      const codeword = uniqueCodeword("mcp-responses-stream");
      const result = streamText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: config.openai.responses(model),
        tools,
        stopWhen: stepCountIs(3),
        prompt:
          `Use the echo_codeword tool with codeword ${codeword}. After the tool runs, reply with only the tool result text.`,
      });

      // Same reasoning as the streaming chat MCP test: fullStream proves the
      // AI SDK Responses parser can assemble response.function_call_arguments.*
      // events into a structured tool-call part.
      const collected = await collectFullStream(result.fullStream);
      assert(
        collected.toolCalls.length >= 1,
        `streaming Responses MCP did not surface any reassembled tool calls: ${
          JSON.stringify(
            collected,
          )
        }`,
      );
      const echoCall = collected.toolCalls.find(
        (call) => call.toolName === "echo_codeword",
      );
      assert(
        echoCall != null,
        `streaming Responses MCP did not call echo_codeword: ${
          JSON.stringify(
            collected.toolCalls,
          )
        }`,
      );
      const args = echoCall.input as { codeword?: unknown } | undefined;
      assertEquals(
        args?.codeword,
        codeword,
        `reassembled tool call args were wrong: ${JSON.stringify(echoCall)}`,
      );
      // Some live models stop after the tool result without emitting final text;
      // the tool-call assertions above prove streaming tool execution completed.
    });
  },
);

integrationTest(
  "AI SDK tool_choice=none prevents the model from invoking registered tools",
  async (config) => {
    const model = await selectedModel(config);
    await withMCPTools(async (tools) => {
      const codeword = uniqueCodeword("tool-choice-none");
      const result = await generateText({
        abortSignal: AbortSignal.timeout(config.timeoutMs),
        model: config.openai.chat(model),
        tools,
        toolChoice: "none",
        stopWhen: stepCountIs(2),
        prompt:
          `An MCP tool called echo_codeword is registered. Do NOT call it. Instead, reply with one short sentence that mentions the codeword ${codeword} verbatim.`,
      });

      const allToolCalls = result.steps.flatMap((step) => step.toolCalls);
      assertEquals(
        allToolCalls.length,
        0,
        `tool_choice=none was not honored; tool calls observed: ${
          JSON.stringify(
            allToolCalls,
          )
        }`,
      );
      assertNonEmptyText(result.text, "tool_choice=none generateText");
      assert(
        result.text.includes(codeword),
        `tool_choice=none response should still address the prompt: ${
          JSON.stringify(
            result.text,
          )
        }`,
      );
    });
  },
);

integrationTest(
  "AI SDK image inputs upload through chat completions",
  async (config) => {
    const model = await selectedVisionModel(config);
    if (model == null) {
      console.warn(
        "No model advertised supports_vision=true; set COPILOT_API_TEST_VISION_MODEL to force this test.",
      );
      return;
    }

    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.chat(model),
      messages: [
        {
          role: "user",
          content: [
            {
              type: "text",
              text:
                "The attached 128x128 PNG has four colored quadrants and two black diagonal lines forming an X. In one short sentence, name at least one color you see or mention the diagonal lines.",
            },
            {
              type: "image",
              image: testPngBytes(),
              mediaType: "image/png",
              providerOptions: { openai: { imageDetail: "low" } },
            },
          ],
        },
      ],
    });

    assertNonEmptyText(result.text, "chat image input generateText");
    // A model that never actually received the image tends to either refuse
    // or give a generic "I see an image" answer. Requiring at least one
    // color or shape word forces real vision pipeline coverage.
    const visionKeywords = [
      "red",
      "green",
      "blue",
      "yellow",
      "orange",
      "purple",
      "magenta",
      "cyan",
      "pink",
      "black",
      "white",
      "diagonal",
      "cross",
      "quadrant",
      "square",
      "x ",
      "x-shaped",
      "lines",
    ];
    assert(
      mentionsAny(result.text, visionKeywords),
      `chat vision response did not mention any color or shape from the fixture: ${
        JSON.stringify(
          result.text,
        )
      }`,
    );
  },
);

integrationTest(
  "AI SDK image inputs upload through the Responses API",
  async (config) => {
    const model = await selectedVisionModel(config);
    if (model == null) {
      console.warn(
        "No model advertised supports_vision=true; set COPILOT_API_TEST_VISION_MODEL to force this test.",
      );
      return;
    }

    const result = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      messages: [
        {
          role: "user",
          content: [
            {
              type: "text",
              text:
                "The attached 128x128 PNG has four colored quadrants and two black diagonal lines forming an X. In one short sentence, name at least one color you see or mention the diagonal lines.",
            },
            {
              type: "image",
              image: testPngBytes(),
              mediaType: "image/png",
              providerOptions: { openai: { imageDetail: "low" } },
            },
          ],
        },
      ],
    });

    assertNonEmptyText(result.text, "Responses image input generateText");
    assert(
      result.finishReason != null,
      "Responses image input did not report a finish reason",
    );
    const visionKeywords = [
      "red",
      "green",
      "blue",
      "yellow",
      "orange",
      "purple",
      "magenta",
      "cyan",
      "pink",
      "black",
      "white",
      "diagonal",
      "cross",
      "quadrant",
      "square",
      "x ",
      "x-shaped",
      "lines",
    ];
    assert(
      mentionsAny(result.text, visionKeywords),
      `Responses vision response did not mention any color or shape from the fixture: ${
        JSON.stringify(
          result.text,
        )
      }`,
    );
  },
);

integrationTest(
  "AI SDK mid-stream abort lets the server accept a follow-up request",
  async (config) => {
    const model = await selectedModel(config);
    const controller = new AbortController();
    const stream = streamText({
      abortSignal: controller.signal,
      model: config.openai.chat(model),
      prompt:
        "Slowly count from 1 to 60. Put each number on its own line. Do not stop early.",
    });

    let observed = 0;
    try {
      for await (const _ of stream.textStream) {
        observed += 1;
        if (observed >= 1) {
          controller.abort();
          break;
        }
      }
    } catch {
      // The AI SDK surfaces aborts as a thrown error from the iterator; that is
      // exactly the path we want to exercise to prove the server cleans up.
    }
    // Drain any rejected promises hanging off the stream so resources release.
    await Promise.allSettled([stream.text, stream.finishReason, stream.usage]);

    // The proxy must still service new requests after the cancelled stream;
    // a deadlocked tool-call park or leaked SDK session would manifest here.
    const followup = await generateText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.chat(model),
      prompt: "Reply with the single word READY and nothing else.",
    });
    assertNonEmptyText(followup.text, "follow-up after mid-stream abort");
  },
);

Deno.test({
  name: "AI SDK Deno integration suite is disabled by default",
  ignore: ENABLED,
  fn() {
    assertEquals(ENABLED, false);
  },
});
