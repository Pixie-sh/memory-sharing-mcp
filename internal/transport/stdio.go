package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is reported to MCP peers; the command sets it from its build version.
var Version = "dev"

// maxToolArgs stays below the owner's MCP body limit, leaving room for JSON-RPC framing.
const maxToolArgs = 200 << 10

// bearer adds the API key to every request the owner client sends.
type bearer string

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(req)
}

// Proxy serves the owner's memory tools on downstream and forwards every call to the
// owner at endpoint. It connects and lists the tools before serving, so an unreachable
// owner or a rejected key fails here. It never starts a backend or opens the database.
// It returns when the downstream session ends or ctx is done.
func Proxy(ctx context.Context, endpoint, secret string, downstream mcp.Transport) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must not contain credentials, a query, or a fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Host)) {
		return fmt.Errorf("endpoint %s: want https, or http on a loopback host", endpoint)
	}

	// Bound startup only: the SDK detaches the connection's lifetime from this ctx.
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "memory-sharing-mcp", Version: Version}, nil)
	owner, list, err := connectOwner(sctx, client, &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Transport: bearer(secret),
			// Never send the key to a redirect target.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		MaxRetries:           -1, // a write whose outcome is unknown must not be replayed
		DisableStandaloneSSE: true,
	})
	if err != nil {
		return err
	}
	defer owner.Close()

	srv := mcp.NewServer(&mcp.Implementation{Name: "memory-sharing-mcp", Version: Version}, nil)
	for _, t := range list.Tools {
		name := t.Name
		srv.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				// The owner answers oversized bodies with a bare 413, which ends the SDK
				// session; reject them here as an ordinary validation error instead.
				if len(req.Params.Arguments) > maxToolArgs {
					return toolResult("validation: arguments exceed the size limit", true), nil
				}
				res, err := owner.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: req.Params.Arguments})
				if err != nil {
					// Any failure after dispatch may hide an applied write; report it, never retry.
					return toolResult("indeterminate: lost contact with the memory service; "+indeterminate, true), nil
				}
				return res, nil
			})
	}

	ss, err := srv.Connect(ctx, downstream, nil)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- ss.Wait() }()
	lost := make(chan error, 1)
	go func() { lost <- owner.Wait() }()
	select {
	case err := <-done:
		ss.Close()
		return err
	case err := <-lost:
		// A failed owner session never recovers; exit so the agent restarts the adapter.
		ss.Close()
		<-done
		return fmt.Errorf("lost connection to memory service: %v", err)
	case <-ctx.Done():
		ss.Close()
		<-done
		return nil
	}
}

// connectOwner connects to the owner and lists its tools. While the owner is not
// listening yet (connection refused) it retries every second until ctx ends, so an
// agent launched at login does not lose memory for its whole session because the
// service was a moment behind it. Any other failure, such as a rejected key, is final.
func connectOwner(ctx context.Context, client *mcp.Client, t *mcp.StreamableClientTransport) (*mcp.ClientSession, *mcp.ListToolsResult, error) {
	for {
		owner, err := client.Connect(ctx, t, nil)
		if err == nil {
			list, err := owner.ListTools(ctx, nil)
			if err == nil {
				return owner, list, nil
			}
			owner.Close()
			if !errors.Is(err, syscall.ECONNREFUSED) {
				return nil, nil, fmt.Errorf("list memory tools: %w", err)
			}
		} else if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, nil, fmt.Errorf("connect to memory service: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("connect to memory service: %w", err)
		case <-time.After(time.Second):
		}
	}
}
