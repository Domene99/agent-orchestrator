// Package drunworktree wraps the gitworktree workspace adapter with a drun
// session that shadows each agent worktree. Every Create/Restore gives the
// agent a fresh drun session pre-loaded with the worktree's files; every
// StashUncommitted snapshots the session to a .drun file (replacing the
// git-stash-ref mechanism); ForceDestroy closes the in-memory session.
//
// Agent processes receive DRUN_SESSION_ID in their environment so they can
// route all file and bash operations through the MCP tools exposed by
// drun-mcp (already running as a daemon managed by the ao daemon itself).
package drunworktree

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/drun"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Workspace implements ports.Workspace by composing a gitworktree.Workspace
// (for the on-disk worktree) with a drun.Client (for the in-memory sandbox).
type Workspace struct {
	git    *gitworktree.Workspace
	drun   *drun.Client
	log    *slog.Logger
	// sessions maps AO session ID → drun session ID.
	sessions sync.Map
}

var _ ports.Workspace = (*Workspace)(nil)
var _ ports.WorkspaceProject = (*Workspace)(nil)

// New returns a Workspace that creates git worktrees via git and maintains a
// paired drun session for each one.
func New(git *gitworktree.Workspace, client *drun.Client, log *slog.Logger) *Workspace {
	return &Workspace{git: git, drun: client, log: log}
}

// GetDrunSessionID returns the active drun session ID for the given AO session,
// or "" if none has been created yet. The session manager calls this via
// interface assertion to populate DRUN_SESSION_ID in the agent environment.
func (w *Workspace) GetDrunSessionID(id domain.SessionID) string {
	if v, ok := w.sessions.Load(id); ok {
		return v.(string)
	}
	return ""
}

// Create creates a git worktree and a paired drun session pre-loaded with the
// worktree's files. The returned WorkspaceInfo includes the drun session ID.
func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	info, err := w.git.Create(ctx, cfg)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	drunID, err := w.newSession(ctx, cfg.SessionID, info.Path, cfg.ProjectID)
	if err != nil {
		// drun failure is non-fatal: the agent can still run without the sandbox
		// until drun is available. Log and continue.
		w.log.Warn("drunworktree: could not create drun session; agent will run unsandboxed",
			"session", cfg.SessionID, "error", err)
		return info, nil
	}
	info.DrunSessionID = drunID
	return info, nil
}

// Restore re-attaches to an existing git worktree and creates a fresh drun
// session loaded with the worktree's current files. If the session was
// previously snapshotted, ApplyPreserved will later replace this session
// with the snapshot-restored one.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	info, err := w.git.Restore(ctx, cfg)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	drunID, err := w.newSession(ctx, cfg.SessionID, info.Path, cfg.ProjectID)
	if err != nil {
		w.log.Warn("drunworktree: could not restore drun session; agent will run unsandboxed",
			"session", cfg.SessionID, "error", err)
		return info, nil
	}
	info.DrunSessionID = drunID
	return info, nil
}

// StashUncommitted snapshots the drun session to a .drun file (which drun
// stores in its configured snapshots_dir). The snapshot path is returned as
// the "ref" so the session manager persists it in session_worktrees; it is
// passed back to ApplyPreserved on the next daemon boot.
// The git worktree stash is also run as a fallback for agents that bypass drun.
func (w *Workspace) StashUncommitted(ctx context.Context, info ports.WorkspaceInfo) (string, error) {
	// Best-effort git stash for changes made outside drun (hooks, config files, etc.).
	_, _ = w.git.StashUncommitted(ctx, info)

	drunID := w.GetDrunSessionID(info.SessionID)
	if drunID == "" {
		return "", nil
	}
	path, err := w.drun.Snapshot(ctx, drunID)
	if err != nil {
		w.log.Warn("drunworktree: snapshot failed; session state will not survive restart",
			"session", info.SessionID, "error", err)
		return "", nil
	}
	w.log.Info("drunworktree: session snapshotted", "session", info.SessionID, "path", path)
	return path, nil
}

// ApplyPreserved restores a drun session from the snapshot path stored at
// stash time. The session map is updated to the new session ID so
// GetDrunSessionID returns the restored ID for the relaunched agent.
func (w *Workspace) ApplyPreserved(ctx context.Context, info ports.WorkspaceInfo, ref string) error {
	// Close the transient session created by Restore before loading the snapshot.
	if old := w.GetDrunSessionID(info.SessionID); old != "" {
		if err := w.drun.Close(ctx, old); err != nil {
			w.log.Warn("drunworktree: could not close transient session before restore",
				"session", info.SessionID, "error", err)
		}
	}

	restoredID, err := w.drun.Restore(ctx, ref)
	if err != nil {
		return fmt.Errorf("drunworktree: restore snapshot %q: %w", ref, err)
	}
	_ = w.drun.Label(ctx, restoredID, string(info.SessionID))
	w.sessions.Store(info.SessionID, restoredID)
	w.log.Info("drunworktree: session restored from snapshot",
		"session", info.SessionID, "drun_session", restoredID)
	return nil
}

// Destroy closes the drun session and removes the git worktree.
func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	w.closeSession(ctx, info.SessionID)
	return w.git.Destroy(ctx, info)
}

// ForceDestroy closes the drun session and force-removes the git worktree.
func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	w.closeSession(ctx, info.SessionID)
	return w.git.ForceDestroy(ctx, info)
}

// CreateWorkspaceProject delegates to the underlying git workspace for
// multi-repo projects and creates a single drun session for the root worktree.
func (w *Workspace) CreateWorkspaceProject(ctx context.Context, cfg ports.WorkspaceProjectConfig) (ports.WorkspaceProjectInfo, error) {
	info, err := w.git.CreateWorkspaceProject(ctx, cfg)
	if err != nil {
		return ports.WorkspaceProjectInfo{}, err
	}
	drunID, err := w.newSession(ctx, cfg.SessionID, info.Root.Path, cfg.ProjectID)
	if err != nil {
		w.log.Warn("drunworktree: could not create drun session for workspace project",
			"session", cfg.SessionID, "error", err)
		return info, nil
	}
	info.Root.DrunSessionID = drunID
	return info, nil
}

// DestroyWorkspaceProject closes the drun session and removes all worktrees.
func (w *Workspace) DestroyWorkspaceProject(ctx context.Context, info ports.WorkspaceProjectInfo) error {
	w.closeSession(ctx, info.Root.SessionID)
	return w.git.DestroyWorkspaceProject(ctx, info)
}

// newSession creates a fresh drun session, mounts the worktree path into it,
// labels it with the AO session ID, and stores the mapping.
func (w *Workspace) newSession(ctx context.Context, id domain.SessionID, worktreePath string, project domain.ProjectID) (string, error) {
	drunID, err := w.drun.CreateSession(ctx)
	if err != nil {
		return "", fmt.Errorf("create_session: %w", err)
	}
	label := fmt.Sprintf("%s/%s", project, id)
	_ = w.drun.Label(ctx, drunID, label)

	if err := w.drun.Mount(ctx, drunID, worktreePath); err != nil {
		_ = w.drun.Close(ctx, drunID)
		return "", fmt.Errorf("session_mount %q: %w", worktreePath, err)
	}
	w.sessions.Store(id, drunID)
	w.log.Info("drunworktree: drun session created",
		"ao_session", id, "drun_session", drunID, "path", worktreePath)
	return drunID, nil
}

// closeSession closes the drun session for the given AO session ID and removes
// it from the map. Errors are logged but not returned (teardown is best-effort).
func (w *Workspace) closeSession(ctx context.Context, id domain.SessionID) {
	drunID, ok := w.sessions.LoadAndDelete(id)
	if !ok {
		return
	}
	if err := w.drun.Close(ctx, drunID.(string)); err != nil {
		w.log.Warn("drunworktree: could not close drun session", "session", id, "error", err)
	}
}
