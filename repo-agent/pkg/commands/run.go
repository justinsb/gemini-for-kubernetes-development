package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// ExecOptions holds options for the run command.
type ExecOptions struct {
	SandboxName string
	Command     []string

	Daemonize bool
}

// BuildExecCommand creates a new cobra command for running a command in the dev sandbox.
func BuildExecCommand() *cobra.Command {
	var opt ExecOptions

	cmd := &cobra.Command{
		Use:   "exec [sandbox-name] -- [command] [args...]",
		Short: "Run a command in the dev sandbox",
		// We expect at least the sandbox name. The command to run is after "--".
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// If the user runs `gemini run sandbox -- echo hello`
			// args[0] is sandbox.
			// args[1:] is command.
			if len(args) < 2 {
				return fmt.Errorf("requires sandbox name and command (separated by --)")
			}
			opt.SandboxName = args[0]
			opt.Command = args[1:]

			return RunRun(cmd.Context(), opt)
		},
	}

	cmd.AddCommand(NewRunAgentCommand())

	return cmd
}

// RunRun runs a command in the specified dev sandbox.
func RunRun(ctx context.Context, opt ExecOptions) error {
	// 1. Find the pod
	podID, err := findSandboxPod(ctx, opt.SandboxName)
	if err != nil {
		return err
	}

	if podID == nil {
		return fmt.Errorf("no pod found for sandbox %q", opt.SandboxName)
	}

	// 2. kubectl exec
	// repo-sandbox run agent -- <cmd> <args>
	k8sArgs := []string{
		"exec",
		"--namespace", podID.Namespace,
		"-c", "agent",
		podID.Name,
		"--",
		repoSandboxBinary,
		"run",
		"agent",
		"--",
	}
	k8sArgs = append(k8sArgs, opt.Command...)

	cmd := exec.CommandContext(ctx, "kubectl", k8sArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run command in sandbox: %w", err)
	}

	return nil
}
