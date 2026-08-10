// Package llmprovider constructs cogito LLM transports from nib configuration.
package llmprovider

import (
	"fmt"
	"strings"

	"github.com/mudler/cogito"
	"github.com/mudler/cogito/clients"
	"github.com/mudler/nib/codexapp"
	"github.com/mudler/nib/types"
)

const (
	ProviderOpenAI = "openai"
	ProviderCodex  = "codex"
)

// New returns an independent LLM transport. OpenAI means any
// OpenAI-compatible HTTP endpoint, including a local LocalAI server.
func New(config types.ModelProviderConfig) (cogito.LLM, error) {
	return NewWithTemperature(config, 0)
}

// NewWithTemperature applies agent-specific sampling to OpenAI-compatible
// providers. Codex app-server owns its reasoning configuration and ignores the
// compatibility hint.
func NewWithTemperature(config types.ModelProviderConfig, temperature float32) (cogito.LLM, error) {
	switch normalize(config.Provider) {
	case ProviderOpenAI:
		llm := clients.NewLocalAILLM(config.Model, config.APIKey, config.BaseURL)
		llm.SetTemperature(temperature)
		llm.SetMetadata(config.Metadata)
		llm.SetReasoningEffort(config.ReasoningEffort)
		return llm, nil
	case ProviderCodex:
		return codexapp.New(codexapp.Config{
			Command: config.Command,
			Args:    config.Args,
			Model:   config.Model,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported LLM provider %q (want openai or codex)", config.Provider)
	}
}

func normalize(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "openai", "openai-compatible", "openai_compatible":
		return ProviderOpenAI
	case "codex", "codex-app-server", "codex_app_server":
		return ProviderCodex
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func IsCodex(config types.ModelProviderConfig) bool {
	return normalize(config.Provider) == ProviderCodex
}
