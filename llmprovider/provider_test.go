package llmprovider

import (
	"testing"

	"github.com/mudler/nib/codexapp"
	"github.com/mudler/nib/types"
)

func TestNewSelectsProviders(t *testing.T) {
	openaiLLM, err := New(types.ModelProviderConfig{Provider: "openai-compatible", Model: "local", BaseURL: "http://localhost:8080/v1"})
	if err != nil || openaiLLM == nil {
		t.Fatalf("openai-compatible provider: llm=%T err=%v", openaiLLM, err)
	}
	codexLLM, err := New(types.ModelProviderConfig{Provider: "codex", Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codexLLM.(*codexapp.LLM); !ok {
		t.Fatalf("codex provider returned %T", codexLLM)
	}
}

func TestNewRejectsUnknownProvider(t *testing.T) {
	if _, err := New(types.ModelProviderConfig{Provider: "mystery"}); err == nil {
		t.Fatal("unknown provider was accepted")
	}
}
