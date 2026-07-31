import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

const server = new Server(
  { name: "copilot-api-ai-sdk-deno-test", version: "1.0.0" },
  { capabilities: { tools: {} } },
);

server.setRequestHandler(ListToolsRequestSchema, () => ({
  tools: [
    {
      name: "echo_codeword",
      description:
        "Echoes a codeword with an MCP_OK prefix. Use this when the user asks for the MCP codeword echo.",
      inputSchema: {
        type: "object",
        properties: {
          codeword: {
            type: "string",
            description: "The codeword to echo back.",
          },
        },
        required: ["codeword"],
        additionalProperties: false,
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, (request: {
  params: { name: string; arguments?: unknown };
}) => {
  if (request.params.name !== "echo_codeword") {
    throw new Error(`Unknown tool: ${request.params.name}`);
  }
  const args = request.params.arguments as { codeword?: unknown } | undefined;
  const codeword = typeof args?.codeword === "string" ? args.codeword : "";
  return {
    content: [{ type: "text", text: `MCP_OK:${codeword}` }],
  };
});

await server.connect(new StdioServerTransport());
