package httpapi

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails this package if a test leaves a goroutine behind.
//
// This package's handlers own SSE keep-alive tickers, WebSocket ping and idle
// watchdogs, and the producer goroutine behind every stream. A leaked one is a
// parked handler holding a live SDK session in production, so it should be a
// test failure here rather than something only a production heap profile
// finds. It already paid for itself: the spinning WebSocket read loop fixed in
// the commit before this one was found this way.
//
// Any entry added below must name the loop it covers and say why that loop
// legitimately outlives a test. An unexplained ignore is how leak detection
// stops detecting leaks.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// net/http's client-side connection pool. Tests talk to httptest
		// servers through http.DefaultTransport, whose idle connections are
		// serviced by a read/write goroutine pair that outlives the response
		// body and only exits on the transport's own idle timeout. They belong
		// to net/http's pool, not to any handler this package owns.
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
	)
}
