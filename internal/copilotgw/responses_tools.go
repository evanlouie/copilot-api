package copilotgw

import (
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

// ValidateLoadedTools reports whether a client-supplied tool_search_output
// catalog can actually be installed on a Copilot session. It is the gateway's
// job because only the gateway knows how a normalized tool is projected onto
// the SDK tool set; the transport calls it so a doomed catalog is rejected on
// the way in rather than mid-turn.
func ValidateLoadedTools(tools []toolcatalog.NormalizedTool) error {
	_, err := toolproxy.FlattenResponsesTools(tools)
	return err
}
