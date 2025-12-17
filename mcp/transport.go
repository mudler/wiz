package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/mudler/wiz/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// commandTransport creates a new transport for a command
func commandTransport(cmd string, args []string, env ...string) mcp.Transport {
	command := exec.Command(cmd, args...)
	command.Env = os.Environ()
	command.Env = append(command.Env, env...)

	transport := &mcp.CommandTransport{Command: command}
	return transport
}

func StartTransports(ctx context.Context, cfg types.Config) ([]mcp.Transport, error) {
	// Set MCP servers
	bashMCPServerTransport, bashMCPServerClient := mcp.NewInMemoryTransports()

	go func() {
		if err := startBashMCPServer(ctx, bashMCPServerTransport); err != nil {
			fmt.Fprintf(os.Stderr, "MCP server error: %v\n", err)
		}
	}()

	transports := []mcp.Transport{bashMCPServerClient}

	for _, c := range cfg.MCPServers {
		envs := []string{}
		for k, v := range c.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", k, v))
		}
		transports = append(transports, commandTransport(c.Command, c.Args, envs...))
	}

	return transports, nil
}
