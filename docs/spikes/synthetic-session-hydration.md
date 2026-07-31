# Spike: synthetic Copilot SDK session hydration

Date: 2026-05-29

A note about the readiness of v1: the implementation now targets
`github.com/github/copilot-sdk/go v1.0.0`. I did the original live experiment
below against SDK `v0.3.0`. The lessons about compatibility are still correct.
But the code and the documentation must use the v1 names:
`InitialWorkingDirectory`, `CreateSessionFSProvider`, `ConfigDirectory` and
`SessionFS...`.

## Verdict

**It is possible, but there are cautions.** The GitHub Copilot SDK can resume a
synthetic `/session-state/events.jsonl` file that an isolated
`SessionFSProvider` created. The resumed model uses the seeded events as the
context of the conversation. The events are not only user-interface history.
Live model calls proved this for two cases:

- normal prior turns with `user.message` and `assistant.message`; and
- a synthetic prior assistant tool-call turn with `tool.execution_start` and a
  `tool.execution_complete` result.

Recommendation: **use the synthetic hydration as the primary path for the
role-native history of Chat Completions**. But keep **the serialization of the
transcript as an alternative path**. The reason is that the hydration depends on
an internal schema of saved events and on the resume behavior of the runtime.
It does not depend on a stable public “import messages” API.

## SDK source and tests inspected

The current path of the SDK:
`$(go env GOPATH)/pkg/mod/github.com/github/copilot-sdk/go@v1.0.0`

The important files:

- `session_fs_provider.go`
  - It defines `SessionFSProvider` with `ReadFile`, `WriteFile`, `AppendFile`,
    `Exists`, `Stat`, `MakeDirectory`, `ReadDirectory`,
    `ReadDirectoryWithTypes`, `Remove` and `Rename`.
  - The adapter maps the file system errors of Go to the RPC `SessionFSError`
    values. This hook lets this project own a virtual file system for each
    session. Thus the project can seed `/session-state/events.jsonl` before
    `session.resume`.
- `client.go`
  - `CreateSession` and `ResumeSessionWithOptions` both register the `SessionFS`
    adapter of the session. They do this before they send the `session.create`
    or `session.resume` RPC.
  - If `ClientOptions.SessionFS` is configured, the create configuration and the
    resume configuration both need `CreateSessionFSProvider`.
  - `ResumeSessionWithOptions` sends `SystemMessage`, `Tools`, `AvailableTools`,
    `ExcludedTools`, `WorkingDirectory`, `ConfigDirectory`, `DisableResume` and
    more into the resume RPC.
- `types.go`
  - `SessionFSConfig` has `InitialWorkingDirectory`, `SessionStatePath` and
    `Conventions`.
  - `SystemMessageConfig{Mode: "replace", Content: ...}` is the path of the SDK
    to replace the system prompt that the SDK controls. It does not add to that
    prompt.
  - `SessionConfig` and `ResumeSessionConfig` support `CreateSessionFSProvider`,
    `AvailableTools`, `Tools` for one request, and `OnPermissionRequest`.
- `rpc/zsession_events.go`
  - It defines the envelope of the saved event. It also defines the typed
    payloads for `session.start`, `session.resume`, `system.message`,
    `user.message`, `assistant.message`, `assistant.turn_start`,
    `assistant.turn_end`, `tool.execution_start`, `tool.execution_complete` and
    more.
- `internal/e2e/session_fs_e2e_test.go`
  - It confirms that the provider saves the events at
    `/session-state/events.jsonl`.
  - The test “should load session data from fs provider on resume” creates a
    session. It asks `What is 50 + 50?`. It disconnects. It resumes through the
    provider. Then it asks `What is that times 3?`. The expected answer contains
    `300`. This is strong evidence in the source code that the saved state from
    the provider becomes model context.
  - It also covers the workspace metadata (`workspace.yaml`, the checkpoints),
    the rewrite of the events by compaction, and the routing of the temporary
    files.
- `internal/e2e/session_test.go`
  - It confirms that a resume with the same client and a resume with a new
    client both continue the conversation with the state. It also confirms that
    the resumed messages include the prior `user.message` and `session.resume`.

## Experiment performed

I used a scratch location only: `/tmp/copilot-synth-spike` and
`/tmp/copilot-synth-spike/*state`. I changed no implementation file of the
project. I wrote only this report in the repository.

The original historical commands:

```sh
cd /tmp/copilot-synth-spike
go mod init copilot-synth-spike
go get github.com/github/copilot-sdk/go@v0.3.0
go get -tool github.com/github/copilot-sdk/go/cmd/bundler@v0.3.0
go tool bundler --output .
goimports -w main.go
go run .
```

A historical note about the environment: the installed `copilot` CLI was
`1.0.55`. It failed against SDK `v0.3.0` with this error:

```text
json: cannot unmarshal string into Go struct field PingResponse.timestamp of type int64
```

Thus I used the bundler of the SDK to embed the CLI version that the tool found
automatically for SDK `v0.3.0`: **Copilot CLI `1.0.36-0`**.

### Experiment A: real provider-backed resume

The program created a real session with these properties:

- a real `SessionFSProvider` with the root at `/tmp/copilot-synth-spike/state`;
- `SystemMessageConfig{Mode: "replace", Content: "You are a neutral chat completion model..."}`;
- no custom tools, and a permission handler that denies all requests;
- `AvailableTools: []string{"__none__"}`, to keep the built-in tools hidden
  during the probe.

The seed prompt:

```text
Remember this exact nonce for the next turn: nonce-real-1e281015. Reply with OK only.
```

After the disconnection, `events.jsonl` existed. It contained these events:

```text
session.start
session.model_change
system.message
user.message
assistant.turn_start
assistant.message
assistant.turn_end
session.shutdown
```

Then the program resumed the same session. It asked this question:

```text
What exact nonce did I ask you to remember? Reply with the nonce only.
```

The observed output:

```text
REAL_FOLLOW_REPLY nonce-real-1e281015
```

This confirms that normal saved event logs are model context after a resume.

### Experiment B: synthetic text history

Then the program wrote a new session directory by hand. The directory contained
only `/session-state/events.jsonl` with synthetic events. It contained no
`workspace.yaml` and no checkpoints:

```text
session.start
user.message
assistant.turn_start
assistant.message
assistant.turn_end
```

The seeded nonce:

```text
nonce-synth-7251f004
```

Then it called `ResumeSession` for that synthetic session ID. It asked the same
second question.

The observed output:

```text
SYNTH_FOLLOW_REPLY nonce-synth-7251f004
```

After the resume, the SDK added the normal runtime events to the synthetic log:

```text
session.resume
session.model_change
system.message
user.message
assistant.turn_start
assistant.message
assistant.turn_end
session.shutdown
```

This is the main proof: **the model used the synthetic prior events as context
after `ResumeSession`.**

### Experiment C: real tool event shapes

I registered a scratch custom tool `echo_value` with `SkipPermission = true`.
The prompt:

```text
Call echo_value once with value alpha-123. Then answer with the exact tool output only.
```

The observed tool call and the final answer:

```text
TOOL_INVOKED call_J1HnPWroYRFzzqa7uJvztLnQ echo_value map[string]interface {}{"value":"alpha-123"}
REPLY tool-output:alpha-123
```

The saved event sequence around the tool call:

```text
user.message
assistant.turn_start
assistant.message               # content="", toolRequests=[...]
tool.execution_start
hook.start
hook.end
tool.execution_complete
assistant.turn_end
assistant.turn_start
assistant.message               # final text
assistant.turn_end
```

A request for two tool calls produced two tool calls. But the model and the
runtime did them one after the other, as separate assistant tool-request turns.
They did not use one parallel `toolRequests` array. The schema permits more than
one item in `toolRequests`. Synthetic parallel hydration must put more than one
request in one assistant message. It must also add one start event and one
complete event for each `toolCallId`. But no live test proved that exact
parallel synthetic shape.

### Experiment D: synthetic tool-result history

A separate scratch program seeded a synthetic session with these events:

```text
session.start
user.message
assistant.turn_start
assistant.message         # toolRequests: echo_value(call_synth_charlie)
tool.execution_start
tool.execution_complete   # result.content = tool-output-charlie-789
assistant.turn_end
```

The seed contained no final assistant answer. After `ResumeSession`, the program
asked this question:

```text
What exact output did the previous tool execution return? Reply with that output only.
```

The observed output:

```text
SESSION synth-tool-9d84522e-c889-49e3-b852-9b3aaa843f0c EXPECTED tool-output-charlie-789 REPLY tool-output-charlie-789
```

This shows that the model can also use synthetic tool execution result events as
context.

## Minimal event schema for synthetic hydration

The envelope. There is one JSON object on each line:

```json
{
  "id": "uuid-v4-like string",
  "timestamp": "2026-05-29T21:21:43.292823Z",
  "parentId": null,
  "type": "session.start",
  "data": {}
}
```

The observed requirements and behavior:

- Each event must have a unique `id`.
- The `timestamp` must use RFC3339 or RFC3339Nano.
- `parentId` makes a linked list. It is `null` for the first event. Synthetic
  parent chains operated correctly.
- The events are newline-delimited JSON in `/session-state/events.jsonl`.
- The runtime adds `session.resume` with an `eventCount` that is equal to the
  number of seeded events.

### Required first event

```json
{
  "type": "session.start",
  "data": {
    "copilotVersion": "synthetic",
    "producer": "copilot-api-spike",
    "sessionId": "synth-...",
    "startTime": "2026-05-29T21:21:43.292823Z",
    "version": 1
  }
}
```

Real sessions also contain optional data. Here is an example:

```json
{
  "alreadyInUse": false,
  "context": { "cwd": "/tmp" },
  "remoteSteerable": false,
  "selectedModel": "..."
}
```

### User message

The minimal synthetic form that operated correctly:

```json
{
  "type": "user.message",
  "data": {
    "content": "Remember this exact nonce..."
  }
}
```

Real sessions contain more optional fields:

```json
{
  "attachments": [],
  "content": "...",
  "interactionId": "uuid",
  "supportedNativeDocumentMimeTypes": [],
  "transformedContent": "<current_datetime>...</current_datetime>\n\n..."
}
```

### Assistant text message

The minimal synthetic form that operated correctly:

```json
{
  "type": "assistant.message",
  "data": {
    "content": "OK",
    "messageId": "uuid"
  }
}
```

Real sessions can add `encryptedContent`, `interactionId`, `outputTokens`,
`phase`, `reasoningOpaque`, `reasoningText`, `requestId` and `toolRequests`.

### Assistant turn delimiters

These events operated correctly in the synthetic logs:

```json
{ "type": "assistant.turn_start", "data": { "turnId": "0" } }
{ "type": "assistant.turn_end", "data": { "turnId": "0" } }
```

Real sessions contain `interactionId` on `assistant.turn_start`.

### System/developer message

The generated type supports this event:

```json
{
  "type": "system.message",
  "data": {
    "content": "...",
    "role": "system"
  }
}
```

`role` is `"system"` or `"developer"`. In the live synthetic experiments I did
not have to seed this event by hand. The reason is that
`ResumeSessionConfig.SystemMessage` with `Mode: "replace"` made the SDK add a
new `system.message` at the resume. For this project, there is a safer path.
Send the current effective OpenAI system instructions and developer instructions
through `SystemMessageConfig{Mode:"replace"}` at each create operation and each
resume operation. Do not depend only on seeded system events.

### Tool call request and result

Assistant tool request:

```json
{
  "type": "assistant.message",
  "data": {
    "content": "",
    "messageId": "uuid",
    "toolRequests": [
      {
        "name": "echo_value",
        "toolCallId": "call_synth_charlie",
        "type": "function",
        "arguments": { "value": "charlie-789" }
      }
    ]
  }
}
```

Tool execution start:

```json
{
  "type": "tool.execution_start",
  "data": {
    "toolCallId": "call_synth_charlie",
    "toolName": "echo_value",
    "arguments": { "value": "charlie-789" }
  }
}
```

Tool execution complete:

```json
{
  "type": "tool.execution_complete",
  "data": {
    "toolCallId": "call_synth_charlie",
    "success": true,
    "result": {
      "content": "tool-output-charlie-789",
      "detailedContent": "tool-output-charlie-789"
    }
  }
}
```

The real `tool.execution_complete` also contained `interactionId` and `model`.
The synthetic tool-result proof did not need these fields.

## Assessment for copilot-api

### Ordinary multi-turn Chat Completions

Synthetic hydration can keep the role-native history of the user and the
assistant. It does not put all the prior turns into one prompt. Do these steps
for each stateless Chat Completions request:

1. Create a new isolated synthetic session ID. Create a new root for the session
   file system.
2. Write `/session-state/events.jsonl` with `session.start`. Add the prior
   OpenAI messages after you convert them to SDK events.
3. Resume the synthetic session with `SystemMessageConfig{Mode:"replace"}`. This
   value must contain only the system instructions and the developer
   instructions from the caller. It can also contain a minimal neutral text for
   the adapter.
4. Send only the current user message as the new turn.
5. Delete the synthetic state after the response, or let it expire. Keep it only
   if the system needs it.

### Tool-call turns

This is possible for a normal OpenAI tool history:

- prior assistant message with `tool_calls` -> synthetic
  `assistant.message.toolRequests`;
- prior `role: tool` outputs -> synthetic `tool.execution_start` +
  `tool.execution_complete` linked by `toolCallId`;
- next user/current prompt -> sent as the new SDK turn.

For the continuation of a parked turn in the Responses API, the existing plan is
still more simple when it is possible. That plan keeps the live SDK handler
parked. Then it unblocks the handler with the client output. Synthetic tool
hydration is most useful for a stateless Chat Completions continuation. In that
case the client sends the assistant tool calls and the tool results again in a
later request.

Parallel tool calls: the event schema permits `toolRequests: []` and one
execution pair for each `toolCallId`. The live prompt for two tools produced
tool calls one after the other. It did not produce one assistant message with
more than one call. Thus the exact runtime behavior for a synthetic parallel
batch in one message is still an open item for validation.

## Risks and caveats

- **A change of the schema:** an SDK schema generates `events.jsonl`. But the
  resume behavior for saved events is not a public “import messages” contract.
  Pin the versions of the SDK and the CLI. Add regression tests for the
  hydration.
- **The coupling of the SDK version and the CLI version:** the original spike
  showed that an installed CLI with an incorrect version can fail at the start.
  The error is about the shape of the protocol. The service must control the
  pair of the SDK and the CLI. It must prefer the embedded CLI that agrees with
  the SDK. It must fail at the start if it cannot read the runtime status.
- **The parent IDs and the timestamps:** unique synthetic IDs, RFC3339
  timestamps and a linear `parentId` chain operated correctly. Do not assume
  that an incorrect chain continues to operate.
- **The checkpoints and the compaction:** the e2e tests show that compaction
  writes `events.jsonl` again with checkpoint data. A long synthetic history can
  cause compaction or a summary. Keep the synthetic Chat sessions short in time.
  Keep an alternative path with a transcript.
- **`transformedContent`:** a real `user.message` contains `transformedContent`,
  which the SDK generates. Minimal synthetic messages with no such field
  operated correctly. A future runtime can depend more on the transformed
  content.
- **The safety of the system prompt:** never depend on the defaults of the SDK.
  Always send `SystemMessageConfig{Mode:"replace"}` at each create operation and
  each resume operation. Decide how to combine the OpenAI `system` messages and
  the `developer` messages. Make one replacement prompt, or make checked
  `system.message` events.
- **The tools:** real tool calls add hook events and optional telemetry fields.
  Minimal synthetic tool result events operated correctly. But more tests are
  necessary for error results, partial results, binary and multimodal tool
  outputs, and parallel batches.
- **The built-in tools:** the spike kept the SDK built-in tools away. It used a
  replacement system prompt, no registered SDK tools, a permission handler that
  denies all requests, and a sentinel value in `AvailableTools`. This is not a
  full proof that the built-in tools are off.
- **The concurrency:** write or rewrite `events.jsonl` only before
  `ResumeSession`. Do not change a state file while a session is active. The
  product invariant of one user with a concurrent proxy helps. But a lock for
  each session is still necessary.
- **The interaction IDs and the request IDs:** the synthetic events do not
  contain them, and the tests operated correctly. They can be important for the
  telemetry, for the accounting or for a future reconstruction of the context.
- **The workspace files:** a minimal synthetic resume did not need
  `workspace.yaml` or the checkpoints. But the runtime can create them after the
  resume.
- **The assumptions that have no support:** this method changes the state file
  directly. Treat it as an internal adapter with tests and an alternative path.
  Do not treat it as a guarantee of a stable upstream API.

## Recommendation for implementation plan

Change the plan. Do not “serialize the OpenAI messages into one prompt
transcript”. Use this plan:

1. **The primary path:** do synthetic session hydration with a
   `SessionFSProvider` that the project owns. Then call `ResumeSession`, and
   send only the newest user turn or input turn.
2. **The safety path:** always use `SystemMessageConfig{Mode:"replace"}` and
   defaults that deny the tools. Register the client proxy tools for one request
   only, and only when the request contains OpenAI tools.
3. **The alternative path:** the serialization of the transcript is still
   available. Use it when the hydration fails, when the schema validation
   rejects a history shape, when a change of the SDK version or the CLI version
   breaks the tests, or when a history feature has no map yet.

This plan improves the control of the role-native history. It also keeps the
strict product invariants.

## Next concrete implementation tasks

1. Add an internal implementation of `SessionFSProvider`. Use a directory for
   each request or session, or use an in-memory file system. Give it atomic
   write and append operations, and a lock for each session.
2. Define an internal component that converts an OpenAI message to an SDK event:
   - system/developer -> replacement `SystemMessageConfig` first; optionally
     `system.message` only when needed;
   - user -> `user.message`;
   - assistant text -> `assistant.turn_start`, `assistant.message`,
     `assistant.turn_end`;
   - assistant tool calls -> `assistant.message.toolRequests`;
   - tool results -> `tool.execution_start` + `tool.execution_complete`.
3. Add schema validation. Unmarshal each generated line into
   `copilot.SessionEvent` before the resume.
4. Pin the versions of the SDK and the CLI, and check their compatibility.
   Prefer the embedded CLI path of the SDK, because the result is reproducible.
   Write the runtime status to the log at the start.
5. Add integration tests behind a flag for opt-in live tests:
   - the recall of a nonce from a synthetic text history;
   - the recall of a synthetic tool result;
   - more than one tool result, and `toolRequests` with a parallel shape;
   - the alternative path when the resume fails.
6. Add a cleanup operation and a TTL for the synthetic session state. Make sure
   that the code does not use an ambient global session again.
7. Keep the implementation of the transcript serialization as an alternative
   path with tests. Keep it until the hydration has sufficient version coverage.

## Open questions requiring parent/user decision

- Must the synthetic hydration be the default for the Chat Completions MVP? Then
  the transcript serialization is only the alternative path. Note that the
  hydration depends on the internal event behavior of the SDK.
- How must the code combine more than one OpenAI `system` message and
  `developer` message? There are three options. Make one replacement system
  prompt. Make separate synthetic `system.message` events. Or reject the unusual
  system messages and developer messages in the middle of a conversation, in
  strict mode.
- For parallel tool calls, must the MVP make the natural parallel shape
  immediately? Or must it serialize the tool results carefully until a live test
  proves a synthetic parallel batch?
- What behavior is acceptable when the hydration fails in strict compatibility
  mode? The server can return an error in the OpenAI shape. Or it can use the
  transcript serialization with no message.
