package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"lightagent/internal/config"
)

// ServerTool is one MCP tool together with the server that provides it.
type ServerTool struct {
	Server string
	Tool   Tool
}

// Manager owns every MCP server connection and the tools they expose.
type Manager struct {
	mu     sync.Mutex
	conns  map[string]*Client
	tools  []ServerTool
	closed bool
}

// Connect loads every enabled server from cfg, performs the MCP handshake and
// lists its tools. A failing server is reported through the returned error but
// does not prevent the remaining servers from being used.
func Connect(ctx context.Context, cfg config.MCPConfig) (*Manager, error) {
	m := &Manager{conns: make(map[string]*Client)}
	if !cfg.Enabled {
		return m, nil
	}

	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		server := cfg.Servers[name]
		if !server.Enabled {
			continue
		}
		client, err := connectServer(ctx, server)
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp server %q: %w", name, err))
			continue
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			_ = client.close()
			errs = append(errs, fmt.Errorf("mcp server %q: tools/list: %w", name, err))
			continue
		}
		m.conns[name] = client
		for _, tool := range tools {
			m.tools = append(m.tools, ServerTool{Server: name, Tool: tool})
		}
	}
	return m, errors.Join(errs...)
}

func connectServer(ctx context.Context, server config.MCPServerConfig) (*Client, error) {
	switch server.EffectiveType() {
	case config.MCPTransportStdio:
		env, err := stdioEnv(server)
		if err != nil {
			return nil, err
		}
		tr, err := newStdioTransport(server.Command, server.Args, env)
		if err != nil {
			return nil, err
		}
		client := newClient(tr)
		if err := client.initialize(ctx); err != nil {
			_ = client.close()
			return nil, err
		}
		return client, nil
	case config.MCPTransportHTTP:
		client := newClient(newHTTPTransport(server.URL, server.Headers))
		if err := client.initialize(ctx); err != nil {
			_ = client.close()
			return nil, err
		}
		return client, nil
	case config.MCPTransportSSE:
		tr, err := newSSETransport(ctx, server.URL, server.Headers)
		if err != nil {
			return nil, err
		}
		client := newClient(tr)
		if err := client.initialize(ctx); err != nil {
			_ = client.close()
			return nil, err
		}
		return client, nil
	default:
		return nil, fmt.Errorf("unsupported transport %q", server.EffectiveType())
	}
}

// stdioEnv merges the env_file entries and inline env map with the current
// process environment.
func stdioEnv(server config.MCPServerConfig) ([]string, error) {
	extra := map[string]string{}
	if strings.TrimSpace(server.EnvFile) != "" {
		path := server.EnvFile
		if !filepath.IsAbs(path) {
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
		}
		vars, err := LoadEnvFile(path)
		if err != nil {
			return nil, err
		}
		for key, value := range vars {
			extra[key] = value
		}
	}
	for key, value := range server.Env {
		extra[key] = value
	}
	return buildEnv(extra), nil
}

// Tools returns every discovered tool.
func (m *Manager) Tools() []ServerTool {
	if m == nil {
		return nil
	}
	return append([]ServerTool(nil), m.tools...)
}

// ServerNames returns the names of the connected servers, sorted.
func (m *Manager) ServerNames() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.conns))
	for name := range m.conns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ServerInfo returns the information (serverInfo + instructions) that a
// connected server reported during initialize. It reports ok=false for unknown
// or disconnected servers.
func (m *Manager) ServerInfo(server string) (ServerInfo, bool) {
	if m == nil {
		return ServerInfo{}, false
	}
	m.mu.Lock()
	client := m.conns[server]
	m.mu.Unlock()
	if client == nil {
		return ServerInfo{}, false
	}
	return client.ServerInfo(), true
}

// CallTool invokes a tool on its server and renders the result as text. The
// second return value reports whether the server marked the call as an error.
func (m *Manager) CallTool(ctx context.Context, server, tool string, args map[string]any) (string, bool, error) {
	if m == nil {
		return "", false, errors.New("mcp: manager is nil")
	}
	m.mu.Lock()
	client := m.conns[server]
	m.mu.Unlock()
	if client == nil {
		return "", false, fmt.Errorf("mcp: server %q is not connected", server)
	}
	res, err := client.callTool(ctx, tool, args)
	if err != nil {
		return "", false, err
	}
	return renderCallResult(res), res.IsError, nil
}

// Close closes every server connection. The connections are shut down in
// parallel: a stdio server may be given a moment to exit on its own before its
// tree is force-terminated, and the exit path must not serialize those waits.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*Client, 0, len(m.conns))
	for _, client := range m.conns {
		clients = append(clients, client)
	}
	m.conns = make(map[string]*Client)
	m.mu.Unlock()

	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func(i int, client *Client) {
			defer wg.Done()
			errs[i] = client.close()
		}(i, client)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// renderCallResult flattens an MCP tool result into text for the model.
func renderCallResult(res *CallResult) string {
	if res == nil {
		return ""
	}
	parts := make([]string, 0, len(res.Content))
	for _, item := range res.Content {
		switch item.Type {
		case "text", "":
			if strings.TrimSpace(item.Text) != "" {
				parts = append(parts, item.Text)
			}
		case "image":
			parts = append(parts, fmt.Sprintf("[MCP returned image content (%s)]", normalizeMIME(item.MIMEType)))
		case "audio":
			parts = append(parts, fmt.Sprintf("[MCP returned audio content (%s)]", normalizeMIME(item.MIMEType)))
		case "resource", "resource_link":
			if strings.TrimSpace(item.URI) != "" {
				parts = append(parts, fmt.Sprintf("[MCP returned resource %q (%s)]", item.URI, normalizeMIME(item.MIMEType)))
			} else {
				parts = append(parts, "[MCP returned a resource]")
			}
		default:
			parts = append(parts, fmt.Sprintf("[MCP returned %s content]", item.Type))
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			text = string(b)
		}
	}
	return text
}

func normalizeMIME(mimeType string) string {
	if strings.TrimSpace(mimeType) == "" {
		return "application/octet-stream"
	}
	return mimeType
}
