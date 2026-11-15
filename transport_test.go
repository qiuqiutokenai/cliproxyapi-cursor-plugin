package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func setTransport(t *testing.T, p *pluginRuntime, mode string) {
	t.Helper()
	raw := encodeJSON(map[string]any{"config_yaml": []byte("http-transport: " + mode + "\n")})
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(dispatch(p, "plugin.reconfigure", raw, nil), &response); err != nil || !response.OK {
		t.Fatalf("configure transport: %v, ok=%v", err, response.OK)
	}
}

func TestTransportConfigurationAndScopeSnapshot(t *testing.T) {
	p := newTestRuntime(t)
	legacy := p.newScope(nil, "old")
	defer legacy.close()
	setTransport(t, p, "host-default")
	current := p.newScope(nil, "new")
	defer current.close()
	if legacy.hostDefaultTransport || !current.hostDefaultTransport {
		t.Fatal("reconfigure changed an existing scope or missed a new scope")
	}
	for _, raw := range []string{"http-transport: unsupported", "http-transport: [bad", "http-transport: 2", "http-transport: http1\nhttp-transport: host-default"} {
		if err := p.configureTransport([]byte(raw)); err == nil {
			t.Fatalf("invalid configuration accepted: %q", raw)
		}
	}
	stillCurrent := p.newScope(nil, "still-current")
	defer stillCurrent.close()
	if !stillCurrent.hostDefaultTransport {
		t.Fatal("invalid configuration replaced the active mode")
	}
	if err := p.configureTransport(nil); err != nil {
		t.Fatal(err)
	}
	reset := p.newScope(nil, "reset")
	defer reset.close()
	if reset.hostDefaultTransport {
		t.Fatal("omitting the option did not restore compatibility mode")
	}
}

// socketHost implements the callback boundary with real sockets and an explicitly
// shared host transport. It tests the plugin's wire selection and cancellation,
// not CPA's transport selection or production proxy pooling policy.
type socketHost struct {
	*mockHost
	t             *testing.T
	client        *http.Client
	target        string
	expectDefault bool
	callbackID    string
}

func newSocketHost(t *testing.T, target string, transport *http.Transport, mode string, callbackID string) *socketHost {
	t.Helper()
	h := &socketHost{mockHost: newMockHost(t), t: t, client: &http.Client{Transport: transport}, target: target, expectDefault: mode == "host-default", callbackID: callbackID}
	t.Cleanup(transport.CloseIdleConnections)
	h.respond = func(ctx context.Context, req mockRequest) (mockResponse, error) {
		parsed, err := url.Parse(req.URL)
		if err != nil {
			return mockResponse{}, err
		}
		r, err := http.NewRequestWithContext(ctx, req.Method, h.target+parsed.RequestURI(), bytes.NewReader(req.Body))
		if err != nil {
			return mockResponse{}, err
		}
		r.Header = req.Headers.Clone()
		client := h.client
		if !h.expectDefault {
			// Model the v8.0.66 wire-profile lifecycle, retaining the local TLS
			// trust and dial policy while forcing HTTP/1 and fresh connections.
			one := transport.Clone()
			one.ForceAttemptHTTP2 = false
			one.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			one.DisableKeepAlives = true
			if one.TLSClientConfig != nil {
				one.TLSClientConfig.NextProtos = []string{"http/1.1"}
			}
			client = &http.Client{Transport: one}
			defer one.CloseIdleConnections()
		}
		resp, err := client.Do(r)
		if err != nil {
			return mockResponse{}, err
		}
		return mockResponse{Status: resp.StatusCode, Read: func(context.Context) ([]byte, bool, error) {
			b := make([]byte, 32*1024)
			n, errRead := resp.Body.Read(b)
			if errRead != nil {
				_ = resp.Body.Close()
			}
			if errRead == io.EOF {
				return b[:n], true, nil
			}
			return b[:n], false, errRead
		}}, nil
	}
	return h
}

func (h *socketHost) call(method string, raw []byte) ([]byte, error) {
	var req struct {
		CallbackID string          `json:"host_callback_id"`
		Profile    json.RawMessage `json:"wire_profile"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.CallbackID != h.callbackID {
		return nil, fmt.Errorf("callback scope crossed account boundary")
	}
	if method == "host.http.do_stream" {
		if h.expectDefault && len(req.Profile) != 0 {
			return nil, fmt.Errorf("host-default sent a wire profile")
		}
		if !h.expectDefault && string(req.Profile) != `{"http1_only":true}` {
			return nil, fmt.Errorf("compatibility profile changed")
		}
	}
	return h.mockHost.call(method, raw)
}

type socketObservation struct {
	mu       sync.Mutex
	requests []string
	conns    map[string]bool
}

func (o *socketObservation) observe(r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, r.Proto)
	if o.conns == nil {
		o.conns = map[string]bool{}
	}
	o.conns[r.RemoteAddr] = true
}

func socketRequest(t *testing.T, s *requestScope, path, token string) {
	t.Helper()
	resp, err := s.do(s.ctx, http.MethodPost, "https://api2.cursor.sh"+path, headers(token), []byte("binary-body"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestTransportHostDefaultNegotiatesAndReusesRealConnections(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		h2         bool
		wantProto  string
		wantConns  int
	}{
		{"compatibility", "http1", true, "HTTP/1.1", 4},
		{"http2", "host-default", true, "HTTP/2.0", 1},
		{"http1-fallback", "host-default", false, "HTTP/1.1", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var observed socketObservation
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed.observe(r)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "binary-body" || r.Header.Get("Authorization") != "Bearer account-one" {
					t.Errorf("request framing or account header changed: %v", err)
				}
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = test.h2
			server.StartTLS()
			defer server.Close()
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.ForceAttemptHTTP2 = true
			h := newSocketHost(t, server.URL, transport, test.mode, "one")
			p := newTestRuntime(t)
			setTransport(t, p, test.mode)
			for i := 0; i < 4; i++ {
				s := p.newScope(h.call, "one")
				socketRequest(t, s, "/aiserver.v1.BidiService/BidiAppend", "account-one")
				s.close()
			}
			h.assertClosed(t)
			observed.mu.Lock()
			defer observed.mu.Unlock()
			if len(observed.conns) != test.wantConns {
				t.Fatalf("connections=%d, want %d", len(observed.conns), test.wantConns)
			}
			for _, proto := range observed.requests {
				if proto != test.wantProto {
					t.Fatalf("protocol=%s, want %s", proto, test.wantProto)
				}
			}
		})
	}
}

func TestTransportHostDefaultRunSSEAndBidiAppendConcurrently(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			appendSeen := make(chan struct{})
			runStarted := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch {
				case strings.HasSuffix(r.URL.Path, "RunSSE"):
					w.Header().Set("Content-Type", "application/connect+proto")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(runStarted)
					select {
					case <-appendSeen:
						_, _ = w.Write(syntheticReply())
					case <-r.Context().Done():
					}
				case strings.HasSuffix(r.URL.Path, "BidiAppend"):
					select {
					case <-runStarted:
					case <-r.Context().Done():
						return
					}
					close(appendSeen)
					_, _ = io.WriteString(w, "{}")
				}
			}))
			server.EnableHTTP2 = h2
			server.StartTLS()
			defer server.Close()
			tr := server.Client().Transport.(*http.Transport).Clone()
			tr.ForceAttemptHTTP2 = true
			h := newSocketHost(t, server.URL, tr, "host-default", "run")
			p := newTestRuntime(t)
			setTransport(t, p, "host-default")
			s := p.newScope(h.call, "run")
			defer s.close()
			events := p.run(s, "fixture-token", modelResolution{ID: "fixture-model"}, chatConversation{Last: "hello"}, nil, "conversation")
			done := make(chan error, 1)
			go func() {
				stopped := false
				for ev := range events {
					if ev.err != nil {
						done <- ev.err
						return
					}
					stopped = stopped || ev.stop != ""
				}
				if !stopped {
					done <- fmt.Errorf("RunSSE ended without completion")
					return
				}
				done <- nil
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				s.close()
				t.Fatal("RunSSE and BidiAppend blocked each other")
			}
			s.close()
			h.assertClosed(t)
		})
	}
}

func TestTransportHostDefaultCancellationAndAccountIsolation(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			started := make(chan struct{})
			canceled := make(chan struct{})
			var mu sync.Mutex
			accountConns := map[string]map[string]bool{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := r.Header.Get("Authorization")
				mu.Lock()
				if accountConns[token] == nil {
					accountConns[token] = map[string]bool{}
				}
				accountConns[token][r.RemoteAddr] = true
				mu.Unlock()
				if r.URL.Path == "/silent" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(started)
					<-r.Context().Done()
					close(canceled)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, "ok")
			}))
			server.EnableHTTP2 = h2
			server.StartTLS()
			defer server.Close()
			trA := server.Client().Transport.(*http.Transport).Clone()
			trA.ForceAttemptHTTP2 = true
			trB := trA.Clone()
			a := newSocketHost(t, server.URL, trA, "host-default", "account-a")
			b := newSocketHost(t, server.URL, trB, "host-default", "account-b")
			p := newTestRuntime(t)
			setTransport(t, p, "host-default")
			sa := p.newScope(a.call, "account-a")
			defer sa.close()
			sb := p.newScope(b.call, "account-b")
			defer sb.close()
			response, err := sa.do(sa.ctx, http.MethodGet, "https://api2.cursor.sh/silent", headers("a"), nil)
			if err != nil {
				t.Fatal(err)
			}
			<-started
			readDone := make(chan error, 1)
			go func() { _, errRead := io.ReadAll(response.Body); readDone <- errRead }()
			socketRequest(t, sb, "/ok", "b")
			sa.close()
			select {
			case errRead := <-readDone:
				if errRead == nil {
					t.Fatal("canceled read succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("canceled read stalled")
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not observe cancellation")
			}
			socketRequest(t, sb, "/ok", "b")
			sb.close()
			a.assertClosed(t)
			b.assertClosed(t)
			mu.Lock()
			defer mu.Unlock()
			for conn := range accountConns["Bearer a"] {
				if accountConns["Bearer b"][conn] {
					t.Fatal("host's separate account transports shared a connection")
				}
			}
			if len(accountConns["Bearer b"]) != 1 {
				t.Fatal("canceling account A disrupted account B's reusable connection")
			}
		})
	}
}

func TestTransportHostDefaultPreservesHostProxy(t *testing.T) {
	seen := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Host
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "proxied")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != proxyURL.Host {
			return nil, fmt.Errorf("attempted to bypass host proxy")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	h := newSocketHost(t, "http://cursor.invalid", tr, "host-default", "proxy")
	p := newTestRuntime(t)
	setTransport(t, p, "host-default")
	s := p.newScope(h.call, "proxy")
	defer s.close()
	socketRequest(t, s, "/BidiAppend", "proxy-account")
	if got := <-seen; got != "cursor.invalid" {
		t.Fatalf("proxy target=%q", got)
	}
}
