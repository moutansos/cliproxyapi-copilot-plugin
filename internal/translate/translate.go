package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

const (
	EndpointResponses       = "/responses"
	EndpointChatCompletions = "/chat/completions"
	EndpointMessages        = "/v1/messages"
)

var registry = builtin.Registry()

func ClaudeToResponses(model string, body []byte, stream bool) ([]byte, error) {
	return request(sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, model, body, stream)
}

func ResponsesToClaude(ctx context.Context, model string, original, translated, body []byte) ([]byte, error) {
	return response(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, body)
}

func RequestForEndpoint(model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	return RequestForEndpointFrom(sdktranslator.FormatOpenAIResponse.String(), model, body, stream, endpoint)
}

func RequestForEndpointFrom(source, model string, body []byte, stream bool, endpoint string) ([]byte, error) {
	from := sdktranslator.FromString(source)
	to, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	var out []byte
	if from == to {
		out = append([]byte(nil), body...)
	} else {
		out, err = request(from, to, model, body, stream)
	}
	if err != nil {
		return nil, err
	}
	out, err = setModelAndStream(out, model, stream)
	if err != nil || endpoint != EndpointResponses {
		return out, err
	}
	return clampResponsesOutputTokens(out)
}

// minResponsesOutputTokens is the smallest max_output_tokens Copilot's /responses endpoint
// accepts; smaller values are rejected with HTTP 400 instead of being honoured.
const minResponsesOutputTokens = 16

// clampResponsesOutputTokens raises max_output_tokens to the Copilot minimum when a client
// (or a translated Claude max_tokens) asks for fewer. Absent or larger values are unchanged.
func clampResponsesOutputTokens(body []byte) ([]byte, error) {
	var value map[string]any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return nil, fmt.Errorf("decode translated request: %w", errUnmarshal)
	}
	limit, ok := value["max_output_tokens"].(float64)
	if !ok || limit >= minResponsesOutputTokens {
		return body, nil
	}
	value["max_output_tokens"] = minResponsesOutputTokens
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode translated request: %w", errMarshal)
	}
	return out, nil
}

func ResponseToResponses(ctx context.Context, endpoint, model string, original, translated, body []byte) ([]byte, error) {
	return ResponseFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, body)
}

func ResponseFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, body []byte) ([]byte, error) {
	from, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	to := sdktranslator.FromString(destination)
	if from == to {
		if !json.Valid(body) {
			return nil, fmt.Errorf("Copilot API returned invalid JSON")
		}
		return append([]byte(nil), body...), nil
	}
	return response(ctx, from, to, model, original, translated, body)
}

func StreamToResponses(ctx context.Context, endpoint, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return StreamFromEndpoint(ctx, endpoint, sdktranslator.FormatOpenAIResponse.String(), model, original, translated, frame, state)
}

func StreamFromEndpoint(ctx context.Context, endpoint, destination, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	from, err := endpointFormat(endpoint)
	if err != nil {
		return nil, err
	}
	to := sdktranslator.FromString(destination)
	if from == to {
		return [][]byte{append([]byte(nil), frame...)}, nil
	}
	return stream(ctx, from, to, model, original, translated, frame, state)
}

func ResponsesSSEToClaude(ctx context.Context, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	return stream(ctx, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, model, original, translated, frame, state)
}

func request(from, to sdktranslator.Format, model string, body []byte, stream bool) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	if from == sdktranslator.FormatClaude && to == sdktranslator.FormatOpenAIResponse {
		return claudeRequestToResponses(model, body, stream)
	}
	if from == sdktranslator.FormatOpenAI && to == sdktranslator.FormatOpenAIResponse {
		// No official OpenAI-chat -> Responses route exists; go through Claude, whose
		// Responses bridge is this package's own.
		claudeBody := registry.TranslateRequest(from, sdktranslator.FormatClaude, model, body, stream)
		if len(claudeBody) == 0 || !json.Valid(claudeBody) {
			return nil, fmt.Errorf("official request translation from %s to %s failed", from, sdktranslator.FormatClaude)
		}
		return claudeRequestToResponses(model, claudeBody, stream)
	}
	if from != to && !registry.HasRequestTransformer(from, to) {
		intermediate, ok := intermediateFormat(from, to)
		if !ok {
			return nil, fmt.Errorf("official translator has no request route from %s to %s", from, to)
		}
		first := registry.TranslateRequest(from, intermediate, model, body, stream)
		out := registry.TranslateRequest(intermediate, to, model, first, stream)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop request translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateRequest(from, to, model, body, stream)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official request translation from %s to %s failed", from, to)
	}
	return out, nil
}

func response(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("response body is not valid JSON")
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesResponseToClaude(model, body)
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatOpenAI {
		// The official Claude -> chat non-stream converter only reads Claude SSE "data:" lines
		// and returns an empty completion for a Claude JSON body, so convert directly.
		return responsesResponseToChat(model, body)
	}
	if from != to && !registry.HasNonStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no response route from %s to %s", from, to)
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, false)
		first := registry.TranslateNonStream(ctx, from, intermediate, model, intermediateRequest, translated, body, nil)
		out := registry.TranslateNonStream(ctx, intermediate, to, model, original, intermediateRequest, first, nil)
		if len(out) == 0 || !json.Valid(out) {
			return nil, fmt.Errorf("official two-hop response translation from %s to %s failed", from, to)
		}
		return out, nil
	}
	out := registry.TranslateNonStream(ctx, from, to, model, original, translated, body, nil)
	if len(out) == 0 || !json.Valid(out) {
		return nil, fmt.Errorf("official response translation from %s to %s failed", from, to)
	}
	return out, nil
}

func stream(ctx context.Context, from, to sdktranslator.Format, model string, original, translated, frame []byte, state *any) ([][]byte, error) {
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatClaude {
		return responsesStreamToClaude(model, frame, state)
	}
	if from == sdktranslator.FormatOpenAIResponse && to == sdktranslator.FormatOpenAI {
		if state == nil {
			return nil, fmt.Errorf("stream translation via claude requires state")
		}
		hopState, okState := (*state).(*twoHopStreamState)
		if !okState {
			hopState = &twoHopStreamState{}
			*state = hopState
		}
		claudeRequest := registry.TranslateRequest(to, sdktranslator.FormatClaude, model, original, true)
		claudeFrames, errClaude := responsesStreamToClaude(model, frame, &hopState.First)
		if errClaude != nil {
			return nil, errClaude
		}
		var out [][]byte
		for _, claudeFrame := range claudeFrames {
			// The official Claude -> chat stream converter ignores any frame that does not
			// start with "data:", so hand it only the data line of each Claude SSE event.
			for _, dataLine := range sseDataLines(claudeFrame) {
				out = append(out, registry.TranslateStream(ctx, sdktranslator.FormatClaude, to, model, original, claudeRequest, dataLine, &hopState.Second)...)
			}
		}
		return out, nil
	}
	if from != to && !registry.HasStreamResponseTransformer(to, from) {
		intermediate, ok := intermediateFormat(to, from)
		if !ok {
			return nil, fmt.Errorf("official translator has no stream route from %s to %s", from, to)
		}
		if state == nil {
			return nil, fmt.Errorf("two-hop stream translation requires state")
		}
		hopState, okState := (*state).(*twoHopStreamState)
		if !okState {
			hopState = &twoHopStreamState{}
			*state = hopState
		}
		intermediateRequest := registry.TranslateRequest(to, intermediate, model, original, true)
		first := registry.TranslateStream(ctx, from, intermediate, model, intermediateRequest, translated, frame, &hopState.First)
		var out [][]byte
		for _, firstFrame := range first {
			out = append(out, registry.TranslateStream(ctx, intermediate, to, model, original, intermediateRequest, firstFrame, &hopState.Second)...)
		}
		return out, nil
	}
	out := registry.TranslateStream(ctx, from, to, model, original, translated, frame, state)
	if len(out) == 1 && bytes.Equal(out[0], frame) && from != to {
		return nil, fmt.Errorf("official stream translation from %s to %s fell back unchanged", from, to)
	}
	return out, nil
}

type twoHopStreamState struct {
	First  any
	Second any
}

func intermediateFormat(from, to sdktranslator.Format) (sdktranslator.Format, bool) {
	for _, candidate := range []sdktranslator.Format{
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
	} {
		if candidate != from && candidate != to &&
			registry.HasRequestTransformer(from, candidate) &&
			registry.HasRequestTransformer(candidate, to) {
			return candidate, true
		}
	}
	return "", false
}

func endpointFormat(endpoint string) (sdktranslator.Format, error) {
	switch endpoint {
	case EndpointResponses:
		return sdktranslator.FormatOpenAIResponse, nil
	case EndpointChatCompletions:
		return sdktranslator.FormatOpenAI, nil
	case EndpointMessages:
		return sdktranslator.FormatClaude, nil
	default:
		return "", fmt.Errorf("unsupported Copilot endpoint %q", endpoint)
	}
}

func setModelAndStream(body []byte, model string, streamEnabled bool) ([]byte, error) {
	var value map[string]any
	if errUnmarshal := json.Unmarshal(body, &value); errUnmarshal != nil {
		return nil, fmt.Errorf("decode translated request: %w", errUnmarshal)
	}
	value["model"] = model
	value["stream"] = streamEnabled
	out, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, fmt.Errorf("encode translated request: %w", errMarshal)
	}
	return out, nil
}
