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

// PaneAttachment is the web-layer form of an atomic tmux pane handoff.
// Snapshot ends at the tmux watermark; Chunks contains only subsequent output.
type PaneAttachment struct {
	Snapshot []byte
	Chunks   <-chan []byte
	Cleanup  func()
}

type AttachPaneFunc func(ctx context.Context, session, window string) (*PaneAttachment, error)

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
		stop := make(chan struct{})
		var cleanupOnce sync.Once
		go func() {
			defer close(chunks)
			for line := range lines {
				select {
				case chunks <- []byte(line):
				case <-stop:
					return
				}
			}
		}()
		return chunks, func() {
			cleanupOnce.Do(func() {
				close(stop)
				if cleanup != nil {
					cleanup()
				}
			})
		}, nil
	}
}

func newWSWriter(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) *wsWriter {
	return newWSWriterWithPingPeriod(ctx, conn, cancel, wsPingPeriod)
}

func newWSWriterWithPingPeriod(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc, pingPeriod time.Duration) *wsWriter {
	writer := &wsWriter{
		conn:       conn,
		ctx:        ctx,
		cancel:     cancel,
		messages:   make(chan wsMessage, wsWriterBuffer),
		pingPeriod: pingPeriod,
		done:       make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (w *wsWriter) run() {
	defer close(w.done)

	var ping <-chan time.Time
	var ticker *time.Ticker
	if w.pingPeriod > 0 {
		ticker = time.NewTicker(w.pingPeriod)
		defer ticker.Stop()
		ping = ticker.C
	}

	for {
		select {
		case <-w.ctx.Done():
			return
		case msg, ok := <-w.messages:
			if !ok {
				return
			}
			if err := writeWSJSON(w.conn, msg); err != nil {
				if w.cancel != nil {
					w.cancel()
				}
				return
			}
		case <-ping:
			if err := writeWSPing(w.conn); err != nil {
				if w.cancel != nil {
					w.cancel()
				}
				return
			}
		}
	}
}

func (w *wsWriter) send(ctx context.Context, msg wsMessage) error {
	select {
	case <-w.done:
		return errWSWriterClosed
	case <-ctx.Done():
		return ctx.Err()
	case w.messages <- msg:
		return nil
	}
}

func (w *wsWriter) close() {
	w.closeOnce.Do(func() {
		close(w.messages)
	})
	<-w.done
}

type ledgerWSClient struct {
	conn *websocket.Conn
	send chan wsMessage
}

type wsWriter struct {
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	messages   chan wsMessage
	pingPeriod time.Duration
	done       chan struct{}
	closeOnce  sync.Once
}

var errWSWriterClosed = errors.New("websocket writer is closed")

type tmuxStreamRegistry struct {
	mu             sync.Mutex
	streams        map[string]*tmuxSharedStream
	attachmentLock map[string]*sync.Mutex
}

type tmuxSharedStream struct {
	key         string
	cleanup     func()
	cleanupOnce sync.Once
	subscribers map[*tmuxSubscriber]struct{}
	snapshot    []byte
	closed      bool
}

type tmuxSubscriber struct {
	mu          sync.Mutex
	chunks      chan []byte
	slow        chan struct{}
	closed      bool
	slowEvicted bool
}

type tmuxStreamSubscription struct {
	chunks <-chan []byte
	slow   <-chan struct{}
}

func (s *tmuxSubscriber) deliver(chunk []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.chunks <- chunk:
		return false
	default:
		s.closed = true
		s.slowEvicted = true
		close(s.slow)
		close(s.chunks)
		return true
	}
}

func (s *tmuxSubscriber) closeNormally() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.chunks)
}

func (s *tmuxStreamSubscription) wasSlowEvicted() bool {
	select {
	case <-s.slow:
		return true
	default:
		return false
	}
}

func (r *tmuxStreamRegistry) lockAttachment(key string) func() {
	r.mu.Lock()
	if r.attachmentLock == nil {
		r.attachmentLock = make(map[string]*sync.Mutex)
	}
	lock := r.attachmentLock[key]
	if lock == nil {
		lock = &sync.Mutex{}
		r.attachmentLock[key] = lock
	}
	r.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (r *tmuxStreamRegistry) addSubscriberLocked(key string, stream *tmuxSharedStream) (*tmuxStreamSubscription, []byte, func()) {
	sub := &tmuxSubscriber{chunks: make(chan []byte, 128), slow: make(chan struct{})}
	stream.subscribers[sub] = struct{}{}
	snapshot := append([]byte(nil), stream.snapshot...)
	var once sync.Once
	cleanup := func() {
		var closePipe bool
		once.Do(func() {
			r.mu.Lock()
			current := r.streams[key]
			if current == stream && !stream.closed {
				if _, ok := stream.subscribers[sub]; ok {
					delete(stream.subscribers, sub)
					sub.closeNormally()
				}
				if len(stream.subscribers) == 0 {
					stream.closed = true
					delete(r.streams, key)
					closePipe = true
				}
			}
			r.mu.Unlock()
		})
		if closePipe {
			stream.closePipe()
		}
	}
	return &tmuxStreamSubscription{chunks: sub.chunks, slow: sub.slow}, snapshot, cleanup
}

func (r *tmuxStreamRegistry) subscribeAttachedWithStatus(ctx context.Context, key, session, window string, attach AttachPaneFunc) ([]byte, *tmuxStreamSubscription, func(), error) {
	if attach == nil {
		return nil, nil, nil, errors.New("tmux pane attachment is not configured")
	}
	unlockAttachment := r.lockAttachment(key)
	defer unlockAttachment()

	r.mu.Lock()
	if r.streams == nil {
		r.streams = make(map[string]*tmuxSharedStream)
	}
	stream := r.streams[key]
	if stream != nil && !stream.closed {
		subscription, snapshot, cleanup := r.addSubscriberLocked(key, stream)
		r.mu.Unlock()
		return snapshot, subscription, cleanup, nil
	}
	r.mu.Unlock()

	// Do not hold the registry mutex across tmux control-mode I/O. The
	// per-key lock above still prevents duplicate attachments for this pane,
	// while unrelated panes can attach concurrently.
	attachment, err := attach(ctx, session, window)
	if err != nil {
		return nil, nil, nil, err
	}
	if attachment == nil || attachment.Chunks == nil {
		if attachment != nil && attachment.Cleanup != nil {
			attachment.Cleanup()
		}
		return nil, nil, nil, errors.New("tmux pane attachment returned no stream")
	}

	r.mu.Lock()
	if stream = r.streams[key]; stream != nil && !stream.closed {
		subscription, snapshot, cleanup := r.addSubscriberLocked(key, stream)
		r.mu.Unlock()
		if attachment.Cleanup != nil {
			attachment.Cleanup()
		}
		return snapshot, subscription, cleanup, nil
	}
	stream = &tmuxSharedStream{
		key:         key,
		cleanup:     attachment.Cleanup,
		snapshot:    append([]byte(nil), attachment.Snapshot...),
		subscribers: make(map[*tmuxSubscriber]struct{}),
	}
	r.streams[key] = stream
	subscription, snapshot, cleanup := r.addSubscriberLocked(key, stream)
	r.mu.Unlock()
	go r.runStream(stream, attachment.Chunks)
	return snapshot, subscription, cleanup, nil
}

type orchestratorStartLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

const (
	wsWriteTimeout = 5 * time.Second
	wsPongWait     = 60 * time.Second
	wsPingPeriod   = (wsPongWait * 9) / 10
	wsWriterBuffer = 128
)

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
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	writer := newWSWriter(ctx, conn, cancel)
	defer writer.close()

	var snapshot []byte
	var subscription *tmuxStreamSubscription
	var cleanup func()
	if s.attachPane != nil {
		if s.tmuxStreams == nil {
			s.tmuxStreams = &tmuxStreamRegistry{}
		}
		snapshot, subscription, cleanup, err = s.tmuxStreams.subscribeAttachedWithStatus(ctx, session+"\x00"+window, session, window, s.attachPane)
	} else {
		if s.capturePane != nil {
			captureCtx, captureCancel := context.WithTimeout(ctx, followupTmuxOperationTimeout)
			snapshot, err = s.capturePane(captureCtx, session, window)
			captureCancel()
			if err != nil {
				_ = writer.send(ctx, wsMessage{Type: "error", Data: "capture " + label + " pane"})
				return
			}
		}
		subscription, cleanup, err = s.subscribeTmuxStreamWithStatus(session, window)
	}
	if err != nil {
		_ = writer.send(ctx, wsMessage{Type: "error", Data: "open " + label + " log stream"})
		return
	}
	defer cleanup()
	go handleTmuxClientMessages(ctx, conn, session, window, s.sendRawKeys, s.resizePane, cancel)

	if len(snapshot) > 0 {
		if err := writer.send(ctx, wsMessage{Type: "chunk", Data: string(snapshot)}); err != nil {
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-subscription.slow:
			// The shared pipe has advanced beyond this subscriber's bounded
			// buffer. Send an explicit resync signal when possible, then close
			// instead of continuing with a corrupted byte stream.
			_ = writer.send(ctx, wsMessage{Type: "error", Data: "log stream fell behind; reconnecting"})
			return
		case chunk, ok := <-subscription.chunks:
			if !ok {
				if subscription.wasSlowEvicted() {
					_ = writer.send(ctx, wsMessage{Type: "error", Data: "log stream fell behind; reconnecting"})
				} else {
					_ = writer.send(ctx, wsMessage{Type: "closed"})
				}
				return
			}
			if err := writer.send(ctx, wsMessage{Type: "chunk", Data: string(chunk)}); err != nil {
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
	if err := configureWSReader(conn, maxFollowupMessageBytes); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
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
	writer := newWSWriter(ctx, conn, cancel)
	defer writer.close()
	go discardWSReads(ctx, conn, cancel)

	if err := writer.send(ctx, wsMessage{Type: "ready"}); err != nil {
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
			if err := writer.send(ctx, msg); err != nil {
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

func writeWSPing(conn *websocket.Conn) error {
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.PingMessage, nil)
}

func configureWSReader(conn *websocket.Conn, readLimit int64) error {
	conn.SetReadLimit(readLimit)
	return configureWSReaderWithWait(conn, wsPongWait)
}

func configureWSReaderWithWait(conn *websocket.Conn, pongWait time.Duration) error {
	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return err
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	return nil
}

func discardWSReads(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	defer cancel()
	if err := configureWSReader(conn, maxFollowupMessageBytes); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

func (s *Server) subscribeTmuxStreamWithStatus(session, window string) (*tmuxStreamSubscription, func(), error) {
	if s.tmuxStreams == nil {
		chunks, cleanup, err := s.pipeBytes(session, window)
		if err != nil {
			return nil, nil, err
		}
		return &tmuxStreamSubscription{chunks: chunks}, cleanup, nil
	}
	return s.tmuxStreams.subscribeWithStatus(session+"\x00"+window, session, window, s.pipeBytes)
}

func (r *tmuxStreamRegistry) subscribeWithStatus(key, session, window string, pipe PipeBytesFunc) (*tmuxStreamSubscription, func(), error) {
	if pipe == nil {
		return nil, nil, errors.New("tmux pipe is not configured")
	}
	r.mu.Lock()
	if r.streams == nil {
		r.streams = make(map[string]*tmuxSharedStream)
	}
	stream := r.streams[key]
	var streamChunks <-chan []byte
	startStream := false
	if stream == nil || stream.closed {
		chunks, cleanup, err := pipe(session, window)
		if err != nil {
			r.mu.Unlock()
			return nil, nil, err
		}
		stream = &tmuxSharedStream{
			key:         key,
			cleanup:     cleanup,
			subscribers: make(map[*tmuxSubscriber]struct{}),
		}
		r.streams[key] = stream
		streamChunks = chunks
		startStream = true
	}
	sub := &tmuxSubscriber{
		chunks: make(chan []byte, 128),
		slow:   make(chan struct{}),
	}
	stream.subscribers[sub] = struct{}{}
	r.mu.Unlock()
	if startStream {
		go r.runStream(stream, streamChunks)
	}

	var once sync.Once
	cleanup := func() {
		var closePipe bool
		once.Do(func() {
			r.mu.Lock()
			current := r.streams[key]
			if current == stream && !stream.closed {
				if _, ok := stream.subscribers[sub]; ok {
					delete(stream.subscribers, sub)
					sub.closeNormally()
				}
				if len(stream.subscribers) == 0 {
					stream.closed = true
					delete(r.streams, key)
					closePipe = true
				}
			}
			r.mu.Unlock()
		})
		if closePipe {
			stream.closePipe()
		}
	}
	return &tmuxStreamSubscription{chunks: sub.chunks, slow: sub.slow}, cleanup, nil
}

func (r *tmuxStreamRegistry) runStream(stream *tmuxSharedStream, chunks <-chan []byte) {
	defer stream.closePipe()
	for chunk := range chunks {
		closePipe := false
		r.mu.Lock()
		if stream.closed {
			r.mu.Unlock()
			return
		}
		for sub := range stream.subscribers {
			if sub.deliver(chunk) {
				// Do not drop a terminal chunk and continue a corrupted stream.
				// Remove only this subscriber so healthy clients keep receiving
				// the shared pipe and the handler can request a fresh stream.
				delete(stream.subscribers, sub)
				if len(stream.subscribers) == 0 {
					stream.closed = true
					delete(r.streams, stream.key)
					closePipe = true
				}
			}
			if closePipe {
				break
			}
		}
		r.mu.Unlock()
		if closePipe {
			stream.closePipe()
			return
		}
	}

	r.mu.Lock()
	if current := r.streams[stream.key]; current == stream {
		delete(r.streams, stream.key)
		stream.closed = true
		for sub := range stream.subscribers {
			sub.closeNormally()
		}
		stream.subscribers = nil
	}
	r.mu.Unlock()
	// The source ending is a terminal lifecycle event, not just a subscriber
	// notification: release the tmux attachment even when clients stayed open.
	stream.closePipe()
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
