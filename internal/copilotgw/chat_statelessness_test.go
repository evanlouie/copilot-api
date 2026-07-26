package copilotgw

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

// TestChatTurnPersistsNoResponseRecord pins the boundary between the two
// surfaces: Chat Completions is stateless. It has no `store`, no
// `previous_response_id` and no retrievable response, so a chat turn must
// persist session metadata only. Anything that starts writing a response record
// for a chat turn would also start consuming the responses retention quota and
// would make `GET /v1/responses/{id}` answer for ids Chat never handed out.
func TestChatTurnPersistsNoResponseRecord(t *testing.T) {
	t.Parallel()
	dataDir, stateDir := t.TempDir(), t.TempDir()
	store := sessionstore.New(dataDir, stateDir, t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	g := &RealGateway{
		cfg:   config.Config{DataDir: dataDir},
		store: store,
		fs:    sessionfs.NewManager(dataDir),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	const sessionID = "chat_persistence_probe"
	result := &TurnResult{ID: "chatcmpl_probe", Model: "gpt-5", Text: "hi", FinishReason: "stop"}
	g.saveChatSessionMetadata(sessionID, g.fs.SessionRoot(sessionID), "gpt-5", result)

	if _, err := os.Stat(filepath.Join(dataDir, "sessions", sessionID, "metadata.json")); err != nil {
		t.Fatalf("chat turn did not persist its session metadata: %v", err)
	}
	if _, err := store.LoadResponse(result.ID); !errors.Is(err, sessionstore.ErrNotFound) {
		t.Fatalf("LoadResponse(%q) error = %v, want ErrNotFound", result.ID, err)
	}
	// The responses tree must hold no record and no retention link, so a chat
	// turn cannot displace a stored response under the retention budget.
	responsesDir := filepath.Join(stateDir, "responses")
	err := filepath.WalkDir(responsesDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Base(path) == ".owner" {
			return nil
		}
		return errors.New("chat turn wrote into the responses tree: " + path)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// chatSurfaceEntryPoints are the gateway methods a Chat Completions request can
// enter through.
var chatSurfaceEntryPoints = []string{
	"Chat",
	"StreamChat",
	"ContinueChatToolCalls",
	"StreamContinueChatToolCalls",
	"prepareChatTurn",
	"saveChatSessionMetadata",
	"resolveChatHistoryWithImageBudget",
	"chatRequestFromContinuation",
	"continueChatToolCallsFromTranscript",
	"streamContinueChatToolCallsFromTranscript",
}

// responseStateOperations are the store operations that create or hold
// Responses-only durable state.
var responseStateOperations = []string{"SaveResponse", "PinResponse", "LoadResponse", "LoadResponseForContinuation", "DeleteResponse"}

// TestChatSurfaceNeverReachesResponseStore is the structural half of the
// stateless-Chat invariant. The behavioral test above can only observe the code
// paths a unit test can drive; this one walks the whole call graph reachable
// from the Chat entry points and fails if any of it touches Responses-only
// durable state. It is deliberately strict: unifying the two stacks is expected
// future work, and this is the fence that keeps that work from quietly giving
// Chat `store` semantics.
func TestChatSurfaceNeverReachesResponseStore(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	funcs := map[string][]*ast.FuncDecl{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn)
			}
		}
	}

	visited := map[string]struct{}{}
	queue := slices.Clone(chatSurfaceEntryPoints)
	for len(queue) > 0 {
		name := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, seen := visited[name]; seen {
			continue
		}
		visited[name] = struct{}{}
		decls, ok := funcs[name]
		if !ok {
			if slices.Contains(chatSurfaceEntryPoints, name) {
				t.Fatalf("chat entry point %q no longer exists; update this test with its replacement", name)
			}
			continue
		}
		for _, decl := range decls {
			ast.Inspect(decl, func(node ast.Node) bool {
				// Skip branches the shared code already gates on the Responses
				// surface (`if kind == "response"`). Those cannot run for a chat turn,
				// which passes kind "chat".
				if stmt, ok := node.(*ast.IfStmt); ok && guardedOnResponseKind(stmt.Cond) {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := ""
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					callee = fn.Name
				case *ast.SelectorExpr:
					callee = fn.Sel.Name
				default:
					return true
				}
				if slices.Contains(responseStateOperations, callee) {
					t.Errorf("%s: chat path %q reaches Responses-only store operation %q", fset.Position(call.Pos()), name, callee)
				}
				queue = append(queue, callee)
				return true
			})
		}
	}
}

// guardedOnResponseKind reports whether a condition includes a `kind ==
// "response"` test, which is how the shared turn runner fences Responses-only
// behavior off from chat turns.
func guardedOnResponseKind(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(node ast.Node) bool {
		binary, ok := node.(*ast.BinaryExpr)
		if !ok || binary.Op != token.EQL {
			return true
		}
		ident, ok := binary.X.(*ast.Ident)
		if !ok || ident.Name != "kind" {
			return true
		}
		lit, ok := binary.Y.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && lit.Value == `"response"` {
			found = true
		}
		return true
	})
	return found
}
