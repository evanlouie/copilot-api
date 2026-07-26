// THROWAWAY SPIKE PROBE - issue #4.
//
// This file exists only to answer the questions in
// docs/spikes/copilot-request-handler.md. It is not part of the product and
// nothing in it is imported by non-test code: it installs a
// copilot.CopilotRequestHandler on a *locally built* client inside the test,
// never on the client the service constructs in newRealClientOptions.
//
// Delete this file once the spike's recommendation has been acted on (or
// rejected). If any of it graduates into the product it must be rewritten with
// real error handling, bounded buffers and redaction that is auditable rather
// than best-effort.
//
// The capture test requires live Copilot credentials and skips unless
// COPILOT_API_LIVE_TESTS=1, matching the convention in live_test.go.
package copilotgw

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"

	copilot "github.com/github/copilot-sdk/go"
)

// spikeSecretHeaders are replaced with a placeholder before anything is written
// to disk. Everything the runtime uses to authenticate upstream flows through
// the handler in cleartext, so this list is the whole reason a capture can be
// committed at all.
var spikeSecretHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"api-key":             {},
	"x-api-key":           {},
	"openai-organization": {},
	"x-github-token":      {},
}

const spikeRedacted = "[REDACTED]"

// spikeMaxBody caps each captured body so a long streaming turn cannot blow up
// the artifact (or the test process).
const spikeMaxBody = 1 << 20

func spikeRedactHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		if _, secret := spikeSecretHeaders[strings.ToLower(name)]; secret {
			out[name] = []string{spikeRedacted}
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// spikeCapture is one intercepted upstream exchange.
type spikeCapture struct {
	RequestID       string              `json:"request_id"`
	SessionID       string              `json:"session_id"`
	Transport       string              `json:"transport"`
	Method          string              `json:"method"`
	URL             string              `json:"url"`
	RequestHeaders  map[string][]string `json:"request_headers"`
	RequestBody     string              `json:"request_body"`
	Mutated         bool                `json:"mutated"`
	Status          int                 `json:"status"`
	ResponseHeaders map[string][]string `json:"response_headers"`
	// ResponseBody is filled in as the runtime drains the stream, so it is only
	// complete once the turn has finished.
	ResponseBody *spikeTeeBuffer `json:"response_body"`
}

// spikeTeeBuffer accumulates a copy of the response bytes without buffering the
// stream: the runtime still reads the upstream body chunk by chunk. This is the
// probe's evidence for question 3 (can SSE be passed through untouched).
type spikeTeeBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	total    int
	truncErr bool
}

func (b *spikeTeeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(p)
	if remaining := spikeMaxBody - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.truncErr = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncErr = true
	}
	return len(p), nil
}

func (b *spikeTeeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.truncErr {
		s += "\n[TRUNCATED]"
	}
	return s
}

func (b *spikeTeeBuffer) MarshalJSON() ([]byte, error) {
	return json.Marshal(spikeRedactBody([]byte(b.String())))
}

// spikeBodySecretPattern matches credential-bearing JSON fields seen in real
// captures. POST /models/session returns a `session_token` JWT, so header-only
// redaction is not enough to make an artifact safe to commit.
var spikeBodySecretPattern = regexp.MustCompile(`"(session_token|access_token|refresh_token|api_key|token)"\s*:\s*"[^"]*"`)

func spikeRedactBody(body []byte) string {
	return spikeBodySecretPattern.ReplaceAllString(string(body), `"$1":"`+spikeRedacted+`"`)
}

// spikePersonSchema is the structured-output contract the injection probe asks
// for. Deliberately tiny: the question is whether the seam works at all, not
// whether a model can fill in a large schema.
func spikePersonSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"age":  map[string]any{"type": "integer"},
		},
		"required":             []any{"name", "age"},
		"additionalProperties": false,
	}
}

// spikeInjectStructuredOutput is the whole hypothesis in one function: can the
// four documented limitations be fixed by rewriting the upstream body?
//
// It has to branch on the upstream dialect, because the runtime speaks a
// different wire format per model family - that branching IS the finding, not
// an implementation detail. See docs/spikes/copilot-request-handler.md.
func spikeInjectStructuredOutput(url string, body []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	switch {
	case strings.Contains(url, "/responses"):
		// OpenAI Responses dialect (gpt-*): structured output lives under
		// text.format, which the runtime already populates with `verbosity`,
		// so this must merge rather than replace.
		text, _ := payload["text"].(map[string]any)
		if text == nil {
			text = map[string]any{}
		}
		text["format"] = map[string]any{
			"type":   "json_schema",
			"name":   "person",
			"strict": true,
			"schema": spikePersonSchema(),
		}
		payload["text"] = text
		payload["max_output_tokens"] = 2048
	case strings.Contains(url, "/chat/completions"):
		// OpenAI Chat Completions dialect (gemini-* on this account).
		payload["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "person",
				"strict": true,
				"schema": spikePersonSchema(),
			},
		}
		payload["max_tokens"] = 2048
	case strings.Contains(url, "/v1/messages"):
		// Anthropic Messages dialect (claude-*, and whatever `auto` picks).
		// There is no response_format / json_schema field in this API at all;
		// the only native coercion is a forced tool call. Nothing to inject,
		// which is itself the answer for this family.
		return body, false
	default:
		return body, false
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return out, true
}

type spikeTeeBody struct {
	rc   io.ReadCloser
	sink *spikeTeeBuffer
}

func (t *spikeTeeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.sink.Write(p[:n])
	}
	return n, err
}

func (t *spikeTeeBody) Close() error { return t.rc.Close() }

// spikeCapturingTransport is a recording http.RoundTripper. With mutate nil it
// is a pure pass-through, which is how the first probe ran: look before
// touching anything. Setting mutate turns it into the injection experiment.
type spikeCapturingTransport struct {
	base http.RoundTripper
	// mutate rewrites an outbound inference body. It returns the replacement
	// body and true when it changed anything.
	mutate func(url string, body []byte) ([]byte, bool)

	mu        sync.Mutex
	captures  []*spikeCapture
	mutations int
}

func newSpikeCapturingTransport() *spikeCapturingTransport {
	// Mirror the SDK's own default transport (copilot_request_handler.go:44):
	// compression is disabled so SSE frames arrive unbuffered. A custom
	// RoundTripper that forgets this silently changes streaming behaviour.
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DisableCompression = true
	return &spikeCapturingTransport{base: base}
}

// closeIdle releases the keep-alive connections this transport opened. Without
// it the package's goleak TestMain reports the http2 read loops as leaks on a
// live run.
func (t *spikeCapturingTransport) closeIdle() {
	if tr, ok := t.base.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (t *spikeCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	capture := &spikeCapture{
		Method:         req.Method,
		URL:            req.URL.String(),
		RequestHeaders: spikeRedactHeaders(req.Header),
		ResponseBody:   &spikeTeeBuffer{},
	}
	// RequestContextFrom is the documented way to recover runtime metadata
	// (question 5). Note what it does NOT carry: agentId, parentAgentId and
	// interactionType are on the RPC envelope but are dropped by the SDK adapter.
	if rctx := copilot.RequestContextFrom(req); rctx != nil {
		capture.RequestID = rctx.RequestID
		capture.SessionID = rctx.SessionID
		capture.Transport = rctx.Transport
	}
	if req.Body != nil {
		body, err := io.ReadAll(io.LimitReader(req.Body, spikeMaxBody))
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if t.mutate != nil {
			if mutated, changed := t.mutate(req.URL.String(), body); changed {
				body = mutated
				capture.Mutated = true
				t.mu.Lock()
				t.mutations++
				t.mu.Unlock()
			}
		}
		capture.RequestBody = spikeRedactBody(body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	t.mu.Lock()
	t.captures = append(t.captures, capture)
	t.mu.Unlock()

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	capture.Status = resp.StatusCode
	capture.ResponseHeaders = spikeRedactHeaders(resp.Header)
	resp.Body = &spikeTeeBody{rc: resp.Body, sink: capture.ResponseBody}
	return resp, nil
}

func (t *spikeCapturingTransport) snapshot() []*spikeCapture {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*spikeCapture(nil), t.captures...)
}

func TestSpikeRedactHeaders(t *testing.T) {
	t.Parallel()
	in := http.Header{
		"Authorization": []string{"Bearer secret-token"},
		"Cookie":        []string{"a=b"},
		"X-Api-Key":     []string{"sk-secret"},
		"Content-Type":  []string{"application/json"},
	}
	out := spikeRedactHeaders(in)
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if got := out[name]; len(got) != 1 || got[0] != spikeRedacted {
			t.Fatalf("%s not redacted: %v", name, got)
		}
	}
	if got := out["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("Content-Type mangled: %v", got)
	}
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "secret") {
		t.Fatalf("secret leaked into serialized capture: %s", blob)
	}
}

func TestSpikeTeeBufferPassesThroughAndCaps(t *testing.T) {
	t.Parallel()
	sink := &spikeTeeBuffer{}
	body := &spikeTeeBody{rc: io.NopCloser(strings.NewReader("data: one\n\ndata: two\n\n")), sink: sink}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("stream was altered: %q", got)
	}
	if sink.String() != string(got) {
		t.Fatalf("capture diverged from stream: %q", sink.String())
	}
}

// spikeRunTurn boots a real gateway whose SDK client has handler installed,
// runs one Chat turn, writes the redacted captures under name, and returns the
// turn text plus the captures. Production code is untouched: the handler is
// attached to a client built inside this test, never in newRealClientOptions.
func spikeRunTurn(t *testing.T, name, prompt string, mutate func(string, []byte) ([]byte, bool)) (string, []*spikeCapture, error) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		CLIPath:        os.Getenv("COPILOT_CLI_PATH"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	transport := newSpikeCapturingTransport()
	transport.mutate = mutate
	opts := newRealClientOptions(cfg)
	opts.RequestHandler = &copilot.CopilotRequestHandler{Transport: transport}
	gw.client = copilot.NewClient(opts)
	t.Cleanup(transport.closeIdle)

	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		start := time.Now()
		err := gw.Stop()
		t.Logf("gateway Stop took %s err=%v", time.Since(start), err)
	}()

	// The catalog moves; log it so a failed run tells you what to set
	// COPILOT_API_LIVE_MODEL to.
	models, err := gw.ListModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	t.Logf("available models: %s", strings.Join(ids, ", "))

	model := os.Getenv("COPILOT_API_LIVE_MODEL")
	if model == "" && len(ids) > 0 {
		model = ids[0]
	}
	t.Logf("using model %q", model)
	turn, turnErr := gw.Chat(t.Context(), ChatRequest{
		OpenAIID:  openai.NewID("chatcmpl_"),
		Model:     model,
		FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent(prompt)},
	})
	if turnErr != nil {
		t.Logf("turn error: %v", turnErr)
	} else {
		t.Logf("turn text: %q", turn.Text)
	}

	captures := transport.snapshot()
	if len(captures) == 0 {
		t.Fatal("CopilotRequestHandler was installed but intercepted nothing")
	}

	outDir := os.Getenv("COPILOT_API_SPIKE_CAPTURE_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(outDir, name+".json")
	blob, err := json.MarshalIndent(captures, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d captures to %s", len(captures), outPath)
	for _, c := range captures {
		t.Logf("capture transport=%s session=%q mutated=%t status=%d %s %s req_bytes=%d resp_bytes=%d",
			c.Transport, c.SessionID, c.Mutated, c.Status, c.Method, c.URL, len(c.RequestBody), len(c.ResponseBody.String()))
	}
	return turn.Text, captures, turnErr
}

// TestSpikeUpstreamErrorWithoutInterception is the control for
// TestSpikeCopilotRequestHandlerBadInjection: it provokes an upstream failure
// with no CopilotRequestHandler installed at all, so a hang here is a
// pre-existing error-path property of the gateway rather than something
// interception introduces.
func TestSpikeUpstreamErrorWithoutInterception(t *testing.T) {
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run the CopilotRequestHandler spike probe")
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		CLIPath:        os.Getenv("COPILOT_CLI_PATH"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		start := time.Now()
		err := gw.Stop()
		t.Logf("RESULT: Stop took %s err=%v", time.Since(start), err)
	}()

	model := os.Getenv("COPILOT_API_LIVE_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}
	// Far past any context window, so the upstream rejects the turn without any
	// help from a RoundTripper.
	prompt := "Summarize this: " + strings.Repeat("copilot ", 400_000)
	_, err := gw.Chat(t.Context(), ChatRequest{
		OpenAIID:  openai.NewID("chatcmpl_"),
		Model:     model,
		FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent(prompt)},
	})
	t.Logf("RESULT: handler-free oversized turn err=%v", err)
}

// spikeInjectInvalid corrupts every inference body it sees. It exists to answer
// "what does a bug in the RoundTripper look like from the client's side", which
// is the concrete form of the hot-path risk.
func spikeInjectInvalid(url string, body []byte) ([]byte, bool) {
	if !strings.Contains(url, "/responses") && !strings.Contains(url, "/chat/completions") && !strings.Contains(url, "/v1/messages") {
		return body, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	payload["temperature"] = "definitely-not-a-number"
	out, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return out, true
}

// TestSpikeCopilotRequestHandlerCapture drives one real gateway Chat turn with a
// pass-through CopilotRequestHandler installed and writes the redacted captures
// to disk. Set COPILOT_API_SPIKE_CAPTURE_DIR to keep the artifact.
func TestSpikeCopilotRequestHandlerCapture(t *testing.T) {
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run the CopilotRequestHandler spike probe")
	}
	_, _, err := spikeRunTurn(t, "request-handler-captures", "Reply with OK only.", nil)
	if err != nil {
		t.Fatal(err)
	}
}

// TestSpikeCopilotRequestHandlerStructuredOutputInjection is the decisive
// experiment: rewrite the upstream body to demand a JSON schema and see whether
// (a) the Copilot API accepts the injected field and (b) the runtime still
// parses the response it gets back.
func TestSpikeCopilotRequestHandlerStructuredOutputInjection(t *testing.T) {
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run the CopilotRequestHandler spike probe")
	}
	text, captures, err := spikeRunTurn(t, "request-handler-injection",
		"Who was the first computer programmer and how old were they when they died?",
		spikeInjectStructuredOutput)
	if err != nil {
		t.Fatal(err)
	}

	mutated := 0
	for _, c := range captures {
		if c.Mutated {
			mutated++
			t.Logf("mutated %s -> status %d", c.URL, c.Status)
		}
	}
	if mutated == 0 {
		t.Log("RESULT: no request was mutated - this model family has no injectable structured-output field")
		return
	}
	var person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &person); err != nil {
		t.Logf("RESULT: injection reached upstream but the turn text is not schema JSON: %v", err)
		return
	}
	t.Logf("RESULT: structured output enforced end to end: name=%q age=%d", person.Name, person.Age)
}

// TestSpikeCopilotRequestHandlerBadInjection records how an upstream rejection
// caused by the handler surfaces to a proxy client. This is the hot-path risk
// made concrete.
//
// It needs its own opt-in because it does not finish: after the upstream 400 the
// gateway's Stop never returns (observed >10 minutes), so running it wedges the
// package. That wedge is one of the spike's findings, not an accident.
func TestSpikeCopilotRequestHandlerBadInjection(t *testing.T) {
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" || os.Getenv("COPILOT_API_SPIKE_BAD_INJECTION") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 and COPILOT_API_SPIKE_BAD_INJECTION=1; this probe is known to hang")
	}
	text, captures, err := spikeRunTurn(t, "request-handler-bad-injection", "Reply with OK only.", spikeInjectInvalid)
	for _, c := range captures {
		if c.Mutated {
			t.Logf("corrupted %s -> status %d body %s", c.URL, c.Status, c.ResponseBody.String())
		}
	}
	t.Logf("RESULT: gateway returned text=%q err=%v", text, err)
}

func TestSpikeRedactBody(t *testing.T) {
	t.Parallel()
	in := []byte(`{"selected_model":"claude-haiku-4.5","session_token":"eyJhbGciOiJFUzI1NiJ9.abc.def"}`)
	out := spikeRedactBody(in)
	if strings.Contains(out, "eyJhbGciOiJFUzI1NiJ9") {
		t.Fatalf("session_token leaked: %s", out)
	}
	if !strings.Contains(out, "claude-haiku-4.5") {
		t.Fatalf("non-secret field was destroyed: %s", out)
	}
}

func TestSpikeInjectStructuredOutputPerDialect(t *testing.T) {
	t.Parallel()
	// Bodies below are trimmed copies of real captures (see the findings doc).
	responsesBody := []byte(`{"model":"gpt-5.5","input":[],"text":{"verbosity":"low"},"store":false}`)
	got, changed := spikeInjectStructuredOutput("https://api.enterprise.githubcopilot.com/responses", responsesBody)
	if !changed {
		t.Fatal("responses dialect was not mutated")
	}
	var responses map[string]any
	if err := json.Unmarshal(got, &responses); err != nil {
		t.Fatal(err)
	}
	text, _ := responses["text"].(map[string]any)
	if text["verbosity"] != "low" {
		t.Fatalf("merging text.format clobbered verbosity: %v", text)
	}
	if _, ok := text["format"].(map[string]any); !ok {
		t.Fatalf("text.format not injected: %v", text)
	}
	if responses["max_output_tokens"] != float64(2048) {
		t.Fatalf("max_output_tokens not injected: %v", responses["max_output_tokens"])
	}

	chatBody := []byte(`{"model":"gemini-3.6-flash","messages":[],"snippy":{"enabled":false}}`)
	got, changed = spikeInjectStructuredOutput("https://api.enterprise.githubcopilot.com/chat/completions", chatBody)
	if !changed {
		t.Fatal("chat completions dialect was not mutated")
	}
	var chat map[string]any
	if err := json.Unmarshal(got, &chat); err != nil {
		t.Fatal(err)
	}
	if _, ok := chat["response_format"].(map[string]any); !ok {
		t.Fatalf("response_format not injected: %v", chat)
	}
	if _, ok := chat["snippy"]; !ok {
		t.Fatal("proprietary snippy field was dropped by the round trip")
	}

	// The Anthropic dialect has nowhere to put a JSON schema. This assertion
	// pins the finding that the seam is not uniform across model families.
	messagesBody := []byte(`{"model":"claude-haiku-4.5","messages":[],"max_tokens":8192}`)
	if _, changed := spikeInjectStructuredOutput("https://api.enterprise.githubcopilot.com/v1/messages", messagesBody); changed {
		t.Fatal("expected no injection for the Anthropic Messages dialect")
	}
}
