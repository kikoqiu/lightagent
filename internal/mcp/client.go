// Package mcp implements a minimal Model Context Protocol (MCP) client built
// only on the Go standard library. It speaks JSON-RPC 2.0 over three
// transports: stdio (a local child process), Streamable HTTP (a POST per
// message) and the legacy HTTP+SSE transport.
//
// The client is used to connect to MCP servers declared in config.json and to
// expose their tools to the agent (as locked/deferred functions when the
// unlock discovery mode is enabled).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// protocolVersion is the MCP revision advertised during initialize. The server
// may negotiate a different one; the negotiated value is what the Streamable
// HTTP transport then sends in the MCP-Protocol-Version header.
const protocolVersion = "2025-06-18"

// clientName / clientVersion identify this client during initialize.
const (
	clientName    = "lightagent"
	clientVersion = "0.1.0"
)

// request is a JSON-RPC 2.0 request or notification. ID is 0 for
// notifications.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// response is a JSON-RPC 2.0 response.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "mcp: unknown rpc error"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("mcp: rpc error %d: %s (%s)", e.Code, e.Message, strings.TrimSpace(string(e.Data)))
	}
	return fmt.Sprintf("mcp: rpc error %d: %s", e.Code, e.Message)
}

// parseMessageID decodes a JSON-RPC id into the numeric form used by this
// client. It also accepts a quoted numeric string for lenient servers.
func parseMessageID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "null" || s == "" {
		return 0, false
	}
	if unquoted, err := strconv.Unquote(s); err == nil {
		s = unquoted
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// transport carries JSON-RPC messages to and from an MCP server.
type transport interface {
	// roundTrip sends a request and returns the matching response.
	roundTrip(ctx context.Context, req *request) (*response, error)
	// notify sends a notification (no response is expected).
	notify(ctx context.Context, req *request) error
	// close releases the transport (kills the child process / closes streams).
	close() error
}

// Client is a connected MCP session.
type Client struct {
	tr      transport
	nextID  atomic.Int64
	version string // negotiated protocol version
	info    ServerInfo
}

// ServerInfo carries the information an MCP server reports about itself in the
// initialize result: serverInfo (name/title/version) and the optional
// instructions describing how to use the server and its features.
type ServerInfo struct {
	Name         string
	Title        string
	Version      string
	Instructions string
}

func newClient(tr transport) *Client {
	return &Client{tr: tr, version: protocolVersion}
}

// call sends a request and returns its result payload.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req := &request{JSONRPC: "2.0", ID: c.nextID.Add(1), Method: method, Params: params}
	resp, err := c.tr.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("mcp: empty response for %s", method)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// notify sends a notification.
func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.tr.notify(ctx, &request{JSONRPC: "2.0", Method: method, Params: params})
}

// close closes the underlying transport.
func (c *Client) close() error { return c.tr.close() }

// initialize performs the MCP handshake and then notifies the server that the
// client is ready.
func (c *Client) initialize(ctx context.Context) error {
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	})
	if err != nil {
		return err
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &res); err == nil {
		if strings.TrimSpace(res.ProtocolVersion) != "" {
			c.version = res.ProtocolVersion
		}
		c.info = ServerInfo{
			Name:         strings.TrimSpace(res.ServerInfo.Name),
			Title:        strings.TrimSpace(res.ServerInfo.Title),
			Version:      strings.TrimSpace(res.ServerInfo.Version),
			Instructions: strings.TrimSpace(res.Instructions),
		}
	}
	if pv, ok := c.tr.(interface{ setProtocolVersion(string) }); ok {
		pv.setProtocolVersion(c.version)
	}
	return c.notify(ctx, "notifications/initialized", map[string]any{})
}

// ServerInfo returns the information the server reported during initialize.
func (c *Client) ServerInfo() ServerInfo { return c.info }

// Tool describes one MCP tool exposed by a server.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// listTools lists all tools, following pagination cursors.
func (c *Client) listTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor,omitempty"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
		}
		all = append(all, res.Tools...)
		if strings.TrimSpace(res.NextCursor) == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
}

// ContentItem is one entry of an MCP tool result's content array.
type ContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// CallResult is the result of tools/call.
type CallResult struct {
	Content           []ContentItem `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// callTool invokes a tool on the server.
func (c *Client) callTool(ctx context.Context, name string, args map[string]any) (*CallResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call: %w", err)
	}
	return &res, nil
}
