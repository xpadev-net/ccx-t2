package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PipeOutputFunc streams tmux pane output as lines and returns a cleanup
// function that stops the stream.
type PipeOutputFunc func(session, window string) (<-chan string, func(), error)

// PipeBytesFunc streams tmux pane output as raw bytes and returns a cleanup
// function that stops the stream.
type PipeBytesFunc func(session, window string) (<-chan []byte, func(), error)

type CapturePaneFunc func(ctx context.Context, session, window string) ([]byte, error)

type SendKeysFunc func(ctx context.Context, session, window, keys string) error

type SendRawKeysFunc func(ctx context.Context, session, window, keys string) error

type SessionAliveFunc func(ctx context.Context, session string) (bool, error)

type WindowAliveFunc func(ctx context.Context, session, window string) (bool, error)

type PaneIdleFunc func(ctx context.Context, session, window string) (bool, error)

type ResizePaneFunc func(ctx context.Context, session, window string, cols, rows int) error

type wsMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

func pipeLinesAsBytes(pipe PipeOutputFunc) PipeBytesFunc {
	return func(session, window string) (<-chan []byte, func(), error) {
		lines, cleanup, err := pipe(session, window)
		if err != nil {
			return nil, nil, err
		}
		chunks := make(chan []byte, 128)
		go func() {
			defer close(chunks)
			for line := range lines {
				chunks <- []byte(line)
			}
		}()
		return chunks, cleanup, nil
	}
}

type ledgerWSClient struct {
	conn *websocket.Conn
	send chan wsMessage
}

type tmuxStreamRegistry struct {
	mu      sync.Mutex
	streams map[string]*tmuxSharedStream
}

type tmuxSharedStream struct {
	key         string
	cleanup     func()
	cleanupOnce sync.Once
	subscribers map[chan []byte]struct{}
	closed      bool
}

type orchestratorStartLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

const wsWriteTimeout = 5 * time.Second

func (s *Server) handleWorkerLogWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	window := strings.TrimPrefix(r.URL.Path, "/ws/worker/")
	window = strings.TrimSpace(window)
	if window == "" || strings.Contains(window, "/") {
		writeError(w, http.StatusBadRequest, "worker window is required")
		return
	}
	projectServer, ok, err := s.selectedProjectWorkerLogServer()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "selected project is not configured")
		return
	}
	if ok {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/ws/worker/" + window
		projectServer.handleWorkerLogWS(w, r2)
		return
	}
	status, message := s.authorizeWorkerLogWindow(window)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	s.handleTmuxLogWS(w, r, window, "worker")
}

func (s *Server) selectedProjectWorkerLogServer() (*Server, bool, error) {
	if s.projectScoped || s.manager == nil {
		return nil, false, nil
	}
	cfg, ok := s.configSnapshot()
	if !ok {
		return nil, false, nil
	}
	slug := strings.TrimSpace(cfg.Project.Slug)
	if slug == "" {
		return nil, false, nil
	}
	projectServer, err := s.projectServer(slug)
	if err != nil {
		return nil, false, err
	}
	return projectServer, true, nil
}

func (s *Server) authorizeWorkerLogWindow(window string) (int, string) {
	if ok, err := s.hasActiveLedgerWorker(window); err != nil {
		return http.StatusInternalServerError, "load worker task"
	} else if ok {
		if s.projectScoped && !s.hasProjectWorkerPrefix(window) {
			return http.StatusForbidden, "worker window is outside project"
		}
		return 0, ""
	}
	if s.projectScoped && !s.hasProjectWorkerPrefix(window) {
		return http.StatusForbidden, "worker window is outside project"
	}
	if s.registry != nil {
		if _, ok := s.registry.Get(window); ok {
			return 0, ""
		}
	}
	return http.StatusNotFound, "worker not found"
}

func (s *Server) hasActiveLedgerWorker(workerID string) (bool, error) {
	if s.ledger == nil {
		return false, nil
	}
	if _, err := s.activeTaskForWorker(workerID); err != nil {
		if errors.Is(err, errWorkerNotActive) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Server) hasProjectWorkerPrefix(window string) bool {
	prefix := s.projectWorkerPrefix()
	return prefix != "" && strings.HasPrefix(window, prefix)
}

func (s *Server) projectWorkerPrefix() string {
	cfg, ok := s.configSnapshot()
	if !ok {
		return ""
	}
	slug := strings.TrimSpace(cfg.Project.Slug)
	if slug == "" {
		return ""
	}
	return slug + "-"
}

func (s *Server) handleOrchestratorLogWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if r.URL.Path != "/ws/orchestrator" {
		writeError(w, http.StatusNotFound, "orchestrator websocket route not found")
		return
	}
	window, err := s.orchestratorWindowName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	session, err := s.tmuxSessionName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.ensureOrchestratorAttached(r.Context(), session, window); err != nil {
		writeError(w, orchestratorAttachStatus(err), err.Error())
		return
	}
	s.handleTmuxLogWS(w, r, window, "orchestrator")
}

var (
	errOrchestratorMissingTrigger = errors.New("orchestrator trigger is not configured")
	errOrchestratorAttachTimeout  = errors.New("orchestrator tmux pane did not become active")
)

func orchestratorAttachStatus(err error) int {
	switch {
	case errors.Is(err, errOrchestratorMissingTrigger):
		return http.StatusNotFound
	case errors.Is(err, errOrchestratorAttachTimeout), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) ensureOrchestratorAttached(ctx context.Context, session, window string) error {
	needsStart, err := s.orchestratorNeedsStart(ctx, session, window)
	if err != nil {
		return err
	}
	if !needsStart {
		return nil
	}
	unlock := s.lockOrchestratorStart(session, window)
	defer unlock()
	needsStart, err = s.orchestratorNeedsStart(ctx, session, window)
	if err != nil {
		return err
	}
	if !needsStart {
		return nil
	}
	if s.trigger == nil {
		return errOrchestratorMissingTrigger
	}
	triggerCtx, cancel := context.WithTimeout(ctx, orchestratorStartTimeout)
	err = s.trigger.Trigger(triggerCtx, "browser orchestrator web shell opened")
	cancel()
	if err != nil {
		return fmt.Errorf("start orchestrator for web shell: %w", err)
	}
	return s.waitForOrchestratorWindow(ctx, session, window)
}

func (s *Server) lockOrchestratorStart(session, window string) func() {
	if s.startLocks == nil {
		s.startLocks = &orchestratorStartLocks{}
	}
	return s.startLocks.lock(session + "\x00" + window)
}

func (l *orchestratorStartLocks) lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	lock := l.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[key] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (s *Server) orchestratorNeedsStart(ctx context.Context, session, window string) (bool, error) {
	aliveCtx, cancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
	sessionAlive, err := s.isSessionAlive(aliveCtx, session)
	cancel()
	if err != nil {
		return false, fmt.Errorf("check tmux session: %w", err)
	}
	if !sessionAlive {
		return true, nil
	}
	aliveCtx, cancel = context.WithTimeout(ctx, followupTmuxOperationTimeout)
	windowAlive, err := s.isWindowAlive(aliveCtx, session, window)
	cancel()
	if err != nil {
		return false, fmt.Errorf("check orchestrator tmux window: %w", err)
	}
	if !windowAlive {
		return true, nil
	}
	if s.isPaneIdle == nil {
		return false, nil
	}
	idleCtx, cancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
	idle, err := s.isPaneIdle(idleCtx, session, window)
	cancel()
	if err != nil {
		return false, fmt.Errorf("check orchestrator tmux pane: %w", err)
	}
	return idle, nil
}

func (s *Server) waitForOrchestratorWindow(ctx context.Context, session, window string) error {
	deadline := time.NewTimer(orchestratorStartTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
		alive, err := s.isWindowAlive(checkCtx, session, window)
		idle := false
		if err == nil && alive && s.isPaneIdle != nil {
			idle, err = s.isPaneIdle(checkCtx, session, window)
		}
		cancel()
		if err != nil {
			return fmt.Errorf("check started orchestrator tmux pane: %w", err)
		}
		if alive && !idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errOrchestratorAttachTimeout
		case <-ticker.C:
		}
	}
}

func (s *Server) handleTmuxLogWS(w http.ResponseWriter, r *http.Request, window, label string) {
	session, err := s.tmuxSessionName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	if s.capturePane != nil {
		captureCtx, captureCancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
		snapshot, err := s.capturePane(captureCtx, session, window)
		captureCancel()
		if err != nil {
			_ = writeWSJSON(conn, wsMessage{Type: "error", Data: "capture " + label + " pane"})
			return
		}
		if len(snapshot) > 0 {
			if err := writeWSJSON(conn, wsMessage{Type: "chunk", Data: string(snapshot)}); err != nil {
				return
			}
		}
	}
	chunks, cleanup, err := s.subscribeTmuxStream(session, window)
	if err != nil {
		_ = writeWSJSON(conn, wsMessage{Type: "error", Data: "open " + label + " log stream"})
		return
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go handleTmuxClientMessages(ctx, conn, session, window, s.sendRawKeys, s.resizePane, cancel)
	for {
		select {
		case <-ctx.Done():
			return
		case chunk, ok := <-chunks:
			if !ok {
				_ = writeWSJSON(conn, wsMessage{Type: "closed"})
				return
			}
			if err := writeWSJSON(conn, wsMessage{Type: "chunk", Data: string(chunk)}); err != nil {
				return
			}
		}
	}
}

func handleTmuxClientMessages(ctx context.Context, conn *websocket.Conn, session, window string, sendRawKeys SendRawKeysFunc, resizePane ResizePaneFunc, cancel context.CancelFunc) {
	defer cancel()
	if sendRawKeys == nil && resizePane == nil {
		discardWSReads(ctx, conn, cancel)
		return
	}
	conn.SetReadLimit(maxFollowupMessageBytes)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) {
				_ = conn.Close()
			}
			return
		}
		switch msg.Type {
		case "input":
			if msg.Data == "" || sendRawKeys == nil {
				continue
			}
			sendCtx, sendCancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
			err := sendRawKeys(sendCtx, session, window, msg.Data)
			sendCancel()
			if err != nil {
				_ = conn.Close()
				return
			}
		case "resize":
			if resizePane == nil || msg.Cols <= 0 || msg.Rows <= 0 {
				continue
			}
			resizeCtx, resizeCancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
			err := resizePane(resizeCtx, session, window, msg.Cols, msg.Rows)
			resizeCancel()
			if err != nil {
				_ = conn.Close()
				return
			}
		default:
			continue
		}
	}
}

func (s *Server) handleProjectWS(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ws/projects/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "project websocket route not found")
		return
	}
	projectServer, err := s.projectServer(parts[0])
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	switch parts[1] {
	case "ledger":
		if len(parts) == 2 {
			if s.manager == nil {
				s.handleLedgerWS(w, r)
			} else {
				s.handleLedgerWSForScope(w, r, parts[0])
			}
			return
		}
	case "worker":
		if len(parts) == 3 {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/ws/worker/" + parts[2]
			projectServer.handleWorkerLogWS(w, r2)
			return
		}
	case "orchestrator":
		if len(parts) == 2 {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/ws/orchestrator"
			projectServer.handleOrchestratorLogWS(w, r2)
			return
		}
	}
	writeError(w, http.StatusNotFound, "project websocket route not found")
}

func (s *Server) handleLedgerWS(w http.ResponseWriter, r *http.Request) {
	s.handleLedgerWSForScope(w, r, "")
}

func (s *Server) handleLedgerWSForScope(w http.ResponseWriter, r *http.Request, scope string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	client := &ledgerWSClient{conn: conn, send: make(chan wsMessage, 16)}
	s.addLedgerClient(scope, client)
	defer s.removeLedgerClient(scope, client)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go discardWSReads(ctx, conn, cancel)

	if err := writeWSJSON(conn, wsMessage{Type: "ready"}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-client.send:
			if !ok {
				return
			}
			if err := writeWSJSON(conn, msg); err != nil {
				return
			}
		}
	}
}

func (s *Server) upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return s.isAllowedWSOrigin(r)
		},
	}
	return upgrader.Upgrade(w, r, nil)
}

func (s *Server) isAllowedWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if s.allowedOrigins[origin] {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" {
		return false
	}
	return strings.EqualFold(originURL.Scheme, requestScheme(r)) &&
		strings.EqualFold(originURL.Host, r.Host)
}

func requestScheme(r *http.Request) string {
	if proto := forwardedProto(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func forwardedProto(header string) string {
	proto, _, _ := strings.Cut(header, ",")
	proto = strings.TrimSpace(proto)
	if strings.EqualFold(proto, "http") {
		return "http"
	}
	if strings.EqualFold(proto, "https") {
		return "https"
	}
	return ""
}

func writeWSJSON(conn *websocket.Conn, msg wsMessage) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteJSON(msg)
}

func discardWSReads(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, _, err := conn.NextReader(); err != nil {
			if !errors.Is(err, websocket.ErrCloseSent) {
				_ = conn.Close()
			}
			return
		}
	}
}

func (s *Server) subscribeTmuxStream(session, window string) (<-chan []byte, func(), error) {
	if s.tmuxStreams == nil {
		return s.pipeBytes(session, window)
	}
	return s.tmuxStreams.subscribe(session+"\x00"+window, session, window, s.pipeBytes)
}

func (r *tmuxStreamRegistry) subscribe(key, session, window string, pipe PipeBytesFunc) (<-chan []byte, func(), error) {
	if pipe == nil {
		return nil, nil, errors.New("tmux pipe is not configured")
	}
	r.mu.Lock()
	if r.streams == nil {
		r.streams = make(map[string]*tmuxSharedStream)
	}
	stream := r.streams[key]
	if stream == nil || stream.closed {
		chunks, cleanup, err := pipe(session, window)
		if err != nil {
			r.mu.Unlock()
			return nil, nil, err
		}
		stream = &tmuxSharedStream{
			key:         key,
			cleanup:     cleanup,
			subscribers: make(map[chan []byte]struct{}),
		}
		r.streams[key] = stream
		go r.runStream(stream, chunks)
	}
	sub := make(chan []byte, 128)
	stream.subscribers[sub] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			current := r.streams[key]
			if current != stream || stream.closed {
				return
			}
			delete(stream.subscribers, sub)
			if len(stream.subscribers) == 0 {
				stream.closed = true
				delete(r.streams, key)
				stream.closePipe()
			}
		})
	}
	return sub, cleanup, nil
}

func (r *tmuxStreamRegistry) runStream(stream *tmuxSharedStream, chunks <-chan []byte) {
	defer stream.closePipe()
	for chunk := range chunks {
		r.mu.Lock()
		subs := make([]chan []byte, 0, len(stream.subscribers))
		for sub := range stream.subscribers {
			subs = append(subs, sub)
		}
		r.mu.Unlock()
		for _, sub := range subs {
			select {
			case sub <- chunk:
			default:
				// Keep the shared tmux pipe live even if one browser falls behind.
			}
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.streams[stream.key]; current == stream {
		delete(r.streams, stream.key)
	}
	stream.closed = true
	for sub := range stream.subscribers {
		close(sub)
	}
	stream.subscribers = nil
}

func (s *tmuxSharedStream) closePipe() {
	if s.cleanup == nil {
		return
	}
	s.cleanupOnce.Do(s.cleanup)
}

func (s *Server) addLedgerClient(scope string, client *ledgerWSClient) {
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	if s.ledgerClients == nil {
		s.ledgerClients = make(map[string]map[*ledgerWSClient]struct{})
	}
	clients := s.ledgerClients[scope]
	if clients == nil {
		clients = make(map[*ledgerWSClient]struct{})
		s.ledgerClients[scope] = clients
	}
	clients[client] = struct{}{}
}

func (s *Server) removeLedgerClient(scope string, client *ledgerWSClient) {
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	clients := s.ledgerClients[scope]
	if _, ok := clients[client]; ok {
		delete(clients, client)
		if len(clients) == 0 {
			delete(s.ledgerClients, scope)
		}
		close(client.send)
	}
}

func (s *Server) broadcastLedgerChange() {
	s.broadcastLedgerChangeForScope("")
}

func (s *Server) broadcastLedgerChangeForScope(scope string) {
	msg := wsMessage{Type: "ledger_changed"}
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	for client := range s.ledgerClients[scope] {
		select {
		case client.send <- msg:
		default:
			log.Printf("web: dropping slow ledger websocket client")
		}
	}
}
