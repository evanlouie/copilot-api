package httpapi

import (
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
