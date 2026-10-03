package main

import "testing"

func TestPluginRegistrationDeclaresQuotaProvider(t *testing.T) {
	t.Parallel()

	if !pluginRegistration().Capabilities.QuotaProvider {
		t.Fatal("quota_provider is not declared")
	}
}

func TestPluginRegistrationDeclaresOpenAIChatFormat(t *testing.T) {
	t.Parallel()

	caps := pluginRegistration().Capabilities
	for name, formats := range map[string][]string{"input": caps.ExecutorInputFormats, "output": caps.ExecutorOutputFormats} {
		found := false
		for _, format := range formats {
			if format == "openai" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s formats %v do not include openai", name, formats)
		}
	}
}
