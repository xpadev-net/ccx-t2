package orchestrator

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/ledger"
)

type fakeTmux struct {
	mu       sync.Mutex
	alive    bool
	idle     bool
	onAlive  func()
	commands []string
	prompts  []string
	kills    int
	creates  int
}

func (f *fakeTmux) EnsureSession(session string) error { return nil }

func (f *fakeTmux) CreateWindow(session, name, startDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = true
	f.idle = false
	f.creates++
	return nil
}

func (f *fakeTmux) SendKeys(session, window, keys string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(keys, "You are the Orchestrator agent") {
		f.prompts = append(f.prompts, keys)
	} else {
		f.commands = append(f.commands, keys)
	}
	return nil
}

func (f *fakeTmux) KillWindow(session, window string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = false
	f.idle = false
	f.kills++
	return nil
}

func (f *fakeTmux) IsWindowAlive(session, window string) (bool, error) {
	f.mu.Lock()
	onAlive := f.onAlive
	f.onAlive = nil
	alive := f.alive
	f.mu.Unlock()
	if onAlive != nil {
		onAlive()
	}
	return alive, nil
}

func (f *fakeTmux) IsPaneIdle(session, window string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idle, nil
}

func (f *fakeTmux) setIdle(idle bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idle = idle
}

func (f *fakeTmux) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, len(f.commands), len(f.prompts)
}

func TestTriggerStartsOrchestratorWithSnapshotAndMCPArgs(t *testing.T) {
	l, cfg := newTestDeps(t)
	if err := l.Add(ledger.Task{ID: "task-001", Title: "Task", Status: "unstarted", Body: "Do it"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	fake := &fakeTmux{idle: true}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond

	if err := o.Trigger(context.Background(), "heartbeat"); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	if got := fake.creates; got != 1 {
		t.Fatalf("creates = %d, want 1", got)
	}
	if len(fake.commands) != 1 {
		t.Fatalf("commands = %#v, want one command", fake.commands)
	}
	args, err := shellquote.Split(fake.commands[0])
	if err != nil {
		t.Fatalf("command shell syntax: %v", err)
	}
	wantArgs := []string{"sh", "-c", "http://localhost:8080/mcp/orchestrator", "tok en"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("command args = %#v, want %#v", args, wantArgs)
	}
	if len(fake.prompts) != 1 {
		t.Fatalf("prompts = %#v, want one prompt", fake.prompts)
	}
	for _, want := range []string{
		"Trigger reason: heartbeat",
		`"id": "task-001"`,
		`"title": "Task"`,
		`"name": "worker"`,
	} {
		if !strings.Contains(fake.prompts[0], want) {
			t.Fatalf("prompt does not contain %q:\n%s", want, fake.prompts[0])
		}
	}
}

func TestTriggerQueuesWhileWindowActiveAndDrainsAfterIdle(t *testing.T) {
	l, cfg := newTestDeps(t)
	fake := &fakeTmux{alive: true, idle: false}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond

	if err := o.Trigger(context.Background(), "first"); err != nil {
		t.Fatalf("Trigger first: %v", err)
	}
	creates, commands, prompts := fake.counts()
	if creates != 0 || commands != 0 || prompts != 0 {
		t.Fatalf("active trigger launched immediately: creates=%d commands=%d prompts=%d", creates, commands, prompts)
	}

	fake.setIdle(true)
	deadline := time.After(time.Second)
	for {
		creates, commands, prompts = fake.counts()
		if creates == 1 && commands == 1 && prompts == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("queued trigger did not drain: creates=%d commands=%d prompts=%d", creates, commands, prompts)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if fake.kills != 1 {
		t.Fatalf("kills = %d, want idle window reset", fake.kills)
	}
}

func TestTriggerCanceledContextDoesNotQueueWhenIdle(t *testing.T) {
	l, cfg := newTestDeps(t)
	fake := &fakeTmux{}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := o.Trigger(ctx, "canceled"); err == nil {
		t.Fatal("Trigger canceled context error = nil, want error")
	}
	if queued := queuedSnapshot(o); len(queued) != 0 {
		t.Fatalf("queued = %#v, want empty", queued)
	}
	creates, commands, prompts := fake.counts()
	if creates != 0 || commands != 0 || prompts != 0 {
		t.Fatalf("canceled trigger launched: creates=%d commands=%d prompts=%d", creates, commands, prompts)
	}
}

func TestTriggerCanceledContextDoesNotQueueWhenActive(t *testing.T) {
	l, cfg := newTestDeps(t)
	fake := &fakeTmux{alive: true, idle: false}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := o.Trigger(ctx, "canceled"); err == nil {
		t.Fatal("Trigger canceled context error = nil, want error")
	}
	if queued := queuedSnapshot(o); len(queued) != 0 {
		t.Fatalf("queued = %#v, want empty", queued)
	}
	fake.setIdle(true)
	time.Sleep(10 * time.Millisecond)
	creates, commands, prompts := fake.counts()
	if creates != 0 || commands != 0 || prompts != 0 {
		t.Fatalf("canceled active trigger drained later: creates=%d commands=%d prompts=%d", creates, commands, prompts)
	}
}

func TestTriggerMidCallCancellationDoesNotQueue(t *testing.T) {
	l, cfg := newTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeTmux{
		alive: true,
		idle:  false,
		onAlive: func() {
			cancel()
		},
	}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond

	if err := o.Trigger(ctx, "canceled during check"); err == nil {
		t.Fatal("Trigger mid-call canceled context error = nil, want error")
	}
	if queued := queuedSnapshot(o); len(queued) != 0 {
		t.Fatalf("queued = %#v, want empty", queued)
	}
	fake.setIdle(true)
	time.Sleep(10 * time.Millisecond)
	creates, commands, prompts := fake.counts()
	if creates != 0 || commands != 0 || prompts != 0 {
		t.Fatalf("mid-call canceled trigger drained later: creates=%d commands=%d prompts=%d", creates, commands, prompts)
	}
}

func TestTriggerMidCallCancellationKeepsOlderQueuedWork(t *testing.T) {
	l, cfg := newTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeTmux{
		alive: true,
		idle:  false,
		onAlive: func() {
			cancel()
		},
	}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.queued = []string{"same"}

	if err := o.Trigger(ctx, "same"); err == nil {
		t.Fatal("Trigger mid-call canceled context error = nil, want error")
	}
	if queued := queuedSnapshot(o); !reflect.DeepEqual(queued, []string{"same"}) {
		t.Fatalf("queued = %#v, want older queued work preserved", queued)
	}
}

func TestTriggerMidCallCancellationDrainsLaterQueuedWork(t *testing.T) {
	l, cfg := newTestDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	startedSecond := make(chan struct{})
	releaseAlive := make(chan struct{})
	fake := &fakeTmux{
		alive: true,
		idle:  false,
		onAlive: func() {
			cancel()
			close(startedSecond)
			<-releaseAlive
		},
	}
	o := New(l, cfg, "proj", "http://localhost:8080")
	o.tmux = fake
	o.pollInterval = time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		errCh <- o.Trigger(ctx, "canceled owner")
	}()
	<-startedSecond
	if err := o.Trigger(context.Background(), "later"); err != nil {
		t.Fatalf("Trigger later: %v", err)
	}
	fake.setIdle(true)
	close(releaseAlive)
	if err := <-errCh; err == nil {
		t.Fatal("owner Trigger error = nil, want context cancellation")
	}

	deadline := time.After(time.Second)
	for {
		creates, commands, prompts := fake.counts()
		if creates == 1 && commands == 1 && prompts == 1 {
			break
		}
		select {
		case <-deadline:
			creates, commands, prompts := fake.counts()
			t.Fatalf("later queued trigger did not drain: creates=%d commands=%d prompts=%d queued=%#v", creates, commands, prompts, queuedSnapshot(o))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if queued := queuedSnapshot(o); len(queued) != 0 {
		t.Fatalf("queued = %#v, want empty after later trigger drains", queued)
	}
}

func queuedSnapshot(o *Orchestrator) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.queued...)
}

func TestBuildMCPTokensRejectsInvalidTemplateShellSyntax(t *testing.T) {
	_, err := buildMCPTokens("--mcp-url '{url}", "http://localhost:8080/mcp/orchestrator", "")
	if err == nil {
		t.Fatal("buildMCPTokens() error = nil, want invalid shell syntax error")
	}
}

func newTestDeps(t *testing.T) (*ledger.Ledger, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	l := ledger.NewLedger(filepath.Join(dir, "ledger.md"), filepath.Join(dir, "archive"))
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Slug:              "proj",
			RepoPath:          dir,
			ValidationCommand: "go test ./...",
		},
		Server: config.ServerConfig{McpSecret: "tok en"},
		Orchestrator: config.OrchestratorConfig{
			Harness: "orch",
		},
		WorkerHarnesses: []string{"worker"},
		Harnesses: map[string]config.HarnessConfig{
			"orch": {
				Command: "sh",
				McpArgs: "-c {url} {secret}",
			},
			"worker": {
				Command: "sh",
				McpArgs: "-c {url} {secret}",
			},
		},
	}
	return l, cfg
}
