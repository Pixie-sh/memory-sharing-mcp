package transport

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pixie-sh/memory-sharing-mcp/internal/gateway"
	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
)

// The input types are shared with the REST API. Optional fields use omitempty so
// the SDK does not mark them required.
type (
	recallInput struct {
		Query string `json:"query" jsonschema:"text to search for in stored memories"`
		TopK  int    `json:"top_k,omitempty" jsonschema:"maximum number of results, 1-50 (default 5)"`
	}
	rememberInput struct {
		Content    string   `json:"content" jsonschema:"memory text to store"`
		Source     string   `json:"source,omitempty" jsonschema:"where the memory came from (default: key name)"`
		Importance *float64 `json:"importance,omitempty" jsonschema:"importance from 0 to 1 (default 0.5)"`
	}
	statsInput  struct{}
	forgetInput struct {
		ID string `json:"id" jsonschema:"id of the memory to delete, as returned by recall (\"<provider>:<id>\")"`
	}
	sleepInput struct{}
)

func (in recallInput) op() gateway.Op {
	return gateway.Recall{Query: in.Query, TopK: in.TopK}
}

func (in rememberInput) op() gateway.Op {
	return gateway.Remember{Content: in.Content, Source: in.Source, Importance: in.Importance}
}

func (statsInput) op() gateway.Op { return gateway.Stats{} }

func (in forgetInput) op() gateway.Op { return gateway.Forget{ID: in.ID} }

func (sleepInput) op() gateway.Op { return gateway.Sleep{} }

// newServer returns the MCP server with the five memory tools. Every call
// authenticates with the key of its own HTTP request, not the session's key.
func newServer(g *gateway.Gateway, log *slog.Logger) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "memory-sharing-mcp", Version: Version}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "memory_recall", Description: "Search long-term memory shared across agents and sessions. Call it before substantial work and whenever earlier preferences, decisions, project facts, or past fixes could help. Returns the best matches first, with complete content; ids look like \"<provider>:<id>\"."},
		func(ctx context.Context, req *mcp.CallToolRequest, in recallInput) (*mcp.CallToolResult, any, error) {
			return runTool(ctx, g, log, req, "recall", in.op()), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "memory_remember", Description: "Save one memory to long-term memory shared across agents and sessions. Use it when asked to remember something, and for durable preferences, confirmed decisions, and reusable project gotchas. Keep it concise and self-contained; never store secrets or transient status."},
		func(ctx context.Context, req *mcp.CallToolRequest, in rememberInput) (*mcp.CallToolResult, any, error) {
			return runTool(ctx, g, log, req, "remember", in.op()), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "memory_stats", Description: "Show statistics for each memory store."},
		func(ctx context.Context, req *mcp.CallToolRequest, in statsInput) (*mcp.CallToolResult, any, error) {
			return runTool(ctx, g, log, req, "stats", in.op()), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "memory_forget", Description: "Delete one memory by the id memory_recall returned, or hide it from recall where the store cannot delete it. Needs a maintenance key and server support."},
		func(ctx context.Context, req *mcp.CallToolRequest, in forgetInput) (*mcp.CallToolResult, any, error) {
			return runTool(ctx, g, log, req, "forget", in.op()), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "memory_sleep", Description: "Consolidate memories in each store that supports it. Needs a maintenance key and server support."},
		func(ctx context.Context, req *mcp.CallToolRequest, in sleepInput) (*mcp.CallToolResult, any, error) {
			return runTool(ctx, g, log, req, "sleep", in.op()), nil, nil
		})
	return srv
}

// runTool runs op for the key on the current request. Every failure is a tool
// error, so the HTTP exchange still returns 200.
func runTool(ctx context.Context, g *gateway.Gateway, log *slog.Logger, req *mcp.CallToolRequest, name string, op gateway.Op) *mcp.CallToolResult {
	start := time.Now()
	key, ok := requestKey(req)
	if !ok {
		logCall(log, "mcp", name, "", "unauthorized", start)
		return toolResult("unauthorized: no API key on request", true)
	}
	raw, err := g.Do(ctx, key, op)
	if err != nil {
		class, _, msg := outcome(err)
		logCall(log, "mcp", name, key.ID, class, start)
		return toolResult(fmt.Sprintf("%s: %s", class, msg), true)
	}
	logCall(log, "mcp", name, key.ID, "ok", start)
	return toolResult(string(raw), false)
}

// requestKey returns the key that verifyKey attached to the current HTTP request.
func requestKey(req *mcp.CallToolRequest) (keys.Key, bool) {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return keys.Key{}, false
	}
	k, ok := req.Extra.TokenInfo.Extra["key"].(keys.Key)
	return k, ok
}

func toolResult(text string, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isError, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}
