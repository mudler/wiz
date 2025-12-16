package types

import (
	"bytes"
	"os"
	"os/user"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// Config holds configuration for creating a new session
type Config struct {
	Model      string               `yaml:"model"`
	APIKey     string               `yaml:"api_key"`
	BaseURL    string               `yaml:"base_url"`
	LogLevel   string               `yaml:"log_level"`
	Prompt     string               `yaml:"prompt"`
	MCPServers map[string]MCPServer `yaml:"mcp_servers"`
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
