# Spike: Copilot SDK tool_choice enforcement

Date: 2026-05-29

Current v1 readiness note: the implementation now targets `github.com/github/copilot-sdk/go v1.0.0`. The v1 generated RPC shape includes a lower-level `RequiredTool` availability check, but the public `MessageOptions` path used by this service does not expose it, and it still is not an OpenAI-compatible way to force the model to call a function or require tool use for a turn.

## Verdict

**The Copilot SDK/runtime does not expose or send true OpenAI `tool_choice` / `function_call` / required-tool-use controls.**

What is supportable with evidence:

- **`auto`**: yes, by exposing the desired tools and letting the model decide. The provider request does not include `tool_choice`; this is effectively provider/model default behavior.
- **`none`**: yes, but by **withholding all tools** (`tools: []`) rather than by sending `tool_choice: "none"`.
- **forced specific function**: only a **best-effort approximation** is possible by registering/exposing only the selected tool alias. This does not force a tool call; live tests showed the model can still answer directly.
- **`required`**: not enforceable through SDK/runtime. Live tests showed the model can answer directly when tools are available.
- **`parallel_tool_calls: false`**:
  - Chat Completions wire path: captured requests include `"parallel_tool_calls": false`, apparently hard-coded by the CLI.
  - Responses wire path: captured request included `"parallel_tool_calls": true`; no SDK field was found to set it false.

Recommended MVP stance: claim support for `auto` and `none`; treat forced specific and `required` as unsupported unless the API deliberately documents a non-OpenAI-compatible best-effort approximation.

## SDK / RPC source findings

Searched `github.com/github/copilot-sdk/go@v1.0.0` for `tool_choice`, `toolChoice`, `function_call`, `parallel_tool`, `required`, provider request controls, and RPC structs. The original experiment was performed against `v0.3.0`; the source-level conclusion remains the same for OpenAI-compatible tool-choice semantics.

Findings:

- `SessionConfig` and `ResumeSessionConfig` expose tool registration/filtering through `Tools`, `AvailableTools`, and `ExcludedTools`, plus provider/model configuration. They do not expose `ToolChoice`, `FunctionCall`, `Required`, or `ParallelToolCalls`.
- `MessageOptions` exposes prompt, attachments, delivery mode, agent mode, request headers, and display prompt. It does not expose per-turn tool-choice controls.
- Create/resume requests serialize `tools`, `availableTools`, `excludedTools`, and provider settings. They do not serialize an OpenAI-style tool-choice field.
- `ProviderConfig` supports provider routing and credentials. It does not expose provider request override fields for tool choice.
- Generated RPC tool structs cover tool registration/listing and pending tool-call handling. They do not provide OpenAI-style `tool_choice`, `function_call`, `parallel_tool_calls`, or required-tool-use controls.

Conclusion from source: the SDK-level controls are tool registration/filtering (`Tools`, `AvailableTools`, `ExcludedTools`) plus handlers/permissions. The v1 beta generated RPC `SendRequest.RequiredTool` field checks that a named tool is available on a lower-level send path; it is not exposed through public `MessageOptions` used by this service and does not force the model to emit a tool call.

## SDK e2e harness findings

Relevant files:

- `internal/e2e/testharness/proxy.go:17-18`: `CapiProxy` is a replaying proxy to AI endpoints backed by the repo's shared `test/harness/server.ts`.
- `internal/e2e/testharness/proxy.go:137-158`: `GetExchanges()` retrieves captured exchanges.
- `internal/e2e/testharness/proxy.go:167-172`: `ChatCompletionRequest` parses only `model`, `messages`, and `tools`; it does not model `tool_choice` or `parallel_tool_calls`.
- `internal/e2e/session_test.go:238-270`: the SDK's own `availableTools` test asserts only that the outbound `tools` array is filtered to the selected names.

The published Go module did not include the sibling TypeScript harness files, so I used a scratch local HTTP provider equivalent under `/tmp/toolchoice_spike` with `ProviderConfig{Type:"openai", BaseURL:<local>/v1}` and the embedded matched CLI at `/Users/evlouie/Library/Caches/copilot-sdk/copilot_1.0.36-0`.

## Provider request capture findings

Scratch capture command shape:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/main.go
```

The scratch program created sessions with the embedded CLI `1.0.36-0`, custom OpenAI-compatible provider, `SystemMessageConfig{Mode:"replace", Content:" "}`, and a harmless local provider that returned a normal assistant message. Captures were written to `/tmp/toolchoice_spike/captures.json`.

### Chat Completions wire path (`/v1/chat/completions`)

All captured Chat Completions requests had these top-level keys:

```json
[
  "model",
  "messages",
  "temperature",
  "top_p",
  "frequency_penalty",
  "presence_penalty",
  "parallel_tool_calls",
  "tools"
]
```

Notably absent in every case: `tool_choice`, `function_call`, or any equivalent required/forced-use field.

Summary of cases:

| Case | SDK config | Captured tools | Captured `tool_choice` | Captured `parallel_tool_calls` |
|---|---|---:|---|---|
| no custom tools | `Tools:nil`, `AvailableTools:nil` | 17 built-ins | absent | `false` |
| one custom tool | `Tools:[alpha]`, no `AvailableTools` | 17 built-ins + `alpha` | absent | `false` |
| multiple custom tools | `Tools:[alpha,beta]`, no `AvailableTools` | 17 built-ins + `alpha`,`beta` | absent | `false` |
| restrict to one custom tool | `AvailableTools:["alpha"]` | `alpha` only | absent | `false` |
| impossible sentinel / no available tools | `AvailableTools:["__none__"]` | empty `[]` | absent | `false` |

Representative captured snippets:

Restrict to one custom tool:

```json
{
  "model": "gpt-4o-mini",
  "parallel_tool_calls": false,
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "alpha",
        "description": "Harmless tool alpha",
        "parameters": { "type": "object", "properties": { "x": { "type": "string" } } }
      }
    }
  ]
}
```

Impossible sentinel / no available tools:

```json
{
  "model": "gpt-4o-mini",
  "parallel_tool_calls": false,
  "tools": []
}
```

No `tool_choice` was sent in either case.

Important nuance: with no custom tools and no `AvailableTools` restriction, the CLI exposed many built-in tools by default (`bash`, `view`, `web_fetch`, `report_intent`, `sql`, `task`, etc.). For copilot-api's accepted design of disabling built-ins by default, it must actively restrict `AvailableTools` to the request-scoped aliases, or to an impossible sentinel for no tools.

### Responses wire path (`/v1/responses`)

Scratch command shape:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/responses.go
```

Captured request with `ProviderConfig{WireApi:"responses"}` and `AvailableTools:["alpha"]`:

```json
{
  "model": "gpt-4o-mini",
  "instructions": " ",
  "input": [
    {
      "role": "user",
      "content": [
        { "type": "input_text", "text": "<current_datetime>...\n\nSay ok." }
      ],
      "type": "message"
    }
  ],
  "parallel_tool_calls": true,
  "tools": [
    {
      "name": "alpha",
      "description": "Harmless",
      "parameters": { "type": "object", "properties": {} },
      "strict": false,
      "type": "function"
    }
  ],
  "reasoning": { "effort": "medium", "summary": "auto" },
  "text": { "verbosity": "low" },
  "store": false,
  "include": ["reasoning.encrypted_content"]
}
```

Responses conclusion: separate wire path, same lack of `tool_choice`; additionally, it appears to hard-code `parallel_tool_calls: true` with no SDK control found.

## Behavioral live-model experiments

Live calls used the embedded CLI `1.0.36-0` with normal Copilot auth and harmless custom tool `alpha`. Command shape:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/live.go
```

I ran the three-case program twice. Results were identical both runs:

```text
CASE none_equiv calls=0 err=<nil> content="NO_TOOL_AVAILABLE"
CASE forced_specific_attempt calls=0 err=<nil> content="DIRECT"
CASE required_attempt calls=0 err=<nil> content="4"

CASE none_equiv calls=0 err=<nil> content="NO_TOOL_AVAILABLE"
CASE forced_specific_attempt calls=0 err=<nil> content="DIRECT"
CASE required_attempt calls=0 err=<nil> content="4"
```

Cases:

1. **`none` equivalent**: `AvailableTools:["__none__"]`, custom tool registered but unavailable, prompt strongly required calling `alpha`.
   - Tool handler calls: `0/2`.
   - Model answered `NO_TOOL_AVAILABLE`.
   - Confidence: strong that withholding tools prevents SDK-level custom tool calls.
2. **forced specific approximation attempt**: `AvailableTools:["alpha"]`, prompt explicitly said do not call tools and answer `DIRECT`.
   - Tool handler calls: `0/2`.
   - Model answered directly.
   - This demonstrates that "only selected tool is available" does **not** force a tool call.
3. **`required` approximation attempt**: `AvailableTools:["alpha"]`, prompt answerable without tools.
   - Tool handler calls: `0/2`.
   - Model answered `4` directly.
   - This demonstrates that available tools do **not** imply required tool use.

These were not exhaustive deterministic tests, but they directly test the key semantic distinction. The provider-bound capture also explains the behavior: the CLI never sent a required/forced tool-choice field.

## Is "register only the selected tool" strong enough for forced `tool_choice`?

No. It narrows the model's options if it chooses to call a tool, but it does not require a tool call.

OpenAI forced function choice means the assistant must produce a tool/function call to the named function. With the Copilot SDK/CLI path:

- The provider sees only `tools:[selected]` and no `tool_choice`.
- The model remains free to emit ordinary assistant text.
- Live tests confirmed direct answers happen even with exactly one available tool.

Therefore, exposing only the selected alias is a **best-effort approximation**, not true OpenAI-compatible forced tool choice.

## Recommended MVP behavior

For copilot-api compatibility, recommended behavior is fail-closed where semantics cannot be honored:

- `tool_choice` omitted or `"auto"`:
  - Expose request-scoped client tool aliases in `AvailableTools`.
  - Let the model decide whether to call tools.
- `tool_choice: "none"`:
  - Enforce by setting `AvailableTools` to an impossible sentinel so provider request has `tools: []`.
  - Do not rely on an SDK `tool_choice` field; none exists.
- `tool_choice: {"type":"function","function":{"name":"..."}}`:
  - Do **not** claim true support.
  - Safest MVP: reject with an OpenAI-style error such as unsupported `tool_choice` forced function for this backend.
  - If product chooses best-effort later, expose only that opaque alias and document that the model may still answer directly.
- `tool_choice: "required"`:
  - Reject as unsupported for this backend. There is no SDK/CLI/provider-bound required-use control.
- `parallel_tool_calls:false`:
  - Chat Completions: captured SDK/CLI requests already send `parallel_tool_calls:false`, but there is no public knob. It is probably safe to accept `false` for Chat Completions and reject/ignore `true` unless verified.
  - Responses: captured SDK/CLI request sends `parallel_tool_calls:true`; no public knob found. Do not claim `false` enforcement for Responses through this SDK path.

## Risks and next steps

Risks:

- CLI internals are opaque; this spike used black-box capture against embedded CLI `1.0.36-0`. Future CLI/SDK versions may add or change tool-choice behavior.
- The Go module's e2e TypeScript harness was not included in the module cache, so captures used a scratch equivalent provider rather than the repository harness.
- Live behavioral testing was intentionally minimal to avoid unnecessary model calls; results are still aligned with provider request evidence.
- Built-in tools appear unless `AvailableTools` restricts them. copilot-api must continue to use request-scoped aliases and explicit availability filtering to avoid exposing unintended tools.

Next steps:

1. Implement `auto`/omitted and `none` using tool availability filtering only.
2. Decide product/API stance for forced function and `required`: recommended fail closed with a clear unsupported error.
3. Add regression tests around outbound provider request shape using a local provider capture: no `tool_choice`, `tools: []` for `none`, only selected alias for best-effort forced if ever enabled.
4. Re-run this spike when upgrading beyond SDK `v1.0.0`.
