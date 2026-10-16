package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/redact"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/sse"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/translate"
	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const (
	copilotUserAgent     = "GitHubCopilotChat/0.35.0"
	copilotEditorVersion = "vscode/1.107.0"
	copilotPluginVersion = "copilot-chat/0.35.0"
	copilotIntegrationID = "vscode-chat"
	copilotAPIVersion    = "2025-04-01"
)

type ExecuteRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type HTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return pluginapi.ExecutorResponse{}, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorResponse{}, errParse
	}
	endpoint, token, errEndpoint := s.endpointForModel(ctx, req.HostCallbackID, req.AuthID, storage, req.Model)
	if errEndpoint != nil {
		return pluginapi.ExecutorResponse{}, errEndpoint
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, req.Model, req.Payload, false, endpoint)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	resp, token, errDo := s.doModelRequest(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody, false, copilotHeaders(token.Token, false, copilotInteraction(req, requestBody)))
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	body, errResponse := translate.ResponseFromEndpoint(ctx, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, resp.Body)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errResponse.Error(), http.StatusBadGateway)
	}
	return pluginapi.ExecutorResponse{
		Payload: body,
		Headers: filterResponseHeaders(resp.Headers),
		Metadata: map[string]any{
			"copilot_endpoint": endpoint,
			"token_expires_at": token.ExpiresAt.UTC().Format(http.TimeFormat),
		},
	}, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req ExecuteRequest) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusError("invalid_request", "stream_id is required", http.StatusBadRequest)
	}
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return nil, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	endpoint, token, errEndpoint := s.endpointForModel(ctx, req.HostCallbackID, req.AuthID, storage, req.Model)
	if errEndpoint != nil {
		return nil, errEndpoint
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, req.Model, req.Payload, true, endpoint)
	if errTranslate != nil {
		return nil, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	upstream, token, errOpen := s.openModelStream(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody, copilotHeaders(token.Token, true, copilotInteraction(req, requestBody)))
	if errOpen != nil {
		return nil, errOpen
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		body, errCollect := s.collectStreamError(ctx, upstream)
		if errCollect != nil {
			return nil, errCollect
		}
		return nil, upstreamStatusError(upstream.StatusCode, redact.ErrorBody(body, token.Token, storage.GitHubAccessToken))
	}
	go s.pumpStream(req.StreamID, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, upstream)
	headers := filterResponseHeaders(upstream.Headers)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	return headers, nil
}

func (s *Service) doModelRequest(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte, stream bool, headers http.Header) (transport.Response, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: headers,
		Body:    body,
	}
	resp, errDo := s.host.Do(ctx, callbackID, request)
	if errDo != nil {
		return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.invalidateAuth(authID)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Response{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = withCopilotAuthorization(headers, token.Token)
		resp, errDo = s.host.Do(ctx, callbackID, request)
		if errDo != nil {
			return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint after token refresh: %w", errDo)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, token, upstreamStatusError(resp.StatusCode, redact.ErrorBody(resp.Body, token.Token, storage.GitHubAccessToken))
	}
	return resp, token, nil
}

func (s *Service) openModelStream(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte, headers http.Header) (transport.Stream, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: headers,
		Body:    body,
	}
	stream, errOpen := s.host.OpenStream(ctx, callbackID, request)
	if errOpen != nil {
		return transport.Stream{}, token, fmt.Errorf("open Copilot model stream: %w", errOpen)
	}
	if stream.StatusCode == http.StatusUnauthorized {
		_ = s.host.CloseStream(ctx, stream.ID)
		s.invalidateAuth(authID)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Stream{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = withCopilotAuthorization(headers, token.Token)
		stream, errOpen = s.host.OpenStream(ctx, callbackID, request)
		if errOpen != nil {
			return transport.Stream{}, token, fmt.Errorf("open Copilot model stream after token refresh: %w", errOpen)
		}
	}
	return stream, token, nil
}

func (s *Service) collectStreamError(ctx context.Context, stream transport.Stream) ([]byte, error) {
	defer func() { _ = s.host.CloseStream(context.Background(), stream.ID) }()
	var body []byte
	for {
		chunk, errRead := s.host.ReadStream(ctx, stream.ID)
		if errRead != nil {
			return nil, fmt.Errorf("read Copilot error stream: %w", errRead)
		}
		body = append(body, chunk.Payload...)
		if chunk.Error != "" {
			return nil, fmt.Errorf("read Copilot error stream: %s", redact.Text(chunk.Error))
		}
		if chunk.Done {
			return body, nil
		}
	}
}

func (s *Service) pumpStream(outputID, endpoint, destination, model string, original, translated []byte, upstream transport.Stream) {
	ctx := context.Background()
	var terminalErr error
	defer func() {
		_ = s.host.CloseStream(ctx, upstream.ID)
		message := ""
		if terminalErr != nil {
			message = redact.Text(terminalErr.Error())
		}
		s.host.CloseOutput(ctx, outputID, message)
	}()

	decoder := &sse.Decoder{}
	var state any
	emit := func(frame []byte) error {
		frames, errTranslate := translate.StreamFromEndpoint(ctx, endpoint, destination, model, original, translated, frame, &state)
		if errTranslate != nil {
			return errTranslate
		}
		for _, output := range frames {
			if len(output) == 0 {
				continue
			}
			if errEmit := s.host.Emit(ctx, outputID, output); errEmit != nil {
				return errEmit
			}
		}
		return nil
	}

	for {
		chunk, errRead := s.host.ReadStream(ctx, upstream.ID)
		if errRead != nil {
			terminalErr = fmt.Errorf("read Copilot stream: %w", errRead)
			return
		}
		if chunk.Error != "" {
			terminalErr = fmt.Errorf("Copilot stream error: %s", redact.Text(chunk.Error))
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if errEmit := emit(frame); errEmit != nil {
				terminalErr = fmt.Errorf("translate Copilot stream: %w", errEmit)
				return
			}
		}
		if chunk.Done {
			if trailing := decoder.Flush(); len(trailing) > 0 {
				if errEmit := emit(trailing); errEmit != nil {
					terminalErr = fmt.Errorf("translate final Copilot stream frame: %w", errEmit)
				}
			}
			return
		}
	}
}

func normalizeRequestFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "responses", "openai-response", "openai-responses":
		return "openai-response"
	case "claude", "anthropic":
		return "claude"
	default:
		return ""
	}
}

func (s *Service) HTTP(ctx context.Context, req HTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorHTTPResponse{}, errParse
	}
	token, errToken := s.copilotToken(ctx, req.HostCallbackID, req.AuthID, storage)
	if errToken != nil {
		return pluginapi.ExecutorHTTPResponse{}, errToken
	}
	target, errURL := url.Parse(strings.TrimSpace(req.URL))
	if errURL != nil || target.Hostname() == "" {
		return pluginapi.ExecutorHTTPResponse{}, statusError("invalid_request", "executor HTTP URL is invalid", http.StatusBadRequest)
	}
	base, _ := url.Parse(token.APIBaseURL)
	if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return pluginapi.ExecutorHTTPResponse{}, statusError("forbidden_url", "executor HTTP URL is outside the authenticated Copilot API origin", http.StatusForbidden)
	}
	headers := cloneHeader(req.Headers)
	headers.Set("Authorization", "Bearer "+token.Token)
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	resp, errDo := s.host.Do(ctx, req.HostCallbackID, transport.Request{
		Method:  req.Method,
		URL:     target.String(),
		Headers: headers,
		Body:    append([]byte(nil), req.Body...),
	})
	if errDo != nil {
		return pluginapi.ExecutorHTTPResponse{}, errDo
	}
	return pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    filterResponseHeaders(resp.Headers),
		Body:       resp.Body,
	}, nil
}

type copilotInteractionContext struct {
	ID        string
	Type      string
	Initiator string
}

func copilotHeaders(token string, stream bool, interaction copilotInteractionContext) http.Header {
	requestID, _ := randomIdentifier(16)
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Copilot-Integration-Id", copilotIntegrationID)
	headers.Set("Editor-Plugin-Version", copilotPluginVersion)
	headers.Set("Editor-Version", copilotEditorVersion)
	headers.Set("OpenAI-Intent", "conversation-edits")
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	headers.Set("X-Initiator", interaction.Initiator)
	headers.Set("X-Interaction-Id", interaction.ID)
	headers.Set("X-Interaction-Type", interaction.Type)
	headers.Set("X-Request-Id", requestID)
	return headers
}

func withCopilotAuthorization(headers http.Header, token string) http.Header {
	cloned := cloneHeader(headers)
	cloned.Set("Authorization", "Bearer "+token)
	return cloned
}

func copilotInteraction(req ExecuteRequest, translated []byte) copilotInteractionContext {
	sessionID := copilotSessionID(req)
	interactionType := "conversation-agent"
	if sessionID == "" {
		sessionID = copilotDerivedSessionID(req, translated)
	}
	initiator := "user"
	if copilotAgentTurn(req, translated) {
		initiator = "agent"
	}
	if copilotParentSession(req) != "" {
		interactionType = "conversation-subagent"
		initiator = "agent"
	}
	return copilotInteractionContext{
		ID:        sessionID,
		Type:      interactionType,
		Initiator: initiator,
	}
}

func copilotSessionID(req ExecuteRequest) string {
	parent := copilotParentSession(req)
	if parent != "" {
		return parent
	}
	for _, name := range []string{
		"X-Session-Id",
		"X-Session-ID",
		"x-session-affinity",
		"x-opencode-session",
		"X-Opencode-Session",
		"X-Opencode-Session-Id",
		"Session-Id",
		"Session_id",
	} {
		if value := headerValue(req.Headers, name); value != "" {
			return value
		}
	}
	if value := metadataString(req.Metadata, "execution_session_id"); value != "" {
		return value
	}
	if value := metadataString(req.Metadata, "canonical_session_id"); value != "" {
		return value
	}
	if value := metadataString(req.Metadata, "derived_session_id"); value != "" {
		return value
	}
	return claudeMetadataSessionID(req.OriginalRequest)
}

func copilotParentSession(req ExecuteRequest) string {
	for _, name := range []string{"x-parent-session-id", "x-opencode-parent-session-id"} {
		if value := headerValue(req.Headers, name); value != "" {
			return value
		}
	}
	return metadataString(req.Metadata, "parent_session_id")
}

func copilotDerivedSessionID(req ExecuteRequest, translated []byte) string {
	material := copilotSessionMaterial(req.OriginalRequest)
	if len(material) == 0 {
		material = copilotSessionMaterial(translated)
	}
	if len(material) == 0 {
		requestID, errID := randomIdentifier(16)
		if errID != nil {
			return "copilot-request"
		}
		return requestID
	}
	sum := sha256.New()
	sum.Write([]byte(strings.TrimSpace(req.Model)))
	sum.Write([]byte{0})
	sum.Write(material)
	return hex.EncodeToString(sum.Sum(nil))
}

func copilotSessionMaterial(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	var material []byte
	if instructions := strings.TrimSpace(root.Get("instructions").String()); instructions != "" {
		material = append(material, instructions...)
		material = append(material, 0)
	}
	if system := claudeSystemMaterial(root.Get("system")); system != "" {
		material = append(material, system...)
		material = append(material, 0)
	}
	if first := firstUserMaterial(root); first != "" {
		material = append(material, first...)
	}
	return material
}

func claudeSystemMaterial(system gjson.Result) string {
	if system.Type == gjson.String {
		return strings.TrimSpace(system.String())
	}
	var text strings.Builder
	system.ForEach(func(_, part gjson.Result) bool {
		if part.Type == gjson.String {
			text.WriteString(strings.TrimSpace(part.String()))
			return true
		}
		if strings.TrimSpace(part.Get("type").String()) == "text" {
			text.WriteString(strings.TrimSpace(part.Get("text").String()))
		}
		return true
	})
	return strings.TrimSpace(text.String())
}

func firstUserMaterial(root gjson.Result) string {
	found := ""
	root.Get("messages").ForEach(func(_, message gjson.Result) bool {
		if strings.TrimSpace(message.Get("role").String()) != "user" {
			return true
		}
		found = contentMaterial(message.Get("content"))
		return false
	})
	if found != "" {
		return found
	}
	root.Get("input").ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "", "message":
			if strings.TrimSpace(item.Get("role").String()) != "user" {
				return true
			}
			found = contentMaterial(item.Get("content"))
			return false
		}
		return true
	})
	return found
}

func contentMaterial(content gjson.Result) string {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	var text strings.Builder
	content.ForEach(func(_, part gjson.Result) bool {
		partType := strings.TrimSpace(part.Get("type").String())
		switch partType {
		case "", "text", "input_text", "output_text":
			if value := strings.TrimSpace(part.Get("text").String()); value != "" {
				text.WriteString(value)
			} else if part.Type == gjson.String {
				text.WriteString(strings.TrimSpace(part.String()))
			}
		}
		return true
	})
	return strings.TrimSpace(text.String())
}

func copilotAgentTurn(req ExecuteRequest, translated []byte) bool {
	if copilotAgentTurnBody(translated) || copilotAgentTurnBody(req.Payload) || copilotAgentTurnBody(req.OriginalRequest) {
		return true
	}
	return false
}

func copilotAgentTurnBody(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	input := root.Get("input")
	if input.IsArray() {
		last := lastArrayValue(input)
		switch strings.TrimSpace(last.Get("type").String()) {
		case "function_call_output", "function_call", "reasoning":
			return true
		case "message":
			if strings.TrimSpace(last.Get("role").String()) != "user" {
				return strings.TrimSpace(last.Get("role").String()) != ""
			}
			return !messageHasNonToolContent(last.Get("content"))
		}
	}
	messages := root.Get("messages")
	if !messages.IsArray() {
		return false
	}
	last := lastArrayValue(messages)
	role := strings.TrimSpace(last.Get("role").String())
	switch role {
	case "assistant", "tool":
		return true
	case "user":
		return !messageHasNonToolContent(last.Get("content"))
	default:
		return false
	}
}

func messageHasNonToolContent(content gjson.Result) bool {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) != ""
	}
	if !content.IsArray() {
		return false
	}
	hasNonTool := false
	content.ForEach(func(_, part gjson.Result) bool {
		partType := strings.TrimSpace(part.Get("type").String())
		if partType == "" && part.Type == gjson.String {
			partType = "text"
		}
		if partType != "tool_result" {
			hasNonTool = true
			return false
		}
		return true
	})
	return hasNonTool
}

func lastArrayValue(value gjson.Result) gjson.Result {
	var last gjson.Result
	value.ForEach(func(_, item gjson.Result) bool {
		last = item
		return true
	})
	return last
}

func claudeMetadataSessionID(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if gjson.Valid(userID) {
		if sessionID := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

func headerValue(headers http.Header, name string) string {
	if len(headers) == 0 {
		return ""
	}
	return strings.TrimSpace(headers.Get(name))
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func filterResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "content-type", "cache-control", "retry-after", "x-github-request-id", "x-request-id":
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

func cloneHeader(headers http.Header) http.Header {
	if len(headers) == 0 {
		return http.Header{}
	}
	return headers.Clone()
}
