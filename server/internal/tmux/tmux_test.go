package tmux

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

type tmuxTestContextKey struct{}

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
	wantArgs := []string{"new-window", "-t", "session", "-n", "worker-task-001", "-c", "/tmp/worktree"}
	if gotName != "tmux" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("command = %s %#v, want tmux %#v", gotName, gotArgs, wantArgs)
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
