package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

func (a *app) mcpCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Use the Zipbox MCP server from Claude, Codex, Cursor and other clients",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Serve the Zipbox MCP tools over stdio (for clients that launch a command)",
		Long: "Bridges stdio to the hosted server (" + defaultMCPURL + ") with your API key, so the\n" +
			"stdio server exposes exactly the hosted tool set. ZIPBOX_MCP_URL overrides the URL.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := a.apiKey()
			if key == "" {
				return errNotLoggedIn
			}
			return (&mcpBridge{url: a.mcpURL(), key: key, http: a.http}).run(cmd.Context(), a.in, a.out)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:       "init <claude|codex|cursor|windsurf>",
		Short:     "Configure an MCP client to use Zipbox",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"claude", "codex", "cursor", "windsurf"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.mcpInit(args[0])
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "config",
		Short: "Print the stdio MCP server config snippet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.printJSON(map[string]any{"mcpServers": map[string]any{"zipbox": a.stdioServer("zipbox")}})
		},
	})
	return cmd
}

func (a *app) stdioServer(command string) map[string]any {
	return map[string]any{"command": command, "args": []string{"mcp", "start"}}
}

func (a *app) home() (string, error) {
	if h := a.getenv("HOME"); h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}

type mcpInitResult struct {
	Client  string `json:"client"`
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
}

func (a *app) mcpInit(client string) error {
	home, err := a.home()
	if err != nil {
		return err
	}
	exe := a.exePath
	if exe == "" {
		exe = "zipbox"
	}
	var result mcpInitResult
	switch client {
	case "claude":
		result = mcpInitResult{
			Client: client,
			Command: fmt.Sprintf(`claude mcp add --transport http zipbox %s --header "Authorization: Bearer $ZIPBOX_API_KEY"`,
				a.mcpURL()),
		}
		return a.emit(result, func() {
			fmt.Fprintln(a.out, "Run this (Claude Code), or add "+a.mcpURL()+" under Settings → Connectors (Claude Desktop, OAuth):")
			fmt.Fprintln(a.out, "  "+result.Command)
			fmt.Fprintln(a.out, "Or over stdio with this CLI's key:")
			fmt.Fprintln(a.out, "  claude mcp add zipbox -- "+exe+" mcp start")
		})
	case "codex":
		dir := a.getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		path := filepath.Join(dir, "config.toml")
		section := fmt.Sprintf("[mcp_servers.zipbox]\nurl = %q\nbearer_token_env_var = \"ZIPBOX_API_KEY\"\n", a.mcpURL())
		if err := upsertTOMLSection(path, "mcp_servers.zipbox", section); err != nil {
			return err
		}
		result = mcpInitResult{Client: client, Path: path}
	case "cursor":
		path := filepath.Join(home, ".cursor", "mcp.json")
		if err := upsertJSONServer(path, a.stdioServer(exe)); err != nil {
			return err
		}
		result = mcpInitResult{Client: client, Path: path}
	case "windsurf":
		path := filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
		if err := upsertJSONServer(path, a.stdioServer(exe)); err != nil {
			return err
		}
		result = mcpInitResult{Client: client, Path: path}
	default:
		return fmt.Errorf("unknown client %q (claude, codex, cursor, windsurf)", client)
	}
	return a.emit(result, func() {
		fmt.Fprintf(a.out, "Configured %s: %s\n", client, result.Path)
		if client == "codex" {
			fmt.Fprintln(a.out, "Export ZIPBOX_API_KEY in the shell Codex runs from.")
		}
	})
}

// upsertJSONServer sets mcpServers.zipbox in a client's JSON config and keeps
// everything else in it.
func upsertJSONServer(path string, server map[string]any) error {
	doc := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["zipbox"] = server
	doc["mcpServers"] = servers
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// upsertTOMLSection replaces [name] (and its [name.*] subtables) in a TOML
// file with section, or appends it.
func upsertTOMLSection(path, name, section string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	skipping := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			header := strings.Trim(trimmed, "[] ")
			skipping = header == name || strings.HasPrefix(header, name+".")
		}
		if !skipping {
			out = append(out, line)
		}
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if text != "" {
		text += "\n\n"
	}
	text += section
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// mcpBridge relays newline-delimited JSON-RPC between stdio and a streamable
// HTTP MCP server: each stdin message is POSTed; JSON and SSE replies are
// written to stdout one message per line.
type mcpBridge struct {
	url  string
	key  string
	http *http.Client

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	outMu           sync.Mutex
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result *struct {
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"result,omitempty"`
}

func (b *mcpBridge) run(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	var wg sync.WaitGroup
	for {
		line, err := reader.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			msg = append([]byte(nil), msg...)
			var env rpcEnvelope
			_ = json.Unmarshal(msg, &env)
			if env.Method == "" || len(env.ID) == 0 {
				// Notifications and replies are delivered in order before the
				// next line is read: `notifications/initialized` must reach the
				// server ahead of the first request after it.
				b.forward(ctx, msg, out)
			} else {
				// Requests run concurrently, so a long tool call never holds up
				// the next request or a cancellation.
				wg.Add(1)
				go func() {
					defer wg.Done()
					b.forward(ctx, msg, out)
				}()
			}
		}
		if err != nil {
			wg.Wait()
			b.closeSession()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (b *mcpBridge) forward(ctx context.Context, msg []byte, out io.Writer) {
	var env rpcEnvelope
	_ = json.Unmarshal(msg, &env)
	req, err := http.NewRequestWithContext(ctx, "POST", b.url, bytes.NewReader(msg))
	if err != nil {
		b.writeError(out, env.ID, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.key)
	b.mu.Lock()
	if b.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", b.sessionID)
	}
	if b.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", b.protocolVersion)
	}
	b.mu.Unlock()
	resp, err := b.http.Do(req)
	if err != nil {
		b.writeError(out, env.ID, "cannot reach "+b.url+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		b.mu.Lock()
		b.sessionID = sid
		b.mu.Unlock()
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if resp.StatusCode == http.StatusNotFound {
			// The server forgot the session; the client must initialize again.
			b.mu.Lock()
			b.sessionID = ""
			b.mu.Unlock()
		}
		b.writeError(out, env.ID, fmt.Sprintf("%s: HTTP %d: %s", b.url, resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b.relaySSE(resp.Body, out, env.Method == "initialize")
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.writeError(out, env.ID, err.Error())
		return
	}
	b.emit(out, body, env.Method == "initialize")
}

func (b *mcpBridge) relaySSE(body io.Reader, out io.Writer, initialize bool) {
	reader := bufio.NewReaderSize(body, 1<<20)
	var data []string
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			if len(data) > 0 {
				b.emit(out, []byte(strings.Join(data, "\n")), initialize)
				data = nil
			}
		case strings.HasPrefix(trimmed, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " "))
		}
		if err != nil {
			if len(data) > 0 {
				b.emit(out, []byte(strings.Join(data, "\n")), initialize)
			}
			return
		}
	}
}

func (b *mcpBridge) emit(out io.Writer, payload []byte, initialize bool) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return
	}
	if initialize {
		var env rpcEnvelope
		if json.Unmarshal(compact.Bytes(), &env) == nil && env.Result != nil && env.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.protocolVersion = env.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	compact.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = out.Write(compact.Bytes())
}

// writeError answers a request the bridge could not deliver, so the client
// is not left waiting. Notifications (no id) are dropped.
func (b *mcpBridge) writeError(out io.Writer, id json.RawMessage, message string) {
	if len(id) == 0 {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": "zipbox mcp bridge: " + message},
	})
	b.emit(out, payload, false)
}

func (b *mcpBridge) closeSession() {
	b.mu.Lock()
	sid := b.sessionID
	b.mu.Unlock()
	if sid == "" {
		return
	}
	req, err := http.NewRequest("DELETE", b.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+b.key)
	req.Header.Set("Mcp-Session-Id", sid)
	if resp, err := b.http.Do(req); err == nil {
		resp.Body.Close()
	}
}
