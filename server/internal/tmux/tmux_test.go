package tmux

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type tmuxTestContextKey struct{}

func TestListWindowsContextParsesMetadataWithTabs(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotCtx context.Context
	var gotArgs [][]string
	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotCtx = ctx
		gotArgs = append(gotArgs, append([]string(nil), args...))
		var output string
		switch len(gotArgs) {
		case 1:
			output = "0\t@0\n"
		case 2:
			output = "@0\n"
		case 3:
			output = "alpha shell\t1\n"
		case 4:
			output = "/Users/me/My Repo\t(a|b)\n"
		case 5:
			output = "zsh\n"
		case 6:
			output = "@0\n"
		}
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	windows, err := ListWindowsContext(ctx, "ccx-t2")
	if err != nil {
		t.Fatalf("ListWindowsContext: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("ListWindowsContext did not pass caller context")
	}
	wantArgs := []string{"list-windows", "-t", "ccx-t2", "-F", windowListFormat}
	if !reflect.DeepEqual(gotArgs[0], wantArgs) {
		t.Fatalf("tmux list args = %#v, want %#v", gotArgs[0], wantArgs)
	}
	want := []WindowInfo{{
		Index:          0,
		ID:             "@0",
		Name:           "alpha shell\t1",
		CurrentPath:    "/Users/me/My Repo\t(a|b)",
		CurrentCommand: "zsh",
	}}
	if !reflect.DeepEqual(windows, want) {
		t.Fatalf("windows = %#v, want %#v", windows, want)
	}
}

func TestListWindowsContextDistinguishesMissingSessionFromCommandFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stderr    string
		wantError bool
	}{
		{name: "missing", stderr: "can't find session: ccx-t2"},
		{name: "failure", stderr: "permission denied", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldExecCommandContext := execCommandContext
			execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
				return exec.Command("sh", "-c", "printf '%s' \"$1\" >&2; exit 1", "sh", tc.stderr)
			}
			t.Cleanup(func() { execCommandContext = oldExecCommandContext })

			windows, err := ListWindowsContext(context.Background(), "ccx-t2")
			if tc.wantError {
				if err == nil || errors.Is(err, ErrSessionNotFound) {
					t.Fatalf("ListWindowsContext error = %v, want command failure", err)
				}
				return
			}
			if !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("ListWindowsContext error = %v, want ErrSessionNotFound", err)
			}
			if windows != nil {
				t.Fatalf("windows = %#v, want nil for missing session", windows)
			}
		})
	}
}

func TestListProjectWindowsContextTreatsMissingSessionAsEmpty(t *testing.T) {
	oldExecCommandContext := execCommandContext
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' 'no such session' >&2; exit 1")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	windows, err := ListProjectWindowsContext(context.Background(), "ccx-t2", "alpha")
	if err != nil {
		t.Fatalf("ListProjectWindowsContext: %v", err)
	}
	if windows == nil || len(windows) != 0 {
		t.Fatalf("windows = %#v, want empty non-nil slice", windows)
	}
}

func TestListWindowsContextClassifiesSessionLossDuringMetadataRead(t *testing.T) {
	oldExecCommandContext := execCommandContext
	call := 0
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		call++
		if call == 1 {
			return exec.Command("sh", "-c", "printf '%s' '0\t@0\n'")
		}
		return exec.Command("sh", "-c", "printf '%s' 'no such session' >&2; exit 1")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	_, err := ListWindowsContext(context.Background(), "ccx-t2")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ListWindowsContext error = %v, want ErrSessionNotFound", err)
	}
}

func TestValidateProjectShellWindowNames(t *testing.T) {
	valid := []string{"alpha-shell-1", "alpha-shell-99"}
	for _, name := range valid {
		if err := ValidateProjectShellWindowName("alpha", name); err != nil {
			t.Errorf("ValidateProjectShellWindowName(%q): %v", name, err)
		}
	}
	invalid := []string{"", "alpha shell-1", "beta-shell-1", "alpha-worker-1", "-alpha-shell-1", "alpha-shell-", "alpha-shell-foo", "alpha-shell-1-extra", "alpha-shell-01", "alpha-shell-0"}
	for _, name := range invalid {
		if err := ValidateProjectShellWindowName("alpha", name); err == nil {
			t.Errorf("ValidateProjectShellWindowName(%q) = nil, want error", name)
		}
	}
	if err := ValidateWindowPrefix("alpha"); err == nil {
		t.Fatal("ValidateWindowPrefix without separator = nil, want error")
	}
}

func TestCreateProjectShellWindowContextChoosesDeterministicFreeName(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotContexts []context.Context
	var gotArgs [][]string
	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotContexts = append(gotContexts, ctx)
		gotArgs = append(gotArgs, append([]string(nil), args...))
		var output string
		switch len(gotArgs) {
		case 1:
			output = "0\t@0\n1\t@1\n"
		case 2:
			output = "@0\n"
		case 3:
			output = "alpha-shell-1\n"
		case 4:
			output = "/repo with spaces\n"
		case 5:
			output = "bash\n"
		case 6:
			output = "@0\n"
		case 7:
			output = "@1\n"
		case 8:
			output = "alpha-shell-3\n"
		case 9:
			output = "/repo with spaces\n"
		case 10:
			output = "zsh\n"
		case 11:
			output = "@1\n"
		default:
			return exec.Command("sh", "-c", "true")
		}
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	window, err := CreateProjectShellWindowContext(ctx, "ccx-t2", "alpha", "/repo with spaces/$HOME")
	if err != nil {
		t.Fatalf("CreateProjectShellWindowContext: %v", err)
	}
	if window.Name != "alpha-shell-2" || window.CurrentPath != "/repo with spaces/$HOME" {
		t.Fatalf("window = %#v, want alpha-shell-2 with exact repository path", window)
	}
	wantCreateArgs := []string{"new-window", "-t", "ccx-t2:", "-n", "alpha-shell-2", "-c", "/repo with spaces/$HOME"}
	if !reflect.DeepEqual(gotArgs[11], wantCreateArgs) {
		t.Fatalf("create args = %#v, want %#v", gotArgs[11], wantCreateArgs)
	}
	for i, gotCtx := range gotContexts {
		if gotCtx != ctx {
			t.Fatalf("command %d context was not propagated", i)
		}
	}
}

func TestCreateProjectShellWindowContextStartsInteractiveShellInRepository(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not installed: %v", err)
	}
	session := "ccx-tmux-project-shell-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	repoPath := t.TempDir()
	if err := exec.Command("tmux", "new-session", "-d", "-s", session, "-n", "alpha-orchestrator", "-c", repoPath).Run(); err != nil {
		t.Fatalf("create test session: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", session).Run()
	})

	created, err := CreateProjectShellWindowContext(context.Background(), session, "alpha", repoPath)
	if err != nil {
		t.Fatalf("CreateProjectShellWindowContext: %v", err)
	}
	windows, err := ListWindowsContext(context.Background(), session)
	if err != nil {
		t.Fatalf("ListWindowsContext: %v", err)
	}
	var found WindowInfo
	for _, window := range windows {
		if window.Name == created.Name {
			found = window
			break
		}
	}
	if found.Name == "" {
		t.Fatalf("created window %q not found in %#v", created.Name, windows)
	}
	wantPath, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		t.Fatalf("resolve repository path: %v", err)
	}
	gotPath, err := filepath.EvalSymlinks(found.CurrentPath)
	if err != nil {
		t.Fatalf("resolve tmux pane path %q: %v", found.CurrentPath, err)
	}
	if gotPath != wantPath {
		t.Fatalf("pane cwd = %q, want %q", gotPath, wantPath)
	}
	if found.CurrentCommand == "" {
		t.Fatal("created window has no current interactive command")
	}
}

func TestCreateProjectShellWindowContextRejectsCanceledContextBeforeTmux(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oldExecCommandContext := execCommandContext
	called := false
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		called = true
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	_, err := CreateProjectShellWindowContext(ctx, "ccx-t2", "alpha", "/repo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateProjectShellWindowContext error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("tmux command was started after context cancellation")
	}
}

func TestCreateProjectWindowContextRejectsForeignNameBeforeTmux(t *testing.T) {
	oldExecCommandContext := execCommandContext
	called := false
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		called = true
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "beta-shell-1", "/repo")
	if !errors.Is(err, ErrInvalidWindowPrefix) {
		t.Fatalf("CreateProjectWindowContext error = %v, want ErrInvalidWindowPrefix", err)
	}
	if called {
		t.Fatal("tmux command was started for a foreign project window")
	}
}

func TestCreateWindowContextUsesCommandContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotCtx context.Context
	var gotName string
	var gotArgs []string

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotCtx = ctx
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := CreateWindowContext(ctx, "session", "worker-task-001", "/tmp/worktree"); err != nil {
		t.Fatalf("CreateWindowContext: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("CreateWindowContext did not pass caller context to tmux command")
	}
	wantArgs := []string{"new-window", "-t", "session:", "-n", "worker-task-001", "-c", "/tmp/worktree"}
	if gotName != "tmux" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("command = %s %#v, want tmux %#v", gotName, gotArgs, wantArgs)
	}
}

func TestCreateWindowContextUsesNextAvailableIndexWithOccupiedAdjacentWindows(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not installed: %v", err)
	}
	session := "ccx-tmux-create-window-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if err := exec.Command("tmux", "new-session", "-d", "-s", session, "-n", "occupied-0").Run(); err != nil {
		t.Fatalf("create test session: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-session", "-t", session).Run()
	})
	indexOut, err := exec.Command("tmux", "display-message", "-t", session+":", "-p", "#{window_index}").CombinedOutput()
	if err != nil {
		t.Fatalf("read initial window index: %v\n%s", err, indexOut)
	}
	if strings.TrimSpace(string(indexOut)) != "0" {
		if err := exec.Command("tmux", "move-window", "-s", session+":", "-t", session+":0").Run(); err != nil {
			t.Fatalf("move initial window to index 0: %v", err)
		}
	}
	if err := exec.Command("tmux", "new-window", "-t", session+":1", "-n", "occupied-1").Run(); err != nil {
		t.Fatalf("create occupied adjacent window: %v", err)
	}

	if err := CreateWindowContext(context.Background(), session, "worker-task-001", t.TempDir()); err != nil {
		t.Fatalf("CreateWindowContext with occupied adjacent indexes: %v", err)
	}
	out, err := exec.Command("tmux", "list-windows", "-t", session, "-F", "#{window_index}:#{window_name}").CombinedOutput()
	if err != nil {
		t.Fatalf("list windows: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "2:worker-task-001\n") {
		t.Fatalf("created window index/name not found in tmux windows:\n%s", out)
	}
}

func TestSessionExistsContextReportsMissingSession(t *testing.T) {
	for _, stderr := range []string{"no such session", "can't find session missing"} {
		t.Run(stderr, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
			var gotCtx context.Context
			var gotName string
			var gotArgs []string

			oldExecCommandContext := execCommandContext
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				gotCtx = ctx
				gotName = name
				gotArgs = append([]string(nil), args...)
				return exec.Command("sh", "-c", "printf '%s\n' \"$1\" >&2; exit 1", "sh", stderr)
			}
			t.Cleanup(func() { execCommandContext = oldExecCommandContext })

			exists, err := SessionExistsContext(ctx, "missing")
			if err != nil {
				t.Fatalf("SessionExistsContext: %v", err)
			}
			if exists {
				t.Fatal("exists = true, want false")
			}
			if gotCtx != ctx {
				t.Fatal("SessionExistsContext did not pass caller context to tmux command")
			}
			wantArgs := []string{"has-session", "-t", "missing"}
			if gotName != "tmux" || !reflect.DeepEqual(gotArgs, wantArgs) {
				t.Fatalf("command = %s %#v, want tmux %#v", gotName, gotArgs, wantArgs)
			}
		})
	}
}

func TestSendKeysContextUsesCommandContextForLiteralAndEnter(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotContexts []context.Context
	var gotNames []string
	var gotArgs [][]string

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotContexts = append(gotContexts, ctx)
		gotNames = append(gotNames, name)
		gotArgs = append(gotArgs, append([]string(nil), args...))
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := SendKeysContext(ctx, "session", "worker-task-001", "hello"); err != nil {
		t.Fatalf("SendKeysContext: %v", err)
	}
	if len(gotArgs) != 2 {
		t.Fatalf("tmux command calls = %d, want 2", len(gotArgs))
	}
	for i := range gotContexts {
		if gotContexts[i] != ctx {
			t.Fatalf("call %d did not pass caller context", i)
		}
		if gotNames[i] != "tmux" {
			t.Fatalf("call %d name = %q, want tmux", i, gotNames[i])
		}
	}
	wantLiteral := []string{"send-keys", "-t", "session:worker-task-001", "-l", "hello"}
	wantEnter := []string{"send-keys", "-t", "session:worker-task-001", "Enter"}
	if !reflect.DeepEqual(gotArgs[0], wantLiteral) || !reflect.DeepEqual(gotArgs[1], wantEnter) {
		t.Fatalf("send-keys calls = %#v, want %#v then %#v", gotArgs, wantLiteral, wantEnter)
	}
}

func TestSendRawKeysContextDoesNotAppendEnter(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotCtx context.Context
	var gotName string
	var gotArgs []string

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotCtx = ctx
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := SendRawKeysContext(ctx, "session", "orchestrator", "\x1b[B"); err != nil {
		t.Fatalf("SendRawKeysContext: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("SendRawKeysContext did not pass caller context")
	}
	wantArgs := []string{"send-keys", "-t", "session:orchestrator", "-l", "\x1b[B"}
	if gotName != "tmux" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("command = %s %#v, want tmux %#v", gotName, gotArgs, wantArgs)
	}
}

func TestResizePaneContextUsesCommandContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotCtx context.Context
	var gotName string
	var gotArgs []string

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotCtx = ctx
		gotName = name
		gotArgs = append([]string(nil), args...)
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := ResizePaneContext(ctx, "session", "orchestrator", 132, 31); err != nil {
		t.Fatalf("ResizePaneContext: %v", err)
	}
	if gotCtx != ctx {
		t.Fatal("ResizePaneContext did not pass caller context")
	}
	wantArgs := []string{"resize-window", "-t", "session:orchestrator", "-x", "132", "-y", "31"}
	if gotName != "tmux" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("command = %s %#v, want tmux %#v", gotName, gotArgs, wantArgs)
	}
}

func TestSendKeysContextRejectsCanceledContextBeforeCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	oldExecCommandContext := execCommandContext
	called := false
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		called = true
		return exec.Command("sh", "-c", "true")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := SendKeysContext(ctx, "session", "worker-task-001", "hello")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SendKeysContext canceled error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("tmux command was started after context was already canceled")
	}
}
