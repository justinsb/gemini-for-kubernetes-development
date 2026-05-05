package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// ExecOptions holds options for the Exec function.
type ExecOptions struct {
	Command []string

	EnvVars []string

	Stdin []byte

	// StdoutFile specifies that stdout should be written to the specified file.
	StdoutFile string

	// StderrFile specifies that stderr should be written to the specified file.
	StderrFile string
}

// BuildExecCommand creates a new hidden cobra command for running commands on the agent.
func BuildExecCommand() *cobra.Command {
	var opt ExecOptions

	cmd := &cobra.Command{
		Use:    "exec -- [command] [args...]",
		Short:  "Run a command on the agent",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("requires command")
			}
			opt.Command = args
			return RunExec(cmd.Context(), opt)
		},
	}
	cmd.Flags().StringSliceVar(&opt.EnvVars, "env", opt.EnvVars, "specify env vars")
	cmd.Flags().StringVar(&opt.StdoutFile, "stdout-file", opt.StdoutFile, "specify stdout file")
	cmd.Flags().StringVar(&opt.StderrFile, "stderr-file", opt.StderrFile, "specify stderr file")
	return cmd
}

// RunExec runs the command on the agent.
func RunExec(ctx context.Context, opt ExecOptions) error {
	log := klog.FromContext(ctx)

	// Ignore SIGHUP so that the process keeps running even if the terminal disconnects.
	// This also means the child process will inherit SIGHUP ignore.
	signal.Ignore(syscall.SIGHUP)

	// Use an uncancellable context to avoid issues with the connection being lost.
	// This ensures that even if the client disconnects (kubectl exec terminates),
	// the process spawned here continues running if it handles signals appropriately.
	detachedCtx := context.WithoutCancel(ctx)

	cmd := exec.CommandContext(detachedCtx, opt.Command[0], opt.Command[1:]...)

	// cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if opt.StdoutFile != "" {
		stdoutFile, err := os.OpenFile(opt.StdoutFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("failed to open stdout file: %w", err)
		}
		defer stdoutFile.Close()
		cmd.Stdout = io.MultiWriter(cmd.Stdout, stdoutFile)
	}

	if opt.StderrFile != "" {
		stderrFile, err := os.OpenFile(opt.StderrFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("failed to open stderr file: %w", err)
		}
		defer stderrFile.Close()
		cmd.Stderr = io.MultiWriter(cmd.Stderr, stderrFile)
	}

	env := os.Environ()
	var envKeys []string
	for _, e := range opt.EnvVars {
		tokens := strings.SplitN(e, "=", 2)
		envKeys = append(envKeys, tokens[0])
		env = append(env, e)
	}
	cmd.Env = env

	log.Info("Running command via run agent", "command", opt.Command, "env", envKeys)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to run command: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("command failed: %w", err)
	}

	return nil
}
