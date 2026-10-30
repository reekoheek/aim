package main

import (
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd(serve).Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd(serveRunner serveFunc) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "aim",
		Short:   "aim — an AI Management Server",
		Long:    "aim is a lightweight AI management server that forwards requests to supported APIs and llama.cpp server.",
		Version: version,
	}

	cmd.AddCommand(newServeCmd(serveRunner))

	return cmd
}
