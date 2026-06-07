package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// PipeOutputFunc streams tmux pane output as lines and returns a cleanup
// function that stops the stream.
type PipeOutputFunc func(session, window string) (<-chan string, func(), error)

type wsMessage struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
}

type ledgerWSClient struct {
	conn *websocket.Conn
	send chan wsMessage
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
	cfg, ok := s.configSnapshot()
	if !ok {
		writeError(w, http.StatusInternalServerError, "config is not configured")
		return
	}
	if cfg.Project.Slug == "" {
		writeError(w, http.StatusInternalServerError, "tmux session is not configured")
		return
	}
	if !s.reserveWorkerStream(window) {
		writeError(w, http.StatusConflict, "worker log stream already open")
		return
	}
	defer s.releaseWorkerStream(window)
	conn, err := s.upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer conn.Close()
	lines, cleanup, err := s.pipeOutput(cfg.Project.Slug, window)
	if err != nil {
		_ = writeWSJSON(conn, wsMessage{Type: "error", Data: "open worker log stream"})
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

func (s *Server) reserveWorkerStream(window string) bool {
	s.workerStreamsMu.Lock()
	defer s.workerStreamsMu.Unlock()
	if s.workerStreams == nil {
		s.workerStreams = make(map[string]struct{})
	}
	if _, ok := s.workerStreams[window]; ok {
		return false
	}
	s.workerStreams[window] = struct{}{}
	return true
}

func (s *Server) releaseWorkerStream(window string) {
	s.workerStreamsMu.Lock()
	defer s.workerStreamsMu.Unlock()
	delete(s.workerStreams, window)
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
