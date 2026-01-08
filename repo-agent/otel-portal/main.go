package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/otel-portal/pkg/commands"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "otel-portal",
		Short: "OpenTelemetry Portal",
	}

	rootCmd.AddCommand(commands.BuildOtelListenerCommand())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		log.Fatal(err)
	}
}