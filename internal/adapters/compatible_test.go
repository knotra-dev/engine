package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

func compatibleOutput(name, arguments string) string {
	encoded, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":17,"completion_tokens":9}}`, name, encoded)
}

func writeCompatibleOutput(w http.ResponseWriter, name, arguments string, streaming bool) {
	if !streaming {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, compatibleOutput(name, arguments))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, ": keep-alive\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\r\n\r\n")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n", name)
	for _, fragment := range []string{arguments[:len(arguments)/2], arguments[len(arguments)/2:]} {
		encoded, _ := json.Marshal(fragment)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%s}}]},\"finish_reason\":null}]}\n\n", encoded)
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9}}\n\ndata: [DONE]\n\n")
}

func compatibleTestRequest(endpoint string) Request {
	req := testRequest(endpoint)
	req.Plan.Profile.Spec.Models["local"] = contract.ModelConnection{Provider: "openai_compatible", Model: "vendor/model", BaseURL: endpoint}
	return req
}

func TestCompatibleRequestConvertsToolHistory(t *testing.T) {
	call := toolCall{ID: "call_1"}
	call.Function.Name, call.Function.Arguments = "work", json.RawMessage(`{"value":7}`)
	messages := []message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Work."},
		{Role: "assistant", ToolCalls: []toolCall{call}, Thinking: "private-thinking", ProviderContext: json.RawMessage(`{"private":"context"}`)},
		{Role: "tool", ToolCallID: "call_1", ToolName: "work", Content: `{"result":7}`},
	}
	tools := []functionTool{toolDefinition(structuredOutputTool, "Return output.", json.RawMessage(`{"type":"object"}`))}
	body, err := compatibleRequest(contract.ModelConnection{Model: "vendor/model", Parameters: map[string]any{"max_tokens": 128}}, messages, tools, true)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			Role, Content string
			ToolCalls     []compatibleToolCall `json:"tool_calls"`
			ToolCallID    string               `json:"tool_call_id"`
		}
		Tools      []functionTool
		ToolChoice struct {
			Type     string
			Function struct{ Name string }
		} `json:"tool_choice"`
	}
	if json.Unmarshal(data, &wire) != nil || len(wire.Messages) != 4 || len(wire.Tools) != 1 || wire.ToolChoice.Type != "function" || wire.ToolChoice.Function.Name != structuredOutputTool {
		t.Fatalf("wrong request: %s", data)
	}
	if len(wire.Messages[2].ToolCalls) != 1 || wire.Messages[2].ToolCalls[0].Function.Arguments != `{"value":7}` || wire.Messages[2].ToolCalls[0].Type != "function" || wire.Messages[3].ToolCallID != "call_1" {
		t.Fatal("tool identity or JSON-string arguments lost")
	}
	if strings.Contains(string(data), "private-") || strings.Contains(string(data), "provider_context") || body["max_tokens"] != 128 || body["parallel_tool_calls"] != false {
		t.Fatal("private context leaked or generation parameters lost")
	}
	if _, err := compatibleRequest(contract.ModelConnection{}, []message{{Role: "tool", Content: "result"}}, nil, false); err == nil {
		t.Fatal("tool result without call identity accepted")
	}
}

func TestCompatibleReaderAcceptsTextAndFragmentedTools(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeCompatibleOutput(w, "work", `{"value":7}`, streaming)
			}))
			defer server.Close()
			request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = res.Body.Close() }()
			result, _, err := readCompatibleResponse(context.Background(), res, time.Now(), nil, "model", 1)
			if err != nil || len(result.Message.ToolCalls) != 1 || result.PromptEvalCount != 17 || result.EvalCount != 9 {
				t.Fatalf("response=%+v err=%v", result, err)
			}
			call := result.Message.ToolCalls[0]
			if call.ID != "call_1" || call.Function.Name != "work" || string(call.Function.Arguments) != `{"value":7}` {
				t.Fatalf("tool=%+v", call)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	result, first, err := readCompatibleResponse(context.Background(), res, time.Now(), nil, "model", 1)
	if err != nil || result.Message.Content != "Hello world" || first <= 0 || !result.Done {
		t.Fatalf("response=%+v first=%v err=%v", result, first, err)
	}
}

func TestCompatibleStructuredOutputAndReplay(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				body := decodeRequest(t, req)
				if req.Method != "POST" || req.URL.Path != "/chat/completions" || req.Header.Get("Authorization") != "" || string(body["model"]) != `"vendor/model"` || string(body["stream"]) != "true" {
					t.Error("wrong compatible generation request")
				}
				var choice struct {
					Type     string
					Function struct{ Name string }
				}
				if json.Unmarshal(body["tool_choice"], &choice) != nil || choice.Type != "function" || choice.Function.Name != structuredOutputTool {
					t.Error("structured output tool is not forced")
				}
				writeCompatibleOutput(w, structuredOutputTool, `{"answer":42}`, streaming)
			}))
			defer server.Close()
			hooks := newObservationHooks()
			runner := &Runner{Hooks: hooks}
			req := compatibleTestRequest(server.URL)
			for range 2 {
				values, err := runner.Execute(context.Background(), req)
				if err != nil || string(values["answer"].JSON) != "42" {
					t.Fatalf("values=%v err=%v", values, err)
				}
			}
			if calls.Load() != 1 || hooks.calls["model"] != 1 {
				t.Fatal("replay called the model again")
			}
			for _, event := range hooks.snapshot() {
				if event.Type == "model.completed" && (event.Data["inputTokens"] != json.Number("17") || event.Data["outputTokens"] != json.Number("9")) {
					t.Fatal("final usage chunk was lost")
				}
			}
		})
	}
}

func TestCompatibleToolConversationPreservesIDs(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("configured bearer credential missing")
		}
		body := decodeRequest(t, req)
		if calls.Add(1) == 1 {
			writeCompatibleOutput(w, "work", `{"value":7}`, true)
			return
		}
		var history []struct {
			Role       string
			ToolCallID string               `json:"tool_call_id"`
			ToolCalls  []compatibleToolCall `json:"tool_calls"`
		}
		if json.Unmarshal(body["messages"], &history) != nil || len(history) != 3 || len(history[1].ToolCalls) != 1 || history[1].ToolCalls[0].ID != "call_1" || history[1].ToolCalls[0].Function.Arguments != `{"value":7}` || history[2].ToolCallID != "call_1" {
			t.Error("tool history lost")
		}
		writeCompatibleOutput(w, "knotra_finish", `{"answer":42}`, false)
	}))
	defer server.Close()
	req := compatibleTestRequest(server.URL)
	connection := req.Plan.Profile.Spec.Models["local"]
	key := "fixture-key"
	connection.Auth = map[string]contract.Credential{"key": {Value: &key}}
	req.Plan.Profile.Spec.Models["local"] = connection
	runner := &Runner{Hooks: newHooks()}
	messages := []message{{Role: "user", Content: "Use work, then finish."}}
	tools := []functionTool{toolDefinition("work", "Work", json.RawMessage(`{"type":"object"}`))}
	first, err := runner.chat(context.Background(), req, "model", 0, messages, tools, nil)
	if err != nil || len(first.ToolCalls) != 1 {
		t.Fatalf("first turn: %v", err)
	}
	messages = append(messages, first, message{Role: "tool", ToolName: "work", ToolCallID: first.ToolCalls[0].ID, Content: `{"result":7}`})
	second, err := runner.chat(context.Background(), req, "model", 1, messages, tools, nil)
	if err != nil || len(second.ToolCalls) != 1 || second.ToolCalls[0].Function.Name != "knotra_finish" || calls.Load() != 2 {
		t.Fatalf("second turn=%+v err=%v", second, err)
	}
}

func TestCompatibleInvalidResponsesRemainDurable(t *testing.T) {
	stop := `{"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n"
	for name, fixture := range map[string]struct{ kind, body string }{
		"missing choice":          {"application/json", `{}`},
		"length":                  {"application/json", strings.Replace(stop, `"stop"`, `"length"`, 1)},
		"filtered":                {"application/json", strings.Replace(stop, `"stop"`, `"content_filter"`, 1)},
		"wrong role":              {"application/json", strings.Replace(stop, `"assistant"`, `"user"`, 1)},
		"missing finish":          {"application/json", strings.Replace(stop, `,"finish_reason":"stop"`, "", 1)},
		"bad tool arguments":      {"application/json", compatibleOutput(structuredOutputTool, "{")},
		"missing tool type":       {"application/json", strings.Replace(compatibleOutput("work", `{}`), `"type":"function",`, "", 1)},
		"duplicate tool IDs":      {"application/json", `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"same","type":"function","function":{"name":"work","arguments":"{}"}},{"id":"same","type":"function","function":{"name":"work","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`},
		"wrong tool finish":       {"application/json", strings.Replace(compatibleOutput("work", `{}`), `"finish_reason":"tool_calls"`, `"finish_reason":"stop"`, 1)},
		"schema mismatch":         {"application/json", compatibleOutput(structuredOutputTool, `{"answer":"wrong"}`)},
		"provider error":          {"application/json", `{"error":{"message":"fixture-key private prompt"}}`},
		"malformed event":         {"text/event-stream", "data: nope\n\n"},
		"done without finish":     {"text/event-stream", "data: [DONE]\n\n"},
		"unfinished choice":       {"text/event-stream", strings.Replace(stream, `,"finish_reason":"stop"`, "", 1) + "data: [DONE]\n\n"},
		"missing done":            {"text/event-stream", stream},
		"error after finish":      {"text/event-stream", stream + "data: {\"error\":{\"message\":\"fixture-key private prompt\"}}\n\ndata: [DONE]\n\n"},
		"content after finish":    {"text/event-stream", stream + stream + "data: [DONE]\n\n"},
		"negative usage":          {"text/event-stream", stream + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-1}}\n\ndata: [DONE]\n\n"},
		"stream has wrong role":   {"text/event-stream", strings.Replace(stream, `"assistant"`, `"tool"`, 1) + "data: [DONE]\n\n"},
		"stream has wrong finish": {"text/event-stream", strings.Replace(stream, `"stop"`, `"length"`, 1) + "data: [DONE]\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if name != "schema mismatch" {
				response := &http.Response{Header: http.Header{"Content-Type": []string{fixture.kind}}, Body: io.NopCloser(strings.NewReader(fixture.body))}
				_, _, err := readCompatibleResponse(context.Background(), response, time.Now(), nil, "model", 1)
				_ = response.Body.Close()
				if err == nil {
					t.Fatal("invalid wire response accepted")
				}
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", fixture.kind)
				_, _ = fmt.Fprint(w, fixture.body)
			}))
			defer server.Close()
			runner := &Runner{Hooks: newHooks()}
			req := compatibleTestRequest(server.URL)
			for range 2 {
				_, err := runner.Execute(context.Background(), req)
				var failed *Failure
				if !errors.As(err, &failed) || failed.Code != "OUTPUT_INVALID" || failed.Retryable || failed.Unknown || strings.Contains(err.Error(), "fixture-key") || strings.Contains(err.Error(), "private prompt") {
					t.Fatalf("invalid provider response: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("invalid response caused another external call")
			}
		})
	}
}
