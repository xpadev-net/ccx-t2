package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"github.com/xpadev/ccx-t2/internal/config"
	"github.com/xpadev/ccx-t2/internal/harness"
	"github.com/xpadev/ccx-t2/internal/ledger"
	"github.com/xpadev/ccx-t2/internal/tmux"
)

const (
	windowName   = "orchestrator"
	maxReasonLen = 200
)

type tmuxClient interface {
	EnsureSession(session string) error
	CreateWindow(session, name, startDir string) error
	SendKeys(session, window, keys string) error
	KillWindow(session, window string) error
	IsWindowAlive(session, window string) (bool, error)
	IsPaneIdle(session, window string) (bool, error)
}

type realTmux struct{}

func (realTmux) EnsureSession(session string) error { return tmux.EnsureSession(session) }
func (realTmux) CreateWindow(session, name, startDir string) error {
	return tmux.CreateWindow(session, name, startDir)
}
func (realTmux) SendKeys(session, window, keys string) error {
	return tmux.SendKeys(session, window, keys)
}
func (realTmux) KillWindow(session, window string) error {
	return tmux.KillWindow(session, window)
}
func (realTmux) IsWindowAlive(session, window string) (bool, error) {
	return tmux.IsWindowAlive(session, window)
}
func (realTmux) IsPaneIdle(session, window string) (bool, error) {
	return tmux.IsPaneIdle(session, window)
}

// Orchestrator starts and queues stateless orchestrator harness runs.
type Orchestrator struct {
	ledger       *ledger.Ledger
	cfg          *config.Config
	session      string
	baseURL      string
	tmux         tmuxClient
	pollInterval time.Duration
	done         chan struct{}
	closeOnce    sync.Once
	cond         *sync.Cond

	mu       sync.Mutex
	queued   []string
	draining bool
	closed   bool
	starting bool
	runStart time.Time
}

// New creates an Orchestrator triggerer.
func New(l *ledger.Ledger, cfg *config.Config, session, baseURL string) *Orchestrator {
	o := &Orchestrator{
		ledger:       l,
		cfg:          cfg,
		session:      strings.TrimSpace(session),
		baseURL:      strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		tmux:         realTmux{},
		pollInterval: time.Second,
		done:         make(chan struct{}),
	}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// Trigger queues an orchestrator run. If the orchestrator window is idle or
// missing, it starts immediately; if a run is active, the request is preserved
// and started by a background drain loop after the pane becomes idle.
func (o *Orchestrator) Trigger(ctx context.Context, reason string) error {
	if err := o.validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reason = normalizeReason(reason)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("orchestrator is closed")
	}
	queuedIndex := len(o.queued)
	o.queued = append(o.queued, reason)
	if o.draining {
		o.mu.Unlock()
		return nil
	}
	o.draining = true
	o.mu.Unlock()

	if _, err := o.tryStartNext(ctx); err != nil {
		o.mu.Lock()
		o.removeQueuedAtLocked(queuedIndex, reason)
		hasRemaining := len(o.queued) > 0
		o.draining = hasRemaining
		o.mu.Unlock()
		if hasRemaining {
			go o.drain()
		}
		return err
	}
	go o.drain()
	return nil
}

func normalizeReason(reason string) string {
	normalized := strings.Join(strings.Fields(reason), " ")
	if len(normalized) <= maxReasonLen {
		return normalized
	}
	var sb strings.Builder
	for _, r := range normalized {
		if sb.Len()+len(string(r)) > maxReasonLen {
			break
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// Close stops the background drain loop. It does not kill the tmux window.
func (o *Orchestrator) Close() {
	if o == nil || o.done == nil {
		return
	}
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closed = true
		for o.starting {
			o.cond.Wait()
		}
		o.mu.Unlock()
		close(o.done)
	})
}

func (o *Orchestrator) drain() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-o.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.done:
			o.mu.Lock()
			o.draining = false
			o.mu.Unlock()
			return
		case <-ticker.C:
		}
		startedOrBusy, err := o.tryStartNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				o.mu.Lock()
				o.draining = false
				o.mu.Unlock()
				return
			}
			// Keep the request queued and retry on the next tick.
			log.Printf("warn: orchestrator trigger drain retrying after error: %v", err)
			continue
		}
		if startedOrBusy {
			continue
		}
		o.mu.Lock()
		if len(o.queued) == 0 {
			o.draining = false
			o.mu.Unlock()
			return
		}
		o.mu.Unlock()
	}
}

func (o *Orchestrator) tryStartNext(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return false, context.Canceled
	}
	if len(o.queued) == 0 {
		o.mu.Unlock()
		return false, nil
	}
	reason := o.queued[0]
	o.mu.Unlock()

	active, err := o.isActive()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	o.mu.Lock()
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return false, context.Canceled
	}
	if active && !o.activeTimedOut() {
		return true, nil
	}
	if err := o.claimStart(); err != nil {
		return false, err
	}
	defer o.releaseStart()
	if active {
		if err := o.killTimedOutRun(); err != nil {
			return false, err
		}
	}
	started, err := o.start(ctx, reason)
	if err != nil {
		return false, err
	}
	if !started {
		return true, nil
	}
	o.mu.Lock()
	if len(o.queued) > 0 {
		o.queued = o.queued[1:]
	}
	o.mu.Unlock()
	return true, nil
}

func (o *Orchestrator) claimStart() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return context.Canceled
	}
	o.starting = true
	return nil
}

func (o *Orchestrator) releaseStart() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.starting = false
	o.cond.Broadcast()
}

func (o *Orchestrator) removeQueuedAtLocked(index int, reason string) {
	if index >= 0 && index < len(o.queued) && o.queued[index] == reason {
		o.removeQueuedIndexLocked(index)
		return
	}
	for i := len(o.queued) - 1; i >= 0; i-- {
		if o.queued[i] == reason {
			o.removeQueuedIndexLocked(i)
			return
		}
	}
}

func (o *Orchestrator) removeQueuedIndexLocked(index int) {
	if index < 0 || index >= len(o.queued) {
		return
	}
	copy(o.queued[index:], o.queued[index+1:])
	o.queued[len(o.queued)-1] = ""
	o.queued = o.queued[:len(o.queued)-1]
}

func (o *Orchestrator) isActive() (bool, error) {
	alive, err := o.tmux.IsWindowAlive(o.session, windowName)
	if err != nil {
		return false, fmt.Errorf("check orchestrator window: %w", err)
	}
	if !alive {
		o.clearRunStart()
		return false, nil
	}
	idle, err := o.tmux.IsPaneIdle(o.session, windowName)
	if err != nil {
		return false, fmt.Errorf("check orchestrator pane: %w", err)
	}
	if idle {
		o.clearRunStart()
		return false, nil
	}
	o.markRunObserved()
	return true, nil
}

func (o *Orchestrator) markRunObserved() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.runStart.IsZero() {
		o.runStart = time.Now()
	}
}

func (o *Orchestrator) markRunStarted() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runStart = time.Now()
}

func (o *Orchestrator) clearRunStart() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runStart = time.Time{}
}

func (o *Orchestrator) activeTimedOut() bool {
	timeout := o.cfg.Orchestrator.Timeout
	if timeout <= 0 {
		return false
	}
	o.mu.Lock()
	runStart := o.runStart
	o.mu.Unlock()
	return !runStart.IsZero() && time.Since(runStart) >= timeout
}

func (o *Orchestrator) killTimedOutRun() error {
	log.Printf("warn: orchestrator window exceeded timeout %s; restarting", o.cfg.Orchestrator.Timeout)
	if err := o.tmux.KillWindow(o.session, windowName); err != nil {
		return fmt.Errorf("kill timed-out orchestrator window: %w", err)
	}
	o.clearRunStart()
	return nil
}

func (o *Orchestrator) start(ctx context.Context, reason string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	hCfg, ok := o.cfg.Harnesses[o.cfg.Orchestrator.Harness]
	if !ok {
		return false, fmt.Errorf("orchestrator harness %q not configured", o.cfg.Orchestrator.Harness)
	}
	tokens, err := buildMCPTokens(hCfg.McpArgs, o.baseURL+"/mcp/orchestrator", o.cfg.Server.McpSecret)
	if err != nil {
		return false, fmt.Errorf("orchestrator mcp_args: %w", err)
	}
	prompt, err := o.buildPrompt(reason)
	if err != nil {
		return false, err
	}

	if err := o.tmux.EnsureSession(o.session); err != nil {
		return false, fmt.Errorf("ensure tmux session: %w", err)
	}
	alive, err := o.tmux.IsWindowAlive(o.session, windowName)
	if err != nil {
		return false, fmt.Errorf("check orchestrator window: %w", err)
	}
	if alive {
		idle, err := o.tmux.IsPaneIdle(o.session, windowName)
		if err != nil {
			return false, fmt.Errorf("check orchestrator pane: %w", err)
		}
		if !idle {
			return false, nil
		}
		if err := o.tmux.KillWindow(o.session, windowName); err != nil {
			return false, fmt.Errorf("reset idle orchestrator window: %w", err)
		}
	}
	if err := o.tmux.CreateWindow(o.session, windowName, o.cfg.Project.RepoPath); err != nil {
		return false, fmt.Errorf("create orchestrator window: %w", err)
	}
	if err := o.tmux.SendKeys(o.session, windowName, buildHarnessCommand(hCfg.Command, tokens)); err != nil {
		return false, fmt.Errorf("send orchestrator command: %w", err)
	}
	if err := o.tmux.SendKeys(o.session, windowName, prompt); err != nil {
		return false, fmt.Errorf("send orchestrator prompt: %w", err)
	}
	o.markRunStarted()
	return true, nil
}

func (o *Orchestrator) buildPrompt(reason string) (string, error) {
	tasks, err := o.ledger.Load()
	if err != nil {
		return "", fmt.Errorf("load ledger snapshot: %w", err)
	}
	taskJSON, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal ledger snapshot: %w", err)
	}
	harnessJSON, err := json.MarshalIndent(harness.List(o.cfg), "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal harness snapshot: %w", err)
	}

	var sb strings.Builder
	sb.WriteString("You are the Orchestrator agent for this repository.\n\n")
	if reason != "" {
		sb.WriteString("Trigger reason: " + reason + "\n\n")
	}
	sb.WriteString("Use the MCP tools at /mcp/orchestrator to inspect and mutate state. ")
	sb.WriteString("Decide which unstarted, blocked, or in-progress tasks need action. ")
	sb.WriteString("Spawn workers for actionable unstarted tasks, archive completed tasks, ")
	sb.WriteString("stop or follow up workers when appropriate, and create/split/update tasks only through MCP tools.\n\n")
	sb.WriteString("Project:\n")
	sb.WriteString("  slug: " + o.cfg.Project.Slug + "\n")
	sb.WriteString("  repo_path: " + o.cfg.Project.RepoPath + "\n")
	sb.WriteString("  validation_command: " + o.cfg.Project.ValidationCommand + "\n\n")
	sb.WriteString("Ledger snapshot:\n```json\n")
	sb.Write(taskJSON)
	sb.WriteString("\n```\n\nHarness snapshot:\n```json\n")
	sb.Write(harnessJSON)
	sb.WriteString("\n```\n")
	return sb.String(), nil
}

func (o *Orchestrator) validate() error {
	if o == nil {
		return fmt.Errorf("orchestrator is nil")
	}
	if o.ledger == nil {
		return fmt.Errorf("orchestrator ledger is nil")
	}
	if o.cfg == nil {
		return fmt.Errorf("orchestrator config is nil")
	}
	if o.tmux == nil {
		return fmt.Errorf("orchestrator tmux client is nil")
	}
	if strings.TrimSpace(o.session) == "" {
		return fmt.Errorf("orchestrator tmux session is required")
	}
	if strings.TrimSpace(o.baseURL) == "" {
		return fmt.Errorf("orchestrator base URL is required")
	}
	return nil
}

func buildMCPTokens(template, mcpURL, secret string) ([]string, error) {
	templateTokens, err := shellquote.Split(template)
	if err != nil {
		return nil, err
	}
	tokens := make([]string, len(templateTokens))
	replacer := strings.NewReplacer("{url}", mcpURL, "{secret}", secret)
	for i, tok := range templateTokens {
		tokens[i] = replacer.Replace(tok)
	}
	return tokens, nil
}

func buildHarnessCommand(command string, mcpTokens []string) string {
	parts := make([]string, 0, 1+len(mcpTokens))
	parts = append(parts, shellQuoteArg(command))
	for _, tok := range mcpTokens {
		parts = append(parts, shellQuoteArg(tok))
	}
	return strings.Join(parts, " ")
}

func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
