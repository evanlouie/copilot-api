# Spike: Chat Completions session-pool latency

Date: 2026-07-15

## Question

Does a pool of Copilot SDK sessions that exist in advance make the latency of
Chat Completions much lower? Compare it with the preparation of a session at the
moment of the request.

The important comparison is:

1. create a session, disconnect it, write its stored history again, and resume
   it;
2. write again and resume a session that the program created and disconnected in
   advance; and
3. the current Chat Completions implementation. This implementation does not
   call `CreateSession`. It writes a new synthetic session history, and then it
   calls `ResumeSession` directly.

## Environment

- Copilot Go SDK: `v1.0.6`
- Copilot CLI: `1.0.71-0`
- Protocol: `3`
- Model configuration: `gpt-5.5`
- Host: local macOS development machine
- Samples: 100 for each history profile and each path, after 5 warm-up cycles
- Each path used one Copilot CLI process that was already in operation. Each
  path used the same session configuration as the gateway.

The start of the CLI process took 424 ms. The first listing of the models took
1,055 ms. These are start costs of the gateway. The results for one session do
not include them.

The raw data of the run stays in the `tmp/` directory. Git ignores this
directory. The files are:

- `tmp/session_pool_latency_experiment/main.go`
- `tmp/session-pool-latency-report.json`
- `tmp/session-pool-latency-samples.csv`
- `tmp/session-pool-latency-summary.txt`

To do the experiment again, use this command:

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

The critical path is:

1. build the synthetic JSONL;
2. write and sync `events.jsonl` as one atomic operation; and
3. call `ResumeSession` with a new session ID.

This is the path of `prepareChatTurn` today. It does not need a `CreateSession`
call to the SDK.

### Cold create path

The critical path is:

1. call `CreateSession`;
2. call `Disconnect`, because then the program can replace the stored history
   safely;
3. build and write the synthetic JSONL; and
4. call `ResumeSession`.

This path creates a true SDK session at the moment of the request. Then it
writes the session again.

### Prewarmed path

The setup is outside the measured critical path of the request:

1. call `CreateSession`; and
2. call `Disconnect`.

The measured critical path is:

1. build the synthetic JSONL and write it over the event log of the session that
   exists in advance;
2. call `ResumeSession`.

The program must resume a session that exists in advance. If the SDK `Session`
stays active, and the program writes the files again, the CLI does not load that
history into its memory again.

## Results

The time `ready` stops when `ResumeSession` returns. The results do not include
the cleanup and the disconnect after the resume.

| History               | Synthetic JSONL | Current p50 / p95 | Cold create p50 / p95 | Prewarmed p50 / p95 | Mean saving of the prewarm path |
| --------------------- | --------------: | ----------------: | --------------------: | ------------------: | ------------------------------: |
| Empty (0 messages)    |           364 B |    31.8 / 45.3 ms |        40.5 / 59.8 ms |      32.5 / 45.2 ms |                          9.7 ms |
| Short (10 messages)   |          5.6 KB |    28.4 / 48.8 ms |        36.1 / 54.5 ms |      28.1 / 44.5 ms |                          8.4 ms |
| Medium (100 messages) |         65.9 KB |    33.2 / 48.4 ms |        42.4 / 63.6 ms |      32.9 / 47.9 ms |                         10.1 ms |
| Large (500 messages)  |        456.6 KB |    45.6 / 60.5 ms |        57.1 / 77.3 ms |      45.3 / 60.3 ms |                         12.5 ms |

The prewarm setup cost 6.1 ms at p50 and 11.1 ms at p95. The prewarm moves this
work out of the path of the request. It does not remove the work from the
system.

### Phase means

| History |  Build JSONL | History write to disk | Resume, current | Resume, prewarmed |
| ------- | -----------: | --------------------: | --------------: | ----------------: |
| Empty   | 0.02–0.04 ms |          13.1–13.8 ms |         18.3 ms |           18.9 ms |
| Short   | 0.09–0.16 ms |          12.4–13.0 ms |         17.7 ms |           16.5 ms |
| Medium  |     ~0.70 ms |          12.7–13.5 ms |         20.3 ms |           21.2 ms |
| Large   |   3.6–3.7 ms |          13.7–14.5 ms |         29.2 ms |           29.3 ms |

The atomic write of `events.jsonl` to the disk and the `ResumeSession` call are
the largest parts of the session preparation. The creation of the session is not
the largest cost.

### Paired differences

The readiness time of the cold-create path minus the readiness time of the
prewarmed path gives a mean saving of 8.4 ms to 12.5 ms. The approximate 95%
confidence intervals for the mean difference were:

- empty: 7.5–11.9 ms;
- short: 6.3–10.5 ms;
- medium: 7.8–12.4 ms; and
- large: 9.3–15.7 ms.

The readiness time of the current synthetic path minus the readiness time of the
prewarmed path gives a mean difference of -0.4 ms to +1.5 ms. Each approximate
95% confidence interval contains zero. Thus the two paths are not different at
the level of the noise in this run.

## Semantic validation

The experiment also checked that the candidate optimization operates correctly:

1. create a session and disconnect it;
2. write a synthetic conversation over its history. The conversation contains a
   random marker;
3. resume the session; and
4. ask the model to give the marker from the previous conversation.

The model gave the correct marker. This shows that `ResumeSession` used the new
history of the session that exists in advance. It did not use the old data from
the original creation.

## Conclusion

A prewarmed pool makes a hypothetical **create-then-write-again** design better
by approximately **8 ms to 13 ms** on this host. This is a measurable saving of
latency, but it is small. The pool does not remove the serialization of the
history, the write to the disk, or the `ResumeSession` call. Together, these
three operations are almost all of the latency of the preparation.

The current Chat Completions implementation does not call `CreateSession`. This
is more important. A new synthetic session and a session that exists in advance
and that the program writes again had the same readiness latency. The difference
was usually about 1 ms at p50. There was no clear statistical difference of the
mean. Thus a session pool does not give a useful improvement of the latency for
the current design.

A pool also adds complexity for the lifecycle, the replenishment, the
compatibility keys, the cleanup and the management of the resources. This run
shows that you must not add the pool. If you must optimize, work on the write of
the history to the disk and on the resume path. Measure the work against the
end-to-end time to the first token. Some tens of milliseconds of local
preparation are probably a small part of the latency of the model.

## Limitations

- The results are specific to this host, this file system, this SDK version and
  this CLI version.
- The benchmark is sequential. It measures the latency, and not the throughput
  at saturation.
- The benchmark stops when the session is ready. It does not include `Send`, the
  inference on the network, or the time to the first token.
- The model of a prewarmed pool was a set of sessions that the program created
  and disconnected safely. If the sessions stay active, the program cannot
  remove `ResumeSession` and keep the current mechanism that writes the stored
  history again.
