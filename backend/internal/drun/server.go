package drun

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// embeddedInstallPath is where ao extracts the bundled drun-mcp binary on first run.
func embeddedInstallPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ao", "bin", binaryName), nil
}

// extractEmbeddedBinary writes embeddedDrunMCP to ~/.ao/bin/drun-mcp if the
// embedded slice is non-empty and the file is not already there. This runs on
// every daemon Start so a corrupt or missing binary is self-healed automatically.
func (s *Server) extractEmbeddedBinary() error {
	if len(embeddedDrunMCP) == 0 {
		return nil // built without bundled_drun tag; rely on PATH discovery
	}
	dest, err := embeddedInstallPath()
	if err != nil {
		return fmt.Errorf("drun: resolve install path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("drun: create bin dir: %w", err)
	}
	// Skip if the file already exists and has the same size (fast idempotency check).
	if fi, err := os.Stat(dest); err == nil && fi.Size() == int64(len(embeddedDrunMCP)) {
		return nil
	}
	if err := os.WriteFile(dest, embeddedDrunMCP, 0o755); err != nil {
		return fmt.Errorf("drun: extract binary: %w", err)
	}
	s.log.Info("drun-mcp extracted", "path", dest)
	return nil
}

const (
	// EnvBinOverride lets operators point ao at a specific drun-mcp binary.
	EnvBinOverride = "AO_DRUN_BIN"
	// binaryName is the expected name of the drun-mcp executable.
	binaryName = "drun-mcp"
)

// Server manages the lifecycle of the drun-mcp subprocess. The ao daemon starts
// one Server on boot; it starts drun-mcp if it is not already running and
// exposes a Client for making tool calls.
type Server struct {
	dataDir string
	log     *slog.Logger
	proc    *os.Process
	client  *Client
}

// NewServer returns a Server that will manage drun-mcp with its working
// directory (and default snapshot dir) under dataDir.
func NewServer(dataDir string, log *slog.Logger) *Server {
	return &Server{
		dataDir: dataDir,
		log:     log,
		client:  NewClient(""),
	}
}

// Client returns the MCP client used to talk to the managed server.
func (s *Server) Client() *Client { return s.client }

// Start ensures drun-mcp is running. If it is already reachable (started by a
// previous daemon instance or by the user), Start is a no-op. Otherwise it
// extracts the bundled binary (when ao was built with -tags bundled_drun),
// locates the binary, starts it as a subprocess, and waits for readiness.
func (s *Server) Start(ctx context.Context) error {
	if err := s.probeAlive(ctx); err == nil {
		s.log.Info("drun-mcp already running, reusing")
		if err := s.registerWithClaudeCode(); err != nil {
			s.log.Warn("drun: could not register MCP server with Claude Code", "err", err)
		}
		return nil
	}

	if err := s.extractEmbeddedBinary(); err != nil {
		s.log.Warn("drun: could not extract bundled binary; falling back to PATH", "err", err)
	}

	bin, err := s.resolveBinary()
	if err != nil {
		return fmt.Errorf("drun-mcp not found: %w (install it or set %s)", err, EnvBinOverride)
	}

	snapshotsDir := filepath.Join(s.dataDir, "drun-snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		return fmt.Errorf("drun: create snapshots dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = s.dataDir
	cmd.Env = append(os.Environ(),
		// Point the snapshot default inside ao's data dir.
		"DRUN_SNAPSHOTS_DIR="+snapshotsDir,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("drun-mcp start: %w", err)
	}
	s.proc = cmd.Process
	s.log.Info("drun-mcp started", "pid", s.proc.Pid, "bin", bin)

	// Wait up to 10 s for the server to become ready.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.probeAlive(ctx); err == nil {
			s.log.Info("drun-mcp ready")
			if err := s.registerWithClaudeCode(); err != nil {
				s.log.Warn("drun: could not register MCP server with Claude Code", "err", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("drun-mcp did not become ready within 10s")
}

// registerWithClaudeCode ensures drun is registered as a user-scoped MCP server
// in ~/.claude.json. This writes at the top level (above any project entry), so
// per-project mcpServers overrides can never shadow it. It is idempotent: if the
// entry already has the right type and URL it returns without writing.
func (s *Server) registerWithClaudeCode() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	claudeJSON := filepath.Join(home, ".claude.json")

	data, _ := os.ReadFile(claudeJSON)
	root := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", claudeJSON, err)
		}
	}

	servers := map[string]any{}
	if raw, ok := root["mcpServers"]; ok {
		_ = json.Unmarshal(raw, &servers)
	}

	want := map[string]any{
		"type": "http",
		"url":  DefaultMCPURL,
		// drun-mcp requires text/event-stream in Accept or returns 406.
		"headers": map[string]string{
			"Accept": "application/json, text/event-stream",
		},
	}
	if existing, ok := servers["drun"].(map[string]any); ok {
		if existing["type"] == "http" && existing["url"] == DefaultMCPURL {
			return nil // already correct
		}
	}

	servers["drun"] = want
	b, _ := json.Marshal(servers)
	root["mcpServers"] = b

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.WriteFile(claudeJSON, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", claudeJSON, err)
	}
	s.log.Info("drun: registered MCP server in ~/.claude.json (user scope)")
	return nil
}

// Stop signals the managed subprocess to exit. If drun-mcp was already
// running when Start was called (not started by this Server), Stop is a no-op.
func (s *Server) Stop() {
	if s.proc == nil {
		return
	}
	if err := s.proc.Signal(os.Interrupt); err != nil {
		_ = s.proc.Kill()
	}
	_, _ = s.proc.Wait()
	s.log.Info("drun-mcp stopped")
}

// probeAlive tries to initialize an MCP session to confirm the server is up.
func (s *Server) probeAlive(ctx context.Context) error {
	probe := NewClient("")
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return probe.IsAlive(pctx)
}

// resolveBinary finds the drun-mcp binary in order:
//  1. AO_DRUN_BIN env var
//  2. ~/.ao/bin/drun-mcp  (standard install location alongside ao)
//  3. PATH
func (s *Server) resolveBinary() (string, error) {
	if v := os.Getenv(EnvBinOverride); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, ".ao", "bin", binaryName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(binaryName)
	if err != nil {
		return "", fmt.Errorf("%s not on PATH", binaryName)
	}
	return path, nil
}
