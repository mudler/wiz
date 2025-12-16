package config

import (
	"os"
	"path/filepath"

	"github.com/mudler/aish/types"

	"gopkg.in/yaml.v3"
)

// configPaths returns the list of config file paths to try, in order of priority
func configPaths() []string {
	var paths []string

	// First priority: XDG config directory
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		paths = append(paths, filepath.Join(xdgConfig, "aish", "config.yaml"))
	}

	// Second priority: ~/.config/aish/config.yaml
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "aish", "config.yaml"))
		// Third priority: ~/.aish.yaml
		paths = append(paths, filepath.Join(home, ".aish.yaml"))
	}

	return paths
}

// loadFromFile attempts to load config from the first existing config file
func loadFromFile() types.Config {
	var cfg types.Config

	for _, path := range configPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}

		// Found and parsed a config file
		break
	}

	return cfg
}

// Load loads the configuration from YAML file and environment variables.
// Environment variables take precedence over YAML config.
func Load() types.Config {
	// Load from YAML file first
	cfg := loadFromFile()

	// Override with environment variables if set
	if model := os.Getenv("MODEL"); model != "" {
		cfg.Model = model
	}
	if apiKey := os.Getenv("API_KEY"); apiKey != "" {
		cfg.APIKey = apiKey
	}
	if baseURL := os.Getenv("BASE_URL"); baseURL != "" {
		cfg.BaseURL = baseURL
	}

	if cfg.Prompt == "" {
		cfg.Prompt = `
You are a Operative System terminal assistant that helps the user into automatizing common tasks, and can also do perform coding tasks.
You will use the tools at your disposal to fullfill the user request, and, for instance run bash scripts to execute and automate things.

Current directory: {{.CurrentDirectory}}
Current user: {{.CurrentUser}}
`
	}

	// Set default cogito options
	if cfg.AgentOptions.Iterations == 0 {
		cfg.AgentOptions.Iterations = 10
	}
	if cfg.AgentOptions.MaxAttempts == 0 {
		cfg.AgentOptions.MaxAttempts = 3
	}
	if cfg.AgentOptions.MaxRetries == 0 {
		cfg.AgentOptions.MaxRetries = 3
	}
	// ForceReasoning defaults to false (zero value), which is intentional
	// Users must explicitly enable it in config

	return cfg
}
