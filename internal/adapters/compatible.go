package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

type compatibleToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type compatibleMessage struct {
	Role         string               `json:"role"`
	Content      string               `json:"content"`
	Refusal      string               `json:"refusal"`
	ToolCalls    []compatibleToolCall `json:"tool_calls"`
	FunctionCall json.RawMessage      `json:"function_call"`
}

type compatibleChoice struct {
	Index        int               `json:"index"`
	Message      compatibleMessage `json:"message"`
	Delta        compatibleMessage `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type compatibleUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type compatibleResponse struct {
	Choices []compatibleChoice `json:"choices"`
	Usage   *compatibleUsage   `json:"usage"`
	Error   json.RawMessage    `json:"error"`
}

func compatibleRequest(c contract.ModelConnection, messages []message, tools []functionTool, structured bool) (map[string]any, error) {
	history := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		entry := map[string]any{"role": msg.Role, "content": msg.Content}
		switch msg.Role {
		case "system", "user":
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				calls := make([]compatibleToolCall, 0, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					if call.ID == "" || call.Function.Name == "" || !json.Valid(call.Function.Arguments) {
						return nil, invalidModelResponse()
					}
					wire := compatibleToolCall{ID: call.ID, Type: "function"}
					wire.Function.Name, wire.Function.Arguments = call.Function.Name, string(call.Function.Arguments)
					calls = append(calls, wire)
				}
				entry["tool_calls"] = calls
			}
		case "tool":
			if msg.ToolCallID == "" {
				return nil, fmt.Errorf("OpenAI-compatible tool result lacks call ID")
			}
			entry["tool_call_id"] = msg.ToolCallID
		default:
			return nil, fmt.Errorf("unsupported OpenAI-compatible message role")
		}
		history = append(history, entry)
	}
	body := map[string]any{"model": c.Model, "messages": history, "stream": true, "stream_options": map[string]any{"include_usage": true}}
	for name, value := range c.Parameters {
		body[name] = value
	}
	if len(tools) > 0 {
		body["tools"], body["parallel_tool_calls"] = tools, false
		if structured {
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": structuredOutputTool}}
		}
	}
	return body, nil
}

func compatibleHasError(raw json.RawMessage) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) != "null"
}

func normalizeCompatible(response compatibleResponse) (chatResponse, error) {
	if compatibleHasError(response.Error) || len(response.Choices) != 1 || response.Choices[0].Index != 0 {
		return chatResponse{}, invalidModelResponse()
	}

	choice := response.Choices[0]
	if choice.FinishReason == nil ||
		choice.Message.Role != "assistant" ||
		choice.Message.Refusal != "" ||
		compatibleHasError(choice.Message.FunctionCall) {
		return chatResponse{}, invalidModelResponse()
	}
	if *choice.FinishReason != "stop" && *choice.FinishReason != "tool_calls" {
		return chatResponse{}, invalidModelResponse()
	}
	if (*choice.FinishReason == "tool_calls") != (len(choice.Message.ToolCalls) > 0) {
		return chatResponse{}, invalidModelResponse()
	}

	result := chatResponse{Done: true, DoneReason: *choice.FinishReason, Message: message{Role: "assistant", Content: choice.Message.Content}}
	ids := map[string]bool{}
	for _, wire := range choice.Message.ToolCalls {
		if wire.ID == "" || wire.Type != "function" || wire.Function.Name == "" || ids[wire.ID] || !json.Valid([]byte(wire.Function.Arguments)) {
			return chatResponse{}, invalidModelResponse()
		}
		ids[wire.ID] = true
		call := toolCall{ID: wire.ID}
		call.Function.Name, call.Function.Arguments = wire.Function.Name, json.RawMessage(wire.Function.Arguments)
		result.Message.ToolCalls = append(result.Message.ToolCalls, call)
	}
	if result.Message.Content == "" && len(result.Message.ToolCalls) == 0 {
		return chatResponse{}, invalidModelResponse()
	}
	if response.Usage != nil {
		if response.Usage.PromptTokens < 0 || response.Usage.CompletionTokens < 0 {
			return chatResponse{}, invalidModelResponse()
		}
		result.PromptEvalCount, result.EvalCount = response.Usage.PromptTokens, response.Usage.CompletionTokens
	}
	return result, nil
}

func readCompatibleResponse(ctx context.Context, res *http.Response, started time.Time, observation *executionObservation, id string, step int) (chatResponse, time.Duration, error) {
	if !isEventStream(res) {
		var response compatibleResponse
		if err := readModelJSON(res.Body, &response); err != nil {
			return chatResponse{}, 0, err
		}
		result, err := normalizeCompatible(response)
		return result, 0, err
	}

	type streamTool struct {
		id, kind        string
		name, arguments strings.Builder
		emitted         int
	}
	tools := map[int]*streamTool{}
	var text strings.Builder
	response := compatibleResponse{Choices: []compatibleChoice{{Message: compatibleMessage{Role: "assistant"}}}}
	finished := false
	return readModelEvents(ctx, res.Body, started, observation, id, step, func(data []byte) (string, *chatResponse, error) {
		// Usage and errors may follow the finish choice; commit only at the stream terminator.
		if string(data) == "[DONE]" {
			if !finished {
				return "", nil, invalidModelResponse()
			}
			choice := &response.Choices[0]
			choice.Message.Content = text.String()
			for index := 0; index < len(tools); index++ {
				tool := tools[index]
				if tool == nil {
					return "", nil, invalidModelResponse()
				}
				call := compatibleToolCall{ID: tool.id, Type: tool.kind}
				call.Function.Name, call.Function.Arguments = tool.name.String(), tool.arguments.String()
				choice.Message.ToolCalls = append(choice.Message.ToolCalls, call)
			}
			result, err := normalizeCompatible(response)
			return "", &result, err
		}

		var event compatibleResponse
		if json.Unmarshal(data, &event) != nil || compatibleHasError(event.Error) {
			return "", nil, invalidModelResponse()
		}
		if event.Usage != nil {
			response.Usage = event.Usage
		}
		if len(event.Choices) == 0 {
			if !finished || event.Usage == nil {
				return "", nil, invalidModelResponse()
			}
			return "", nil, nil
		}
		if finished || len(event.Choices) != 1 || event.Choices[0].Index != 0 {
			return "", nil, invalidModelResponse()
		}

		choice := event.Choices[0]
		delta := choice.Delta
		if (delta.Role != "" && delta.Role != "assistant") || delta.Refusal != "" || compatibleHasError(delta.FunctionCall) {
			return "", nil, invalidModelResponse()
		}

		_, _ = text.WriteString(delta.Content)
		visible := delta.Content
		for _, fragment := range delta.ToolCalls {
			if fragment.Index == nil || *fragment.Index < 0 || (fragment.Type != "" && fragment.Type != "function") {
				return "", nil, invalidModelResponse()
			}

			tool := tools[*fragment.Index]
			if tool == nil {
				tool = &streamTool{}
				tools[*fragment.Index] = tool
			}
			if fragment.ID != "" {
				if tool.id != "" && tool.id != fragment.ID {
					return "", nil, invalidModelResponse()
				}
				tool.id = fragment.ID
			}
			if fragment.Type != "" {
				tool.kind = fragment.Type
			}
			_, _ = tool.name.WriteString(fragment.Function.Name)
			_, _ = tool.arguments.WriteString(fragment.Function.Arguments)
			if tool.name.String() == structuredOutputTool {
				arguments := tool.arguments.String()
				visible += arguments[tool.emitted:]
				tool.emitted = len(arguments)
			}
		}
		if choice.FinishReason != nil {
			response.Choices[0].FinishReason, finished = choice.FinishReason, true
		}
		return visible, nil, nil
	})
}
