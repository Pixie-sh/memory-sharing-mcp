// Package transport exposes the gateway over authenticated Streamable HTTP MCP and a
// small JSON REST API, plus unauthenticated health probes. Both surfaces map
// gateway errors the same way; MCP failures are tool errors on HTTP 200.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pixie-sh/memory-sharing-mcp/internal/gateway"
	"github.com/pixie-sh/memory-sharing-mcp/internal/keys"
	"github.com/pixie-sh/memory-sharing-mcp/internal/provider"
)

const (
	maxBody       = 256 << 10 // REST and MCP: fits maximal content even with every byte JSON-escaped, plus framing
	indeterminate = "the operation may or may not have been applied; do not retry blindly"
)

// Handler returns the routes: /mcp (Streamable HTTP), /v1/* (REST), /healthz, and /readyz.
func Handler(g *gateway.Gateway, ks *keys.Set, log *slog.Logger) http.Handler {
	xo := http.NewCrossOriginProtection()
	srv := newServer(g, log)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// Stateless: every request is authenticated on its own and the tools need no
		// server-to-client messages, so sessions would only add expiry and hijack risks.
		Stateless:           true,
		MaxRequestBodyBytes: maxBody,
	})
	bearer := auth.RequireBearerToken(verifyKey(ks), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})

	mux := http.NewServeMux()
	mux.Handle("/mcp", xo.Handler(bearer(mcpHandler)))
	mux.Handle("POST /v1/recall", xo.Handler(rest(g, ks, log, "recall", recallInput.op)))
	mux.Handle("POST /v1/remember", xo.Handler(rest(g, ks, log, "remember", rememberInput.op)))
	mux.Handle("POST /v1/stats", xo.Handler(rest(g, ks, log, "stats", statsInput.op)))
	mux.Handle("POST /v1/forget", xo.Handler(rest(g, ks, log, "forget", forgetInput.op)))
	mux.Handle("POST /v1/sleep", xo.Handler(rest(g, ks, log, "sleep", sleepInput.op)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !g.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "not ready")
			return
		}
		io.WriteString(w, "ready")
	})
	return loopbackHost(mux)
}

// loopbackHost rejects requests that arrived on a loopback address with a
// non-loopback Host header (DNS rebinding), for every route. The SDK applies
// the same rule to /mcp only.
func loopbackHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && isLoopback(local.String()) && !isLoopback(r.Host) {
			writeError(w, http.StatusForbidden, "forbidden", "invalid Host header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// verifyKey is the MCP token verifier. The key rides in TokenInfo so tools can read
// it on each request.
func verifyKey(ks *keys.Set) auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		k, ok := ks.Authenticate(token)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: k.ID, Extra: map[string]any{"key": k}}, nil
	}
}

// bearerKey authenticates the request's Authorization header the same way verifyKey does.
func bearerKey(ks *keys.Set, r *http.Request) (keys.Key, bool) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "bearer") {
		return keys.Key{}, false
	}
	return ks.Authenticate(fields[1])
}

// rest authenticates the request, decodes the body into In, and runs the operation.
// Each request logs one line.
func rest[In any](g *gateway.Gateway, ks *keys.Set, log *slog.Logger, name string, mk func(In) gateway.Op) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		key, ok := bearerKey(ks, r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			logCall(log, "rest", name, "", "unauthorized", start)
			return
		}
		var in In
		if err := decode(w, r, &in); err != nil {
			status, class, msg := http.StatusBadRequest, "validation", "body must be one JSON object with only the documented fields"
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status, class, msg = http.StatusRequestEntityTooLarge, "too_large", "body exceeds the size limit"
			}
			writeError(w, status, class, msg)
			logCall(log, "rest", name, key.ID, class, start)
			return
		}
		raw, err := g.Do(r.Context(), key, mk(in))
		if err != nil {
			class, status, msg := outcome(err)
			writeError(w, status, class, msg)
			logCall(log, "rest", name, key.ID, class, start)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
		logCall(log, "rest", name, key.ID, "ok", start)
	})
}

// decode reads exactly one JSON object of at most maxBody bytes with no unknown
// fields. An empty body decodes as the zero value.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		return errors.New("body must contain a single JSON object")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// outcome maps a gateway error to a stable class, an HTTP status, and a message
// that never includes memory content.
func outcome(err error) (class string, status int, msg string) {
	switch {
	case errors.Is(err, gateway.ErrValidation):
		return "validation", http.StatusBadRequest, err.Error()
	case errors.Is(err, gateway.ErrForbidden):
		return "forbidden", http.StatusForbidden, err.Error()
	case errors.Is(err, gateway.ErrBusy):
		return "busy", http.StatusTooManyRequests, err.Error()
	case errors.Is(err, gateway.ErrUnavailable), errors.Is(err, gateway.ErrShuttingDown):
		return "unavailable", http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, provider.ErrIndeterminate),
		// The caller stopped waiting; a dispatched call still runs to completion.
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "indeterminate", http.StatusBadGateway, indeterminate
	}
	var opErr *provider.OpError
	if errors.As(err, &opErr) {
		// Only the exception type: provider messages may quote memory content.
		typ, _, _ := strings.Cut(opErr.Msg, ":")
		if !isIdent(typ) {
			typ = "unknown"
		}
		return "provider", http.StatusUnprocessableEntity, "provider error: " + typ
	}
	return "internal", http.StatusInternalServerError, "internal error"
}

func writeError(w http.ResponseWriter, status int, class, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{class, msg})
}

// logCall writes the one log line per request or tool call. It never includes the
// secret, the Authorization header, or memory content.
func logCall(log *slog.Logger, transport, op, keyID, outcome string, start time.Time) {
	log.Info("call", "transport", transport, "op", op, "key", keyID, "outcome", outcome, "duration", time.Since(start))
}

// isIdent reports whether s looks like an exception type name, so nothing else
// from a provider message can reach a client.
func isIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
