package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// RunAgentOptions holds options for the RunAgent function.
type RunAgentOptions struct {
	Command []string
}

// NewRunAgentCommand creates a new hidden cobra command for running commands on the agent.
func NewRunAgentCommand() *cobra.Command {
	var opt RunAgentOptions

	cmd := &cobra.Command{
		Use:    "agent -- [command] [args...]",
		Short:  "Run a command on the agent",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("requires command")
			}
			opt.Command = args
			return RunRunAgent(cmd.Context(), opt)
		},
	}
	return cmd
}

// RunRunAgent runs the command on the agent.
func RunRunAgent(ctx context.Context, opt RunAgentOptions) error {
	log := klog.FromContext(ctx)

	// Ignore SIGHUP so that the process keeps running even if the terminal disconnects.
	// This also means the child process will inherit SIGHUP ignore.
	signal.Ignore(syscall.SIGHUP)

	// Use an uncancellable context to avoid issues with the connection being lost.
	// This ensures that even if the client disconnects (kubectl exec terminates),
	// the process spawned here continues running if it handles signals appropriately.
	detachedCtx := context.WithoutCancel(ctx)

	cmd := exec.CommandContext(detachedCtx, opt.Command[0], opt.Command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Info("Running command via run agent", "command", opt.Command)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run command: %w", err)
	}

	return nil
}
