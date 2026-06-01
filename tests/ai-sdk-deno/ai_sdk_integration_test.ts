import { createMCPClient } from "@ai-sdk/mcp";
import { Experimental_StdioMCPTransport } from "@ai-sdk/mcp/mcp-stdio";
import { createOpenAI } from "@ai-sdk/openai";
import { assert, assertEquals } from "@std/assert";
import { generateText, stepCountIs, streamText } from "ai";

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
      const id = models.data?.find((item) =>
        typeof item.id === "string" && item.id.length > 0
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
    return typeof item.id === "string" && Array.isArray(efforts) &&
      efforts.includes(effort);
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
      ((meta?.capabilities as { supports?: { vision?: unknown } } | undefined)
        ?.supports?.vision === true);
    return typeof item.id === "string" && supports;
  });
  return typeof model?.id === "string" ? model.id : undefined;
}

function assertNonEmptyText(text: string, label: string) {
  assert(text.trim().length > 0, `${label} returned empty text`);
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
  "copilot-api exposes health and models endpoints",
  async (config) => {
    const health = await fetchJSON<Record<string, unknown>>(
      config,
      `${config.serviceBaseURL}/healthz`,
    );
    assertEquals(health.status, "ok");

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
  "AI SDK chat streamText works against /v1/chat/completions",
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
    assert(
      await result.finishReason != null,
      "chat streamText did not report a finish reason",
    );
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
  "AI SDK responses streamText works against /v1/responses",
  async (config) => {
    const model = await selectedModel(config);
    const result = streamText({
      abortSignal: AbortSignal.timeout(config.timeoutMs),
      model: config.openai.responses(model),
      prompt:
        "Reply with one short sentence confirming that Responses streaming works.",
    });

    let text = "";
    for await (const delta of result.textStream) {
      text += delta;
    }

    assertNonEmptyText(text, "responses streamText");
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
        JSON.stringify(result.text)
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
      assert(
        result.text.includes(codeword),
        `MCP final text ${
          JSON.stringify(result.text)
        } did not include ${codeword}`,
      );
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

      let text = "";
      for await (const delta of result.textStream) {
        text += delta;
      }
      assert(
        text.includes(codeword),
        `streaming MCP final text ${
          JSON.stringify(text)
        } did not include ${codeword}`,
      );
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
      assert(
        result.text.includes(codeword),
        `Responses MCP final text ${
          JSON.stringify(result.text)
        } did not include ${codeword}`,
      );
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

      let text = "";
      for await (const delta of result.textStream) {
        text += delta;
      }
      assert(
        text.includes(codeword),
        `streaming Responses MCP final text ${
          JSON.stringify(text)
        } did not include ${codeword}`,
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
                "This message includes a 128 by 128 PNG image with colored regions. Reply with one concise sentence confirming that an image was attached.",
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
                "This message includes a 128 by 128 PNG image with colored regions. Reply with one concise sentence confirming that an image was attached.",
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
  },
);

Deno.test({
  name: "AI SDK Deno integration suite is disabled by default",
  ignore: ENABLED,
  fn() {
    assertEquals(ENABLED, false);
  },
});
