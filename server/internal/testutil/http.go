package testutil

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// LocalOnlyHTTPClient returns a server client that fails requests to any other host.
func LocalOnlyHTTPClient(t testing.TB, srv *httptest.Server) *http.Client {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := *srv.Client()
	client.Transport = onlyHostTransport{
		t:           t,
		allowedHost: parsed.Host,
		base:        client.Transport,
	}
	return &client
}

type onlyHostTransport struct {
	t           testing.TB
	allowedHost string
	base        http.RoundTripper
}

func (t onlyHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.allowedHost {
		t.t.Errorf("unexpected outbound request host %q, want %q", req.URL.Host, t.allowedHost)
		return nil, fmt.Errorf("unexpected outbound request host %q", req.URL.Host)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
