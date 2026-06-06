package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInitializedNotificationWithoutIDReturnsJSONRPCResponse(t *testing.T) {
	s := NewServer("worker", "")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", body)
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rr.Body.String())
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %#v, want 2.0", resp["jsonrpc"])
	}
	if _, ok := resp["id"]; !ok {
		t.Fatalf("response missing id field: %#v", resp)
	}
	if _, ok := resp["result"]; !ok {
		t.Fatalf("response missing result: %#v", resp)
	}
}

func TestInitializedRequestWithIDReturnsJSONRPCResponse(t *testing.T) {
	s := NewServer("worker", "")
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"notifications/initialized"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp/worker", body)
	rr := httptest.NewRecorder()

	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rr.Body.String())
	}
	if resp["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %#v, want 2.0", resp["jsonrpc"])
	}
	if resp["id"].(float64) != 1 {
		t.Fatalf("id = %#v, want 1", resp["id"])
	}
	if _, ok := resp["result"]; !ok {
		t.Fatalf("response missing result: %#v", resp)
	}
}
