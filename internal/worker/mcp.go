package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
)

// The worker's MCP server gives Claude Code Mosso's tools. Claude Code
// starts it as a child process (`mosso-worker mcp`, see SetMCP) and talks
// MCP to it over stdin and stdout: newline-delimited JSON-RPC 2.0. It only
// lists the tools and relays each call to the worker's loopback API, which
// hands it to the Mosso server (calls.go). It has no credentials and does
// nothing itself.

// mcpVersions are the MCP protocol versions the server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// mcpInstructions are given to Claude Code when it connects.
const mcpInstructions = "Mosso's GitHub tools. Mosso runs them on its own server with the workspace's GitHub credentials, for this workspace's repository only; the sandbox has no GitHub credentials. Commit before pushing; push before creating a pull request."

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeMCP serves MCP on in and out until in ends or ctx is cancelled,
// relaying tool calls to the worker at workerURL. Calls run concurrently.
func ServeMCP(ctx context.Context, in io.Reader, out io.Writer, workerURL string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	enc := json.NewEncoder(out)
	send := func(m rpcMessage) {
		m.JSONRPC = "2.0"
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(m)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4*maxCallBytes)
	for sc.Scan() {
		var m rpcMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			send(rpcMessage{ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(m.ID) == 0 {
			continue // a notification, such as notifications/initialized
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			version := mcpVersions[0]
			if slices.Contains(mcpVersions, p.ProtocolVersion) {
				version = p.ProtocolVersion
			}
			send(rpcMessage{ID: m.ID, Result: map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "mosso", "version": Version},
				"instructions":    mcpInstructions,
			}})
		case "ping":
			send(rpcMessage{ID: m.ID, Result: map[string]any{}})
		case "tools/list":
			send(rpcMessage{ID: m.ID, Result: map[string]any{"tools": Tools}})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
				send(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
				continue
			}
			if _, ok := FindTool(p.Name); !ok {
				send(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}})
				continue
			}
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				send(rpcMessage{ID: id, Result: toolResult(CallTool(ctx, workerURL, p.Name, p.Arguments))})
			}(m.ID)
		default:
			send(rpcMessage{ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + m.Method}})
		}
	}
	cancel()
	wg.Wait()
	return sc.Err()
}

// toolResult is an MCP tool result: the result as indented JSON text, or
// the error as text with isError set, so Claude sees what went wrong.
func toolResult(ans CallAnswer, err error) map[string]any {
	text, isError := "", false
	switch {
	case err != nil:
		text, isError = err.Error(), true
	case ans.Error != "":
		text, isError = ans.Error, true
	default:
		var v any
		if json.Unmarshal(ans.Result, &v) == nil {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				text = string(b)
			}
		}
		if text == "" {
			text = string(ans.Result)
		}
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": isError,
	}
}
