package main

import (
	"fmt"
	"os"

	"github.com/8iops/stalwart-agent-tools/internal/catalog"
	"github.com/8iops/stalwart-agent-tools/internal/contract"
)

func main() {
	providerData, err := os.ReadFile("contracts/provider.json")
	if err != nil {
		fatal(err)
	}
	provider, err := contract.Parse(providerData)
	if err != nil {
		fatal(err)
	}
	toolData, err := os.ReadFile("contracts/tools.json")
	if err != nil {
		fatal(err)
	}
	tools, err := catalog.Parse(toolData)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("validated %s with %d typed operations\n", provider.SchemaVersion, len(tools.Operations))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
