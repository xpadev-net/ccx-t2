package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PipeOutputFunc streams tmux pane output as lines and returns a cleanup
// function that stops the stream.
type PipeOutputFunc func(session, window string) (<-chan string, func(), error)

type SendKeysFunc func(ctx context.Context, session, window, keys string) error

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
	s.handleTmuxLogWS(w, r, window, "worker")
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
	cfg, ok := s.configSnapshot()
	if !ok {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	window := "orchestrator"
	if s.projectScoped && cfg.Project.Slug != "" {
		window = cfg.Project.Slug + "-orchestrator"
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
	if _, err := s.projectServer(parts[0]); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	switch parts[1] {
	case "ledger":
		if len(parts) == 2 {
			s.handleLedgerWS(w, r)
			return
		}
	case "worker":
		if len(parts) == 3 {
			projectServer, err := s.projectServer(parts[0])
			if err != nil {
				writeError(w, http.StatusNotFound, "project not found")
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/ws/worker/" + parts[2]
			projectServer.handleWorkerLogWS(w, r2)
			return
		}
	case "orchestrator":
		if len(parts) == 2 {
			projectServer, err := s.projectServer(parts[0])
			if err != nil {
				writeError(w, http.StatusNotFound, "project not found")
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/ws/orchestrator"
			projectServer.handleOrchestratorLogWS(w, r2)
			return
		}
	}
	writeError(w, http.StatusNotFound, "project websocket route not found")
}

func (s *Server) handleLedgerWS(w http.ResponseWriter, r *http.Request) {
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
	s.addLedgerClient(client)
	defer s.removeLedgerClient(client)

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
	return s.allowedOrigins[origin]
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

func (s *Server) addLedgerClient(client *ledgerWSClient) {
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	if s.ledgerClients == nil {
		s.ledgerClients = make(map[*ledgerWSClient]struct{})
	}
	s.ledgerClients[client] = struct{}{}
}

func (s *Server) removeLedgerClient(client *ledgerWSClient) {
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	if _, ok := s.ledgerClients[client]; ok {
		delete(s.ledgerClients, client)
		close(client.send)
	}
}

func (s *Server) broadcastLedgerChange() {
	msg := wsMessage{Type: "ledger_changed"}
	s.ledgerClientsMu.Lock()
	defer s.ledgerClientsMu.Unlock()
	for client := range s.ledgerClients {
		select {
		case client.send <- msg:
		default:
			log.Printf("web: dropping slow ledger websocket client")
		}
	}
}
