// Package mcp implements an MCP over Streamable HTTP server providing
// separate endpoints for Orchestrator and Worker roles.
package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
)

// jsonrpcRequest is a JSON-RPC 2.0 request.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// toolCallParams is the params for a tools/call request.
type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ToolHandler is a function that handles a tool call.
type ToolHandler func(ctx context.Context, args map[string]any) (any, error)

// ToolDef describes a tool for tools/list.
type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// Server handles MCP over HTTP for a single role (orchestrator or worker).
type Server struct {
	role     string
	secret   string // optional Bearer token; empty means no auth required
	tools    map[string]ToolHandler
	toolDefs []ToolDef
}

// NewServer creates a new MCP server for the given role.
// secret is an optional shared secret; if non-empty every request must carry
// "Authorization: Bearer {secret}".
func NewServer(role, secret string) *Server {
	return &Server{
		role:   role,
		secret: secret,
		tools:  make(map[string]ToolHandler),
	}
}

// Register adds a tool handler with its definition.
func (s *Server) Register(def ToolDef, handler ToolHandler) {
	s.tools[def.Name] = handler
	s.toolDefs = append(s.toolDefs, def)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.secret != "" {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := hdr[len("Bearer "):]
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var req jsonrpcRequest
	limited := http.MaxBytesReader(w, r.Body, 4<<20) // 4 MB limit
	if err := json.NewDecoder(limited).Decode(&req); err != nil {
		writeError(w, nil, -32700, "parse error: "+err.Error())
		return
	}

	if req.JSONRPC != "2.0" {
		writeError(w, req.ID, -32600, "invalid request: jsonrpc must be 2.0")
		return
	}

	switch req.Method {
	case "initialize":
		writeResult(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ccx-t2", "version": "1.0.0"},
		})

	case "notifications/initialized":
		w.WriteHeader(http.StatusNoContent)

	case "tools/list":
		writeResult(w, req.ID, map[string]any{"tools": s.toolDefs})

	case "tools/call":
		s.handleToolCall(r.Context(), w, req)

	default:
		writeError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleToolCall(ctx context.Context, w http.ResponseWriter, req jsonrpcRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeError(w, req.ID, -32602, "invalid params: "+err.Error())
		return
	}

	handler, ok := s.tools[params.Name]
	if !ok {
		writeError(w, req.ID, -32601, fmt.Sprintf("tool not found: %s", params.Name))
		return
	}

	result, err := handler(ctx, params.Arguments)
	if err != nil {
		log.Printf("mcp/%s tool %s error: %v", s.role, params.Name, err)
		writeError(w, req.ID, -32000, err.Error())
		return
	}

	// Serialize result to JSON string for content[0].text.
	text, err := json.Marshal(result)
	if err != nil {
		writeError(w, req.ID, -32000, "marshal result: "+err.Error())
		return
	}

	writeResult(w, req.ID, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
	})
}

func writeResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func writeError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// stringArg extracts a required string argument.
func stringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

// optionalStringArg extracts an optional string argument.
func optionalStringArg(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// stringSliceArg extracts a required []string argument.
func stringSliceArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	switch val := v.(type) {
	case []string:
		return val, nil
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("argument %q must be an array of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("argument %q must be an array of strings", key)
}

// intArg extracts a required integer argument. JSON numbers arrive as float64;
// fractional values and out-of-range values are rejected.
func intArg(args map[string]any, key string) (int, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing required argument %q", key)
	}
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("argument %q must be an integer, got %v", key, n)
		}
		if n < math.MinInt || n > math.MaxInt {
			return 0, fmt.Errorf("argument %q out of int range: %v", key, n)
		}
		return int(n), nil
	case int:
		return n, nil
	case int64:
		if n < math.MinInt || n > math.MaxInt {
			return 0, fmt.Errorf("argument %q out of int range: %v", key, n)
		}
		return int(n), nil
	}
	return 0, fmt.Errorf("argument %q must be an integer", key)
}

// inSchema returns a JSON Schema object for tools/list.
// "required" is omitted entirely when empty to avoid emitting null.
func inSchema(properties map[string]any, required []string) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// prop is a convenience function for building schema properties.
func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

// arrayProp creates an array property for schema.
func arrayProp(itemType, description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": itemType},
	}
}

// replaceMcpURL replaces {url} and {secret} in mcp_args.
// {url} is replaced with the MCP endpoint URL.
// {secret} is replaced with the optional shared secret (empty if not configured).
func replaceMcpURL(mcpArgs, url, secret string) string {
	// Use strings.NewReplacer for a single-pass substitution so that neither
	// replacement value is scanned for the other's pattern — prevents a secret
	// containing "{url}" or a URL containing "{secret}" from triggering
	// unintended double-expansion.
	return strings.NewReplacer("{url}", url, "{secret}", secret).Replace(mcpArgs)
}
