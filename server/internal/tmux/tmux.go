package tmux

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// ErrSessionNotFound indicates that a tmux operation targeted a session
	// which does not exist. It is distinct from other tmux command failures.
	ErrSessionNotFound = errors.New("tmux session not found")
	// ErrInvalidWindowName indicates that a window name cannot safely be used
	// as a project-scoped tmux identifier.
	ErrInvalidWindowName = errors.New("invalid tmux window name")
	// ErrInvalidWindowPrefix indicates that a project window prefix is invalid.
	ErrInvalidWindowPrefix = errors.New("invalid tmux window prefix")
	// ErrWindowNameTaken indicates that a requested project window already
	// exists in the target session.
	ErrWindowNameTaken             = errors.New("tmux window name is already taken")
	errProjectWindowNameUnresolved = errors.New("project window name did not converge")
)

const (
	maxWindowNameLength = 128
	windowListFormat    = "#{window_index}\t#{window_id}"
)

// WindowInfo contains stable and display-relevant metadata for a tmux window.
// Name is the stable project-facing identifier; Index and ID are tmux runtime
// metadata and may change when windows are reordered or recreated.
type WindowInfo struct {
	Index            int    `json:"index"`
	ID               string `json:"id"`
	Name             string `json:"name"`
	CurrentPath      string `json:"current_path"`
	CurrentCommand   string `json:"current_command"`
	CreationMarker   string `json:"creation_marker,omitempty"`
	CreationSession  string `json:"creation_session,omitempty"`
	CreationRepoPath string `json:"creation_repo_path,omitempty"`
}

// Window is kept as a concise alias for callers that use tmux window terminology.
type Window = WindowInfo

// ValidateWindowName verifies the strict identifier format used for new
// project-scoped tmux windows. Existing legacy APIs intentionally do not call
// this helper so their behavior remains compatible.
func ValidateWindowName(name string) error {
	if name == "" || len(name) > maxWindowNameLength {
		return fmt.Errorf("%w: must be 1-%d ASCII characters", ErrInvalidWindowName, maxWindowNameLength)
	}
	for i, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !valid || i == 0 && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return fmt.Errorf("%w: %q contains unsupported characters", ErrInvalidWindowName, name)
		}
	}
	return nil
}

// ValidateWindowPrefix verifies a project window prefix, which must be a
// valid window identifier followed by exactly the namespace separator '-'.
func ValidateWindowPrefix(prefix string) error {
	if prefix == "" || !strings.HasSuffix(prefix, "-") {
		return fmt.Errorf("%w: %q must end with '-'", ErrInvalidWindowPrefix, prefix)
	}
	if err := ValidateWindowName(strings.TrimSuffix(prefix, "-")); err != nil {
		return fmt.Errorf("%w: %q: %v", ErrInvalidWindowPrefix, prefix, err)
	}
	return nil
}

// ProjectWindowPrefix returns the namespace prefix used by all windows owned
// by a project. Callers should validate the project slug before using it.
func ProjectWindowPrefix(projectSlug string) string {
	return projectSlug + "-"
}

// ProjectShellWindowPrefix returns the prefix reserved for user-created shell
// windows in a project.
func ProjectShellWindowPrefix(projectSlug string) string {
	return ProjectWindowPrefix(projectSlug) + "shell-"
}

// ValidateProjectWindowName rejects malformed or foreign project window names.
func ValidateProjectWindowName(projectSlug, windowName string) error {
	if err := ValidateWindowName(projectSlug); err != nil {
		return fmt.Errorf("invalid project slug: %w", err)
	}
	if err := ValidateWindowName(windowName); err != nil {
		return err
	}
	if !strings.HasPrefix(windowName, ProjectWindowPrefix(projectSlug)) {
		return fmt.Errorf("%w: %q is outside project %q", ErrInvalidWindowPrefix, windowName, projectSlug)
	}
	return nil
}

// IsProjectWindowName reports whether windowName belongs to projectSlug.
func IsProjectWindowName(projectSlug, windowName string) bool {
	return ValidateProjectWindowName(projectSlug, windowName) == nil
}

// ListWindows returns the windows in a tmux session using a background context.
func ListWindows(session string) ([]WindowInfo, error) {
	return ListWindowsContext(context.Background(), session)
}

// ListWindowsContext enumerates tmux windows without shell parsing. Missing
// sessions return ErrSessionNotFound; other tmux failures are returned as-is.
func ListWindowsContext(ctx context.Context, session string) ([]WindowInfo, error) {
	out, err := outputCtx(ctx, "tmux", "list-windows", "-t", session, "-F", windowListFormat)
	if err != nil {
		if isMissingSessionError(err) {
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
		}
		return nil, err
	}

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []WindowInfo{}, nil
	}
	windows := make([]WindowInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		window, err := parseWindowIdentity(line)
		if err != nil {
			return nil, fmt.Errorf("parse tmux window metadata: %w", err)
		}
		fields := []struct {
			name   string
			dest   *string
			format string
		}{
			{name: "name", dest: &window.Name, format: "#{window_name}"},
			{name: "current path", dest: &window.CurrentPath, format: "#{pane_current_path}"},
			{name: "current command", dest: &window.CurrentCommand, format: "#{pane_current_command}"},
			{name: "creation marker", dest: &window.CreationMarker, format: projectWindowMarkerFormat},
			{name: "creation session", dest: &window.CreationSession, format: projectWindowSessionFormat},
			{name: "creation repository path", dest: &window.CreationRepoPath, format: projectWindowRepoPathFormat},
		}
		skipWindow := false
		for _, field := range fields {
			value, fieldErr := readWindowField(ctx, window.ID, field.format)
			if fieldErr != nil {
				if isMissingSessionError(fieldErr) {
					return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
				}
				if isMissingWindowError(fieldErr) {
					skipWindow = true
					break
				}
				return nil, fmt.Errorf("read tmux window %q %s: %w", window.ID, field.name, fieldErr)
			}
			*field.dest = value
		}
		if skipWindow {
			continue
		}
		present, presentErr := windowInSessionContext(ctx, session, window.ID)
		if presentErr != nil {
			return nil, presentErr
		}
		if !present {
			continue
		}
		windows = append(windows, window)
	}
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Index != windows[j].Index {
			return windows[i].Index < windows[j].Index
		}
		return windows[i].Name < windows[j].Name
	})
	return windows, nil
}

// ListProjectWindowsContext enumerates only windows in a project's namespace.
func ListProjectWindowsContext(ctx context.Context, session, projectSlug string) ([]WindowInfo, error) {
	if err := ValidateWindowName(projectSlug); err != nil {
		return nil, fmt.Errorf("invalid project slug: %w", err)
	}
	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return []WindowInfo{}, nil
		}
		return nil, err
	}
	prefix := ProjectWindowPrefix(projectSlug)
	projectWindows := windows[:0]
	for _, window := range windows {
		if isKnownProjectWindowName(projectSlug, prefix, window.Name) {
			projectWindows = append(projectWindows, window)
		}
	}
	return projectWindows, nil
}

// ListProjectWindows returns project-scoped windows using a background context.
func ListProjectWindows(session, projectSlug string) ([]WindowInfo, error) {
	return ListProjectWindowsContext(context.Background(), session, projectSlug)
}

// CreateProjectShellWindow creates a uniquely named interactive shell window
// in the project's repository using a background context.
func CreateProjectShellWindow(session, projectSlug, repoPath string) (WindowInfo, error) {
	return CreateProjectShellWindowContext(context.Background(), session, projectSlug, repoPath)
}

// CreateProjectShellContext is the concise compatibility spelling for
// CreateProjectShellWindowContext.
func CreateProjectShellContext(ctx context.Context, session, projectSlug, repoPath string) (WindowInfo, error) {
	return CreateProjectShellWindowContext(ctx, session, projectSlug, repoPath)
}

// CreateProjectShellWindowContext creates a uniquely named interactive shell
// window in repoPath. Names use the deterministic project-shell-N sequence and
// the package-level gate prevents concurrent creators in this process from
// selecting the same free name.
func CreateProjectShellWindowContext(ctx context.Context, session, projectSlug, repoPath string) (WindowInfo, error) {
	if err := validateProjectShellInputs(projectSlug, repoPath); err != nil {
		return WindowInfo{}, err
	}
	release, err := acquireProjectShellCreation(ctx, session, projectSlug)
	if err != nil {
		return WindowInfo{}, err
	}
	defer release()

	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		return WindowInfo{}, err
	}
	used := windowNameSet(windows)
	for number := 1; number <= maxProjectShellCreationAttempts; number++ {
		name := fmt.Sprintf("%sshell-%d", ProjectWindowPrefix(projectSlug), number)
		if _, exists := used[name]; exists {
			continue
		}
		created, err := createProjectWindowContext(ctx, session, name, repoPath, windows)
		if errors.Is(err, ErrWindowNameTaken) {
			current, listErr := ListWindowsContext(ctx, session)
			if listErr != nil {
				return WindowInfo{}, listErr
			}
			if recovered, ok := findMarkedProjectShellWindow(current, name, repoPath); ok {
				return recovered, nil
			}
			used[name] = struct{}{}
			continue
		}
		if err != nil {
			if isMissingSessionError(err) {
				return WindowInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
			}
			return WindowInfo{}, err
		}
		return created, nil
	}
	return WindowInfo{}, fmt.Errorf("could not allocate a project shell window after %d attempts", maxProjectShellCreationAttempts)
}

// CreateProjectWindowContext creates a specifically named interactive shell
// window after validating that the name belongs to the project and is unused.
func CreateProjectWindowContext(ctx context.Context, session, projectSlug, windowName, repoPath string) error {
	if err := validateProjectShellInputs(projectSlug, repoPath); err != nil {
		return err
	}
	if err := ValidateProjectShellWindowName(projectSlug, windowName); err != nil {
		return err
	}
	release, err := acquireProjectShellCreation(ctx, session, projectSlug)
	if err != nil {
		return err
	}
	defer release()
	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		return err
	}
	if _, exists := windowNameSet(windows)[windowName]; exists {
		return fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName)
	}
	if _, err := createProjectWindowContext(ctx, session, windowName, repoPath, windows); err != nil {
		if isMissingSessionError(err) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
		}
		return err
	}
	return nil
}

func createProjectWindowContext(ctx context.Context, session, windowName, repoPath string, before []WindowInfo) (WindowInfo, error) {
	pendingName, err := newPendingWindowName()
	if err != nil {
		return WindowInfo{}, err
	}
	out, createErr := outputCtx(ctx, "tmux", "new-window", "-t", sessionTarget(session), "-n", pendingName, "-c", repoPath, "-P", "-F", "#{window_id}")
	windowID := strings.TrimSpace(out)
	if createErr != nil || windowID == "" {
		if reconciled, ok := reconcilePendingWindowContext(ctx, session, pendingName, repoPath, before); ok {
			windowID = reconciled.ID
		} else if createErr != nil {
			return WindowInfo{}, createErr
		} else {
			return WindowInfo{}, fmt.Errorf("tmux new-window returned no window id for pending %q", pendingName)
		}
	}
	if windowID != "" && createErr == nil {
		verified, ok, verifyErr := findPendingWindowContext(ctx, session, pendingName, repoPath, before, windowID)
		if verifyErr != nil && ctx.Err() != nil {
			verified, ok, verifyErr = findPendingWindowContextWithRecovery(ctx, session, pendingName, repoPath, before, windowID)
		}
		if verifyErr != nil {
			_ = cleanupCreatedWindow(windowID)
			return WindowInfo{}, verifyErr
		}
		if !ok {
			_ = cleanupCreatedWindow(windowID)
			return WindowInfo{}, fmt.Errorf("created pending tmux window %q could not be verified", pendingName)
		}
		windowID = verified.ID
	}
	if err := markProjectWindowContext(ctx, session, windowID, pendingName, repoPath); err != nil {
		_ = cleanupCreatedWindow(windowID)
		return WindowInfo{}, err
	}
	return renameProjectWindowContext(ctx, session, windowID, pendingName, windowName, repoPath, before)
}

func markProjectWindowContext(ctx context.Context, session, windowID, marker, repoPath string) error {
	for _, option := range [][2]string{
		{projectWindowMarkerOption, marker},
		{projectWindowSessionOption, session},
		{projectWindowRepoPathOption, canonicalPath(repoPath)},
	} {
		if err := runCtx(ctx, "tmux", "set-option", "-w", "-t", windowID, option[0], option[1]); err != nil {
			return err
		}
	}
	return nil
}

func newPendingWindowName() (string, error) {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate pending tmux window identity: %w", err)
	}
	return pendingProjectWindowPrefix + hex.EncodeToString(token[:]), nil
}

func reconcilePendingWindowContext(ctx context.Context, session, pendingName, repoPath string, before []WindowInfo) (WindowInfo, bool) {
	window, ok, _ := findPendingWindowContextWithRecovery(ctx, session, pendingName, repoPath, before, "")
	return window, ok
}

func findPendingWindowContextWithRecovery(ctx context.Context, session, pendingName, repoPath string, before []WindowInfo, expectedID string) (WindowInfo, bool, error) {
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), projectWindowReconcileTimeout)
	defer cancel()
	return findPendingWindowContext(reconcileCtx, session, pendingName, repoPath, before, expectedID)
}

func findPendingWindowContext(ctx context.Context, session, pendingName, repoPath string, before []WindowInfo, expectedID string) (WindowInfo, bool, error) {
	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		return WindowInfo{}, false, err
	}
	knownIDs := windowNameSetByID(before)
	matches := make([]WindowInfo, 0, 1)
	for _, window := range windows {
		if _, known := knownIDs[window.ID]; window.Name != pendingName || known || !pathsMatch(window.CurrentPath, repoPath) {
			continue
		}
		if expectedID != "" && window.ID != expectedID {
			continue
		}
		matches = append(matches, window)
	}
	if len(matches) != 1 {
		return WindowInfo{}, false, nil
	}
	return matches[0], true, nil
}

func renameProjectWindowContext(ctx context.Context, session, windowID, pendingName, windowName, repoPath string, before []WindowInfo) (WindowInfo, error) {
	windows, err := listWindowsForRecovery(ctx, session)
	if err != nil {
		_ = cleanupCreatedWindow(windowID)
		return WindowInfo{}, err
	}
	for _, window := range windows {
		if window.Name == windowName && window.ID != windowID {
			_ = cleanupCreatedWindow(windowID)
			return WindowInfo{}, fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = cleanupCreatedWindow(windowID)
		return WindowInfo{}, err
	}
	renameErr := runCtx(ctx, "tmux", "rename-window", "-t", windowID, windowName)
	if renameErr != nil {
		if reconciled, ok, reconcileErr := reconcileRenamedWindowContext(ctx, session, windowID, windowName, pendingName, repoPath, before); ok {
			return reconciled, nil
		} else if reconcileErr != nil && (errors.Is(reconcileErr, errProjectWindowNameUnresolved) || errors.Is(reconcileErr, ErrWindowNameTaken)) {
			return WindowInfo{}, reconcileErr
		}
		return WindowInfo{}, cleanupAfterRenameFailure(ctx, session, windowID, pendingName, windowName, repoPath, renameErr)
	}
	verified, verifyErr := verifyRenamedWindowContext(ctx, session, windowID, windowName, pendingName, repoPath, before)
	if verifyErr != nil {
		if errors.Is(verifyErr, errProjectWindowNameUnresolved) || errors.Is(verifyErr, ErrWindowNameTaken) {
			return WindowInfo{}, verifyErr
		}
		return WindowInfo{}, cleanupAfterRenameFailure(ctx, session, windowID, pendingName, windowName, repoPath, verifyErr)
	}
	if verified.Name == pendingName {
		return WindowInfo{}, cleanupAfterRenameFailure(ctx, session, windowID, pendingName, windowName, repoPath, fmt.Errorf("tmux window %q was not renamed", windowName))
	}
	return verified, nil
}

func cleanupAfterRenameFailure(ctx context.Context, session, windowID, pendingName, windowName, repoPath string, cause error) error {
	// This path has not established that this attempt won the duplicate race;
	// only its stable ID may be rolled back. The deterministic winner path is
	// the sole caller allowed to clean other marker-bearing windows.
	ownOnly := []WindowInfo{{ID: windowID, CreationMarker: pendingName}}
	return resolutionRollbackError(ctx, session, windowID, pendingName, repoPath, ownOnly, windowName, cause)
}

func reconcileRenamedWindowContext(ctx context.Context, session, windowID, windowName, attemptMarker, repoPath string, before []WindowInfo) (WindowInfo, bool, error) {
	verified, err := verifyRenamedWindowContext(ctx, session, windowID, windowName, attemptMarker, repoPath, before)
	if err != nil {
		return WindowInfo{}, false, err
	}
	return verified, true, nil
}

func verifyRenamedWindowContext(ctx context.Context, session, windowID, windowName, attemptMarker, repoPath string, before []WindowInfo) (WindowInfo, error) {
	windows, err := listWindowsForRecovery(ctx, session)
	if err != nil {
		return WindowInfo{}, err
	}
	requested := make([]WindowInfo, 0, 1)
	var own WindowInfo
	for _, window := range windows {
		if window.Name == windowName {
			requested = append(requested, window)
		}
		if window.ID == windowID {
			own = window
		}
	}
	if len(requested) > 1 {
		ownOnly := []WindowInfo{{ID: windowID, CreationMarker: attemptMarker}}
		knownIDs := windowNameSetByID(before)
		for _, window := range requested {
			if _, known := knownIDs[window.ID]; known {
				return WindowInfo{}, resolutionRollbackError(ctx, session, windowID, attemptMarker, repoPath, ownOnly, windowName, fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName))
			}
		}
		sort.Slice(requested, func(i, j int) bool { return windowIDLess(requested[i].ID, requested[j].ID) })
		if requested[0].ID != windowID {
			return WindowInfo{}, resolutionRollbackError(ctx, session, windowID, attemptMarker, repoPath, ownOnly, windowName, fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName))
		}
		for _, duplicate := range requested[1:] {
			if !isProjectCreationMarker(duplicate.CreationMarker) {
				return WindowInfo{}, resolutionRollbackError(ctx, session, windowID, attemptMarker, repoPath, ownOnly, windowName, fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName))
			}
		}
		if own.CreationMarker != attemptMarker {
			return WindowInfo{}, resolutionRollbackError(ctx, session, windowID, attemptMarker, repoPath, ownOnly, windowName, fmt.Errorf("created tmux window %q lost its ownership marker", windowName))
		}
		return waitForSoleRenamedWindow(ctx, session, windowID, windowName, attemptMarker, repoPath)
	}
	if own.ID != windowID || own.Name != windowName || !pathsMatch(own.CurrentPath, repoPath) {
		return WindowInfo{}, fmt.Errorf("created tmux window %q could not be verified after rename", windowName)
	}
	return own, nil
}

func waitForSoleRenamedWindow(ctx context.Context, session, windowID, windowName, attemptMarker, repoPath string) (WindowInfo, error) {
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), projectWindowReconcileTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var requested []WindowInfo
	for {
		windows, err := ListWindowsContext(reconcileCtx, session)
		if err != nil {
			if reconcileCtx.Err() != nil {
				return WindowInfo{}, resolutionRollbackError(reconcileCtx, session, windowID, attemptMarker, repoPath, ownWindowRollback(windowID, attemptMarker), windowName, duplicateResolutionCause(requested, windowID, reconcileCtx.Err()))
			}
			return WindowInfo{}, resolutionRollbackError(reconcileCtx, session, windowID, attemptMarker, repoPath, ownWindowRollback(windowID, attemptMarker), windowName, duplicateResolutionCause(requested, windowID, err))
		}
		requested = requested[:0]
		for _, window := range windows {
			if window.Name == windowName {
				requested = append(requested, window)
			}
		}
		if len(requested) == 1 && requested[0].ID == windowID && requested[0].CreationMarker == attemptMarker && pathsMatch(requested[0].CurrentPath, repoPath) {
			return requested[0], nil
		}
		select {
		case <-reconcileCtx.Done():
			return WindowInfo{}, resolutionRollbackError(reconcileCtx, session, windowID, attemptMarker, repoPath, ownWindowRollback(windowID, attemptMarker), windowName, duplicateResolutionCause(requested, windowID, reconcileCtx.Err()))
		case <-ticker.C:
		}
	}
}

func ownWindowRollback(windowID, marker string) []WindowInfo {
	return []WindowInfo{{ID: windowID, CreationMarker: marker}}
}

func duplicateResolutionCause(windows []WindowInfo, ownID string, cause error) error {
	for _, window := range windows {
		if window.ID != ownID && isProjectCreationMarker(window.CreationMarker) {
			return fmt.Errorf("%w: %v", ErrWindowNameTaken, cause)
		}
	}
	return cause
}

func resolutionRollbackError(ctx context.Context, session, windowID, pendingName, repoPath string, requested []WindowInfo, windowName string, cause error) error {
	if rollbackErr := rollbackProjectWindows(ctx, session, windowID, pendingName, repoPath, requested); rollbackErr == nil {
		return fmt.Errorf("%w: created tmux window %q did not converge: %w", errProjectWindowNameUnresolved, windowName, cause)
	} else if restoreErr := restorePendingWindowContext(ctx, session, windowID, pendingName, repoPath); restoreErr == nil {
		return fmt.Errorf("%w: created tmux window %q did not converge; owned window was restored to its unique pending identity: %v: %w", errProjectWindowNameUnresolved, windowName, rollbackErr, cause)
	} else {
		return fmt.Errorf("%w: created tmux window %q did not converge and cleanup failed: %v; pending restore failed: %v: %w", errProjectWindowNameUnresolved, windowName, rollbackErr, restoreErr, cause)
	}
}

func restorePendingWindowContext(ctx context.Context, session, windowID, pendingName, repoPath string) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), projectWindowReconcileTimeout)
	defer cancel()
	owned, err := rollbackWindowOwnershipContext(recoveryCtx, session, windowID, pendingName, repoPath)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err := renameWindowIfOwnedContext(recoveryCtx, windowID, pendingName, pendingName); err != nil {
		present, presentErr := windowInSessionContext(recoveryCtx, session, windowID)
		if presentErr == nil && !present {
			return nil
		}
		return err
	}
	windows, err := ListWindowsContext(recoveryCtx, session)
	if err != nil {
		return err
	}
	for _, window := range windows {
		if window.ID == windowID && window.Name == pendingName && window.CreationMarker == pendingName && window.CreationSession == session && pathsMatch(window.CurrentPath, repoPath) && pathsMatch(window.CreationRepoPath, repoPath) {
			return nil
		}
	}
	return fmt.Errorf("pending window %q could not be verified after rollback", pendingName)
}

func rollbackProjectWindows(ctx context.Context, session, windowID, pendingName, repoPath string, windows []WindowInfo) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), projectWindowReconcileTimeout)
	defer cancel()
	ids := map[string]struct{}{windowID: {}}
	markers := map[string]string{windowID: pendingName}
	for _, window := range windows {
		if isProjectCreationMarker(window.CreationMarker) {
			ids[window.ID] = struct{}{}
			markers[window.ID] = window.CreationMarker
		}
	}
	for {
		for id := range ids {
			owned, err := rollbackWindowOwnershipContext(rollbackCtx, session, id, markers[id], repoPath)
			if err != nil {
				return err
			}
			if !owned {
				continue
			}
			if err := killWindowIfOwnedContext(rollbackCtx, id, markers[id]); err != nil {
				return err
			}
		}
		allGone := true
		for id := range ids {
			present, err := windowInSessionContext(rollbackCtx, session, id)
			if err != nil {
				if rollbackCtx.Err() != nil {
					return rollbackCtx.Err()
				}
				return err
			}
			if present {
				allGone = false
			}
		}
		if allGone {
			return nil
		}
		select {
		case <-rollbackCtx.Done():
			return rollbackCtx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func killWindowIfOwnedContext(ctx context.Context, windowID, marker string) error {
	if !isSafeWindowID(windowID) {
		return fmt.Errorf("unsafe tmux window ID %q", windowID)
	}
	if !isProjectCreationMarker(marker) {
		return fmt.Errorf("unsafe tmux project marker %q", marker)
	}
	condition := ownershipCondition(marker)
	command := "kill-window -t " + windowID
	return runCtx(ctx, "tmux", "if-shell", "-t", windowID, "-F", condition, command, "")
}

func renameWindowIfOwnedContext(ctx context.Context, windowID, pendingName, marker string) error {
	if !isSafeWindowID(windowID) {
		return fmt.Errorf("unsafe tmux window ID %q", windowID)
	}
	if !isProjectCreationMarker(marker) {
		return fmt.Errorf("unsafe tmux project marker %q", marker)
	}
	condition := ownershipCondition(marker)
	command := "rename-window -t " + windowID + " " + pendingName
	return runCtx(ctx, "tmux", "if-shell", "-t", windowID, "-F", condition, command, "")
}

func rollbackWindowOwnershipContext(ctx context.Context, session, windowID, marker, repoPath string) (bool, error) {
	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, window := range windows {
		if window.ID != windowID {
			continue
		}
		if window.CreationMarker != marker {
			return false, fmt.Errorf("rollback ownership marker changed for window %q", windowID)
		}
		if window.CreationSession != session {
			return false, fmt.Errorf("rollback session ownership changed for window %q", windowID)
		}
		if !pathsMatch(window.CurrentPath, repoPath) {
			return false, fmt.Errorf("rollback repository path changed for window %q", windowID)
		}
		if !pathsMatch(window.CreationRepoPath, repoPath) {
			return false, fmt.Errorf("rollback repository ownership changed for window %q", windowID)
		}
		return true, nil
	}
	return false, nil
}

func isSafeWindowID(windowID string) bool {
	if !strings.HasPrefix(windowID, "@") || len(windowID) == 1 {
		return false
	}
	_, err := strconv.Atoi(strings.TrimPrefix(windowID, "@"))
	return err == nil
}

func listWindowsForRecovery(ctx context.Context, session string) ([]WindowInfo, error) {
	if ctx.Err() == nil {
		return ListWindowsContext(ctx, session)
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), projectWindowReconcileTimeout)
	defer cancel()
	return ListWindowsContext(recoveryCtx, session)
}

func windowNameSetByID(windows []WindowInfo) map[string]struct{} {
	known := make(map[string]struct{}, len(windows))
	for _, window := range windows {
		known[window.ID] = struct{}{}
	}
	return known
}

func windowIDLess(left, right string) bool {
	leftNumber, leftErr := strconv.Atoi(strings.TrimPrefix(left, "@"))
	rightNumber, rightErr := strconv.Atoi(strings.TrimPrefix(right, "@"))
	if leftErr == nil && rightErr == nil {
		return leftNumber < rightNumber
	}
	return left < right
}

func pathsMatch(left, right string) bool {
	return canonicalPath(left) == canonicalPath(right)
}

func canonicalPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}

func cleanupCreatedWindow(windowID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cleanupCreatedWindowContext(cleanupCtx, windowID)
}

func cleanupCreatedWindowContext(ctx context.Context, windowID string) error {
	return runCtx(ctx, "tmux", "kill-window", "-t", windowID)
}

// ValidateProjectShellWindowName rejects malformed, foreign, or non-shell
// project window names for user-created shell operations.
func ValidateProjectShellWindowName(projectSlug, windowName string) error {
	if err := ValidateProjectWindowName(projectSlug, windowName); err != nil {
		return err
	}
	if !strings.HasPrefix(windowName, ProjectShellWindowPrefix(projectSlug)) {
		return fmt.Errorf("%w: %q is not a project shell window", ErrInvalidWindowPrefix, windowName)
	}
	suffix := strings.TrimPrefix(windowName, ProjectShellWindowPrefix(projectSlug))
	if suffix == "" || suffix[0] == '0' {
		return fmt.Errorf("%w: %q must end in a positive decimal suffix", ErrInvalidWindowName, windowName)
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return fmt.Errorf("%w: %q must end in a positive decimal suffix", ErrInvalidWindowName, windowName)
		}
	}
	return nil
}

func validateProjectShellInputs(projectSlug, repoPath string) error {
	if err := ValidateWindowName(projectSlug); err != nil {
		return fmt.Errorf("invalid project slug: %w", err)
	}
	if strings.TrimSpace(repoPath) == "" {
		return fmt.Errorf("repository path must not be empty")
	}
	if err := ValidateWindowPrefix(ProjectWindowPrefix(projectSlug)); err != nil {
		return fmt.Errorf("invalid project prefix: %w", err)
	}
	return nil
}

var projectShellCreationLocks = struct {
	sync.Mutex
	locks map[string]chan struct{}
}{locks: make(map[string]chan struct{})}

const maxProjectShellCreationAttempts = 1024
const pendingProjectWindowPrefix = "ccx-shell-pending-"
const projectWindowMarkerOption = "@ccx-project-window-attempt"
const projectWindowMarkerFormat = "#{@ccx-project-window-attempt}"
const projectWindowSessionOption = "@ccx-project-window-session"
const projectWindowSessionFormat = "#{@ccx-project-window-session}"
const projectWindowRepoPathOption = "@ccx-project-window-repo-path"
const projectWindowRepoPathFormat = "#{@ccx-project-window-repo-path}"

var projectWindowReconcileTimeout = 5 * time.Second

func isProjectCreationMarker(marker string) bool {
	suffix := strings.TrimPrefix(marker, pendingProjectWindowPrefix)
	if suffix == marker || len(suffix) != 24 {
		return false
	}
	for _, char := range suffix {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func ownershipCondition(marker string) string {
	markerCondition := fmt.Sprintf("#{==:%s,%s}", projectWindowMarkerFormat, marker)
	sessionCondition := fmt.Sprintf("#{==:#{session_name},%s}", projectWindowSessionFormat)
	pathCondition := fmt.Sprintf("#{==:#{pane_current_path},%s}", projectWindowRepoPathFormat)
	return fmt.Sprintf("#{&&:%s,#{&&:%s,%s}}", markerCondition, sessionCondition, pathCondition)
}

func acquireProjectShellCreation(ctx context.Context, session, projectSlug string) (func(), error) {
	key := session + "\x00" + projectSlug
	projectShellCreationLocks.Lock()
	gate := projectShellCreationLocks.locks[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		projectShellCreationLocks.locks[key] = gate
	}
	projectShellCreationLocks.Unlock()
	var releaseLocal func()
	select {
	case gate <- struct{}{}:
		releaseLocal = func() { <-gate }
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return releaseLocal, nil
}

func windowNameSet(windows []WindowInfo) map[string]struct{} {
	used := make(map[string]struct{}, len(windows))
	for _, window := range windows {
		used[window.Name] = struct{}{}
	}
	return used
}

func findMarkedProjectShellWindow(windows []WindowInfo, name, repoPath string) (WindowInfo, bool) {
	exactName := make([]WindowInfo, 0, 1)
	for _, window := range windows {
		if window.Name == name {
			exactName = append(exactName, window)
		}
	}
	if len(exactName) != 1 || !isProjectCreationMarker(exactName[0].CreationMarker) || !pathsMatch(exactName[0].CurrentPath, repoPath) {
		return WindowInfo{}, false
	}
	return exactName[0], true
}

func isMissingSessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such session") || strings.Contains(msg, "can't find session") || strings.Contains(msg, "no server running")
}

func isMissingWindowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such window") || strings.Contains(msg, "can't find window")
}

func isKnownProjectWindowName(projectSlug, prefix, windowName string) bool {
	if !strings.HasPrefix(windowName, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(windowName, prefix)
	if suffix == "orchestrator" {
		return true
	}
	if strings.HasPrefix(suffix, "worker-") {
		taskID := strings.TrimPrefix(suffix, "worker-")
		return taskID != "" && taskID != "orchestrator" &&
			!strings.HasPrefix(taskID, "shell-") &&
			!strings.HasPrefix(taskID, "worker-") &&
			!strings.Contains(taskID, "-shell-") &&
			!strings.Contains(taskID, "-worker-") &&
			!strings.HasSuffix(taskID, "-orchestrator")
	}
	return ValidateProjectShellWindowName(projectSlug, windowName) == nil
}

func parseWindowIdentity(line string) (WindowInfo, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 2 {
		return WindowInfo{}, fmt.Errorf("expected 2 fields, got %d", len(fields))
	}
	index, err := strconv.Atoi(fields[0])
	if err != nil {
		return WindowInfo{}, fmt.Errorf("window index %q: %w", fields[0], err)
	}
	return WindowInfo{
		Index: index,
		ID:    fields[1],
	}, nil
}

func readWindowField(ctx context.Context, target, format string) (string, error) {
	out, err := outputCtx(ctx, "tmux", "display-message", "-t", target, "-p", format)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(out, "\n"), nil
}

func windowInSessionContext(ctx context.Context, session, windowID string) (bool, error) {
	out, err := outputCtx(ctx, "tmux", "list-windows", "-t", session, "-F", "#{window_id}")
	if err != nil {
		if isMissingSessionError(err) {
			return false, fmt.Errorf("%w: %s", ErrSessionNotFound, session)
		}
		return false, err
	}
	for _, id := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if id == windowID {
			return true, nil
		}
	}
	return false, nil
}

// EnsureSession creates a tmux session with the given name if it does not exist.
func EnsureSession(slug string) error {
	return EnsureSessionContext(context.Background(), slug)
}

// EnsureSessionContext creates a tmux session with the given name if it does
// not exist, aborting command execution when ctx is canceled.
func EnsureSessionContext(ctx context.Context, slug string) error {
	err := runCtx(ctx, "tmux", "has-session", "-t", slug)
	if err == nil {
		return nil
	}
	return runCtx(ctx, "tmux", "new-session", "-d", "-s", slug)
}

// SessionExists reports whether the tmux session exists.
func SessionExists(session string) (bool, error) {
	return SessionExistsContext(context.Background(), session)
}

// SessionExistsContext reports whether the tmux session exists and aborts
// command execution when ctx is canceled.
func SessionExistsContext(ctx context.Context, session string) (bool, error) {
	err := runCtx(ctx, "tmux", "has-session", "-t", session)
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "no such session") || strings.Contains(msg, "can't find session") {
		return false, nil
	}
	return false, err
}

// CreateWindow creates a new window in the given session with the specified
// name and starting directory.
func CreateWindow(session, name, startDir string) error {
	return CreateWindowContext(context.Background(), session, name, startDir)
}

// CreateWindowContext creates a new window and aborts command execution when
// ctx is canceled.
func CreateWindowContext(ctx context.Context, session, name, startDir string) error {
	return runCtx(ctx, "tmux", "new-window", "-t", sessionTarget(session), "-n", name, "-c", startDir)
}

func sessionTarget(session string) string {
	return session + ":"
}

// SendKeys sends keys to a window's pane. The text is sent literally via -l,
// then a separate Enter key event is sent to submit the input. Embedded
// newlines in keys are sent as literal 0x0A and are suitable for multiline
// stdin payloads; the final Enter terminates the input sequence.
func SendKeys(session, window, keys string) error {
	return SendKeysContext(context.Background(), session, window, keys)
}

// SendKeysContext sends keys to a window's pane and aborts command execution
// when ctx is canceled.
func SendKeysContext(ctx context.Context, session, window, keys string) error {
	target := session + ":" + window
	if err := runCtx(ctx, "tmux", "send-keys", "-t", target, "-l", keys); err != nil {
		return err
	}
	return runCtx(ctx, "tmux", "send-keys", "-t", target, "Enter")
}

// SendRawKeys sends literal keys to a window's pane without appending Enter.
func SendRawKeys(session, window, keys string) error {
	return SendRawKeysContext(context.Background(), session, window, keys)
}

// SendRawKeysContext sends literal keys to a window's pane and aborts command
// execution when ctx is canceled.
func SendRawKeysContext(ctx context.Context, session, window, keys string) error {
	target := session + ":" + window
	return runCtx(ctx, "tmux", "send-keys", "-t", target, "-l", keys)
}

// ResizePaneContext resizes a tmux pane to match a terminal viewport.
func ResizePaneContext(ctx context.Context, session, window string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("resize pane dimensions must be positive")
	}
	target := session + ":" + window
	return runCtx(ctx, "tmux", "resize-window", "-t", target, "-x", fmt.Sprint(cols), "-y", fmt.Sprint(rows))
}

// KillWindow terminates a window in a session.
func KillWindow(session, window string) error {
	return KillWindowContext(context.Background(), session, window)
}

// KillWindowContext terminates a window and aborts command execution when ctx
// is canceled.
func KillWindowContext(ctx context.Context, session, window string) error {
	target := session + ":" + window
	return runCtx(ctx, "tmux", "kill-window", "-t", target)
}

// IsWindowAlive reports whether a window with the given name exists in the
// session. Returns (false, nil) when the session does not exist, and
// (false, err) for unexpected errors.
func IsWindowAlive(session, window string) (bool, error) {
	return IsWindowAliveContext(context.Background(), session, window)
}

// IsWindowAliveContext reports whether a window exists and aborts command
// execution when ctx is canceled.
func IsWindowAliveContext(ctx context.Context, session, window string) (bool, error) {
	out, err := outputCtx(ctx, "tmux", "list-windows", "-t", session, "-F", "#{window_name}")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no such session") || strings.Contains(msg, "can't find session") {
			return false, nil
		}
		return false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if scanner.Text() == window {
			return true, nil
		}
	}
	return false, nil
}

// IsPaneIdle reports whether the pane's foreground process is a shell
// (bash, zsh, sh, fish, dash, ksh, tcsh, or nu), indicating that the
// harness process has exited.
func IsPaneIdle(session, window string) (bool, error) {
	return IsPaneIdleContext(context.Background(), session, window)
}

// IsPaneIdleContext reports whether the pane's foreground process is a shell
// and aborts command execution when ctx is canceled.
func IsPaneIdleContext(ctx context.Context, session, window string) (bool, error) {
	target := session + ":" + window
	out, err := outputCtx(ctx, "tmux", "display-message", "-t", target, "-p", "#{pane_current_command}")
	if err != nil {
		return false, fmt.Errorf("display-message: %w", err)
	}
	switch strings.TrimSpace(out) {
	case "bash", "zsh", "sh", "fish", "dash", "ksh", "tcsh", "nu":
		return true, nil
	}
	return false, nil
}

// CapturePaneContext returns the current visible and recent scrollback content
// for a pane, including terminal escape sequences where tmux can preserve them.
func CapturePaneContext(ctx context.Context, session, window string) ([]byte, error) {
	target := session + ":" + window
	out, err := outputBytesCtx(ctx, "tmux", "capture-pane", "-p", "-e", "-S", "-200", "-t", target)
	if err != nil {
		return nil, fmt.Errorf("capture-pane: %w", err)
	}
	return out, nil
}

// PipeOutput redirects pane output to a temporary file and streams it line by
// line on the returned channel. Call the returned cleanup function to stop
// streaming and release resources. Cleanup is safe to call multiple times.
func PipeOutput(session, window string) (<-chan string, func(), error) {
	target := session + ":" + window

	logPath, stopPipe, err := startPipe(target)
	if err != nil {
		return nil, nil, err
	}

	ch := make(chan string, 128)
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(ch)
		defer os.Remove(logPath)
		defer stopPipe()

		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()

		// Use bufio.Reader rather than bufio.Scanner: Scanner sets an internal
		// done flag on the first io.EOF and never reads the file again, making
		// it unsuitable for tailing a growing file. Reader.ReadString returns
		// io.EOF on every read that reaches the current end of file.
		//
		// Partial-line handling: ReadString('\n') may return (partial, io.EOF)
		// when the writer hasn't flushed the trailing newline yet. We accumulate
		// fragments in a strings.Builder and only emit a line once a '\n' is
		// seen (err == nil), avoiding split messages to consumers.
		// Blank lines are emitted as empty strings; callers may filter them.
		reader := bufio.NewReader(f)
		var buf strings.Builder
		for {
			select {
			case <-stop:
				return
			default:
			}
			fragment, err := reader.ReadString('\n')
			buf.WriteString(fragment)
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Transient EOF — file still being written. Sleep and retry.
					time.Sleep(50 * time.Millisecond)
					continue
				}
				// Real read error — stop streaming.
				return
			}
			// Complete line received (ends with '\n').
			line := strings.TrimRight(buf.String(), "\r\n")
			buf.Reset()
			select {
			case ch <- line:
			case <-stop:
				return
			}
		}
	}()

	cleanup := func() {
		once.Do(func() { close(stop) })
	}
	return ch, cleanup, nil
}

// PipeBytes redirects pane output to a temporary file and streams raw byte
// chunks on the returned channel. It preserves ANSI and cursor-control
// sequences for terminal emulators.
func PipeBytes(session, window string) (<-chan []byte, func(), error) {
	target := session + ":" + window

	logPath, stopPipe, err := startPipe(target)
	if err != nil {
		return nil, nil, err
	}

	ch := make(chan []byte, 128)
	stop := make(chan struct{})
	var once sync.Once

	go func() {
		defer close(ch)
		defer os.Remove(logPath)
		defer stopPipe()

		f, err := os.Open(logPath)
		if err != nil {
			return
		}
		defer f.Close()

		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := f.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				select {
				case ch <- chunk:
				case <-stop:
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				return
			}
		}
	}()

	cleanup := func() {
		once.Do(func() { close(stop) })
	}
	return ch, cleanup, nil
}

func startPipe(target string) (string, func(), error) {
	f, err := os.CreateTemp("", "ccx-pipe-*.log")
	if err != nil {
		return "", nil, err
	}
	logPath := f.Name()
	f.Close()

	// Shell-safe path: wrap in single quotes, escaping any embedded single quotes.
	safeLogPath := "'" + strings.ReplaceAll(logPath, "'", "'\\''") + "'"
	if err := run("tmux", "pipe-pane", "-t", target, "cat >> "+safeLogPath); err != nil {
		os.Remove(logPath)
		return "", nil, fmt.Errorf("pipe-pane: %w", err)
	}

	stopPipe := func() {
		// Stop pipe-pane (run with no command argument clears the pipe).
		_ = run("tmux", "pipe-pane", "-t", target)
	}
	return logPath, stopPipe, nil
}

func run(name string, args ...string) error {
	return runCtx(context.Background(), name, args...)
}

func runCtx(ctx context.Context, name string, args ...string) error {
	out, err := combinedOutputCtx(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func outputCtx(ctx context.Context, name string, args ...string) (string, error) {
	out, err := outputBytesCtx(ctx, name, args...)
	return string(out), err
}

func outputBytesCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := execCommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("%s %v: %w", name, args, err)
	}
	return out, nil
}

var execCommandContext = exec.CommandContext

func combinedOutputCtx(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := execCommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out, ctxErr
		}
	}
	return out, err
}
