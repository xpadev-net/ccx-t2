package mcp

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInitializedNotificationWithoutIDReturnsAcceptedNoBody(t *testing.T) {
	s := NewServer("worker", "")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", body)
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rr.Body.String())
	}
}

func TestInitializedNotificationWithIDReturnsAcceptedNoBody(t *testing.T) {
	s := NewServer("worker", "")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", body)
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rr.Body.String())
	}
}

func TestServerRejectsWrongBearerSecret(t *testing.T) {
	s := NewServer("orchestrator", "orchestrator-secret")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/orchestrator", body)
	req.Header.Set("Authorization", "Bearer worker-secret")
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
}

func TestServerAcceptsConfiguredBearerSecret(t *testing.T) {
	s := NewServer("worker", "worker-secret")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", body)
	req.Header.Set("Authorization", "Bearer worker-secret")
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
}
