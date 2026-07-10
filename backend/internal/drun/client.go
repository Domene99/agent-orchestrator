// Package drun provides a Go client for the drun-mcp MCP server.
// It speaks the MCP Streamable HTTP transport (POST /mcp) with lazy
// session initialization.
package drun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultMCPURL = "http://127.0.0.1:7273/mcp"

// Client is a stateful MCP HTTP client for the drun-mcp server.
// The MCP session is initialized lazily on the first tool call.
// A single Client may be shared across goroutines.
type Client struct {
	url          string
	httpClient   *http.Client
	mu           sync.Mutex
	mcpSessionID string // MCP transport session (Mcp-Session-Id header)
	nextID       atomic.Int64
}

// NewClient returns a Client aimed at url (default: 127.0.0.1:7273/mcp).
func NewClient(url string) *Client {
	if url == "" {
		url = DefaultMCPURL
	}
	return &Client{
		url:        url,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// SessionState is the subset of drun's SessionState that the Go client needs.
type SessionState struct {
	SessionID string `json:"session_id"`
}

// rpc types

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// initialize performs the MCP handshake and caches the Mcp-Session-Id header.
// Must be called with c.mu held.
func (c *Client) initialize(ctx context.Context) error {
	type clientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type params struct {
		ProtocolVersion string     `json:"protocolVersion"`
		Capabilities    any        `json:"capabilities"`
		ClientInfo      clientInfo `json:"clientInfo"`
	}
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcReq{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "initialize",
		Params: params{
			ProtocolVersion: "2024-11-05",
			Capabilities:    map[string]any{},
			ClientInfo:      clientInfo{Name: "ao", Version: "1.0.0"},
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("drun initialize: %w", err)
	}
	defer resp.Body.Close()
	c.mcpSessionID = resp.Header.Get("Mcp-Session-Id")

	// Send notifications/initialized (MCP spec requirement; fire-and-forget).
	go func() {
		nctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		notif, _ := json.Marshal(rpcReq{JSONRPC: "2.0", Method: "notifications/initialized"})
		nr, _ := http.NewRequestWithContext(nctx, http.MethodPost, c.url, bytes.NewReader(notif))
		nr.Header.Set("Content-Type", "application/json")
		if c.mcpSessionID != "" {
			nr.Header.Set("Mcp-Session-Id", c.mcpSessionID)
		}
		resp, err := c.httpClient.Do(nr)
		if err == nil {
			resp.Body.Close()
		}
	}()
	return nil
}

// readMCPBody reads the response body and returns raw JSON. When the server
// sends SSE (text/event-stream) it extracts the first "data:" line instead.
func readMCPBody(resp *http.Response, contentType string) ([]byte, error) {
	if !strings.Contains(contentType, "text/event-stream") {
		return io.ReadAll(resp.Body)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			return []byte(data), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no data event in SSE stream")
}

// callTool calls a drun MCP tool and returns the first content block's text.
func (c *Client) callTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	c.mu.Lock()
	if c.mcpSessionID == "" {
		if err := c.initialize(ctx); err != nil {
			c.mu.Unlock()
			return "", err
		}
	}
	mcpSID := c.mcpSessionID
	c.mu.Unlock()

	if args == nil {
		args = map[string]any{}
	}
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcReq{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params:  map[string]any{"name": toolName, "arguments": args},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if mcpSID != "" {
		req.Header.Set("Mcp-Session-Id", mcpSID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("drun %s: %w", toolName, err)
	}
	defer resp.Body.Close()

	// If the server sent SSE despite Accept: application/json, unwrap the first data line.
	contentType := resp.Header.Get("Content-Type")
	body2, err := readMCPBody(resp, contentType)
	if err != nil {
		return "", fmt.Errorf("drun %s: read response: %w", toolName, err)
	}

	var rpc rpcResp
	if err := json.Unmarshal(body2, &rpc); err != nil {
		return "", fmt.Errorf("drun %s: decode response: %w", toolName, err)
	}
	if rpc.Error != nil {
		return "", fmt.Errorf("drun %s: %s (code %d)", toolName, rpc.Error.Message, rpc.Error.Code)
	}
	var tr toolResult
	if err := json.Unmarshal(rpc.Result, &tr); err != nil {
		return "", fmt.Errorf("drun %s: parse tool result: %w", toolName, err)
	}
	if tr.IsError && len(tr.Content) > 0 {
		return "", fmt.Errorf("drun %s: %s", toolName, tr.Content[0].Text)
	}
	if len(tr.Content) == 0 {
		return "", nil
	}
	return tr.Content[0].Text, nil
}

// CreateSession creates a new drun session and returns its session_id.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	text, err := c.callTool(ctx, "create_session", nil)
	if err != nil {
		return "", err
	}
	var state SessionState
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		return "", fmt.Errorf("drun create_session: parse response: %w", err)
	}
	return state.SessionID, nil
}

// Mount loads a host path into a drun session.
func (c *Client) Mount(ctx context.Context, sessionID, hostPath string) error {
	_, err := c.callTool(ctx, "session_mount", map[string]any{
		"session_id": sessionID,
		"path":       hostPath,
	})
	return err
}

// Snapshot serializes a session's checkpoint history to a .drun file and
// returns the path the file was written to.
func (c *Client) Snapshot(ctx context.Context, sessionID string) (string, error) {
	text, err := c.callTool(ctx, "session_snapshot", map[string]any{
		"session_id": sessionID,
	})
	if err != nil {
		return "", err
	}
	var result struct {
		SnapshotPath string `json:"snapshot_path"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return "", fmt.Errorf("drun session_snapshot: parse path: %w", err)
	}
	return result.SnapshotPath, nil
}

// Restore loads a .drun snapshot and returns the new session_id.
func (c *Client) Restore(ctx context.Context, snapshotPath string) (string, error) {
	text, err := c.callTool(ctx, "session_restore", map[string]any{
		"path": snapshotPath,
	})
	if err != nil {
		return "", err
	}
	var state SessionState
	if err := json.Unmarshal([]byte(text), &state); err != nil {
		return "", fmt.Errorf("drun session_restore: parse response: %w", err)
	}
	return state.SessionID, nil
}

// Close terminates a drun session and frees its resources.
func (c *Client) Close(ctx context.Context, sessionID string) error {
	_, err := c.callTool(ctx, "session_close", map[string]any{
		"session_id": sessionID,
	})
	return err
}

// Label assigns a human-readable label to a drun session.
func (c *Client) Label(ctx context.Context, sessionID, label string) error {
	_, err := c.callTool(ctx, "session_label", map[string]any{
		"session_id": sessionID,
		"label":      label,
	})
	return err
}

// IsAlive returns nil if the drun MCP server is reachable.
func (c *Client) IsAlive(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialize(ctx)
}
