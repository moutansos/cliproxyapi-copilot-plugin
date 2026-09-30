package provider

import (
	"testing"

	"github.com/arthur-sommer-etc/cliproxyapi-copilot-plugin/internal/translate"
)

func TestSelectEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     upstreamModel
		want      string
		wantError bool
	}{
		{
			name:  "responses preferred",
			model: upstreamModel{ID: "model-a", SupportedEndpoints: []string{"/chat/completions", "/responses"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "messages fallback",
			model: upstreamModel{ID: "model-b", SupportedEndpoints: []string{"messages"}},
			want:  translate.EndpointMessages,
		},
		{
			name:  "sol forced to responses",
			model: upstreamModel{ID: "gpt-5.6-sol", SupportedEndpoints: []string{"/chat/completions"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "terra forced to responses",
			model: upstreamModel{ID: "GPT-5.6-TERRA"},
			want:  translate.EndpointResponses,
		},
		{
			name:  "anthropic vendor prefers messages",
			model: upstreamModel{ID: "claude-opus-5.5", Vendor: "Anthropic", SupportedEndpoints: []string{"/chat/completions", "/responses", "/v1/messages"}},
			want:  translate.EndpointMessages,
		},
		{
			name:  "claude id prefers messages without vendor",
			model: upstreamModel{ID: "Claude-Sonnet-5", SupportedEndpoints: []string{"/chat/completions", "/v1/messages"}},
			want:  translate.EndpointMessages,
		},
		{
			name:  "claude without messages keeps responses order",
			model: upstreamModel{ID: "claude-haiku-4.5", Vendor: "Anthropic", SupportedEndpoints: []string{"/chat/completions", "/responses"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "non anthropic model keeps responses first",
			model: upstreamModel{ID: "gpt-5.5", Vendor: "Azure OpenAI", SupportedEndpoints: []string{"/chat/completions", "/responses", "/v1/messages"}},
			want:  translate.EndpointResponses,
		},
		{
			name:      "unsupported",
			model:     upstreamModel{ID: "embedding-model", SupportedEndpoints: []string{"/embeddings"}},
			wantError: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectEndpoint(test.model)
			if test.wantError {
				if err == nil {
					t.Fatal("expected an endpoint selection error")
				}
				return
			}
			if err != nil {
				t.Fatalf("select endpoint: %v", err)
			}
			if got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeModelsAddsResponsesMetadata(t *testing.T) {
	t.Parallel()

	models := normalizeModels([]upstreamModel{
		{
			ID:                 "gpt-5.6-sol",
			SupportedEndpoints: []string{"/chat/completions"},
			Capabilities: modelCapabilities{
				Supports: modelSupports{Streaming: true, ToolCalls: true, Vision: true},
				Limits:   modelLimits{MaxPromptTokens: 100, MaxOutputTokens: 20},
			},
		},
	})
	if len(models) != 1 || !contains(models[0].SupportedEndpoints, translate.EndpointResponses) {
		t.Fatalf("responses endpoint was not added: %#v", models)
	}
	info := modelInfos(models)[0]
	if !contains(info.SupportedGenerationMethods, translate.EndpointResponses) {
		t.Fatalf("model metadata omits responses endpoint: %#v", info.SupportedGenerationMethods)
	}
	if !contains(info.SupportedInputModalities, "IMAGE") {
		t.Fatalf("model metadata omits image support: %#v", info.SupportedInputModalities)
	}
}

func TestFilterModelsExcludesConfiguredPrefixes(t *testing.T) {
	t.Parallel()

	models := filterModels([]upstreamModel{
		{ID: "gpt-5.6-sol"},
		{ID: "claude-sonnet-5"},
		{ID: "Claude-Haiku-4.5"},
	}, []string{"claude-"})
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("filtered models = %#v", models)
	}
}

func TestNormalizeModelPrefixes(t *testing.T) {
	t.Parallel()

	got := normalizeModelPrefixes([]string{" Claude- ", "claude-", "", "GPT-"})
	if len(got) != 2 || got[0] != "claude-" || got[1] != "gpt-" {
		t.Fatalf("normalized prefixes = %#v", got)
	}
}

func TestSelectEndpointForOpenAIChatClient(t *testing.T) {
	t.Parallel()

	all := []string{"/chat/completions", "/responses", "/v1/messages"}
	tests := []struct {
		name  string
		model upstreamModel
		want  string
	}{
		{"claude model goes straight to chat", upstreamModel{ID: "claude-opus-5.5", Vendor: "Anthropic", SupportedEndpoints: all}, translate.EndpointChatCompletions},
		{"gpt model goes straight to chat", upstreamModel{ID: "gpt-5-mini", SupportedEndpoints: []string{"/chat/completions", "/responses"}}, translate.EndpointChatCompletions},
		{"responses-only model falls back to responses", upstreamModel{ID: "gpt-6-astra", SupportedEndpoints: []string{"/responses"}}, translate.EndpointResponses},
		{"sol stays on responses", upstreamModel{ID: "gpt-5.6-sol", SupportedEndpoints: all}, translate.EndpointResponses},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectEndpointFor(test.model, "openai")
			if err != nil || got != test.want {
				t.Fatalf("selectEndpointFor() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	if got, _ := selectEndpointFor(upstreamModel{ID: "claude-opus-5.5", Vendor: "Anthropic", SupportedEndpoints: all}, "claude"); got != translate.EndpointMessages {
		t.Fatalf("claude client on a Claude model = %q, want %q", got, translate.EndpointMessages)
	}
}

func TestNormalizeRequestFormatAcceptsOpenAIChat(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"openai", "OpenAI", "chat", "chat-completions"} {
		if got := normalizeRequestFormat(value); got != "openai" {
			t.Fatalf("normalizeRequestFormat(%q) = %q, want openai", value, got)
		}
	}
	if got := normalizeRequestFormat("gemini"); got != "" {
		t.Fatalf("normalizeRequestFormat(gemini) = %q, want unsupported", got)
	}
}
