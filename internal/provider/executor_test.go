package provider

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestCopilotHeadersUseRecognizedIntegration(t *testing.T) {
	headers := copilotHeaders("test-token", false, copilotInteractionContext{
		ID:        "session-1",
		Type:      "conversation-agent",
		Initiator: "user",
	})

	expected := map[string]string{
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Plugin-Version":  "copilot-chat/0.35.0",
		"Editor-Version":         "vscode/1.107.0",
		"OpenAI-Intent":          "conversation-edits",
		"User-Agent":             "GitHubCopilotChat/0.35.0",
		"X-GitHub-Api-Version":   "2025-04-01",
		"X-Initiator":            "user",
		"X-Interaction-Id":       "session-1",
		"X-Interaction-Type":     "conversation-agent",
	}

	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if headers.Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id is empty")
	}
	if headers.Get("X-Request-Id") == headers.Get("X-Interaction-Id") {
		t.Fatal("X-Request-Id must not be reused as the interaction id")
	}
	if headers.Get("X-Agent-Task-Id") != "" {
		t.Fatalf("X-Agent-Task-Id = %q, want empty", headers.Get("X-Agent-Task-Id"))
	}
}

func TestCopilotInteractionKeepsOpenCodeSessionAndMarksToolTurns(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("X-Session-Id", "ses_parent")
	headers.Set("x-opencode-session", "ses_child")
	user := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:           "gpt-5.4",
		Headers:         headers,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"fix the ports"}]}`),
	}}
	first := copilotInteraction(user, nil)
	if first.ID != "ses_parent" || first.Initiator != "user" || first.Type != "conversation-agent" {
		t.Fatalf("user turn = %+v", first)
	}

	tool := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Model:   "gpt-5.4",
		Headers: headers,
		OriginalRequest: []byte(`{"messages":[
			{"role":"user","content":"fix the ports"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		]}`),
	}}
	second := copilotInteraction(tool, nil)
	if second.ID != first.ID || second.Initiator != "agent" || second.Type != "conversation-agent" {
		t.Fatalf("tool turn = %+v, want same interaction as %+v", second, first)
	}

	translated := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the ports"}]},
		{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}
	]}`)
	if got := copilotInteraction(ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Headers: headers}}, translated); got.Initiator != "agent" || got.ID != "ses_parent" {
		t.Fatalf("responses tool turn = %+v", got)
	}
}

func TestCopilotInteractionUsesParentSessionForSubagents(t *testing.T) {
	t.Parallel()

	headers := http.Header{}
	headers.Set("X-Session-Id", "ses_child")
	headers.Set("x-parent-session-id", "ses_parent")
	got := copilotInteraction(ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		Headers:         headers,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"explore this"}]}`),
	}}, nil)
	if got.ID != "ses_parent" || got.Type != "conversation-subagent" || got.Initiator != "agent" {
		t.Fatalf("subagent turn = %+v", got)
	}
}

func TestCopilotDerivedSessionStaysStableWithoutClientSession(t *testing.T) {
	t.Parallel()

	firstBody := []byte(`{"system":"be careful","messages":[{"role":"user","content":"fix the ports"}]}`)
	continued := []byte(`{"system":"be careful","messages":[
		{"role":"user","content":"fix the ports"},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}
	]}`)
	req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gpt-5.4"}}
	first := copilotInteraction(ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gpt-5.4", OriginalRequest: firstBody}}, nil)
	second := copilotInteraction(ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "gpt-5.4", OriginalRequest: continued}}, nil)
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("derived ids = %q and %q", first.ID, second.ID)
	}
	if first.Initiator != "user" || second.Initiator != "agent" {
		t.Fatalf("initiators = %s then %s", first.Initiator, second.Initiator)
	}
	if req.Model != "gpt-5.4" {
		t.Fatal("request mutated")
	}
}

func TestAuthorizationRefreshKeepsInteractionIdentity(t *testing.T) {
	t.Parallel()

	original := copilotHeaders("expired", true, copilotInteractionContext{
		ID:        "ses_parent",
		Type:      "conversation-agent",
		Initiator: "agent",
	})
	refreshed := withCopilotAuthorization(original, "fresh")
	if refreshed.Get("Authorization") != "Bearer fresh" {
		t.Fatalf("authorization = %q", refreshed.Get("Authorization"))
	}
	if refreshed.Get("X-Interaction-Id") != "ses_parent" || refreshed.Get("X-Request-Id") != original.Get("X-Request-Id") || refreshed.Get("X-Initiator") != "agent" {
		t.Fatalf("refreshed headers = %v", refreshed)
	}
	if original.Get("Authorization") != "Bearer expired" {
		t.Fatal("refresh mutated the original authorization header")
	}
}

func TestCountTokensReturnsClaudeInputTokens(t *testing.T) {
	t.Parallel()

	resp, err := (&Service{}).CountTokens(ExecuteRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "claude",
			OriginalRequest: []byte(`{"model":"gpt-5.6-sol","system":"Be concise.","messages":[{"role":"user","content":"hello world"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d; response=%s", got, resp.Payload)
	}
}
