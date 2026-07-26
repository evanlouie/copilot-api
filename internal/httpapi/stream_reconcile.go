package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

// terminalStreamSuffix reconciles a turn's terminal text against the text the
// client has already been sent.
//
// A turn can resolve text or reasoning only at the very end - a consolidated
// reasoning block, or content the SDK never emitted as a delta - so the terminal
// value is allowed to extend what was streamed but never to contradict it. Both
// transports need exactly this decision at four points (chat content, chat
// reasoning, response message text, response reasoning summary) and each one
// used to carry its own copy of it.
//
// It returns the suffix still owed to the client (empty when the stream is
// already complete) or, when the terminal value is not an extension of the
// streamed one, the caller's mismatch error: the stream has been committed with
// a 200, so a divergence can only be reported in-band.
func terminalStreamSuffix(terminal, streamed, mismatch string) (string, error) {
	if terminal == streamed {
		return "", nil
	}
	if !strings.HasPrefix(terminal, streamed) {
		return "", apierr.Upstream(mismatch)
	}
	return strings.TrimPrefix(terminal, streamed), nil
}

// toolArgumentsSuffix reconciles a tool call's finished arguments against the
// argument fragments already delivered for it.
//
// It deliberately does not lead with terminalStreamSuffix's byte-prefix test,
// because for tool arguments that test can essentially never hold. The
// fragments are the model's own bytes. The finished arguments are those bytes
// decoded into a map[string]any and re-encoded by encoding/json (see
// toolproxy.rawArgs), which sorts object keys, drops insignificant whitespace,
// escapes `<`, `>` and `&` as \u003c, \u003e and \u0026, and reformats numbers.
// `{"location": "Paris", "unit": "celsius"}` comes back compacted,
// `{"b":1,"a":2}` comes back reordered, `{"q":"a <b> c"}` comes back escaped,
// and `{"n":1.0}` comes back as `{"n":1}`. Only a compact, single-key,
// ASCII-safe, integer-valued object survives the round trip unchanged, so a
// prefix test would turn nearly every real tool call into a stream failure.
//
// Equality is therefore semantic: fragments that decode to the same JSON value
// as the finished arguments have already delivered this call, and nothing more
// is owed. What the client accumulated is the model's own rendering of that
// same value, which is the same call - tool outputs are matched back by call
// id, never by argument bytes - while the canonical form is still what the
// terminal `.done` event, the non-streaming body and the stored record carry.
//
// A byte prefix is still honoured, because a fragment stream that genuinely
// stopped short is owed its remainder. Anything else is a real divergence and
// is reported through the caller's message.
func toolArgumentsSuffix(terminal, delivered, mismatch string) (string, error) {
	if delivered == "" || terminal == delivered {
		return strings.TrimPrefix(terminal, delivered), nil
	}
	if sameJSONValue(terminal, delivered) {
		return "", nil
	}
	return terminalStreamSuffix(terminal, delivered, mismatch)
}

// sameJSONValue reports whether two JSON texts denote the same value. Both
// sides are decoded with the same rules, so the normalizations that make their
// bytes differ - key order, whitespace, HTML escaping, number formatting - all
// cancel out. Anything that is not valid JSON is equal to nothing.
func sameJSONValue(a, b string) bool {
	var left, right any
	if err := json.Unmarshal([]byte(a), &left); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &right); err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}
