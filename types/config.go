package types

// Config holds configuration for creating a new session
type Config struct {
	Model      string               `yaml:"model"`
	APIKey     string               `yaml:"api_key"`
	BaseURL    string               `yaml:"base_url"`
	LogLevel   string               `yaml:"log_level"`
	MCPServers map[string]MCPServer `yaml:"mcp_servers"`
}

type MCPServer struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}
