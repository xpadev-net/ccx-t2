package web

import (
	"context"
	"errors"
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

type SendKeysFunc func(ctx context.Context, session, window, keys string) error

type SessionAliveFunc func(ctx context.Context, session string) (bool, error)

type WindowAliveFunc func(ctx context.Context, session, window string) (bool, error)

type wsMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

type ledgerWSClient struct {
	conn *websocket.Conn
	send chan wsMessage
}

type tmuxStreamRegistry struct {
	mu      sync.Mutex
	streams map[string]struct{}
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
	aliveCtx, cancel := context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	sessionAlive, err := s.isSessionAlive(aliveCtx, session)
	cancel()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check tmux session")
		return
	}
	if !sessionAlive {
		writeError(w, http.StatusNotFound, "tmux session not found")
		return
	}
	aliveCtx, cancel = context.WithTimeout(r.Context(), followupTmuxOperationTimeout)
	windowAlive, err := s.isWindowAlive(aliveCtx, session, window)
	cancel()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "check orchestrator tmux window")
		return
	}
	if !windowAlive {
		writeError(w, http.StatusNotFound, "orchestrator tmux window not found")
		return
	}
	s.handleTmuxLogWS(w, r, window, "orchestrator")
}

func (s *Server) handleTmuxLogWS(w http.ResponseWriter, r *http.Request, window, label string) {
	session, err := s.tmuxSessionName()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	streamKey := session + "\x00" + window
	if !s.reserveTmuxStream(streamKey) {
		writeError(w, http.StatusConflict, label+" log stream already open")
		return
	}
	defer s.releaseTmuxStream(streamKey)
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	lines, cleanup, err := s.pipeOutput(session, window)
	if err != nil {
		_ = writeWSJSON(conn, wsMessage{Type: "error", Data: "open " + label + " log stream"})
		return
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go discardWSReads(ctx, conn, cancel)
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-lines:
			if !ok {
				_ = writeWSJSON(conn, wsMessage{Type: "closed"})
				return
			}
			if err := writeWSJSON(conn, wsMessage{Type: "line", Data: line}); err != nil {
				return
			}
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

func (s *Server) reserveTmuxStream(key string) bool {
	streams := s.tmuxStreams
	if streams == nil {
		return true
	}
	streams.mu.Lock()
	defer streams.mu.Unlock()
	if streams.streams == nil {
		streams.streams = make(map[string]struct{})
	}
	if _, ok := streams.streams[key]; ok {
		return false
	}
	streams.streams[key] = struct{}{}
	return true
}

func (s *Server) releaseTmuxStream(key string) {
	streams := s.tmuxStreams
	if streams == nil {
		return
	}
	streams.mu.Lock()
	defer streams.mu.Unlock()
	delete(streams.streams, key)
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
