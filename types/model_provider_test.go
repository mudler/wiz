package types

import "testing"

func TestResolvedModelProvidersAreIndependent(t *testing.T) {
	cfg := Config{
		Provider: "codex", Model: "main-model",
		PromptInjectionProtection: PromptInjectionProtectionConfig{
			Enabled: true,
			Classifier: ModelProviderConfig{
				Provider: "openai", Model: "classifier-model",
				BaseURL: "http://classifier.invalid/v1", APIKey: "classifier-key",
			},
		},
	}
	main := cfg.ResolvedMainModel()
	classifier := cfg.ResolvedClassifierModel()
	if main.Provider != "codex" || main.Model != "main-model" {
		t.Fatalf("main = %#v", main)
	}
	if classifier.Provider != "openai" || classifier.Model != "classifier-model" ||
		classifier.BaseURL != "http://classifier.invalid/v1" || classifier.APIKey != "classifier-key" {
		t.Fatalf("classifier = %#v", classifier)
	}
}

func TestResolvedMainModelPreservesLegacyOpenAIConfig(t *testing.T) {
	cfg := Config{Model: "legacy", APIKey: "key", BaseURL: "http://legacy.invalid/v1"}
	main := cfg.ResolvedMainModel()
	if main.Provider != "openai" || main.Model != cfg.Model || main.APIKey != cfg.APIKey || main.BaseURL != cfg.BaseURL {
		t.Fatalf("main = %#v", main)
	}
	classifier := cfg.ResolvedClassifierModel()
	if classifier.Provider != main.Provider || classifier.Model != main.Model ||
		classifier.APIKey != main.APIKey || classifier.BaseURL != main.BaseURL {
		t.Fatalf("classifier = %#v, want inherited %#v", classifier, main)
	}
}

func TestClassifierPartialOverrideInheritsTopLevelEndpoint(t *testing.T) {
	cfg := Config{
		Model: "main", APIKey: "key", BaseURL: "https://api.example/v1",
		PromptInjectionProtection: PromptInjectionProtectionConfig{
			Classifier: ModelProviderConfig{Model: "classifier"},
		},
	}
	classifier := cfg.ResolvedClassifierModel()
	if classifier.Provider != "openai" || classifier.Model != "classifier" ||
		classifier.APIKey != "key" || classifier.BaseURL != "https://api.example/v1" {
		t.Fatalf("classifier = %#v", classifier)
	}
}

func TestResolvedClassifierSupportsCodexIndependentOfOpenAIMain(t *testing.T) {
	cfg := Config{
		Model: "remote-main", BaseURL: "https://api.example/v1",
		PromptInjectionProtection: PromptInjectionProtectionConfig{
			Classifier: ModelProviderConfig{Provider: "codex", Model: "classifier"},
		},
	}
	if got := cfg.ResolvedMainModel().Provider; got != "openai" {
		t.Fatalf("main provider = %q", got)
	}
	classifier := cfg.ResolvedClassifierModel()
	if classifier.Provider != "codex" || classifier.Model != "classifier" {
		t.Fatalf("classifier = %#v", classifier)
	}
}
