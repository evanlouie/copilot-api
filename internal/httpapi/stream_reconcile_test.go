package httpapi

import "testing"

// The fragments a model streams are its own bytes; the finished arguments are
// those bytes round-tripped through map[string]any by toolproxy.rawArgs. Every
// case below except the last is a real round-trip observed from encoding/json,
// and every one of them fails a byte-prefix test - which is why tool arguments
// get their own reconciliation.
func TestToolArgumentsSuffixReconcilesSemanticallyNotByteWise(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		terminal  string
		delivered string
		want      string
		wantErr   bool
	}{
		{name: "whitespace is dropped", terminal: `{"location":"Paris","unit":"celsius"}`, delivered: `{"location": "Paris", "unit": "celsius"}`},
		{name: "object keys are sorted", terminal: `{"a":2,"b":1}`, delivered: `{"b":1,"a":2}`},
		{name: "angle brackets are html-escaped", terminal: `{"q":"a \u003cb\u003e c"}`, delivered: `{"q":"a <b> c"}`},
		{name: "floats are reformatted", terminal: `{"n":1}`, delivered: `{"n":1.0}`},
		{name: "compact single key round-trips", terminal: `{"q":"alpha"}`, delivered: `{"q":"alpha"}`},
		{name: "nothing streamed owes the whole call", terminal: `{"q":"alpha"}`, delivered: "", want: `{"q":"alpha"}`},
		{name: "a stream that stopped short is owed its remainder", terminal: `{"q":"alpha"}`, delivered: `{"q":`, want: `"alpha"}`},
		{name: "a different call is a divergence", terminal: `{"q":"alpha"}`, delivered: `{"q":"beta"}`, wantErr: true},
		{name: "unparseable fragments are a divergence", terminal: `{"q":"alpha"}`, delivered: `not json at all`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := toolArgumentsSuffix(test.terminal, test.delivered, "mismatch")
			if test.wantErr {
				if err == nil {
					t.Fatalf("suffix = %q, want a mismatch error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("suffix = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTerminalStreamSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		terminal string
		streamed string
		want     string
		wantErr  bool
	}{
		{name: "already complete", terminal: "hello", streamed: "hello"},
		{name: "terminal only", terminal: "hello", streamed: "", want: "hello"},
		{name: "extends the stream", terminal: "hello world", streamed: "hello", want: " world"},
		{name: "nothing streamed or resolved", terminal: "", streamed: ""},
		{name: "diverges", terminal: "goodbye", streamed: "hello", wantErr: true},
		{name: "truncates the stream", terminal: "hell", streamed: "hello", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := terminalStreamSuffix(test.terminal, test.streamed, "mismatch")
			if test.wantErr {
				if err == nil {
					t.Fatalf("suffix = %q, want a mismatch error", got)
				}
				if err.Error() == "" {
					t.Fatal("mismatch error lost its message")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("suffix = %q, want %q", got, test.want)
			}
		})
	}
}
