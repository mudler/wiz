package types

import (
	"bytes"
	"os"
	"os/user"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// AgentOptions holds configuration for the cogito ExecuteTools function
type AgentOptions struct {
	Iterations     int  `yaml:"iterations"`
	MaxAttempts    int  `yaml:"max_attempts"`
	MaxRetries     int  `yaml:"max_retries"`
	ForceReasoning bool `yaml:"force_reasoning"`
}

// Config holds configuration for creating a new session
type Config struct {
	Model        string               `yaml:"model"`
	APIKey       string               `yaml:"api_key"`
	BaseURL      string               `yaml:"base_url"`
	LogLevel     string               `yaml:"log_level"`
	Prompt       string               `yaml:"prompt"`
	MCPServers   map[string]MCPServer `yaml:"mcp_servers"`
	AgentOptions AgentOptions         `yaml:"agent_options"`
}

func (c *Config) GetPrompt() string {
	tmpl := template.New("").Funcs(sprig.FuncMap())

	data := bytes.NewBuffer([]byte{})

	currentDirectory, err := os.Getwd()
	if err != nil {
		currentDirectory = ""
	}
	currentUser, err := user.Current()
	if err != nil {
		currentUser = &user.User{}
	}

	if err := tmpl.Execute(data, struct {
		Config           Config
		CurrentDirectory string
		CurrentUser      string
	}{
		Config:           *c,
		CurrentDirectory: currentDirectory,
		CurrentUser:      currentUser.Username,
	}); err != nil {
		return ""
	}

	return data.String()
}

type MCPServer struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}
