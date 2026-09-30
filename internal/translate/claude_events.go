package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// normalizeResponsesInput rewrites a Responses request whose "input" is a plain string into
// the equivalent single user message, the only shape the official translators read.
func normalizeResponsesInput(body []byte) ([]byte, error) {
	root, errDecode := decodeObject(body)
	if errDecode != nil {
		return nil, errDecode
	}
	text, isString := root["input"].(string)
	if !isString {
		return body, nil
	}
	root["input"] = []any{map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}}
	out, errEncode := json.Marshal(root)
	if errEncode != nil {
		return nil, fmt.Errorf("encode responses request: %w", errEncode)
	}
	return out, nil
}

// claudeMessageToSSE renders a non-streaming Claude Messages response as the event stream
// Claude would have sent for it (message_start, one start/delta/stop per content block,
// message_delta with the stop reason and usage, message_stop).
func claudeMessageToSSE(body []byte) ([]byte, error) {
	root, errDecode := decodeObject(body)
	if errDecode != nil {
		return nil, errDecode
	}
	if stringValue(root["type"]) == "error" {
		return nil, fmt.Errorf("claude error response: %s", stringValue(objectValue(root["error"])["message"]))
	}
	usage := objectValue(root["usage"])
	start := map[string]any{}
	for key, value := range root {
		if key != "content" && key != "stop_reason" && key != "stop_sequence" {
			start[key] = value
		}
	}
	start["content"] = []any{}
	start["usage"] = map[string]any{
		"input_tokens":                numberOrZero(usage["input_tokens"]),
		"cache_read_input_tokens":     numberOrZero(usage["cache_read_input_tokens"]),
		"cache_creation_input_tokens": numberOrZero(usage["cache_creation_input_tokens"]),
		"output_tokens":               0,
	}
	var out bytes.Buffer
	out.Write(claudeSSE("message_start", map[string]any{"type": "message_start", "message": start}))
	for index, rawBlock := range arrayValue(root["content"]) {
		block := objectValue(rawBlock)
		kind := stringValue(block["type"])
		opening := map[string]any{"type": kind}
		var delta map[string]any
		switch kind {
		case "text":
			opening["text"] = ""
			delta = map[string]any{"type": "text_delta", "text": rawStringValue(block["text"])}
		case "thinking":
			opening["thinking"] = ""
			delta = map[string]any{"type": "thinking_delta", "thinking": rawStringValue(block["thinking"])}
		case "tool_use":
			opening["id"] = block["id"]
			opening["name"] = block["name"]
			opening["input"] = map[string]any{}
			arguments, errArgs := json.Marshal(block["input"])
			if errArgs != nil {
				return nil, fmt.Errorf("encode tool input: %w", errArgs)
			}
			delta = map[string]any{"type": "input_json_delta", "partial_json": string(arguments)}
		default:
			for key, value := range block {
				opening[key] = value
			}
		}
		out.Write(claudeSSE("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": opening}))
		if delta != nil {
			out.Write(claudeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta}))
		}
		if kind == "thinking" && stringValue(block["signature"]) != "" {
			out.Write(claudeSSE("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": block["signature"]}}))
		}
		out.Write(claudeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
	}
	out.Write(claudeSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": root["stop_reason"], "stop_sequence": root["stop_sequence"]},
		"usage": map[string]any{
			"input_tokens":                numberOrZero(usage["input_tokens"]),
			"cache_read_input_tokens":     numberOrZero(usage["cache_read_input_tokens"]),
			"cache_creation_input_tokens": numberOrZero(usage["cache_creation_input_tokens"]),
			"output_tokens":               numberOrZero(usage["output_tokens"]),
		},
	}))
	out.Write(claudeSSE("message_stop", map[string]any{"type": "message_stop"}))
	return out.Bytes(), nil
}

func numberOrZero(value any) any {
	if value == nil {
		return 0
	}
	return value
}
