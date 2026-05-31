# Spike: synthetic Copilot SDK session hydration

Date: 2026-05-29

Current v1 readiness note: the implementation now targets `github.com/github/copilot-sdk/go v1.0.0-beta.10`. Local module tags currently stop at beta releases, so a stable Go `v1.0.0` tag is not available locally yet. The original live experiment below was run against SDK `v0.3.0`; the compatibility lessons still apply, but code and docs should use the v1 names: `InitialWorkingDirectory`, `CreateSessionFsProvider`, `ConfigDirectory`, and `SessionFs...`.

## Verdict

**Feasible with caveats.** A synthetic `/session-state/events.jsonl` created through an isolated `SessionFsProvider` can be resumed by the GitHub Copilot SDK, and the resumed model uses the seeded events as conversation context, not merely UI/history. This was proven with live model calls for:

- ordinary prior `user.message` + `assistant.message` turns; and
- a synthetic prior assistant tool-call turn with `tool.execution_start` + `tool.execution_complete` result.

Recommendation: **use synthetic hydration as the preferred path for Chat Completions role-native history**, but keep **transcript serialization as a fallback/escape hatch** because this depends on an internal persisted event schema and runtime resume behavior rather than a stable public “import messages” API.

## SDK source and tests inspected

Current SDK path: `$(go env GOPATH)/pkg/mod/github.com/github/copilot-sdk/go@v1.0.0-beta.10`

Key files:

- `session_fs_provider.go`
  - Defines `SessionFsProvider` with `ReadFile`, `WriteFile`, `AppendFile`, `Exists`, `Stat`, `MakeDirectory`, `ReadDirectory`, `ReadDirectoryWithTypes`, `Remove`, `Rename`.
  - The adapter maps Go filesystem errors to RPC `SessionFsError`s. This is the hook that lets this project own a per-session virtual filesystem and seed `/session-state/events.jsonl` before `session.resume`.
- `client.go`
  - `CreateSession` and `ResumeSessionWithOptions` both register the per-session `SessionFs` adapter before issuing `session.create` / `session.resume` RPCs.
  - When `ClientOptions.SessionFs` is configured, `CreateSessionFsProvider` is required in both create and resume configs.
  - `ResumeSessionWithOptions` forwards `SystemMessage`, `Tools`, `AvailableTools`, `ExcludedTools`, `WorkingDirectory`, `ConfigDirectory`, `DisableResume`, etc. into the resume RPC.
- `types.go`
  - `SessionFsConfig` has `InitialWorkingDirectory`, `SessionStatePath`, and `Conventions`.
  - `SystemMessageConfig{Mode: "replace", Content: ...}` is the SDK path for replacing, not appending to, the SDK-managed system prompt.
  - `SessionConfig` / `ResumeSessionConfig` support `CreateSessionFsProvider`, `AvailableTools`, request-scoped `Tools`, and `OnPermissionRequest`.
- `rpc/zsession_events.go`
  - Defines the persisted event envelope and typed payloads for `session.start`, `session.resume`, `system.message`, `user.message`, `assistant.message`, `assistant.turn_start`, `assistant.turn_end`, `tool.execution_start`, `tool.execution_complete`, etc.
- `internal/e2e/session_fs_e2e_test.go`
  - Confirms events are persisted at `/session-state/events.jsonl` through the provider.
  - Test “should load session data from fs provider on resume” creates a session, asks `What is 50 + 50?`, disconnects, resumes through the provider, then asks `What is that times 3?`; expected answer contains `300`. This is strong source-level evidence that provider-backed persisted state feeds model context.
  - Also covers workspace metadata (`workspace.yaml`, checkpoints), compaction rewriting events, and temp file routing.
- `internal/e2e/session_test.go`
  - Confirms same-client and new-client resume continue conversation statefully and that resumed messages include prior `user.message` and `session.resume`.

## Experiment performed

Scratch location only: `/tmp/copilot-synth-spike` and `/tmp/copilot-synth-spike/*state`. No project implementation files were changed; only this report was written in the repo.

Original historical commands run:

```sh
cd /tmp/copilot-synth-spike
go mod init copilot-synth-spike
go get github.com/github/copilot-sdk/go@v0.3.0
go get -tool github.com/github/copilot-sdk/go/cmd/bundler@v0.3.0
go tool bundler --output .
goimports -w main.go
go run .
```

Historical environment note: the installed `copilot` CLI was `1.0.55` and failed against SDK `v0.3.0` with:

```text
json: cannot unmarshal string into Go struct field PingResponse.timestamp of type int64
```

I therefore used the SDK bundler to embed the matching CLI version auto-detected for SDK `v0.3.0`: **Copilot CLI `1.0.36-0`**.

### Experiment A: real provider-backed resume

Program created a real session with:

- custom `SessionFsProvider` rooted at `/tmp/copilot-synth-spike/state`;
- `SystemMessageConfig{Mode: "replace", Content: "You are a neutral chat completion model..."}`;
- no custom tools and a denying permission handler;
- `AvailableTools: []string{"__none__"}` to avoid exposing built-ins during the probe.

Seed prompt:

```text
Remember this exact nonce for the next turn: nonce-real-1e281015. Reply with OK only.
```

After disconnect, `events.jsonl` existed and included:

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

Then the program resumed the same session and asked:

```text
What exact nonce did I ask you to remember? Reply with the nonce only.
```

Observed output:

```text
REAL_FOLLOW_REPLY nonce-real-1e281015
```

This confirms normal persisted event logs are model context on resume.

### Experiment B: synthetic text history

The program then manually wrote a new session directory containing only `/session-state/events.jsonl` with synthetic events and no `workspace.yaml` or checkpoints:

```text
session.start
user.message
assistant.turn_start
assistant.message
assistant.turn_end
```

Seeded nonce:

```text
nonce-synth-7251f004
```

It then called `ResumeSession` for that synthetic session ID and asked the same follow-up question.

Observed output:

```text
SYNTH_FOLLOW_REPLY nonce-synth-7251f004
```

After resume, the SDK appended normal runtime events to the synthetic log:

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

This is the core proof: **synthetic prior events were consumed as model context after `ResumeSession`.**

### Experiment C: real tool event shapes

A scratch custom tool `echo_value` was registered with `SkipPermission = true`. Prompt:

```text
Call echo_value once with value alpha-123. Then answer with the exact tool output only.
```

Observed tool invocation and final answer:

```text
TOOL_INVOKED call_J1HnPWroYRFzzqa7uJvztLnQ echo_value map[string]interface {}{"value":"alpha-123"}
REPLY tool-output:alpha-123
```

Persisted event sequence around the tool call:

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

A request for two tool calls produced two tool calls, but the observed model/runtime executed them sequentially as separate assistant tool-request turns rather than one parallel `toolRequests` array. The schema supports multiple `toolRequests`; synthetic parallel hydration should encode multiple requests in one assistant message plus one start/complete pair per `toolCallId`, but that exact parallel synthetic shape was not separately live-proven.

### Experiment D: synthetic tool-result history

A separate scratch program seeded a synthetic session with:

```text
session.start
user.message
assistant.turn_start
assistant.message         # toolRequests: echo_value(call_synth_charlie)
tool.execution_start
tool.execution_complete   # result.content = tool-output-charlie-789
assistant.turn_end
```

No final assistant answer was seeded. After `ResumeSession`, it asked:

```text
What exact output did the previous tool execution return? Reply with that output only.
```

Observed output:

```text
SESSION synth-tool-9d84522e-c889-49e3-b852-9b3aaa843f0c EXPECTED tool-output-charlie-789 REPLY tool-output-charlie-789
```

This indicates synthetic tool execution result events can also be used as model context.

## Minimal event schema for synthetic hydration

Envelope, one JSON object per line:

```json
{
  "id": "uuid-v4-like string",
  "timestamp": "2026-05-29T21:21:43.292823Z",
  "parentId": null,
  "type": "session.start",
  "data": {}
}
```

Observed requirements/behavior:

- `id` should be unique per event.
- `timestamp` should be RFC3339/RFC3339Nano.
- `parentId` forms a linked list; `null` for the first event. Synthetic parent chains worked.
- Events are newline-delimited JSON in `/session-state/events.jsonl`.
- The runtime appends `session.resume` with `eventCount` equal to the number of seeded events.

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

Real sessions also include optional data such as:

```json
{
  "alreadyInUse": false,
  "context": { "cwd": "/tmp" },
  "remoteSteerable": false,
  "selectedModel": "..."
}
```

### User message

Minimal synthetic form that worked:

```json
{
  "type": "user.message",
  "data": {
    "content": "Remember this exact nonce..."
  }
}
```

Real sessions include additional optional fields:

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

Minimal synthetic form that worked:

```json
{
  "type": "assistant.message",
  "data": {
    "content": "OK",
    "messageId": "uuid"
  }
}
```

Real sessions may add `encryptedContent`, `interactionId`, `outputTokens`, `phase`, `reasoningOpaque`, `reasoningText`, `requestId`, and `toolRequests`.

### Assistant turn delimiters

Used successfully in synthetic logs:

```json
{ "type": "assistant.turn_start", "data": { "turnId": "0" } }
{ "type": "assistant.turn_end", "data": { "turnId": "0" } }
```

Real sessions include `interactionId` on `assistant.turn_start`.

### System/developer message

Generated type supports:

```json
{
  "type": "system.message",
  "data": {
    "content": "...",
    "role": "system"
  }
}
```

`role` is `"system"` or `"developer"`. In the live synthetic experiments I did not need to seed this manually because `ResumeSessionConfig.SystemMessage` with `Mode: "replace"` caused the SDK to append a fresh `system.message` on resume. For this project, the safer path is to pass current effective OpenAI system/developer instructions through `SystemMessageConfig{Mode:"replace"}` on every create/resume rather than rely only on seeded system events.

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

Real `tool.execution_complete` also included `interactionId` and `model`. These were not required in the synthetic tool-result proof.

## Assessment for copilot-api

### Ordinary multi-turn Chat Completions

Synthetic hydration can preserve role-native user/assistant history without collapsing all prior turns into one prompt. For each stateless Chat Completions request:

1. Create a new isolated synthetic session ID and session filesystem root.
2. Write `/session-state/events.jsonl` with `session.start` and prior OpenAI messages converted to SDK events.
3. Resume the synthetic session with `SystemMessageConfig{Mode:"replace"}` containing only caller-supplied system/developer instructions plus any minimal neutral adapter framing.
4. Send only the current user message as the new turn.
5. Delete/expire the synthetic state after the response unless persistence is explicitly needed.

### Tool-call turns

Feasible for normal OpenAI tool history:

- prior assistant message with `tool_calls` -> synthetic `assistant.message.toolRequests`;
- prior `role: tool` outputs -> synthetic `tool.execution_start` + `tool.execution_complete` linked by `toolCallId`;
- next user/current prompt -> sent as the new SDK turn.

For Responses API parked-turn continuations, the existing plan of keeping the live SDK handler parked and unblocking with client output is still cleaner when possible. Synthetic tool hydration is most useful for stateless Chat Completions continuation where the client sends back assistant tool calls and tool results in a later request.

Parallel tool calls: the event schema allows `toolRequests: []` and one execution pair per `toolCallId`. The live two-tool prompt produced sequential tool calls, not a single multi-call assistant message, so exact runtime treatment of a synthetic one-message parallel batch remains an open validation item.

## Risks and caveats

- **Schema drift:** `events.jsonl` is generated from an SDK schema but persisted resume semantics are not a public “import messages” contract. Pin SDK/CLI versions and add regression tests around hydration.
- **SDK/CLI version coupling:** the original spike showed that a mismatched installed CLI can fail at startup with protocol-shape errors. The service should control the SDK/CLI pairing, prefer the SDK-matched embedded CLI, and fail startup if runtime status cannot be read.
- **Parent IDs/timestamps:** synthetic unique IDs, RFC3339 timestamps, and a linear `parentId` chain worked. Do not assume malformed chains will continue to work.
- **Checkpoints/compaction:** e2e tests show compaction rewrites `events.jsonl` with checkpoint data. Long synthetic histories may trigger compaction or summarization. Keep synthetic Chat sessions short-lived and have a transcript fallback.
- **`transformedContent`:** real `user.message` includes SDK-generated `transformedContent`; minimal synthetic messages without it worked. Future runtimes could rely more heavily on transformed content.
- **System prompt safety:** never rely on SDK defaults. Always pass `SystemMessageConfig{Mode:"replace"}` on create/resume. Decide how to combine OpenAI `system` and `developer` messages into one replacement prompt or validated `system.message` events.
- **Tools:** real tool calls add hook events and optional telemetry fields; synthetic minimal tool result events worked, but error results, partial results, binary/multimodal tool outputs, and parallel batches need more tests.
- **Built-in tools:** the spike avoided SDK built-ins with replacement system prompt, no registered SDK tools, a denying permission handler, and a sentinel `AvailableTools`. This is not a full built-in-tool-disable proof.
- **Race/concurrency:** only write or rewrite `events.jsonl` before `ResumeSession`. Do not edit a state file while a session is active. The product invariant of single-user concurrent proxy helps, but per-session locking is still required.
- **Interaction IDs/request IDs:** omitted in synthetic events and worked. They may matter for telemetry/accounting or future context reconstruction.
- **Workspace files:** no `workspace.yaml` or checkpoints were needed for minimal synthetic resume, but the runtime may create them after resume.
- **Unsupported assumptions:** this is effectively state-file surgery. Treat as an internal adapter with tests and fallback, not as a stable upstream API guarantee.

## Recommendation for implementation plan

Update the plan from “serialize OpenAI messages into one prompt transcript” to:

1. **Primary path:** synthetic session hydration via project-owned `SessionFsProvider`, then `ResumeSession` and send only the latest user/input turn.
2. **Safety path:** always use `SystemMessageConfig{Mode:"replace"}` and tool-denying defaults; register only request-scoped client proxy tools when the request includes OpenAI tools.
3. **Fallback path:** transcript serialization remains available when hydration fails, schema validation rejects a history shape, SDK/CLI version changes break tests, or a history feature is not yet mapped.

This should improve role-native history handling while preserving the hard product invariants.

## Next concrete implementation tasks

1. Add an internal `SessionFsProvider` implementation backed by a per-request/session directory or in-memory filesystem with atomic write/append and per-session locking.
2. Define an internal OpenAI-message-to-SDK-event hydrator:
   - system/developer -> replacement `SystemMessageConfig` first; optionally `system.message` only when needed;
   - user -> `user.message`;
   - assistant text -> `assistant.turn_start`, `assistant.message`, `assistant.turn_end`;
   - assistant tool calls -> `assistant.message.toolRequests`;
   - tool results -> `tool.execution_start` + `tool.execution_complete`.
3. Add schema validation by unmarshalling every generated line into `copilot.SessionEvent` before resume.
4. Pin and verify SDK/CLI compatibility; prefer the SDK embedded CLI path for reproducibility and log runtime status at startup.
5. Add integration tests behind an opt-in/live-test flag:
   - synthetic text history nonce recall;
   - synthetic tool-result recall;
   - multiple tool results / parallel-shaped `toolRequests`;
   - fallback when resume fails.
6. Add cleanup/TTL for synthetic session state and ensure no ambient global sessions are reused.
7. Keep transcript serialization implementation as a tested fallback until hydration has enough version coverage.

## Open questions requiring parent/user decision

- Should synthetic hydration be the default for Chat Completions MVP, with transcript serialization only as fallback, despite relying on SDK internal event semantics?
- How should multiple OpenAI `system` and `developer` messages be combined: one replacement system prompt, separate synthetic `system.message` events, or reject uncommon mid-conversation system/developer messages in strict mode?
- For parallel tool calls, should MVP synthesize the natural parallel shape immediately or serialize tool results conservatively until a live parallel synthetic batch is proven?
- What is the acceptable fallback behavior when hydration fails in strict compatibility mode: return an OpenAI-shaped error, or silently use transcript serialization?
