package translate

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeRequestToResponsesPreservesCoreContent(t *testing.T) {
	t.Parallel()

	claudeRequest := []byte(`{
		"model":"ignored",
		"max_tokens":321,
		"system":"Be concise.",
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"describe this"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"inspect first","signature":"sig"},
				{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}
		],
		"tools":[{"name":"lookup","description":"Look up data","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`)

	out, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", claudeRequest, false, EndpointResponses)
	if err != nil {
		t.Fatalf("translate Claude request: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("model").String(); got != "gpt-5.6-sol" {
		t.Fatalf("model = %q", got)
	}
	if got := data.Get("max_output_tokens").Int(); got != 321 {
		t.Fatalf("max_output_tokens = %d", got)
	}
	text := string(out)
	for _, needle := range []string{
		"Be concise.",
		"describe this",
		"data:image/png;base64,YWJj",
		"inspect first",
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`"type":"function_call_output"`,
		`"name":"lookup"`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("translated request omits %q: %s", needle, out)
		}
	}
}

func TestClaudeStructuredOutputAddsResponsesSchemaName(t *testing.T) {
	t.Parallel()

	original := []byte(`{
		"model":"gpt-5.6-terra",
		"messages":[{"role":"user","content":"Create a title."}],
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	out, err := RequestForEndpointFrom("claude", "gpt-5.6-terra", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	if got := gjson.GetBytes(out, "text.format.name").String(); got != "claude_structured_output" {
		t.Fatalf("text.format.name = %q; request=%s", got, out)
	}
}

func TestChatNonStreamResponseToResponses(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	translated, err := RequestForEndpoint("gpt-test", original, false, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	upstream := []byte(`{
		"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"gpt-test",
		"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
	}`)
	out, err := ResponseToResponses(context.Background(), EndpointChatCompletions, "gpt-test", original, translated, upstream)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("status").String(); got != "completed" {
		t.Fatalf("status = %q; response=%s", got, out)
	}
	if got := data.Get("output.0.content.0.text").String(); got != "pong" {
		t.Fatalf("output text = %q; response=%s", got, out)
	}
	if got := data.Get("usage.total_tokens").Int(); got != 3 {
		t.Fatalf("total tokens = %d; response=%s", got, out)
	}
}

func TestResponsesNonStreamResponseToClaude(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-5.6-sol","max_tokens":20,"messages":[{"role":"user","content":"ping"}]}`)
	translated, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", original, false, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	upstream := []byte(`{
		"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5.6-sol",
		"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],
		"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}
	}`)
	out, err := ResponseFromEndpoint(context.Background(), EndpointResponses, "claude", "gpt-5.6-sol", original, translated, upstream)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	data := gjson.ParseBytes(out)
	if got := data.Get("type").String(); got != "message" {
		t.Fatalf("Claude response type = %q; response=%s", got, out)
	}
	if got := data.Get("content.0.text").String(); got != "pong" {
		t.Fatalf("Claude response text = %q; response=%s", got, out)
	}
	if got := data.Get("usage.input_tokens").Int(); got != 2 {
		t.Fatalf("Claude input tokens = %d; response=%s", got, out)
	}
}

func TestChatSSEToResponses(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}]}`)
	translated, err := RequestForEndpoint("gpt-test", original, true, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	chunks := [][]byte{
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"po"},"finish_reason":null}]}`),
		[]byte(`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`),
		[]byte(`data: [DONE]`),
	}
	var state any
	var output strings.Builder
	for _, chunk := range chunks {
		frames, errTranslate := StreamToResponses(context.Background(), EndpointChatCompletions, "gpt-test", original, translated, chunk, &state)
		if errTranslate != nil {
			t.Fatalf("translate SSE: %v", errTranslate)
		}
		for _, frame := range frames {
			output.Write(frame)
		}
	}
	text := output.String()
	for _, needle := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"po"`,
		`"delta":"ng"`,
		"event: response.completed",
		`"total_tokens":3`,
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("translated SSE omits %q:\n%s", needle, text)
		}
	}
}

func TestResponsesSSEToClaude(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-5.6-sol","max_tokens":20,"messages":[{"role":"user","content":"ping"}]}`)
	translated, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	chunks := [][]byte{
		[]byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":2,"output_tokens":0}}}

`),
		[]byte(`event: response.content_part.added
data: {"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"one"}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" two"}

`),
		[]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" three"}

`),
		[]byte(`event: response.content_part.done
data: {"type":"response.content_part.done","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"one two three"}}

`),
		[]byte(`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-5.6-sol","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"one two three"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}

`),
	}
	var state any
	var output strings.Builder
	for _, chunk := range chunks {
		frames, errTranslate := StreamFromEndpoint(context.Background(), EndpointResponses, "claude", "gpt-5.6-sol", original, translated, chunk, &state)
		if errTranslate != nil {
			t.Fatalf("translate SSE: %v", errTranslate)
		}
		for _, frame := range frames {
			output.Write(frame)
		}
	}
	text := output.String()
	for _, needle := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text"`,
		`"text":"one"`,
		`"text":" two"`,
		`"text":" three"`,
		`"stop_reason":"end_turn"`,
		`"output_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("Claude SSE omits %q:\n%s", needle, text)
		}
	}
}

func TestResponsesRequestTranslatesToMessagesEndpoint(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"claude-opus-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"max_output_tokens":64}`)
	out, err := RequestForEndpointFrom("openai-response", "claude-opus-5.5", body, false, EndpointMessages)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom() error = %v", err)
	}
	if !gjson.GetBytes(out, "messages").IsArray() || !strings.Contains(string(out), "hello") {
		t.Fatalf("translated request is not an Anthropic messages payload: %s", out)
	}
}

func TestOpenAIChatPassesThroughToChatEndpoint(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"copilot/gpt-5-mini","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`)
	out, err := RequestForEndpointFrom("openai", "gpt-5-mini", body, true, EndpointChatCompletions)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom() error = %v", err)
	}
	if gjson.GetBytes(out, "model").String() != "gpt-5-mini" || !gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("model/stream not set: %s", out)
	}
	if gjson.GetBytes(out, "messages.0.content").String() != "hello" || gjson.GetBytes(out, "max_tokens").Int() != 10 {
		t.Fatalf("chat body changed on passthrough: %s", out)
	}
	response := []byte(`{"id":"c1","object":"chat.completion","model":"gpt-5-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	got, errResponse := ResponseFromEndpoint(context.Background(), EndpointChatCompletions, "openai", "gpt-5-mini", body, out, response)
	if errResponse != nil || gjson.GetBytes(got, "choices.0.message.content").String() != "ok" {
		t.Fatalf("chat response passthrough = %s, %v", got, errResponse)
	}
}

func TestOpenAIChatToResponsesEndpointRoundTrip(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}],"max_tokens":400}`)
	out, err := RequestForEndpointFrom("openai", "gpt-5.6-sol", body, false, EndpointResponses)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom() error = %v", err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("translated Responses request lost the prompt: %s", out)
	}
	response := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","id":"m1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`)
	got, errResponse := ResponseFromEndpoint(context.Background(), EndpointResponses, "openai", "gpt-5.6-sol", body, out, response)
	if errResponse != nil {
		t.Fatalf("ResponseFromEndpoint() error = %v", errResponse)
	}
	if gjson.GetBytes(got, "choices.0.message.content").String() != "ok" {
		t.Fatalf("chat response is empty after translating from Responses: %s", got)
	}
}

func TestResponsesOutputTokensClampedToCopilotMinimum(t *testing.T) {
	t.Parallel()

	small := []byte(`{"model":"gpt-5.6-sol","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := RequestForEndpointFrom("claude", "gpt-5.6-sol", small, false, EndpointResponses)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom() error = %v", err)
	}
	if got := gjson.GetBytes(out, "max_output_tokens").Int(); got != 16 {
		t.Fatalf("max_output_tokens = %d, want clamped to 16: %s", got, out)
	}
	large := []byte(`{"model":"gpt-5.6-sol","max_tokens":400,"messages":[{"role":"user","content":"hi"}]}`)
	out, err = RequestForEndpointFrom("claude", "gpt-5.6-sol", large, false, EndpointResponses)
	if err != nil || gjson.GetBytes(out, "max_output_tokens").Int() != 400 {
		t.Fatalf("a larger limit must be kept: %s, %v", out, err)
	}
	chat := []byte(`{"model":"gpt-5-mini","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	out, err = RequestForEndpointFrom("openai", "gpt-5-mini", chat, false, EndpointChatCompletions)
	if err != nil || gjson.GetBytes(out, "max_tokens").Int() != 5 {
		t.Fatalf("the chat endpoint must not be clamped: %s, %v", out, err)
	}
}

func TestResponsesSSEToOpenAIChatStream(t *testing.T) {
	t.Parallel()

	original := []byte(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	translated, err := RequestForEndpointFrom("openai", "gpt-5.6-sol", original, true, EndpointResponses)
	if err != nil {
		t.Fatalf("RequestForEndpointFrom() error = %v", err)
	}
	frames := []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"status\":\"in_progress\",\"output\":[]}}\n\n",
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"m1\",\"role\":\"assistant\",\"content\":[]}}\n\n",
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"m1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.6-sol\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}}\n\n",
	}
	var state any
	var joined strings.Builder
	for _, frame := range frames {
		out, errStream := StreamFromEndpoint(context.Background(), EndpointResponses, "openai", "gpt-5.6-sol", original, translated, []byte(frame), &state)
		if errStream != nil {
			t.Fatalf("StreamFromEndpoint() error = %v", errStream)
		}
		for _, chunk := range out {
			joined.Write(chunk)
		}
	}
	if !strings.Contains(joined.String(), `"content":"ok"`) {
		t.Fatalf("chat stream did not carry the text delta: %s", joined.String())
	}
}

func TestResponsesToChatCarriesToolCallsAndUsage(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"resp_2","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12,"input_tokens_details":{"cached_tokens":4}}}`)
	got, err := ResponseFromEndpoint(context.Background(), EndpointResponses, "openai", "gpt-5.6-sol", nil, nil, body)
	if err != nil {
		t.Fatalf("ResponseFromEndpoint() error = %v", err)
	}
	if gjson.GetBytes(got, "choices.0.finish_reason").String() != "tool_calls" ||
		gjson.GetBytes(got, "choices.0.message.tool_calls.0.function.name").String() != "lookup" ||
		gjson.GetBytes(got, "choices.0.message.tool_calls.0.id").String() != "call_1" {
		t.Fatalf("tool call not carried: %s", got)
	}
	if gjson.GetBytes(got, "usage.prompt_tokens").Int() != 9 || gjson.GetBytes(got, "usage.completion_tokens").Int() != 3 ||
		gjson.GetBytes(got, "usage.prompt_tokens_details.cached_tokens").Int() != 4 {
		t.Fatalf("usage not carried: %s", got)
	}
}
