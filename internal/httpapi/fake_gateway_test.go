package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// unimplementedGateway is the base every hand-written gateway fake in this
// package embeds. It satisfies copilotgw.Gateway with methods that panic
// naming themselves.
//
// The fakes used to embed the copilotgw.Gateway interface directly, leaving it
// nil. That made two failure modes indistinguishable and both unreadable: a
// handler routing a request to a method the fake did not override, and a new
// method appearing on the interface, each surfaced as a nil pointer
// dereference deep inside net/http rather than as a statement about which call
// was unexpected. Panicking by name turns both into a one-line diagnosis.
//
// This is deliberately not a recorded "unexpected call" error return: a fake
// reaching a method it does not implement means the test is not exercising the
// path it claims to, and that should fail loudly rather than be absorbed into
// a plausible-looking HTTP response.
type unimplementedGateway struct{}

var _ copilotgw.Gateway = unimplementedGateway{}

func (unimplementedGateway) Start(context.Context) error {
	panic("unexpected call to Gateway.Start")
}

func (unimplementedGateway) Stop() error {
	panic("unexpected call to Gateway.Stop")
}

func (unimplementedGateway) Ready(context.Context) error {
	panic("unexpected call to Gateway.Ready")
}

func (unimplementedGateway) ListModels(context.Context) ([]copilotgw.Model, error) {
	panic("unexpected call to Gateway.ListModels")
}

func (unimplementedGateway) ValidateModel(context.Context, string) error {
	panic("unexpected call to Gateway.ValidateModel")
}

func (unimplementedGateway) Chat(context.Context, copilotgw.ChatRequest) (*copilotgw.TurnResult, error) {
	panic("unexpected call to Gateway.Chat")
}

func (unimplementedGateway) StreamChat(context.Context, copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	panic("unexpected call to Gateway.StreamChat")
}

func (unimplementedGateway) ContinueChatToolCalls(context.Context, copilotgw.ChatContinuationRequest) (*copilotgw.TurnResult, error) {
	panic("unexpected call to Gateway.ContinueChatToolCalls")
}

func (unimplementedGateway) StreamContinueChatToolCalls(context.Context, copilotgw.ChatContinuationRequest) (<-chan copilotgw.StreamEvent, error) {
	panic("unexpected call to Gateway.StreamContinueChatToolCalls")
}

func (unimplementedGateway) CreateResponse(context.Context, copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	panic("unexpected call to Gateway.CreateResponse")
}

func (unimplementedGateway) WarmResponse(context.Context, copilotgw.ResponseRequest) (*copilotgw.WarmResponseResult, error) {
	panic("unexpected call to Gateway.WarmResponse")
}

func (unimplementedGateway) StreamResponse(context.Context, copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	panic("unexpected call to Gateway.StreamResponse")
}

func (unimplementedGateway) GetResponse(context.Context, string) (*openai.Response, error) {
	panic("unexpected call to Gateway.GetResponse")
}

func (unimplementedGateway) DeleteResponse(context.Context, string) error {
	panic("unexpected call to Gateway.DeleteResponse")
}

// misroutedGateway answers model validation and nothing else, so a request that
// reaches /v1/responses lands on an unimplemented method.
type misroutedGateway struct {
	unimplementedGateway
}

func (misroutedGateway) ValidateModel(context.Context, string) error { return nil }

// A handler calling a method its fake does not implement must say which method
// it was. Before unimplementedGateway existed the fakes embedded a nil
// copilotgw.Gateway, so this produced a bare "invalid memory address or nil
// pointer dereference" with a stack rooted in net/http and no mention of the
// call that was actually unexpected.
func TestUnimplementedGatewayMethodsNameThemselves(t *testing.T) {
	logs := &syncBuffer{}
	s := New(config.Config{}, misroutedGateway{}, slog.New(slog.NewTextHandler(logs, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, "unexpected call to Gateway.CreateResponse") {
		t.Fatalf("panic log did not name the unexpected call:\n%s", got)
	}
}
