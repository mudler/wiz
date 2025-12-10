package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)

	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan bool, 1)

	go func() {
		<-sigs
		cancel()
		done <- true
	}()

	// Set MCP servers
	bashMCPServerTransport, bashMCPServerClient := mcp.NewInMemoryTransports()

	go func() {
		if err := runBashMCP(ctx, bashMCPServerTransport); err != nil {
			panic(err)
		}
	}()

	if err := runner(ctx, bashMCPServerClient); err != nil {
		panic(err)
	}

	<-done
	fmt.Println("exiting")
}
