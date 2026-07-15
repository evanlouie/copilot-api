# Spike: Chat Completions session-pool latency

Date: 2026-07-15

## Question

Would a pool of pre-created Copilot SDK sessions materially reduce Chat
Completions latency compared with preparing a session on demand?

The important comparison is:

1. create a session, disconnect it, rewrite its persisted history, and resume it;
2. rewrite and resume a session that was created and disconnected in advance; and
3. the current Chat Completions implementation, which does not call
   `CreateSession` at all: it writes a new synthetic session history and calls
   `ResumeSession` directly.

## Environment

- Copilot Go SDK: `v1.0.6`
- Copilot CLI: `1.0.71-0`
- Protocol: `3`
- Model configuration: `gpt-5.5`
- Host: local macOS development machine
- Samples: 100 per history profile and path, after 5 warm-up cycles
- All paths used one already-running Copilot CLI process and the same session
  configuration as the gateway.

CLI process startup took 424 ms and the initial model listing took 1,055 ms.
Those are gateway startup costs and were excluded from per-session results.

Raw artifacts from the run are intentionally kept under the ignored `tmp/`
directory:

- `tmp/session_pool_latency_experiment/main.go`
- `tmp/session-pool-latency-report.json`
- `tmp/session-pool-latency-samples.csv`
- `tmp/session-pool-latency-summary.txt`

The experiment can be repeated with:

```sh
go run tmp/session_pool_latency_experiment/main.go \
  -n 100 \
  -warmup 5 \
  -validate=true \
  -timeout 10m \
  -out tmp/session-pool-latency-report.json \
  -csv tmp/session-pool-latency-samples.csv
```

## Paths measured

### Current synthetic path

Critical path:

1. build synthetic JSONL;
2. atomically write and sync `events.jsonl`; and
3. call `ResumeSession` with a fresh session ID.

This corresponds to `prepareChatTurn` today. It requires no SDK
`CreateSession` call.

### Cold create path

Critical path:

1. call `CreateSession`;
2. call `Disconnect` so its persisted history can be replaced safely;
3. build and write synthetic JSONL; and
4. call `ResumeSession`.

This represents creating a real SDK session on demand before rewriting it.

### Prewarmed path

Setup outside the measured request critical path:

1. call `CreateSession`; and
2. call `Disconnect`.

Measured critical path:

1. build and write synthetic JSONL over the pre-created session's event log;
2. call `ResumeSession`.

A pre-created session still has to be resumed. Rewriting its filesystem while
keeping the SDK `Session` active does not cause the CLI's in-memory conversation
to reload that history.

## Results

`ready` ends when `ResumeSession` returns. Cleanup/disconnect after resume is
excluded.

| History | Synthetic JSONL | Current p50 / p95 | Cold create p50 / p95 | Prewarmed p50 / p95 | Mean cold saving from prewarm |
|---|---:|---:|---:|---:|---:|
| Empty (0 messages) | 364 B | 31.8 / 45.3 ms | 40.5 / 59.8 ms | 32.5 / 45.2 ms | 9.7 ms |
| Short (10 messages) | 5.6 KB | 28.4 / 48.8 ms | 36.1 / 54.5 ms | 28.1 / 44.5 ms | 8.4 ms |
| Medium (100 messages) | 65.9 KB | 33.2 / 48.4 ms | 42.4 / 63.6 ms | 32.9 / 47.9 ms | 10.1 ms |
| Large (500 messages) | 456.6 KB | 45.6 / 60.5 ms | 57.1 / 77.3 ms | 45.3 / 60.3 ms | 12.5 ms |

The prewarm setup itself cost 6.1 ms at p50 and 11.1 ms at p95. Prewarming
moves that work out of the request path; it does not eliminate it globally.

### Phase means

| History | Build JSONL | Durable history write | Resume, current | Resume, prewarmed |
|---|---:|---:|---:|---:|
| Empty | 0.02–0.04 ms | 13.1–13.8 ms | 18.3 ms | 18.9 ms |
| Short | 0.09–0.16 ms | 12.4–13.0 ms | 17.7 ms | 16.5 ms |
| Medium | ~0.70 ms | 12.7–13.5 ms | 20.3 ms | 21.2 ms |
| Large | 3.6–3.7 ms | 13.7–14.5 ms | 29.2 ms | 29.3 ms |

The durable, atomic `events.jsonl` write and `ResumeSession` dominate session
preparation. Session creation is not the dominant cost.

### Paired differences

For cold-create minus prewarmed readiness, mean savings ranged from 8.4 to
12.5 ms. Approximate 95% confidence intervals for the mean difference were:

- empty: 7.5–11.9 ms;
- short: 6.3–10.5 ms;
- medium: 7.8–12.4 ms; and
- large: 9.3–15.7 ms.

For current-synthetic minus prewarmed readiness, the mean differences ranged
from -0.4 to +1.5 ms. Every approximate 95% confidence interval included zero.
The two paths are therefore indistinguishable at the level of noise observed in
this run.

## Semantic validation

The experiment also checked that the optimization candidate was functionally
valid:

1. create and disconnect a session;
2. overwrite its history with a synthetic conversation containing a random
   marker;
3. resume it; and
4. ask the model to return the marker from the previous conversation.

The model returned the exact marker. This confirms that `ResumeSession` consumed
the rewritten history of the pre-created session rather than stale state from
its original creation.

## Conclusion

A prewarmed pool would improve a hypothetical **create-then-rewrite** design by
roughly **8–13 ms** on this host. That is a measurable but modest latency saving.
It does not avoid history serialization, the durable filesystem write, or
`ResumeSession`, which together account for almost all preparation latency.

More importantly, the current Chat Completions implementation already bypasses
`CreateSession`. A fresh synthetic session and a rewritten pre-created session
had effectively identical readiness latency, generally within about 1 ms at
p50, with no statistically clear mean difference. Therefore a session pool
would not provide a meaningful latency improvement for the current design.

A pool would also add lifecycle, replenishment, compatibility-keying, cleanup,
and resource-management complexity. Based on this run, it should not be added.
Optimization work, if needed, should instead target the durable history write
and resume path, and should be evaluated against end-to-end time-to-first-token,
where tens of milliseconds of local preparation are likely to be a small share
of model latency.

## Limitations

- Results are host-, filesystem-, SDK-, and CLI-version specific.
- The benchmark is sequential and measures latency, not saturation throughput.
- It stops at session readiness; it does not include `Send`, network inference,
  or time to first token.
- A prewarmed pool was modeled as safely created and disconnected sessions.
  Keeping sessions active cannot remove `ResumeSession` while retaining the
  current persisted-history rewrite mechanism.
