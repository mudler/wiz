package mcp

import (
	"context"

	"github.com/mudler/nib/llmprovider"
	"github.com/mudler/nib/provenance"
	"github.com/mudler/nib/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StartWebMCPServer starts the web MCP server exposing web_fetch and web_search.
// web_fetch reuses the session's main model and request options (model, API key,
// base URL, metadata, and reasoning effort) for its extraction pass.
func StartWebMCPServer(ctx context.Context, transport mcp.Transport, cfg types.Config) error {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "web",
		Version: "v1.0.0",
	}, nil)

	llm, err := llmprovider.New(cfg.ResolvedMainModel())
	if err != nil {
		return err
	}
	classifier, err := provenance.ClassifierForConfig(cfg)
	if err != nil {
		return err
	}
	ws := &webServer{
		llm:        llm,
		classifier: classifier,
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "web_fetch",
		Description: "Fetch a URL and answer a prompt against its content. Both url and prompt are required. Returns the model's answer based on the page text. Works with any host including localhost.",
	}, ws.fetch)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "web_search",
		Description: "Search the web via DuckDuckGo. Returns up to max_results (default 5) structured results with title, url, and snippet.",
	}, ws.search)

	return server.Run(ctx, transport)
}
