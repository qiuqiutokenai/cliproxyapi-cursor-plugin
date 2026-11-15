package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestExecutorChatCompletionsFormatCompatibility(t *testing.T) {
	for _, format := range []string{"", "openai", "chat-completions", "chat_completions", "openai-chat-completions", "openai_chat_completions", " OpenAI "} {
		if !isChatCompletionsFormat(format) {
			t.Errorf("rejected Chat Completions format %q", format)
		}
	}
	p := newTestRuntime(t)
	for _, format := range []string{"responses", "openai-response", "claude", "gemini"} {
		for _, method := range []string{"executor.execute", "executor.execute_stream"} {
			result := invokeNative(t, p, method, map[string]any{
				"Format": format, "Payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
			}, nil)
			err := object(result["error"])
			if result["ok"] != false || num(err["http_status"]) != 400 || !strings.Contains(str(err["message"]), "received") {
				t.Fatalf("unsupported format %q was not rejected: %v", format, result)
			}
		}
	}
}

func testUint(field int, n uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(field<<3)), n)
}
func testToolFrame(name, provider, server string, args []byte) []byte {
	argument := bytes.Join([][]byte{wireBytes(1, []byte("magpie-"+name)), wireBytes(2, append(wireBytes(1, []byte("path")), wireBytes(2, args)...)), wireBytes(3, []byte("call_fixture")), wireBytes(4, []byte(provider)), wireBytes(5, []byte(name)), wireBytes(9, []byte(server))}, nil)
	exec := append(append(testUint(1, 7), wireBytes(15, []byte("exec-7"))...), wireBytes(11, argument)...)
	return wireFrame(wireBytes(2, exec), 0)
}
func TestNativeToolDecodingAndAuthority(t *testing.T) {
	for _, name := range []string{"read", "unknown"} {
		for _, provider := range []string{"magpie", "foreign"} {
			t.Run(name+"/"+provider, func(t *testing.T) {
				state := runState{tools: []chatTool{{Name: "read", Schema: map[string]any{}}}, seen: map[string]string{}, estimate: 10}
				data := append(testToolFrame(name, provider, "magpie", wireBytes(3, []byte("a.txt"))), wireFrame(wireBytes(1, wireBytes(27, testUint(1, 1))), 0)...)
				if name != "read" || provider != "magpie" {
					data = append(data, syntheticReply()...)
				}
				var events []runEvent
				var sent [][]byte
				err := state.decode(bytes.NewReader(data), func(v []byte) error { sent = append(sent, v); return nil }, func(ev runEvent) bool { events = append(events, ev); return true })
				if err != nil {
					t.Fatal(err)
				}
				if name == "read" && provider == "magpie" {
					if len(events) != 2 || events[0].tool == nil || events[1].stop != "tool_calls" {
						t.Fatalf("tool was not returned: %+v", events)
					}
					if len(sent) != 0 {
						t.Fatal("caller tool was executed or acknowledged before its result")
					}
					if events[1].usage["cursor_usage_estimated"] != true {
						t.Fatal("missing estimate marker")
					}
				} else {
					if len(sent) < 2 {
						t.Fatal("unavailable tool was not refused")
					}
					for _, ev := range events {
						if ev.tool != nil {
							t.Fatal("unregistered or foreign tool escaped")
						}
					}
				}
			})
		}
	}
}
func TestNativeMalformedToolArgumentsNeverPublishCalls(t *testing.T) {
	for _, argument := range [][]byte{{0x1a, 0x05, 'x'}, {0x18, 0x01}, {0x1a, 0x01, 0xff}, {0x08, 0x02}} {
		t.Run(hex.EncodeToString(argument), func(t *testing.T) {
			state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}}
			published := false
			err := state.decode(bytes.NewReader(testToolFrame("read", "magpie", "magpie", argument)), func([]byte) error { return nil }, func(runEvent) bool { published = true; return true })
			if err == nil || published {
				t.Fatalf("corrupt arguments accepted: err=%v published=%v", err, published)
			}
		})
	}
}
func TestNativeFrameErrorsAndGzip(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write([]byte("frame"))
	_ = gz.Close()
	frame := wireFrame(compressed.Bytes(), 1)
	data, end, err := readConnectFrame(bytes.NewReader(frame))
	if err != nil || end || string(data) != "frame" {
		t.Fatalf("gzip decode failed: %q %v", data, err)
	}
	for _, data := range [][]byte{{0, 0, 0, 0, 3, 'a'}, {0x80, 0, 0, 0, 0}, {0, 0x7f, 0xff, 0xff, 0xff}} {
		if _, _, err = readConnectFrame(bytes.NewReader(data)); err == nil {
			t.Fatalf("malformed frame accepted: %x", data)
		}
	}
}
func TestNativeConnectStatusMapping(t *testing.T) {
	for code, status := range map[string]int{"unauthenticated": 401, "permission_denied": 403, "resource_exhausted": 429, "invalid_argument": 400, "unavailable": 503} {
		err := connectError(200, encodeJSON(map[string]any{"error": map[string]any{"code": code, "message": "fixture failure"}}))
		if errorStatus(err) != status {
			t.Errorf("%s -> %v", code, err)
		}
	}
	if connectError(200, []byte(`{}`)) != nil {
		t.Fatal("successful end frame failed")
	}
}

func TestNativeConnectErrorPreservesStructuredDetails(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantStatus     int
		wantCode       string
		wantRetryable  bool
		wantRetryAfter int
		wantCredential bool
	}{
		{name: "unauthenticated", status: 401, body: `{"error":{"code":"unauthenticated","message":"Invalid User API Key"}}`, wantStatus: 401, wantCode: "unauthenticated", wantCredential: true},
		{name: "permission denied", status: 403, body: `{"error":{"code":"permission_denied","message":"forbidden"}}`, wantStatus: 403, wantCode: "permission_denied"},
		{name: "resource exhausted", status: 429, body: `{"error":{"code":"resource_exhausted","message":"quota exceeded","retry_after_seconds":19}}`, wantStatus: 429, wantCode: "resource_exhausted", wantRetryable: true, wantRetryAfter: 19},
		{name: "explicit account quota", status: 429, body: `{"error":{"code":"resource_exhausted","message":"account-wide quota exceeded","credential_scoped":true}}`, wantStatus: 429, wantCode: "resource_exhausted", wantRetryable: true, wantCredential: true},
		{name: "unknown service code", status: 503, body: `{"error":{"code":"backend_overloaded","message":"try again"}}`, wantStatus: 503, wantCode: "backend_overloaded", wantRetryable: true},
		{name: "model blocked end frame", status: 200, body: `{"code":"model_blocked","message":"Model Blocked: ask your admin to enable access to Claude Fable 5."}`, wantStatus: 404, wantCode: "model_not_found"},
		{name: "nested model blocked end frame", status: 200, body: `{"error":{"message":"Model Blocked: Please ask your admin to enable access"}}`, wantStatus: 404, wantCode: "model_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := connectError(tt.status, []byte(tt.body))
			if err == nil {
				t.Fatal("expected error")
			}
			if got := errorStatus(err); got != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%v)", got, tt.wantStatus, err)
			}
			var detail *statusError
			if !errors.As(err, &detail) {
				t.Fatalf("error does not retain statusError: %T", err)
			}
			if detail.code != tt.wantCode || detail.retryable != tt.wantRetryable || detail.retryAfterSeconds != tt.wantRetryAfter || detail.credentialScoped != tt.wantCredential {
				t.Fatalf("details = %+v, want code=%q retryable=%t retry_after=%d credential_scoped=%t", detail, tt.wantCode, tt.wantRetryable, tt.wantRetryAfter, tt.wantCredential)
			}
		})
	}
	if err := connectError(200, []byte(`{}`)); err != nil {
		t.Fatalf("empty end frame should succeed: %v", err)
	}
	if err := connectError(200, []byte(`not-json`)); errorStatus(err) != 502 {
		t.Fatalf("malformed end frame status = %d, want 502 (%v)", errorStatus(err), err)
	}
	err := connectError(429, []byte(`busy`), 11)
	var detail *statusError
	if !errors.As(err, &detail) || detail.retryAfterSeconds != 11 {
		t.Fatalf("Retry-After hint was lost: %+v", detail)
	}
}

func TestStructuredErrorEnvelope(t *testing.T) {
	err := failureDetails(429, "resource_exhausted", "quota exceeded", true, 23, true)
	var got struct {
		OK    bool      `json:"ok"`
		Error *rpcError `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(errorEnvelope(err), &got); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if got.OK || got.Error == nil {
		t.Fatalf("invalid error envelope: %+v", got)
	}
	if got.Error.Code != "resource_exhausted" || got.Error.HTTPStatus != 429 || !got.Error.Retryable || got.Error.RetryAfterSeconds != 23 || !got.Error.CredentialScoped {
		t.Fatalf("lost error detail: %+v", got.Error)
	}
}

func TestRetryAfterHeaderSeconds(t *testing.T) {
	if got := retryAfterHeader("17"); got != 17 {
		t.Fatalf("Retry-After seconds = %d, want 17", got)
	}
	if got := retryAfterHeader("invalid"); got != 0 {
		t.Fatalf("invalid Retry-After = %d, want 0", got)
	}
}

func TestStreamCloseCarriesStructuredErrorDetail(t *testing.T) {
	p := newTestRuntime(t)
	var closeFields map[string]any
	host := func(method string, raw []byte) ([]byte, error) {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		if method == "host.stream.close" {
			closeFields = fields
		}
		return envelope(message{Result: encodeJSON(map[string]any{})}), nil
	}
	s := p.newScope(host, "stream-context")
	streamReply(s, "stream-1", nil, runEvent{err: failureDetails(429, "resource_exhausted", "quota exceeded", true, 7, true)}, "chatcmpl-test", "cursor/test", 1)
	if closeFields == nil {
		t.Fatal("host.stream.close was not called")
	}
	if closeFields["error"] != "quota exceeded" {
		t.Fatalf("legacy error was not retained: %+v", closeFields)
	}
	detail, ok := closeFields["error_detail"].(map[string]any)
	if !ok {
		t.Fatalf("missing structured error detail: %+v", closeFields)
	}
	if str(detail["code"]) != "resource_exhausted" || int(num(detail["http_status"])) != 429 || !boolValue(detail["retryable"]) || int(num(detail["retry_after_seconds"])) != 7 || !boolValue(detail["credential_scoped"]) {
		t.Fatalf("unexpected structured detail: %+v", detail)
	}
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}
func TestNativeMaxModeRetryIsOnlyForRequiredMode(t *testing.T) {
	for _, msg := range []string{"MAX_MODE_REQUIRED", "this model requires max mode", "max mode required"} {
		if !maxModeRequired.MatchString(msg) {
			t.Errorf("required mode was not recognized: %s", msg)
		}
	}
	for _, msg := range []string{"max mode usage limit reached", "max mode quota exhausted", "max mode unavailable in this region"} {
		if maxModeRequired.MatchString(msg) {
			t.Errorf("quota or region failure would trigger a mode retry: %s", msg)
		}
	}
}

type fixtureRun struct {
	number  int
	queue   chan []byte
	refs    map[uint64]string
	history map[uint64][]byte
	cid     string
}
type toolFixture struct {
	mu              sync.Mutex
	runs            map[string]*fixtureRun
	count           int
	histories       []string
	conversationIDs []string
}

func (f *toolFixture) run(id string) *fixtureRun {
	r := f.runs[id]
	if r == nil {
		f.count++
		r = &fixtureRun{number: f.count, queue: make(chan []byte, 32), refs: map[uint64]string{}, history: map[uint64][]byte{}}
		f.runs[id] = r
	}
	return r
}
func fixtureFields(data []byte, number int) ([]byte, error) {
	fs, err := pbFields(data)
	if err != nil {
		return nil, err
	}
	f := pbGet(fs, number)
	if f == nil {
		return nil, fmt.Errorf("fixture missing protobuf field %d", number)
	}
	return f.data, nil
}
func (f *toolFixture) respond(ctx context.Context, req mockRequest) (mockResponse, error) {
	if !strings.HasSuffix(req.URL, "RunSSE") && !strings.HasSuffix(req.URL, "BidiAppend") {
		body := []byte(`{}`)
		if strings.HasSuffix(req.URL, "AvailableModels") {
			body = []byte(`{"models":[{"name":"fixture-model","contextTokenLimit":200000}]}`)
		}
		return mockResponse{Status: 200, Body: body}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	run := f.run(req.Headers.Get("X-Request-Id"))
	if strings.HasSuffix(req.URL, "RunSSE") {
		return mockResponse{Status: 200, Read: func(ctx context.Context) ([]byte, bool, error) {
			select {
			case payload := <-run.queue:
				return payload, false, nil
			case <-ctx.Done():
				return nil, true, ctx.Err()
			}
		}}, nil
	}
	appendFields, err := pbFields(req.Body)
	if err != nil {
		return mockResponse{}, err
	}
	wire, err := hex.DecodeString(pbText(appendFields, 1))
	if err != nil {
		return mockResponse{}, err
	}
	fields, err := pbFields(wire)
	if err != nil {
		return mockResponse{}, err
	}
	if first := pbGet(fields, 1); first != nil {
		request, err := pbFields(first.data)
		if err != nil {
			return mockResponse{}, err
		}
		run.cid = pbText(request, 5)
		f.conversationIDs = append(f.conversationIDs, run.cid)
		state, err := fixtureFields(first.data, 1)
		if err != nil {
			return mockResponse{}, err
		}
		refs, err := pbFields(state)
		if err != nil {
			return mockResponse{}, err
		}
		index := uint64(1000)
		for _, ref := range refs {
			if ref.number != 1 {
				continue
			}
			run.refs[index] = hex.EncodeToString(ref.data)
			ask := append(testUint(1, index), wireBytes(2, wireBytes(1, ref.data))...)
			run.queue <- wireFrame(wireBytes(4, ask), 0)
			index++
		}
	} else if kv := pbGet(fields, 3); kv != nil {
		reply, err := pbFields(kv.data)
		if err != nil {
			return mockResponse{}, err
		}
		id := pbNum(reply, 1)
		if _, wanted := run.refs[id]; wanted {
			content, err := fixtureFields(kv.data, 2)
			if err != nil {
				return mockResponse{}, err
			}
			blob, err := fixtureFields(content, 1)
			if err != nil {
				return mockResponse{}, err
			}
			run.history[id] = blob
			if len(run.history) == len(run.refs) {
				var history []json.RawMessage
				for i := uint64(1000); i < uint64(1000+len(run.refs)); i++ {
					history = append(history, run.history[i])
				}
				text := string(encodeJSON(history))
				f.histories = append(f.histories, text)
				if run.number == 1 {
					tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))
					run.queue <- append(tool, wireFrame(wireBytes(1, wireBytes(27, testUint(1, 1))), 0)...)
				} else {
					if !strings.Contains(text, "source-marker") {
						return mockResponse{}, fmt.Errorf("tool result missing from rebuilt transcript")
					}
					result := wireFrame(wireBytes(1, wireBytes(1, wireBytes(1, []byte("continued with source-marker")))), 0)
					run.queue <- append(result, wireFrame(wireBytes(1, wireBytes(14, []byte{8, 22, 16, 7})), 0)...)
				}
			}
		}
	}
	return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
}
func TestNativeToolResultContinuationRebuildsHistory(t *testing.T) {
	p := newTestRuntime(t)
	h := newMockHost(t)
	fixture := &toolFixture{runs: map[string]*fixtureRun{}}
	h.respond = fixture.respond
	user := map[string]any{"role": "user", "content": "Read a.txt then explain"}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}
	execute := func(messages []any) map[string]any {
		req := map[string]any{"host_callback_id": "tool-context", "StorageJSON": []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"fixture-tools","expires":9999999999999}}`), "Model": "cursor/fixture-model", "Format": "chat-completions", "AuthID": "fixture", "Metadata": map[string]any{"canonical_session_id": "same-session"}, "Payload": encodeJSON(map[string]any{"messages": messages, "tools": tools})}
		result := invokeNative(t, p, "executor.execute", req, h.call)
		if result["ok"] != true {
			t.Fatalf("execution failed: %v", result)
		}
		raw, _ := base64.StdEncoding.DecodeString(str(object(result["result"])["Payload"]))
		body, err := jsonObject(raw)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := execute([]any{user})
	choice := object(array(first["choices"])[0])
	if str(choice["finish_reason"]) != "tool_calls" {
		t.Fatal(first)
	}
	assistant := object(choice["message"])
	calls := array(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatal(assistant)
	}
	call := object(calls[0])
	if str(object(call["function"])["name"]) != "read" {
		t.Fatal(call)
	}
	second := execute([]any{user, assistant, map[string]any{"role": "tool", "tool_call_id": str(call["id"]), "content": "source-marker"}})
	message := object(object(array(second["choices"])[0])["message"])
	if str(message["content"]) != "continued with source-marker" {
		t.Fatal(second)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.count != 2 || len(fixture.histories) != 2 {
		t.Fatal("continuation did not rebuild exactly one fresh Run")
	}
	if !reflect.DeepEqual(fixture.conversationIDs, []string{fixture.conversationIDs[0], fixture.conversationIDs[0]}) {
		t.Fatal("conversation ID changed")
	}
	h.assertClosed(t)
}

func TestNativeStreamFailurePreservesUpstreamStatus(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := newTestRuntime(t)
			h := newMockHost(t)
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				if strings.HasSuffix(req.URL, "RunSSE") {
					return mockResponse{Status: 429, Body: []byte(`{"code":"resource_exhausted","message":"plan limit"}`)}, nil
				}
				return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
			}
			req := map[string]any{"host_callback_id": "err", "stream_id": "stream", "StorageJSON": []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"fixture-error","expires":9999999999999}}`), "Model": "cursor/test", "Payload": []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}
			method := "executor.execute"
			if stream {
				method = "executor.execute_stream"
			}
			result := invokeNative(t, p, method, req, h.call)
			if num(object(result["error"])["http_status"]) != 429 {
				t.Fatal(result)
			}
			h.assertClosed(t)
		})
	}
}
