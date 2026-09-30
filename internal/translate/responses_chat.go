package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// sseDataLines returns every "data:" line of one SSE frame, each without its trailing newline.
func sseDataLines(frame []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out
}

// responsesResponseToChat converts a non-streaming Responses API body into an OpenAI chat
// completion: message text, function calls as tool_calls, finish reason and token usage.
func responsesResponseToChat(model string, body []byte) ([]byte, error) {
	root, errDecode := decodeObject(body)
	if errDecode != nil {
		return nil, errDecode
	}
	if errFailure := responsesFailure(root); errFailure != nil {
		return nil, errFailure
	}
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	for _, rawItem := range arrayValue(root["output"]) {
		item := objectValue(rawItem)
		switch stringValue(item["type"]) {
		case "message":
			for _, rawPart := range arrayValue(item["content"]) {
				part := objectValue(rawPart)
				if kind := stringValue(part["type"]); kind == "output_text" || kind == "text" {
					text.WriteString(rawStringValue(part["text"]))
				}
			}
		case "reasoning":
			reasoning.WriteString(responsesReasoningText(item))
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id":    firstNonEmptyString(stringValue(item["call_id"]), stringValue(item["id"])),
				"type":  "function",
				"index": len(toolCalls),
				"function": map[string]any{
					"name":      stringValue(item["name"]),
					"arguments": firstNonEmptyString(rawStringValue(item["arguments"]), "{}"),
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	finish := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
	} else if stringValue(root["status"]) == "incomplete" {
		finish = "length"
	}
	usage := objectValue(root["usage"])
	prompt := int64Value(usage["input_tokens"])
	completion := int64Value(usage["output_tokens"])
	outUsage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      firstPositive(int64Value(usage["total_tokens"]), prompt+completion),
	}
	if cached := int64Value(objectValue(usage["input_tokens_details"])["cached_tokens"]); cached > 0 {
		outUsage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if reasoningTokens := int64Value(objectValue(usage["output_tokens_details"])["reasoning_tokens"]); reasoningTokens > 0 {
		outUsage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	out := map[string]any{
		"id":      firstNonEmptyString(stringValue(root["id"]), "chatcmpl-copilot"),
		"object":  "chat.completion",
		"created": int64Value(root["created_at"]),
		"model":   firstNonEmptyString(stringValue(root["model"]), model),
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   outUsage,
	}
	encoded, errEncode := json.Marshal(out)
	if errEncode != nil {
		return nil, fmt.Errorf("encode chat completion: %w", errEncode)
	}
	return encoded, nil
}

func firstPositive(values ...int64) int64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
