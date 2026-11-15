package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type requestScope struct {
	p          *pluginRuntime
	ctx        context.Context
	cancel     context.CancelFunc
	host       hostCaller
	callbackID string
	mu         sync.Mutex
	closed     bool
	bodies     map[*hostBody]struct{}
	pending    sync.WaitGroup
}

func (p *pluginRuntime) newScope(host hostCaller, id string) *requestScope {
	ctx, cancel := context.WithCancel(p.ctx)
	s := &requestScope{p: p, ctx: ctx, cancel: cancel, host: host, callbackID: id, bodies: make(map[*hostBody]struct{})}
	p.mu.Lock()
	p.scopes[s] = struct{}{}
	p.mu.Unlock()
	return s
}
func (s *requestScope) call(method string, fields map[string]any, result any) error {
	if s.host == nil || s.callbackID == "" {
		return failure(502, "CPA HTTP callback is unavailable outside its request scope")
	}
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["host_callback_id"] = s.callbackID
	raw, err := s.host(method, encodeJSON(fields))
	if err != nil {
		return err
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err = json.Unmarshal(raw, &env); err != nil {
		return failure(502, "Invalid CPA callback response: %v", err)
	}
	if !env.OK {
		if env.Error != nil {
			status := env.Error.HTTPStatus
			if status < 400 {
				status = 502
			}
			message := env.Error.Message
			if message == "" {
				message = "CPA HTTP callback failed"
			}
			return failureDetails(status, env.Error.Code, message, env.Error.Retryable, env.Error.RetryAfterSeconds, env.Error.CredentialScoped)
		}
		return failure(502, "CPA HTTP callback failed")
	}
	if result != nil && len(env.Result) > 0 {
		if err = json.Unmarshal(env.Result, result); err != nil {
			return failure(502, "Invalid CPA callback result: %v", err)
		}
	}
	return nil
}
func (s *requestScope) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	list := make([]*hostBody, 0, len(s.bodies))
	for b := range s.bodies {
		list = append(list, b)
	}
	s.mu.Unlock()
	s.cancel()
	for _, b := range list {
		_ = b.Close()
	}
	s.pending.Wait()
	s.p.mu.Lock()
	delete(s.p.scopes, s)
	s.p.mu.Unlock()
}

type hostBody struct {
	s                 *requestScope
	operation, stream string
	pending           []byte
	eof               bool
	once              sync.Once
	stop              func() bool
}

func (s *requestScope) do(ctx context.Context, method, url string, h http.Header, payload []byte) (*http.Response, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	s.pending.Add(1)
	s.mu.Unlock()
	defer s.pending.Done()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var opened struct {
		ID string `json:"operation_id"`
	}
	if err := s.call("host.http.operation_open", nil, &opened); err != nil {
		return nil, err
	}
	if opened.ID == "" {
		return nil, failure(502, "CPA returned no HTTP operation ID")
	}
	b := &hostBody{s: s, operation: opened.ID}
	// Cancellation is registered before the response head is requested.
	s.mu.Lock()
	b.stop = context.AfterFunc(ctx, func() { _ = b.Close() })
	s.mu.Unlock()
	var response struct {
		Status  int         `json:"status_code"`
		Headers http.Header `json:"headers"`
		ID      string      `json:"stream_id"`
	}
	err := s.call("host.http.do_stream", map[string]any{"operation_id": opened.ID, "method": method, "url": url, "headers": h, "body": payload, "wire_profile": map[string]any{"http1_only": true}}, &response)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	if response.ID == "" {
		_ = b.Close()
		return nil, failure(502, "CPA returned no HTTP stream ID")
	}
	// Close may have canceled an operation whose response head arrived late.
	s.mu.Lock()
	b.stream = response.ID
	closed := s.closed || ctx.Err() != nil
	if !closed {
		s.bodies[b] = struct{}{}
	}
	s.mu.Unlock()
	if closed {
		_ = s.call("host.http.stream_close", map[string]any{"stream_id": response.ID}, nil)
		_ = b.Close()
		return nil, context.Canceled
	}
	if response.Status < 100 || response.Status > 599 {
		_ = b.Close()
		return nil, failure(502, "CPA returned an invalid HTTP status")
	}
	return &http.Response{StatusCode: response.Status, Header: response.Headers, Body: b}, nil
}
func (b *hostBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.eof {
			return 0, io.EOF
		}
		var chunk struct {
			Payload []byte `json:"payload"`
			Done    bool   `json:"done"`
			Error   string `json:"error"`
		}
		if err := b.s.call("host.http.stream_read", map[string]any{"stream_id": b.stream}, &chunk); err != nil {
			_ = b.Close()
			return 0, err
		}
		if len(chunk.Payload) > maxWireBytes {
			_ = b.Close()
			return 0, failure(502, "CPA HTTP chunk exceeds the size limit")
		}
		b.pending = chunk.Payload
		b.eof = chunk.Done
		if chunk.Error != "" {
			_ = b.Close()
			return 0, failure(502, "%s", chunk.Error)
		}
		if b.eof {
			_ = b.Close()
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *hostBody) Close() error {
	b.once.Do(func() {
		b.s.mu.Lock()
		id, stop := b.stream, b.stop
		delete(b.s.bodies, b)
		b.s.mu.Unlock()
		if stop != nil {
			stop()
		}
		_ = b.s.call("host.http.cancel", map[string]any{"operation_id": b.operation}, nil)
		if id != "" {
			_ = b.s.call("host.http.stream_close", map[string]any{"stream_id": id}, nil)
		}
	})
	return nil
}
func (s *requestScope) json(ctx context.Context, method, path, token string, body any) (map[string]any, error) {
	return s.jsonClient(ctx, method, path, token, body, agentClientType)
}
func (s *requestScope) jsonClient(ctx context.Context, method, path, token string, body any, clientType string) (map[string]any, error) {
	url := path
	if len(path) > 0 && path[0] == '/' {
		url = cursorAPI + path
	}
	var payload []byte
	if body != nil {
		payload = encodeJSON(body)
	}
	h := headersWithType(token, clientType)
	if strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/oauth/") {
		h.Del("Connect-Protocol-Version")
	}
	resp, err := s.do(ctx, method, url, h, payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxWireBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWireBytes {
		return nil, failure(502, "Cursor JSON response exceeds the size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, connectError(resp.StatusCode, raw, retryAfterHeader(resp.Header.Get("Retry-After")))
	}
	m, err := jsonObject(raw)
	if err != nil {
		return nil, failure(502, "Cursor returned invalid JSON: %v", err)
	}
	return m, nil
}
func connectError(status int, raw []byte, retryAfterHint ...int) error {
	originalStatus := status
	headerRetryAfter := 0
	if len(retryAfterHint) > 0 && retryAfterHint[0] > 0 {
		headerRetryAfter = retryAfterHint[0]
	}
	trimmed := strings.TrimSpace(string(raw))
	if originalStatus >= 200 && originalStatus < 300 && trimmed == "" {
		return nil
	}

	var m map[string]any
	if trimmed != "" {
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			if originalStatus >= 200 && originalStatus < 300 {
				return failureDetails(502, "invalid_cursor_response", "Cursor returned malformed error payload", false, headerRetryAfter, false)
			}
			return failureDetails(originalStatus, "cursor_http_error", trimmed, retryableStatus(originalStatus), headerRetryAfter, originalStatus == http.StatusUnauthorized)
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	if originalStatus >= 200 && originalStatus < 300 && successfulEndMetadata(m) {
		return nil
	}
	root := m
	if nested := object(m["error"]); nested != nil {
		root = nested
	}
	code, msg := strings.TrimSpace(str(root["code"])), strings.TrimSpace(str(root["message"]))
	if code == "" {
		code = strings.TrimSpace(str(m["code"]))
	}
	if msg == "" {
		msg = strings.TrimSpace(str(m["message"]))
	}
	for _, d := range array(root["details"]) {
		detail := object(object(object(d)["debug"])["details"])
		title, text := strings.TrimSpace(str(detail["title"])), strings.TrimSpace(str(detail["detail"]))
		if title != "" || text != "" {
			msg = title
			if text != "" {
				if msg != "" {
					msg += ": "
				}
				msg += text
			}
		}
	}
	if msg == "" {
		msg = code
	}
	if msg == "" && originalStatus >= 200 && originalStatus < 300 && len(m) == 0 {
		return nil
	}
	if msg == "" {
		msg = trimmed
	}
	if msg == "" {
		msg = http.StatusText(originalStatus)
	}

	low := strings.ToLower(msg)
	credentialScoped := boolAny(root["credential_scoped"]) || boolAny(root["credentialScoped"]) || boolAny(m["credential_scoped"]) || boolAny(m["credentialScoped"])
	retryAfter := retryAfterSeconds(root)
	if retryAfter == 0 {
		retryAfter = retryAfterSeconds(m)
	}
	switch {
	case originalStatus >= 400 && originalStatus <= 599:
		// Explicit upstream HTTP errors take precedence over text heuristics.
		status = originalStatus
		credentialScoped = credentialScoped || status == http.StatusUnauthorized
	case code == "model_blocked" || code == "model_not_found":
		status = http.StatusNotFound
	case code == "permission_denied":
		status = http.StatusForbidden
	case code == "unauthenticated":
		status = http.StatusUnauthorized
		credentialScoped = true
	case code == "resource_exhausted":
		status = http.StatusTooManyRequests
	case code == "invalid_argument":
		status = http.StatusBadRequest
	case code == "unavailable":
		status = http.StatusServiceUnavailable
	case code == "" && (low == "model blocked" || strings.HasPrefix(low, "model blocked:")):
		status = http.StatusNotFound
		code = "model_not_found"
	case code == "" && containsAny(low, "region"):
		status = http.StatusForbidden
	case code == "" && containsAny(low, "expired", "invalid user api key"):
		status = http.StatusUnauthorized
		credentialScoped = true
	case code == "" && containsAny(low, "quota", "rate limit", "usage limit"):
		status = http.StatusTooManyRequests
	case code == "" && containsAny(low, "too long", "context length", "too many tokens"):
		status = http.StatusBadRequest
	default:
		// Keep the transport status for unknown upstream codes. A 200 end frame
		// carrying an error has no useful HTTP status, so surface it as 502.
		if originalStatus >= 200 && originalStatus < 300 {
			status = http.StatusBadGateway
		} else {
			status = originalStatus
		}
	}
	if status < 400 {
		status = http.StatusBadGateway
	}
	if retryAfter == 0 {
		retryAfter = headerRetryAfter
	}
	return failureDetails(status, code, msg, retryableStatus(status), retryAfter, credentialScoped)
}

func successfulEndMetadata(m map[string]any) bool {
	for key, value := range m {
		switch key {
		case "metadata":
			if value != nil && object(value) == nil {
				return false
			}
		case "error":
			if value != nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func retryAfterSeconds(m map[string]any) int {
	for _, key := range []string{"retry_after_seconds", "retryAfterSeconds"} {
		if value := int(num(m[key])); value > 0 {
			return value
		}
	}
	return 0
}

func boolAny(v any) bool {
	b, _ := v.(bool)
	return b
}

func retryAfterHeader(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return seconds
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	remaining := time.Until(when)
	if remaining <= 0 {
		return 0
	}
	seconds := int(remaining / time.Second)
	if remaining%time.Second != 0 {
		seconds++
	}
	return seconds
}
func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
