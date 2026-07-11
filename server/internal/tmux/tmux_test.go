package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
		switch {
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			output = "0\t@0\n"
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			output = "alpha shell\t1\n"
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			output = "/Users/me/My Repo\t(a|b)\n"
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			output = "zsh\n"
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
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

func TestAttachPaneContextUsesBodylessSnapshotResponseAsWatermark(t *testing.T) {
	oldExecCommandContext := execCommandContext
	deleteMarker := filepath.Join(t.TempDir(), "delete-complete")
	execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		script := `
printf '%s\n' '%begin 0 1 0' '%end 0 1 0'
while IFS= read -r line; do
  case "$line" in
    display-message*) printf '%s\n' '%begin 0 2 0' '%0' '%end 0 2 0' ;;
    refresh-client*) printf '%s\n' '%begin 0 3 0' '%end 0 3 0' '%output %0 pre-before-capture' ;;
	    capture-pane*)
	      case "$line" in
	        *" -b "*) printf '%s\n' '%begin 0 4 0' '%end 0 4 0' '%output %0 queued\012' ;;
	        *) exit 91 ;;
	      esac ;;
	    save-buffer*)
	      case "$line" in
	        *" -b "*)
	          path=${line##* }
	          path=${path#\'}
	          path=${path%\'}
	          printf '%s\n' '%end 0 5 0' '%error 0 5 0' > "$path"
		          printf '%s\n' '%begin 0 5 0' '%end 0 5 0' '%output %0 live\012' '%output %0 after\134' ;;
	        *) exit 92 ;;
	      esac ;;
	    delete-buffer*)
	      printf '%s' deleted > "$TMUX_DELETE_MARKER"
	      printf '%s\n' '%output %0 delete-queued\012' '%begin 0 6 0' '%end 0 6 0' ;;
  esac
done
`
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Env = append(os.Environ(), "TMUX_DELETE_MARKER="+deleteMarker)
		return cmd
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	attachment, err := AttachPaneContext(context.Background(), "session", "window")
	if err != nil {
		t.Fatalf("AttachPaneContext: %v", err)
	}
	if got, want := string(attachment.Snapshot), "%end 0 5 0\n%error 0 5 0\n"; got != want {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	for i, want := range []string{"queued\n", "live\n", "after\\", "delete-queued\n"} {
		select {
		case chunk := <-attachment.Chunks:
			if got := string(chunk); got != want {
				t.Fatalf("live chunk %d = %q, want %q", i, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("live chunk %d was not delivered", i)
		}
	}
	if got, err := os.ReadFile(deleteMarker); err != nil || string(got) != "deleted" {
		t.Fatalf("delete-buffer marker = %q, err = %v; want synchronized execution", got, err)
	}
	attachment.Cleanup()
	select {
	case _, ok := <-attachment.Chunks:
		if ok {
			t.Fatal("attachment stream remained open after cleanup")
		}
	case <-time.After(time.Second):
		t.Fatal("attachment stream did not close after cleanup")
	}
}

func TestAttachPaneContextCancelsDuringSetup(t *testing.T) {
	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exec sleep 10")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := AttachPaneContext(ctx, "session", "window")
		result <- err
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("AttachPaneContext error = nil, want cancellation failure")
		}
	case <-time.After(time.Second):
		t.Fatal("AttachPaneContext did not stop after cancellation")
	}
}

func TestAttachPaneContextFallsBackToIndependentBufferDelete(t *testing.T) {
	oldExecCommandContext := execCommandContext
	fallbackMarker := filepath.Join(t.TempDir(), "fallback-delete")
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" && len(args) == 3 && args[0] == "delete-buffer" {
			marker := "invalid"
			if strings.HasPrefix(args[2], "ccx-attach-") && !strings.ContainsAny(args[2], "' \t\r\n") {
				marker = "deleted"
			}
			_ = os.WriteFile(fallbackMarker, []byte(marker), 0o600)
			return exec.CommandContext(ctx, "sh", "-c", "true")
		}
		script := `
printf '%s\n' '%begin 0 1 0' '%end 0 1 0'
while IFS= read -r line; do
  case "$line" in
    display-message*) printf '%s\n' '%begin 0 2 0' '%0' '%end 0 2 0' ;;
    refresh-client*) printf '%s\n' '%begin 0 3 0' '%end 0 3 0' ;;
    capture-pane*) printf '%s\n' '%begin 0 4 0' '%end 0 4 0' ;;
    save-buffer*) exit 0 ;;
  esac
done
`
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if _, err := AttachPaneContext(context.Background(), "session", "window"); err == nil {
		t.Fatal("AttachPaneContext error = nil, want broken save response")
	}
	if got, err := os.ReadFile(fallbackMarker); err != nil || string(got) != "deleted" {
		t.Fatalf("fallback delete marker = %q, err = %v; want independent cleanup", got, err)
	}
}

func TestAttachPaneContextUsesRealTmuxControlMode(t *testing.T) {
	socket := fmt.Sprintf("ccx-t9-attach-%d", os.Getpid())
	session := "ccx-t9-attach"
	run := func(args ...string) []byte {
		cmd := exec.Command("tmux", append([]string{"-L", socket, "-f", "/dev/null"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return out
	}
	_ = exec.Command("tmux", "-L", socket, "-f", "/dev/null", "kill-server").Run()
	run("new-session", "-d", "-s", session)
	t.Cleanup(func() {
		cmd := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "kill-server")
		_ = cmd.Run()
	})
	run("send-keys", "-t", "="+session+":0", "printf pre", "C-m")
	run("send-keys", "-t", "="+session+":0", "printf '%s\\n' '%end 1 2 3' '%error 1 2 3'", "C-m")
	time.Sleep(200 * time.Millisecond)

	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, name, append([]string{"-L", socket}, args...)...)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	attachment, err := AttachPaneContext(context.Background(), session, "0")
	if err != nil {
		t.Fatalf("AttachPaneContext: %v", err)
	}
	defer attachment.Cleanup()
	if !bytes.Contains(attachment.Snapshot, []byte("pre")) {
		t.Fatalf("snapshot = %q, want pre-boundary output", attachment.Snapshot)
	}
	for _, literal := range []string{"%end 1 2 3", "%error 1 2 3"} {
		if !bytes.Contains(attachment.Snapshot, []byte(literal)) {
			t.Fatalf("snapshot = %q, want literal pane line %q", attachment.Snapshot, literal)
		}
	}
	run("send-keys", "-t", "="+session+":0", "printf live", "C-m")
	deadline := time.NewTimer(2 * time.Second)
	foundLive := false
	for {
		select {
		case chunk, ok := <-attachment.Chunks:
			if !ok {
				t.Fatal("real tmux control-mode stream closed before live output")
			}
			if bytes.Contains(chunk, []byte("live")) {
				foundLive = true
			}
			if foundLive {
				deadline.Stop()
				attachment.Cleanup()
				for _, name := range strings.Split(strings.TrimSpace(string(run("list-buffers", "-F", "#{buffer_name}"))), "\n") {
					if strings.HasPrefix(name, "ccx-attach-") {
						t.Fatalf("attachment buffer %q remained after cleanup", name)
					}
				}
				return
			}
		case <-deadline.C:
			t.Fatal("real tmux control-mode output was not delivered")
		}
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

func TestListWindowsContextSkipsWindowThatDisappearsDuringHydration(t *testing.T) {
	oldExecCommandContext := execCommandContext
	call := 0
	execCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		call++
		if call == 1 {
			return exec.Command("sh", "-c", "printf '%s' '0\t@0\n'")
		}
		return exec.Command("sh", "-c", "printf '%s' \"can't find window: @0\" >&2; exit 1")
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	windows, err := ListWindowsContext(context.Background(), "ccx-t2")
	if err != nil {
		t.Fatalf("ListWindowsContext: %v", err)
	}
	if windows == nil || len(windows) != 0 {
		t.Fatalf("windows = %#v, want empty result after window churn", windows)
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

func TestWindowIDOrderingUsesNumericSuffix(t *testing.T) {
	if !windowIDLess("@2", "@10") {
		t.Fatal("windowIDLess(@2, @10) = false, want numeric ordering")
	}
	if windowIDLess("@10", "@2") {
		t.Fatal("windowIDLess(@10, @2) = true, want numeric ordering")
	}
}

func TestCreateProjectShellWindowContextChoosesDeterministicFreeName(t *testing.T) {
	ctx := context.WithValue(context.Background(), tmuxTestContextKey{}, "marker")
	var gotContexts []context.Context
	var gotArgs [][]string
	created := false
	renamed := false
	pendingName := ""
	oldExecCommandContext := execCommandContext
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotContexts = append(gotContexts, ctx)
		gotArgs = append(gotArgs, append([]string(nil), args...))
		var output string
		switch {
		case args[0] == "wait-for":
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if created {
				output = "2\t@2\n"
			} else {
				output = "0\t@0\n1\t@1\n"
			}
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created {
				output = "@2\n"
			} else {
				output = "@0\n@1\n"
			}
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if strings.HasSuffix(args[2], "@0") {
				output = "alpha-shell-1\n"
			} else if strings.HasSuffix(args[2], "@1") {
				output = "alpha-shell-3\n"
			} else if strings.HasSuffix(args[2], "@2") && !renamed {
				output = pendingName + "\n"
			} else {
				output = "alpha-shell-2\n"
			}
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			output = "/repo with spaces\n"
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			output = "bash\n"
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			output = "@2\n"
		case args[0] == "rename-window":
			renamed = true
			return exec.Command("sh", "-c", "true")
		default:
			return exec.Command("sh", "-c", "true")
		}
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	window, err := CreateProjectShellWindowContext(ctx, "ccx-t2", "alpha", "/repo with spaces")
	if err != nil {
		t.Fatalf("CreateProjectShellWindowContext: %v", err)
	}
	if window.Name != "alpha-shell-2" || window.CurrentPath != "/repo with spaces" {
		t.Fatalf("window = %#v, want alpha-shell-2 with hydrated repository path", window)
	}
	var createArgs []string
	for _, args := range gotArgs {
		if len(args) > 0 && args[0] == "new-window" {
			createArgs = args
			break
		}
	}
	if len(createArgs) == 0 || createArgs[0] != "new-window" {
		t.Fatalf("create args = %#v, want new-window", createArgs)
	}
	if pendingName == "" || !strings.HasPrefix(pendingName, pendingProjectWindowPrefix) {
		t.Fatalf("pending name = %q, want unique pending prefix", pendingName)
	}
	var renameArgs []string
	for _, args := range gotArgs {
		if len(args) > 0 && args[0] == "rename-window" {
			renameArgs = args
			break
		}
	}
	if !reflect.DeepEqual(renameArgs, []string{"rename-window", "-t", "@2", "alpha-shell-2"}) {
		t.Fatalf("rename args = %#v, want rename to alpha-shell-2", renameArgs)
	}
	for i, gotCtx := range gotContexts {
		if len(gotArgs[i]) >= 2 && gotArgs[i][0] == "wait-for" && gotArgs[i][1] == "-U" {
			continue
		}
		if gotCtx != ctx {
			t.Fatalf("command %d context was not propagated", i)
		}
	}
}

func TestProjectWindowFilteringDoesNotCrossOverlappingSlugs(t *testing.T) {
	prefix := ProjectWindowPrefix("my-project")
	if !isKnownProjectWindowName("my-project", prefix, "my-project-orchestrator") {
		t.Fatal("project orchestrator window was not recognized")
	}
	if !isKnownProjectWindowName("my-project", prefix, "my-project-shell-1") {
		t.Fatal("project shell window was not recognized")
	}
	if isKnownProjectWindowName("my-project", prefix, "my-project-shell-orchestrator") {
		t.Fatal("overlapping project orchestrator was incorrectly included")
	}
	if isKnownProjectWindowName("my-project", prefix, "my-project-worker-orchestrator") {
		t.Fatal("overlapping worker-project orchestrator was incorrectly included")
	}
	for _, foreign := range []string{
		"my-project-worker-abc-shell-1",
		"my-project-worker-abc-worker-task",
		"my-project-worker-worker-task",
	} {
		if isKnownProjectWindowName("my-project", prefix, foreign) {
			t.Fatalf("overlapping project window %q was incorrectly included", foreign)
		}
	}
}

func installExternalDuplicateTmuxStub(t *testing.T, allowRetry bool) *int {
	t.Helper()
	oldExecCommandContext := execCommandContext
	created := 0
	pendingName := ""
	renamed := false
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) == 0 {
			return exec.Command("sh", "-c", "true")
		}
		var output string
		switch {
		case args[0] == "wait-for":
			return exec.Command("sh", "-c", "true")
		case args[0] == "new-window":
			created++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			if created == 1 || !allowRetry {
				output = "@1\n"
			} else {
				output = "@3\n"
			}
		case args[0] == "kill-window":
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			switch created {
			case 0:
				output = ""
			case 1:
				output = "0\t@1\n1\t@2\n"
			default:
				output = "2\t@3\n"
			}
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created == 1 {
				output = "@1\n@2\n"
			} else if created > 1 {
				output = "@3\n"
			}
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if strings.HasSuffix(args[2], "@2") {
				output = "alpha-shell-1\n"
			} else if strings.HasSuffix(args[2], "@3") && renamed {
				output = "alpha-shell-2\n"
			} else if strings.HasSuffix(args[2], "@1") || strings.HasSuffix(args[2], "@3") {
				output = pendingName + "\n"
			} else {
				output = pendingName + "\n"
			}
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			output = "/repo\n"
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			output = "bash\n"
		case args[0] == "rename-window":
			renamed = true
			return exec.Command("sh", "-c", "true")
		}
		return tmuxOutput(output)
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })
	return &created
}

func TestCreateProjectShellWindowContextRetriesExternalDuplicate(t *testing.T) {
	created := installExternalDuplicateTmuxStub(t, true)
	window, err := CreateProjectShellWindowContext(context.Background(), "ccx-t2", "alpha", "/repo")
	if err != nil {
		t.Fatalf("CreateProjectShellWindowContext: %v", err)
	}
	if window.Name != "alpha-shell-2" {
		t.Fatalf("window name = %q, want alpha-shell-2", window.Name)
	}
	if *created != 2 {
		t.Fatalf("new-window calls = %d, want retry after duplicate", *created)
	}
}

func TestCreateProjectWindowContextRejectsExternalDuplicate(t *testing.T) {
	installExternalDuplicateTmuxStub(t, false)
	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if !errors.Is(err, ErrWindowNameTaken) {
		t.Fatalf("CreateProjectWindowContext error = %v, want ErrWindowNameTaken", err)
	}
}

func TestCreateProjectWindowContextReconcilesCreatedWindowAfterClientError(t *testing.T) {
	oldExecCommandContext := execCommandContext
	created := false
	killed := false
	renamed := false
	pendingName := ""
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			return exec.Command("sh", "-c", "printf '%s' 'client output lost' >&2; exit 1")
		case args[0] == "kill-window":
			killed = true
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if created {
				return tmuxOutput("2\t@9\n")
			}
			return tmuxOutput("")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created {
				return tmuxOutput("@9\n")
			}
			return tmuxOutput("")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if renamed {
				return tmuxOutput("alpha-shell-1\n")
			}
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "rename-window":
			renamed = true
			return exec.Command("sh", "-c", "true")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo"); err != nil {
		t.Fatalf("CreateProjectWindowContext: %v", err)
	}
	if !created {
		t.Fatal("new-window was not attempted")
	}
	if killed {
		t.Fatal("reconciliation killed a window without ownership evidence")
	}
}

func TestCreateProjectWindowContextDoesNotClaimExternalWindowAfterCreateError(t *testing.T) {
	oldExecCommandContext := execCommandContext
	external := false
	killed := false
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			external = true
			return exec.Command("sh", "-c", "printf '%s' 'client failed before create' >&2; exit 1")
		case args[0] == "kill-window":
			killed = true
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if external {
				return tmuxOutput("2\t@8\n")
			}
			return tmuxOutput("")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if external {
				return tmuxOutput("@8\n")
			}
			return tmuxOutput("")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if err == nil {
		t.Fatal("CreateProjectWindowContext error = nil, want original create error")
	}
	if killed {
		t.Fatal("external same-name window was incorrectly killed")
	}
}

func TestCreateProjectWindowContextCleansOnlyOwnedWindowOnRenameDuplicate(t *testing.T) {
	oldExecCommandContext := execCommandContext
	created := false
	pendingName := ""
	killedTarget := ""
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			return tmuxOutput("@9\n")
		case args[0] == "kill-window":
			killedTarget = args[2]
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if created {
				return tmuxOutput("2\t@8\n3\t@9\n")
			}
			return tmuxOutput("")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created {
				return tmuxOutput("@8\n@9\n")
			}
			return tmuxOutput("")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if strings.HasSuffix(args[2], "@9") {
				return tmuxOutput(pendingName + "\n")
			}
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			return tmuxOutput(pendingName + "\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if !errors.Is(err, ErrWindowNameTaken) {
		t.Fatalf("CreateProjectWindowContext error = %v, want ErrWindowNameTaken", err)
	}
	if killedTarget != "@9" {
		t.Fatalf("killed window = %q, want owned @9", killedTarget)
	}
}

func TestCreateProjectWindowContextPreservesWinnerAfterPostRenameDuplicate(t *testing.T) {
	oldExecCommandContext := execCommandContext
	created := false
	external := false
	renamed := false
	postRenameLists := 0
	pendingName := ""
	killedTarget := ""
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			return tmuxOutput("@1\n")
		case args[0] == "rename-window":
			renamed = true
			external = true
			return exec.Command("sh", "-c", "true")
		case args[0] == "kill-window":
			killedTarget = args[2]
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			switch {
			case !created:
				return tmuxOutput("")
			case external:
				postRenameLists++
				if postRenameLists > 1 {
					return tmuxOutput("2\t@1\n")
				}
				return tmuxOutput("2\t@1\n3\t@2\n")
			default:
				return tmuxOutput("2\t@1\n")
			}
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if external && postRenameLists == 1 {
				return tmuxOutput("@1\n@2\n")
			}
			return tmuxOutput("@1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if strings.HasSuffix(args[2], "@1") && !renamed {
				return tmuxOutput(pendingName + "\n")
			}
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			return tmuxOutput(pendingName + "\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	if err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo"); err != nil {
		t.Fatalf("CreateProjectWindowContext: %v", err)
	}
	if killedTarget == "@1" {
		t.Fatal("winner-preserving duplicate handling killed the winning owned window")
	}
	if postRenameLists < 2 {
		t.Fatalf("post-rename window list calls = %d, want convergence recheck", postRenameLists)
	}
}

func TestCreateProjectWindowContextRollsBackWinnerWhenDuplicateDoesNotConverge(t *testing.T) {
	oldExecCommandContext := execCommandContext
	oldReconcileTimeout := projectWindowReconcileTimeout
	projectWindowReconcileTimeout = 2 * time.Second
	createCalls := 0
	renamed := false
	rolledBack := false
	pendingName := ""
	killedOwn := false
	killedLoser := false
	ifShellCalled := false
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			createCalls++
			renamed = false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			if createCalls == 1 {
				return tmuxOutput("@1\n")
			}
			return tmuxOutput("@3\n")
		case args[0] == "rename-window":
			renamed = true
			return exec.Command("sh", "-c", "true")
		case args[0] == "if-shell":
			ifShellCalled = true
			if strings.Contains(args[len(args)-2], "@1") {
				rolledBack = true
				killedOwn = true
			} else if strings.Contains(args[len(args)-2], "@2") {
				killedLoser = true
			}
			return exec.Command("sh", "-c", "true")
		case args[0] == "kill-window":
			if args[2] == "@1" {
				rolledBack = true
				killedOwn = true
			} else if args[2] == "@2" {
				killedLoser = true
			}
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if createCalls == 0 || (rolledBack && createCalls == 1) {
				return tmuxOutput("")
			}
			if !renamed {
				if createCalls == 1 {
					return tmuxOutput("2\t@1\n")
				}
				return tmuxOutput("2\t@3\n")
			}
			if rolledBack {
				return tmuxOutput("2\t@3\n")
			}
			return tmuxOutput("2\t@1\n3\t@2\n")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if !renamed {
				if createCalls == 1 {
					return tmuxOutput("@1\n")
				}
				return tmuxOutput("@3\n")
			}
			if rolledBack {
				return tmuxOutput("@3\n")
			}
			return tmuxOutput("@1\n@2\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			if (args[2] == "@1" || args[2] == "@3") && !renamed {
				return tmuxOutput(pendingName + "\n")
			}
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			if args[2] == "@2" {
				return tmuxOutput(pendingProjectWindowPrefix + "0123456789abcdef01234567\n")
			}
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowSessionFormat:
			return tmuxOutput("ccx-t2\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowRepoPathFormat:
			return tmuxOutput("/repo\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() {
		execCommandContext = oldExecCommandContext
		projectWindowReconcileTimeout = oldReconcileTimeout
	})

	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if !errors.Is(err, errProjectWindowNameUnresolved) {
		t.Fatalf("CreateProjectWindowContext error = %v, want non-convergence error", err)
	}
	if !killedOwn {
		t.Fatalf("non-converging duplicate cleanup did not kill the committed winner (if-shell=%v)", ifShellCalled)
	}
	if killedLoser {
		t.Fatal("winner rollback killed another creator's marked window")
	}
	if err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo"); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if createCalls != 2 {
		t.Fatalf("new-window calls = %d, want one retry after rollback", createCalls)
	}
}

func TestRollbackProjectWindowsSkipsChangedAttemptMarker(t *testing.T) {
	oldExecCommandContext := execCommandContext
	killed := false
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			return tmuxOutput("2\t@1\n")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			return tmuxOutput("@1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			return tmuxOutput("foreign-marker\n")
		case args[0] == "kill-window":
			killed = true
			return exec.Command("sh", "-c", "true")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	pendingName := pendingProjectWindowPrefix + "0123456789abcdef01234567"
	err := rollbackProjectWindows(context.Background(), "ccx-t2", "@1", pendingName, "/repo", []WindowInfo{{ID: "@1", CreationMarker: pendingName}})
	if err == nil {
		t.Fatal("rollbackProjectWindows error = nil, want changed-marker failure")
	}
	if killed {
		t.Fatal("rollbackProjectWindows killed a window after its attempt marker changed")
	}
}

func TestRollbackProjectWindowsDoesNotKillAfterOwnershipReadInterleaving(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutated func(*string, *string)
	}{
		{name: "marker changed", mutated: func(marker, path *string) { *marker = "foreign-marker" }},
		{name: "pane path changed", mutated: func(marker, path *string) { *path = "/other-repo" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldExecCommandContext := execCommandContext
			oldTimeout := projectWindowReconcileTimeout
			projectWindowReconcileTimeout = 2 * time.Second
			pendingName := pendingProjectWindowPrefix + "0123456789abcdef01234567"
			marker := pendingName
			path := "/repo"
			mutated := false
			killed := false
			var ifShellArgs []string
			tmuxOutput := func(output string) *exec.Cmd {
				return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
			}
			execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
				switch {
				case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
					return tmuxOutput("2\t@1\n")
				case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
					if !mutated {
						mutated = true
						tc.mutated(&marker, &path)
					}
					return tmuxOutput("@1\n")
				case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
					return tmuxOutput("alpha-shell-1\n")
				case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
					return tmuxOutput(path + "\n")
				case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
					return tmuxOutput("bash\n")
				case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
					return tmuxOutput(marker + "\n")
				case args[0] == "display-message" && args[len(args)-1] == projectWindowSessionFormat:
					return tmuxOutput("ccx-t2\n")
				case args[0] == "display-message" && args[len(args)-1] == projectWindowRepoPathFormat:
					return tmuxOutput("/repo\n")
				case args[0] == "if-shell":
					ifShellArgs = append([]string(nil), args...)
					return exec.Command("sh", "-c", "true")
				case args[0] == "kill-window":
					killed = true
					return exec.Command("sh", "-c", "true")
				default:
					return exec.Command("sh", "-c", "true")
				}
			}
			t.Cleanup(func() {
				execCommandContext = oldExecCommandContext
				projectWindowReconcileTimeout = oldTimeout
			})

			err := rollbackProjectWindows(context.Background(), "ccx-t2", "@1", pendingName, "/repo", []WindowInfo{{ID: "@1", CreationMarker: pendingName}})
			if err == nil {
				t.Fatal("rollbackProjectWindows error = nil, want interleaving ownership failure")
			}
			if killed {
				t.Fatal("rollbackProjectWindows issued an unconditional kill")
			}
			if len(ifShellArgs) == 0 {
				t.Fatal("rollbackProjectWindows did not reach conditional side effect")
			}
			condition := strings.Join(ifShellArgs, " ")
			for _, required := range []string{"session_name", "pane_current_path"} {
				if !strings.Contains(condition, required) {
					t.Fatalf("if-shell condition = %q, missing %q", condition, required)
				}
			}
		})
	}
}

func TestOwnershipConditionIncludesRawAndCanonicalRepositoryPaths(t *testing.T) {
	condition := ownershipCondition(pendingProjectWindowPrefix + "0123456789abcdef01234567")
	for _, required := range []string{projectWindowRepoPathFormat, projectWindowRawRepoPathFormat} {
		if !strings.Contains(condition, required) {
			t.Fatalf("ownership condition = %q, missing repository path guard %q", condition, required)
		}
	}
}

func TestRestorePendingWindowSkipsRenameAfterOwnershipReadInterleaving(t *testing.T) {
	oldExecCommandContext := execCommandContext
	oldTimeout := projectWindowReconcileTimeout
	projectWindowReconcileTimeout = 2 * time.Second
	pendingName := pendingProjectWindowPrefix + "0123456789abcdef01234567"
	path := "/repo"
	mutated := false
	renamed := false
	var ifShellArgs []string
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			return tmuxOutput("2\t@1\n")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if !mutated {
				mutated = true
				path = "/external-repo"
			}
			return tmuxOutput("@1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			return tmuxOutput("alpha-shell-1\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput(path + "\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowSessionFormat:
			return tmuxOutput("ccx-t2\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowRepoPathFormat:
			return tmuxOutput("/repo\n")
		case args[0] == "if-shell":
			ifShellArgs = append([]string(nil), args...)
			return exec.Command("sh", "-c", "true")
		case args[0] == "rename-window":
			renamed = true
			return exec.Command("sh", "-c", "true")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() {
		execCommandContext = oldExecCommandContext
		projectWindowReconcileTimeout = oldTimeout
	})

	err := restorePendingWindowContext(context.Background(), "ccx-t2", "@1", pendingName, "/repo")
	if err == nil {
		t.Fatal("restorePendingWindowContext error = nil, want interleaving ownership failure")
	}
	if renamed {
		t.Fatal("restorePendingWindowContext issued an unguarded rename")
	}
	if len(ifShellArgs) == 0 || !strings.Contains(strings.Join(ifShellArgs, " "), "pane_current_path") {
		t.Fatalf("if-shell args = %#v, want path-guarded conditional rename", ifShellArgs)
	}
}

func TestCreateProjectWindowContextCleansOwnedWindowWhenRenameFails(t *testing.T) {
	oldExecCommandContext := execCommandContext
	created := false
	pendingName := ""
	killedTarget := ""
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			return tmuxOutput("@9\n")
		case args[0] == "rename-window":
			return exec.Command("sh", "-c", "printf '%s' 'rename failed' >&2; exit 1")
		case args[0] == "if-shell":
			killedTarget = "@9"
			return exec.Command("sh", "-c", "true")
		case args[0] == "kill-window":
			killedTarget = args[2]
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if created {
				return tmuxOutput("2\t@9\n")
			}
			return tmuxOutput("")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created {
				return tmuxOutput("@9\n")
			}
			return tmuxOutput("")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowMarkerFormat:
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowSessionFormat:
			return tmuxOutput("ccx-t2\n")
		case args[0] == "display-message" && args[len(args)-1] == projectWindowRepoPathFormat:
			return tmuxOutput("/repo\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateProjectWindowContext(context.Background(), "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if err == nil {
		t.Fatal("CreateProjectWindowContext error = nil, want rename failure")
	}
	if killedTarget != "@9" {
		t.Fatalf("killed window = %q, want owned @9", killedTarget)
	}
}

func TestCreateProjectWindowContextCleansOwnedWindowOnCancellationBeforeRename(t *testing.T) {
	oldExecCommandContext := execCommandContext
	created := false
	pendingName := ""
	killedTarget := ""
	ctx, cancel := context.WithCancel(context.Background())
	tmuxOutput := func(output string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", output)
	}
	execCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		switch {
		case args[0] == "new-window":
			created = true
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-n" {
					pendingName = args[i+1]
				}
			}
			cancel()
			return tmuxOutput("@9\n")
		case args[0] == "kill-window":
			killedTarget = args[2]
			return exec.Command("sh", "-c", "true")
		case args[0] == "list-windows" && args[len(args)-1] == windowListFormat:
			if created {
				return tmuxOutput("2\t@9\n")
			}
			return tmuxOutput("")
		case args[0] == "list-windows" && args[len(args)-1] == "#{window_id}":
			if created {
				return tmuxOutput("@9\n")
			}
			return tmuxOutput("")
		case args[0] == "display-message" && args[len(args)-1] == "#{window_name}":
			return tmuxOutput(pendingName + "\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_path}":
			return tmuxOutput("/repo\n")
		case args[0] == "display-message" && args[len(args)-1] == "#{pane_current_command}":
			return tmuxOutput("bash\n")
		default:
			return exec.Command("sh", "-c", "true")
		}
	}
	t.Cleanup(func() { execCommandContext = oldExecCommandContext })

	err := CreateProjectWindowContext(ctx, "ccx-t2", "alpha", "alpha-shell-1", "/repo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateProjectWindowContext error = %v, want context.Canceled", err)
	}
	if killedTarget != "@9" {
		t.Fatalf("killed window = %q, want owned @9", killedTarget)
	}
}

func TestPathsMatchResolvesRepositorySymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "repo")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("create target: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if !pathsMatch(target, link) {
		t.Fatalf("pathsMatch(%q, %q) = false, want true", target, link)
	}
}

func TestCreateProjectShellWindowContextStartsInteractiveShellInRepositoryWithoutPersistentLock(t *testing.T) {
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
	staleLock := "ccx-stale-project-shell-lock-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	if err := exec.Command("tmux", "wait-for", "-L", staleLock).Run(); err != nil {
		t.Fatalf("create stale tmux lock: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "wait-for", "-U", staleLock).Run()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	created, err := CreateProjectShellWindowContext(ctx, session, "alpha", repoPath)
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
