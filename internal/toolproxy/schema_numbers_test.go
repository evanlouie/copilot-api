package toolproxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// numericSchema exercises the two ways a float64 round trip corrupts a client's
// JSON Schema: an integer beyond 2^53 loses its last digit, and a large
// exponent is re-spelled by Go's float formatter.
const numericSchema = `{"type":"object","properties":{"count":{"type":"integer","enum":[9007199254740993],"maximum":1e21,"minimum":-9007199254740993},"ratio":{"type":"number","multipleOf":0.30000000000000004}}}`

// TestSchemaMapPreservesClientNumbers pins the property that matters: whatever
// the client wrote is what the SDK re-encodes for the model. CanonicalRawJSON
// is the reference because internal/toolcatalog already decodes these documents
// with UseNumber for exactly this reason; the two paths must agree.
func TestSchemaMapPreservesClientNumbers(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(numericSchema)
	params, err := schemaMap("lookup", raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if want := toolcatalog.CanonicalRawJSON(raw); string(got) != want {
		t.Fatalf("schemaMap round trip rewrote the client's schema:\n got  %s\n want %s", got, want)
	}
}

// TestRequestToolsPreserveClientNumbers checks the property end to end, through
// the parameters actually handed to the SDK, so a future caller cannot bypass
// schemaMap and reintroduce the corruption.
func TestRequestToolsPreserveClientNumbers(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{{
		Type:     "function",
		Function: openai.FunctionTool{Name: "lookup", Description: "look things up", Parameters: json.RawMessage(numericSchema)},
	}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	tools := rt.Tools()
	if len(tools) != 1 {
		t.Fatalf("Tools() = %d tools, want 1", len(tools))
	}
	got, err := json.Marshal(tools[0].Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if want := toolcatalog.CanonicalRawJSON(json.RawMessage(numericSchema)); string(got) != want {
		t.Fatalf("SDK tool parameters rewrote the client's schema:\n got  %s\n want %s", got, want)
	}
}

// TestSchemaMapRejectsNonObjectParameters covers the classification, not just
// the rejection: a bad `parameters` is the client's mistake, so it must be an
// invalid-input error naming the tool and the request field rather than a raw
// encoding/json message about map[string]interface {}.
func TestSchemaMapRejectsNonObjectParameters(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`true`, `[]`, `[{"type":"object"}]`, `"object"`, `42`} {
		_, err := schemaMap("lookup", json.RawMessage(raw))
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("schemaMap(%s) error = %v, want *apierr.Error", raw, err)
		}
		if apiErr.Kind != apierr.KindInvalidInput || apiErr.Param != "tools" {
			t.Fatalf("schemaMap(%s) error = %#v, want invalid input on param tools", raw, apiErr)
		}
		if !strings.Contains(apiErr.Message, `"lookup"`) {
			t.Fatalf("schemaMap(%s) message %q does not name the offending tool", raw, apiErr.Message)
		}
		if strings.Contains(apiErr.Message, "map[string]interface") {
			t.Fatalf("schemaMap(%s) leaked a Go type into the client message: %q", raw, apiErr.Message)
		}
	}
}

func TestSchemaMapRejectsMalformedParameters(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{`, `{"type":}`, `{} {}`, `{"a":1} trailing`} {
		_, err := schemaMap("lookup", json.RawMessage(raw))
		var apiErr *apierr.Error
		if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindInvalidInput {
			t.Fatalf("schemaMap(%s) error = %v, want invalid input", raw, err)
		}
	}
}

// TestSchemaMapDefaultsEmptyParameters keeps the absent-schema behavior, which
// several tools rely on, unchanged.
func TestSchemaMapDefaultsEmptyParameters(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{``, `null`, ` null `} {
		params, err := schemaMap("lookup", json.RawMessage(raw))
		if err != nil {
			t.Fatalf("schemaMap(%q) error = %v", raw, err)
		}
		got, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"properties":{},"type":"object"}` {
			t.Fatalf("schemaMap(%q) = %s", raw, got)
		}
	}
}
