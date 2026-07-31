# Spike: Copilot SDK tool_choice enforcement

Date: 2026-05-29

Note on the readiness for v1: the implementation now uses
`github.com/github/copilot-sdk/go v1.0.0`. The generated RPC shape of v1 has a
check for the availability of a `RequiredTool` at a low level. But the public
`MessageOptions` path that this service uses does not give access to that check.
The check is also not an OpenAI-compatible way to force the model to call a
function. It cannot make a tool call necessary for a turn.

## Verdict

**The Copilot SDK and the Copilot runtime do not give access to the true OpenAI
controls `tool_choice`, `function_call` and required tool use. They do not send
these controls.**

The evidence supports these statements:

- **`auto`**: yes. Show the necessary tools, and let the model decide. The
  request to the provider does not include `tool_choice`. Thus this is the
  default behavior of the provider or of the model.
- **`none`**: yes, but you must **hold back all the tools** (`tools: []`). You
  cannot send `tool_choice: "none"`.
- **a specific function that is forced**: only an **approximation** is possible.
  Register or show only the selected tool. This does not force a tool call. The
  live tests showed that the model can still answer directly.
- **`required`**: the SDK and the runtime cannot make this necessary. The live
  tests showed that the model can answer directly when tools are available.
- **`parallel_tool_calls: false`**:
  - The Chat Completions wire path: the captured requests include
    `"parallel_tool_calls": false`. The CLI seems to write this value into the
    code.
  - The Responses wire path: the captured request included
    `"parallel_tool_calls": true`. No SDK field was found to set it to false.

The recommended position for the MVP: give support for `auto` and for `none`.
Give no support for a forced specific function and for `required`. Give support
only if the API documents an approximation that is not OpenAI-compatible.

## Findings in the source of the SDK and the RPC

The search in `github.com/github/copilot-sdk/go@v1.0.0` looked for
`tool_choice`, `toolChoice`, `function_call`, `parallel_tool`, `required`, the
controls of the provider request, and the RPC structs. The first experiment used
`v0.3.0`. The conclusion from the source is the same for the OpenAI-compatible
semantics of the tool choice.

The findings are:

- `SessionConfig` and `ResumeSessionConfig` give access to the registration and
  the filtering of the tools. They do this with `Tools`, `AvailableTools` and
  `ExcludedTools`. They also give the configuration of the provider and of the
  model. They do not give access to `ToolChoice`, `FunctionCall`, `Required` or
  `ParallelToolCalls`.
- `MessageOptions` gives access to the prompt, the attachments, the delivery
  mode, the agent mode, the request headers and the display prompt. It does not
  give a control of the tool choice for one turn.
- The create request and the resume request serialize `tools`, `availableTools`,
  `excludedTools` and the settings of the provider. They do not serialize a
  field for the tool choice in the OpenAI style.
- `ProviderConfig` supports the routing of the provider and the credentials. It
  does not give fields that write over the provider request for the tool choice.
- The generated RPC structs for the tools cover the registration of a tool, the
  listing of the tools and the control of a tool call that waits. They do not
  give `tool_choice`, `function_call` or `parallel_tool_calls` in the OpenAI
  style. They also do not give a control for required tool use.

The conclusion from the source: the controls at the level of the SDK are the
registration and the filtering of the tools (`Tools`, `AvailableTools`,
`ExcludedTools`), the handlers and the permissions. The generated RPC field
`SendRequest.RequiredTool` of the v1 beta checks that a tool with a given name
is available on a send path at a low level. The public `MessageOptions` that
this service uses does not give access to this field. The field also does not
force the model to send a tool call.

## Findings in the e2e harness of the SDK

The applicable files are:

- `internal/e2e/testharness/proxy.go:17-18`: `CapiProxy` is a proxy to the AI
  endpoints that plays the traffic again. The shared file
  `test/harness/server.ts` of the repository supports it.
- `internal/e2e/testharness/proxy.go:137-158`: `GetExchanges()` gets the
  captured exchanges.
- `internal/e2e/testharness/proxy.go:167-172`: `ChatCompletionRequest` parses
  only `model`, `messages` and `tools`. It does not model `tool_choice` or
  `parallel_tool_calls`.
- `internal/e2e/session_test.go:238-270`: the `availableTools` test of the SDK
  asserts only one condition. The outbound `tools` array holds only the selected
  names.

The published Go module did not include the TypeScript harness files. Thus I
used an equivalent local HTTP provider for the test. It is at
`/tmp/toolchoice_spike`, with
`ProviderConfig{Type:"openai", BaseURL:<local>/v1}` and the matched CLI at
`/Users/evlouie/Library/Caches/copilot-sdk/copilot_1.0.36-0`.

## Findings from the capture of the provider request

The shape of the capture command for the test is:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/main.go
```

The test program created sessions with the CLI `1.0.36-0` in the module. It used
a custom OpenAI-compatible provider,
`SystemMessageConfig{Mode:"replace", Content:" "}`, and a local provider with no
effect that returned a usual assistant message. The program wrote the captures
to `/tmp/toolchoice_spike/captures.json`.

### The Chat Completions wire path (`/v1/chat/completions`)

Each captured Chat Completions request had these keys at the top level:

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

These keys were absent in each case: `tool_choice`, `function_call`, and each
equivalent field for required use or forced use.

The summary of the cases is:

| Case                                | SDK config                                |               Captured tools | Captured `tool_choice` | Captured `parallel_tool_calls` |
| ----------------------------------- | ----------------------------------------- | ---------------------------: | ---------------------- | ------------------------------ |
| no custom tool                      | `Tools:nil`, `AvailableTools:nil`         |                  17 built-in | absent                 | `false`                        |
| one custom tool                     | `Tools:[alpha]`, no `AvailableTools`      |        17 built-in + `alpha` | absent                 | `false`                        |
| more than one custom tool           | `Tools:[alpha,beta]`, no `AvailableTools` | 17 built-in + `alpha`,`beta` | absent                 | `false`                        |
| only one custom tool                | `AvailableTools:["alpha"]`                |                 `alpha` only | absent                 | `false`                        |
| impossible name / no available tool | `AvailableTools:["__none__"]`             |                   empty `[]` | absent                 | `false`                        |

These captured parts show the results:

Only one custom tool:

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
        "parameters": {
          "type": "object",
          "properties": { "x": { "type": "string" } }
        }
      }
    }
  ]
}
```

Impossible name / no available tool:

```json
{
  "model": "gpt-4o-mini",
  "parallel_tool_calls": false,
  "tools": []
}
```

No `tool_choice` was sent in the two cases.

An important detail: with no custom tool and no restriction from
`AvailableTools`, the CLI showed many built-in tools by default. Examples are
`bash`, `view`, `web_fetch`, `report_intent`, `sql` and `task`. The accepted
design of copilot-api disables the built-in tools by default. Thus copilot-api
must set `AvailableTools` to the custom-tool filters of the request. For no
tools, it must set `AvailableTools` to an impossible name.

### The Responses wire path (`/v1/responses`)

The shape of the test command is:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/responses.go
```

The captured request with `ProviderConfig{WireApi:"responses"}` and
`AvailableTools:["alpha"]` is:

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

The conclusion for the Responses path: it is a different wire path, but
`tool_choice` is also absent. The path also seems to write
`parallel_tool_calls: true` into the code. No SDK control for this value was
found.

## Experiments with a live model

The live calls used the CLI `1.0.36-0` in the module, the usual Copilot
authentication, and the custom tool `alpha` that has no effect. The shape of the
command is:

```sh
cd /Users/evlouie/Developer/copilot-api
go run /tmp/toolchoice_spike/live.go
```

I ran the program with the three cases two times. The results were the same in
the two runs:

```text
CASE none_equiv calls=0 err=<nil> content="NO_TOOL_AVAILABLE"
CASE forced_specific_attempt calls=0 err=<nil> content="DIRECT"
CASE required_attempt calls=0 err=<nil> content="4"

CASE none_equiv calls=0 err=<nil> content="NO_TOOL_AVAILABLE"
CASE forced_specific_attempt calls=0 err=<nil> content="DIRECT"
CASE required_attempt calls=0 err=<nil> content="4"
```

The cases are:

1. **the equivalent of `none`**: `AvailableTools:["__none__"]`. The custom tool
   was registered, but it was not available. The prompt gave a strong
   instruction to call `alpha`.
   - Calls to the tool handler: `0/2`.
   - The model answered `NO_TOOL_AVAILABLE`.
   - Confidence: it is very probable that a hold-back of the tools prevents the
     custom tool calls at the level of the SDK.
2. **an approximation of a forced specific function**:
   `AvailableTools:["alpha"]`. The prompt said clearly: do not call the tools,
   and answer `DIRECT`.
   - Calls to the tool handler: `0/2`.
   - The model answered directly.
   - This shows that "only the selected tool is available" does **not** force a
     tool call.
3. **an approximation of `required`**: `AvailableTools:["alpha"]`. The model can
   answer the prompt with no tool.
   - Calls to the tool handler: `0/2`.
   - The model answered `4` directly.
   - This shows that available tools do **not** make tool use necessary.

These tests were not complete and not deterministic. But they test the important
difference in the semantics directly. The capture at the provider also gives the
cause of the behavior. The CLI never sent a field for a required tool choice or
a forced tool choice.

## Is "register only the selected tool" sufficient for a forced `tool_choice`?

No. It makes the options of the model fewer if the model decides to call a tool.
But it does not make a tool call necessary.

A forced function choice in OpenAI means that the assistant must produce a tool
call or a function call to the named function. On the path of the Copilot SDK
and the Copilot CLI:

- The provider sees only `tools:[selected]`, and it sees no `tool_choice`.
- The model can still send usual assistant text.
- The live tests showed that direct answers occur, also with exactly one
  available tool.

Thus, to show only the selected tool is an **approximation**. It is not a true
OpenAI-compatible forced tool choice.

## Recommended behavior for the MVP

For the compatibility of copilot-api, refuse the request when the semantics are
not possible:

- `tool_choice` is absent or `"auto"`:
  - Show the client tools of the request through the custom-tool filters in
    `AvailableTools`.
  - Let the model decide if it calls the tools.
- `tool_choice: "none"`:
  - Set `AvailableTools` to an impossible name. Then the provider request has
    `tools: []`.
  - Do not use an SDK field `tool_choice`. No such field exists.
- `tool_choice: {"type":"function","function":{"name":"..."}}`:
  - Do **not** say that there is true support.
  - The safest MVP behavior: refuse the request with an error in the OpenAI
    style. The error says that a forced function in `tool_choice` is not
    supported for this backend.
  - If the product decides to use the approximation later, show only that
    selected custom tool. Then document that the model can still answer
    directly.
- `tool_choice: "required"`:
  - Refuse the request. This backend does not support it. The SDK, the CLI and
    the provider give no control for required use.
- `parallel_tool_calls:false`:
  - Chat Completions: the captured requests of the SDK and the CLI already send
    `parallel_tool_calls:false`. But there is no public control. It is probably
    safe to accept `false` for Chat Completions. Refuse `true` or ignore `true`
    until you check it.
  - Responses: the captured request of the SDK and the CLI sends
    `parallel_tool_calls:true`. No public control was found. Do not say that
    this SDK path can force `false` for Responses.

## Risks and next steps

The risks are:

- The internal operation of the CLI is not visible. This spike captured the
  traffic of the CLI `1.0.36-0` in the module from the outside. A future version
  of the CLI or of the SDK can add or change the behavior of the tool choice.
- The module cache did not include the TypeScript e2e harness of the Go module.
  Thus the captures used an equivalent test provider, and not the harness of the
  repository.
- The tests of the live behavior were small, to prevent unnecessary calls to
  the model. But the results agree with the evidence from the provider requests.
- The built-in tools are visible if `AvailableTools` does not restrict them.
  copilot-api must continue to use the custom-tool filters of the request. It
  must also filter the availability of the tools, to prevent the display of
  unwanted tools.

The next steps are:

1. Implement the absent value, `auto` and `none`. Use only the filter of the
   tool availability.
2. Decide the position of the product and of the API for a forced function and
   for `required`. The recommendation is to refuse the request with a clear
   error that says that there is no support.
3. Add regression tests for the shape of the outbound provider request. Use a
   local provider capture. The tests must check that there is no `tool_choice`,
   that `none` gives `tools: []`, and that the approximation of a forced choice
   gives only the selected custom tool if you enable it.
4. Do this spike again when you go to an SDK version after `v1.0.0`.
