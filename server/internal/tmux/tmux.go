package tmux

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	ErrWindowNameTaken = errors.New("tmux window name is already taken")
)

const (
	maxWindowNameLength = 128
	windowListFormat    = "#{window_index}\t#{window_id}"
)

// WindowInfo contains stable and display-relevant metadata for a tmux window.
// Name is the stable project-facing identifier; Index and ID are tmux runtime
// metadata and may change when windows are reordered or recreated.
type WindowInfo struct {
	Index          int    `json:"index"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	CurrentPath    string `json:"current_path"`
	CurrentCommand string `json:"current_command"`
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
	for number := 1; ; number++ {
		name := fmt.Sprintf("%sshell-%d", ProjectWindowPrefix(projectSlug), number)
		if _, exists := used[name]; exists {
			continue
		}
		created, err := createProjectWindowContext(ctx, session, name, repoPath)
		if errors.Is(err, ErrWindowNameTaken) {
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
	if _, err := createProjectWindowContext(ctx, session, windowName, repoPath); err != nil {
		if isMissingSessionError(err) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, session)
		}
		return err
	}
	return nil
}

func createProjectWindowContext(ctx context.Context, session, windowName, repoPath string) (WindowInfo, error) {
	out, err := outputCtx(ctx, "tmux", "new-window", "-t", sessionTarget(session), "-n", windowName, "-c", repoPath, "-P", "-F", "#{window_id}")
	if err != nil {
		return WindowInfo{}, err
	}
	windowID := strings.TrimSpace(out)
	if windowID == "" {
		return WindowInfo{}, fmt.Errorf("tmux new-window returned no window id for %q", windowName)
	}
	windows, err := ListWindowsContext(ctx, session)
	if err != nil {
		cleanupCreatedWindow(windowID)
		return WindowInfo{}, err
	}
	matches := make([]WindowInfo, 0, 1)
	for _, window := range windows {
		if window.Name == windowName {
			matches = append(matches, window)
		}
	}
	if len(matches) > 1 {
		cleanupCreatedWindow(windowID)
		return WindowInfo{}, fmt.Errorf("%w: %s", ErrWindowNameTaken, windowName)
	}
	if len(matches) == 0 || matches[0].ID != windowID {
		cleanupCreatedWindow(windowID)
		return WindowInfo{}, fmt.Errorf("created tmux window %q could not be verified", windowName)
	}
	return matches[0], nil
}

func cleanupCreatedWindow(windowID string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runCtx(cleanupCtx, "tmux", "kill-window", "-t", windowID)
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
	lockName := projectShellLockName(session, projectSlug)
	if err := runCtx(ctx, "tmux", "wait-for", "-L", lockName); err != nil {
		releaseLocal()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = runCtx(unlockCtx, "tmux", "wait-for", "-U", lockName)
			releaseLocal()
		})
	}, nil
}

func projectShellLockName(session, projectSlug string) string {
	digest := sha256.Sum256([]byte(session + "\x00" + projectSlug))
	return fmt.Sprintf("ccx-project-shell-%x", digest[:12])
}

func windowNameSet(windows []WindowInfo) map[string]struct{} {
	used := make(map[string]struct{}, len(windows))
	for _, window := range windows {
		used[window.Name] = struct{}{}
	}
	return used
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
