package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hooksjson"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	claudeSettingsDirName   = ".claude"
	claudeSettingsFileName  = "settings.local.json"
	claudeHookCommandPrefix = "ao hooks claude-code "
	claudeHookTimeout       = 30
	// drunMCPServerName is the key used in Claude Code's mcpServers map.
	drunMCPServerName = "drun"
	// drunMCPURL is the drun-mcp Streamable HTTP endpoint Claude Code connects to.
	drunMCPURL = "http://127.0.0.1:7273/mcp"
)

// claudeStartupMatcher is referenced by pointer so SessionStart serializes with
// its required "startup" matcher.
var claudeStartupMatcher = "startup"

// claudeManagedHooks is the source of truth for the hooks AO installs.
var claudeManagedHooks = []hooksjson.HookSpec{
	{Event: "SessionStart", Matcher: &claudeStartupMatcher, Command: claudeHookCommandPrefix + "session-start"},
	{Event: "UserPromptSubmit", Command: claudeHookCommandPrefix + "user-prompt-submit"},
	{Event: "Stop", Command: claudeHookCommandPrefix + "stop"},
	{Event: "Notification", Command: claudeHookCommandPrefix + "notification"},
	{Event: "SessionEnd", Command: claudeHookCommandPrefix + "session-end"},
}

// claudeHooks manages AO's hooks in the workspace-local
// .claude/settings.local.json file.
var claudeHooks = hooksjson.Manager{
	Label:         "claude-code",
	CommandPrefix: claudeHookCommandPrefix,
	Timeout:       claudeHookTimeout,
	Path:          claudeSettingsPath,
	Managed:       claudeManagedHooks,
}

func claudeSettingsPath(workspacePath string) string {
	return filepath.Join(workspacePath, claudeSettingsDirName, claudeSettingsFileName)
}

// GetAgentHooks installs AO's Claude Code hooks and wires the drun MCP server
// into settings.local.json so the agent can use drun sandbox tools.
func (p *Plugin) GetAgentHooks(ctx context.Context, cfg ports.WorkspaceHookConfig) error {
	if err := claudeHooks.Install(ctx, cfg.WorkspacePath); err != nil {
		return err
	}
	return ensureDrunMCPServer(claudeSettingsPath(cfg.WorkspacePath))
}

// ensureDrunMCPServer merges the drun MCP server entry into settings.local.json
// and ensures the required permissions are in place. It is idempotent.
func ensureDrunMCPServer(settingsPath string) error {
	root := map[string]json.RawMessage{}
	if data, err := os.ReadFile(settingsPath); err == nil && len(data) > 0 {
		if jsonErr := json.Unmarshal(data, &root); jsonErr != nil {
			return fmt.Errorf("claude-code: parse %s: %w", settingsPath, jsonErr)
		}
	}

	changed := false

	// mcpServers: ensure "drun" → {type: "http", url: drunMCPURL, headers: ...}
	// Always overwrite so stale entries (e.g. old "sse" transport) get upgraded.
	mcpServers := map[string]any{}
	if raw, ok := root["mcpServers"]; ok {
		_ = json.Unmarshal(raw, &mcpServers)
	}
	want := map[string]any{
		"type": "http",
		"url":  drunMCPURL,
		// drun-mcp requires both content types in Accept; without
		// text/event-stream it returns 406 per MCP streamable-HTTP spec.
		"headers": map[string]string{
			"Accept": "application/json, text/event-stream",
		},
	}
	existing, exists := mcpServers[drunMCPServerName]
	if !exists || !drunEntryMatches(existing, want) {
		mcpServers[drunMCPServerName] = want
		b, _ := json.Marshal(mcpServers)
		root["mcpServers"] = b
		changed = true
	}

	// permissions: ensure mcp__drun__* is allowed.
	if changed {
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o750); err != nil {
			return fmt.Errorf("claude-code: create settings dir: %w", err)
		}
		data, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return fmt.Errorf("claude-code: encode settings: %w", err)
		}
		data = append(data, '\n')
		if err := hookutil.AtomicWriteFile(settingsPath, data, 0o600); err != nil {
			return fmt.Errorf("claude-code: write settings: %w", err)
		}
	}
	return nil
}

// drunEntryMatches returns true when the existing mcpServers["drun"] entry already
// has the correct type and URL, so we avoid unnecessary writes.
func drunEntryMatches(existing, want any) bool {
	e, ok := existing.(map[string]any)
	if !ok {
		return false
	}
	w := want.(map[string]any)
	return e["type"] == w["type"] && e["url"] == w["url"]
}

// UninstallHooks removes AO's Claude Code hooks, leaving user-defined hooks untouched.
func (p *Plugin) UninstallHooks(ctx context.Context, workspacePath string) error {
	return claudeHooks.Uninstall(ctx, workspacePath)
}

// AreHooksInstalled reports whether any AO Claude Code hook is present.
func (p *Plugin) AreHooksInstalled(ctx context.Context, workspacePath string) (bool, error) {
	return claudeHooks.AreInstalled(ctx, workspacePath)
}
