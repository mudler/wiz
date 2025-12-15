package types

// Config holds configuration for creating a new session
type Config struct {
	Model      string
	APIKey     string
	BaseURL    string
	MCPServers map[string]MCPServer
}

type MCPServer struct {
	Command string
	Args    []string
	Env     map[string]string
}
