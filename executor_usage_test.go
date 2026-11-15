package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A completed tool batch must not read another frame: Cursor normally waits
// for the caller's tool results instead of sending a TurnEnded update.
type singleFrameReader struct {
	*bytes.Reader
	extraReads int
}

func (r *singleFrameReader) Read(p []byte) (int, error) {
	if r.Len() == 0 {
		r.extraReads++
		return 0, errors.New("decoder read past the completed frame")
	}
	return r.Reader.Read(p)
}

func usageUpdate(input, output, cached, written, reasoning uint64) []byte {
	var fields []byte
	for i, value := range []uint64{input, output, cached, written, reasoning} {
		fields = append(fields, testUint(i+1, value)...)
	}
	return wireBytes(1, wireBytes(14, fields))
}

func TestNativeToolBatchPreservesUsageInCompletedFrame(t *testing.T) {
	tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))[5:]
	batch := wireBytes(1, wireBytes(27, testUint(1, 1)))
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, counters := range []struct {
		name                                   string
		input, output, cached, written, reason uint64
	}{
		{"cached-and-written", 100, 12, 60, 15, 7},
		{"fully-cached", 100, 12, 100, 0, 0},
		{"authoritative-zero", 0, 0, 0, 0, 0},
	} {
		for _, order := range orders {
			t.Run(fmt.Sprintf("%s/%v", counters.name, order), func(t *testing.T) {
				parts := [][]byte{tool, batch, usageUpdate(counters.input, counters.output, counters.cached, counters.written, counters.reason)}
				payload := bytes.Join([][]byte{parts[order[0]], parts[order[1]], parts[order[2]]}, nil)
				reader := &singleFrameReader{Reader: bytes.NewReader(wireFrame(payload, 0))}
				state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}, estimate: 500}
				var events []runEvent
				err := state.decode(reader, func([]byte) error { return nil }, func(event runEvent) bool {
					events = append(events, event)
					return true
				})
				if err != nil || reader.extraReads != 0 {
					t.Fatalf("tool batch did not finish within the frame: err=%v extraReads=%d", err, reader.extraReads)
				}
				if len(events) != 2 || events[0].tool == nil || events[1].stop != "tool_calls" {
					t.Fatalf("unexpected events: %+v", events)
				}
				want := map[string]any{
					"prompt_tokens": counters.input, "completion_tokens": counters.output, "total_tokens": counters.input + counters.output,
					"prompt_tokens_details": map[string]any{"cached_tokens": counters.cached, "cache_write_tokens": counters.written},
				}
				if counters.reason > 0 {
					want["completion_tokens_details"] = map[string]any{"reasoning_tokens": counters.reason}
				}
				if !reflect.DeepEqual(events[1].usage, want) {
					t.Fatalf("upstream counters lost: got=%v want=%v", events[1].usage, want)
				}
			})
		}
	}
}

func TestNativeTurnEndedPreservesNestedUpdatesInCompletedFrame(t *testing.T) {
	text := wireBytes(1, wireBytes(1, []byte("reply")))
	usage := usageUpdate(100, 12, 60, 15, 7)
	// Strip the interaction_update wrapper to exercise multiple updates inside
	// the same message, not just multiple top-level fields in one frame.
	fields, err := pbFields(usage)
	if err != nil {
		t.Fatal(err)
	}
	usage = fields[0].data
	for _, updates := range [][][]byte{{text, usage}, {usage, text}} {
		reader := &singleFrameReader{Reader: bytes.NewReader(wireFrame(wireBytes(1, bytes.Join(updates, nil)), 0))}
		var events []runEvent
		state := runState{estimate: 500}
		err := state.decode(reader, func([]byte) error { return nil }, func(event runEvent) bool {
			events = append(events, event)
			return true
		})
		if err != nil || reader.extraReads != 0 || len(events) != 2 || events[0].text != "reply" || events[1].stop != "stop" {
			t.Fatalf("nested updates lost: err=%v extraReads=%d events=%+v", err, reader.extraReads, events)
		}
		details := object(events[1].usage["prompt_tokens_details"])
		if details["cached_tokens"] != uint64(60) || details["cache_write_tokens"] != uint64(15) {
			t.Fatalf("nested counters lost: %v", events[1].usage)
		}
	}
}

func TestNativeToolBatchWithoutUsageDoesNotReadAnotherFrame(t *testing.T) {
	tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))[5:]
	batch := wireBytes(1, wireBytes(27, testUint(1, 1)))
	for _, parts := range [][][]byte{{tool, batch}, {batch, tool}} {
		reader := &singleFrameReader{Reader: bytes.NewReader(wireFrame(bytes.Join(parts, nil), 0))}
		state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}, estimate: 500}
		var finished runEvent
		err := state.decode(reader, func([]byte) error { return nil }, func(event runEvent) bool {
			if event.stop != "" {
				finished = event
			}
			return true
		})
		if err != nil || reader.extraReads != 0 || finished.stop != "tool_calls" || finished.usage["cursor_usage_estimated"] != true {
			t.Fatalf("normal handoff changed: err=%v extraReads=%d event=%+v", err, reader.extraReads, finished)
		}
	}
}

func TestNativeCompletedFrameDoesNotHideLaterProtocolError(t *testing.T) {
	tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))[5:]
	batch := wireBytes(1, wireBytes(27, testUint(1, 1)))
	badTool := wireBytes(2, []byte{0x7a, 0x05, 'x'})
	for _, parts := range [][][]byte{
		{tool, batch, badTool},
		{wireBytes(1, wireBytes(1, wireBytes(1, []byte("reply")))), usageUpdate(100, 12, 60, 15, 7), badTool},
	} {
		reader := &singleFrameReader{Reader: bytes.NewReader(wireFrame(bytes.Join(parts, nil), 0))}
		state := runState{tools: []chatTool{{Name: "read"}}, seen: map[string]string{}}
		finished := false
		err := state.decode(reader, func([]byte) error { return nil }, func(event runEvent) bool {
			finished = finished || event.stop != ""
			return true
		})
		if !errors.Is(err, io.ErrUnexpectedEOF) || finished || reader.extraReads != 0 {
			t.Fatalf("completion hid a later frame error: err=%v finished=%v extraReads=%d", err, finished, reader.extraReads)
		}
	}
}

func TestNativeCompletedFrameUsageReachesClient(t *testing.T) {
	tool := testToolFrame("read", "magpie", "magpie", wireBytes(3, []byte("a.txt")))[5:]
	batch := wireBytes(1, wireBytes(27, testUint(1, 1)))
	payload := wireFrame(bytes.Join([][]byte{tool, batch, usageUpdate(100, 12, 60, 15, 7)}, nil), 0)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := newTestRuntime(t)
			h := newMockHost(t)
			h.respond = func(_ context.Context, req mockRequest) (mockResponse, error) {
				if strings.HasSuffix(req.URL, "RunSSE") {
					return mockResponse{Status: 200, Body: payload}, nil
				}
				return mockResponse{Status: 200, Body: []byte(`{}`)}, nil
			}
			var chunks []byte
			closed := make(chan struct{})
			callback := func(method string, raw []byte) ([]byte, error) {
				if method == "host.stream.emit" {
					var req struct{ Payload []byte }
					if err := json.Unmarshal(raw, &req); err != nil {
						return nil, err
					}
					chunks = append(chunks, req.Payload...)
					return hostResult(map[string]any{}), nil
				}
				if method == "host.stream.close" {
					close(closed)
					return hostResult(map[string]any{}), nil
				}
				return h.call(method, raw)
			}
			s := p.newScope(callback, "usage-client")
			defer s.close()
			result, err := p.execute(s, rpcRequest{
				StorageJSON: []byte(`{"type":"cursor","cursor_auth":{"type":"oauth","access":"synthetic-usage","expires":9999999999999}}`),
				Payload:     []byte(`{"messages":[{"role":"user","content":"read a.txt"}],"tools":[{"type":"function","function":{"name":"read"}}]}`),
				Format:      "openai", StreamID: "usage-stream",
			}, stream)
			if err != nil {
				t.Fatal(err)
			}
			var body []byte
			if stream {
				select {
				case <-closed:
				case <-time.After(5 * time.Second):
					t.Fatal("stream did not close")
				}
				if !bytes.Contains(chunks, []byte("data: [DONE]")) {
					t.Fatalf("missing stream end: %s", chunks)
				}
				for _, line := range strings.Split(string(chunks), "\n") {
					if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"usage"`) {
						body = []byte(strings.TrimPrefix(line, "data: "))
					}
				}
			} else {
				body = object(result)["Payload"].([]byte)
			}
			response, err := jsonObject(body)
			if err != nil {
				t.Fatalf("invalid client response: %s, %v", body, err)
			}
			usage := object(response["usage"])
			details := object(usage["prompt_tokens_details"])
			if num(usage["prompt_tokens"]) != 100 || num(usage["completion_tokens"]) != 12 || num(details["cached_tokens"]) != 60 || num(details["cache_write_tokens"]) != 15 || usage["cursor_usage_estimated"] != nil {
				t.Fatalf("client did not receive authoritative usage: %s", body)
			}
		})
	}
}
