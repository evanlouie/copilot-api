package httpapi

import "testing"

func TestTerminalStreamSuffix(t *testing.T) {
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
