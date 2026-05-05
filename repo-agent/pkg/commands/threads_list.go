package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"sort"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
)

const repoSandboxBinary = "/repo-agent/repo-sandbox"

// ListThreadsOptions holds options for the ListThreads function.
type ListThreadsOptions struct {
	SandboxName string
}

// NewThreadsListCommand creates a new cobra command for listing LLM threads/chats in the dev sandbox.
func NewThreadsListCommand() *cobra.Command {
	var opt ListThreadsOptions

	cmd := &cobra.Command{
		Use:   "list [sandbox-name]",
		Short: "List LLM threads/chats in the dev sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("threads list command requires exactly one argument: the sandbox name")
			}
			opt.SandboxName = args[0]

			return RunListThreads(cmd.Context(), opt)
		},
	}
	return cmd
}

// RunListThreads lists LLM threads/chats in the specified dev sandbox.
func RunListThreads(ctx context.Context, opt ListThreadsOptions) error {
	// 1. Find the pod
	podID, err := findSandboxPod(ctx, opt.SandboxName)
	if err != nil {
		return err
	}
	if podID == nil {
		return fmt.Errorf("sandbox %q not found", opt.SandboxName)
	}

	threads, err := listThreads(ctx, *podID)
	if err != nil {
		return fmt.Errorf("failed to list threads: %w", err)
	}

	for _, thread := range threads {
		fmt.Fprintf(os.Stdout, "%v\t%v\t%v\t%v\t%v\n", thread.ProjectRoot, thread.SessionID, thread.ProjectHash, thread.StartTime, thread.TotalTokens)
	}

	return nil
}

// listThreads runs the agent to list threads in the given dev sandbox pod.
func listThreads(ctx context.Context, podID types.NamespacedName) ([]ThreadInfo, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "--namespace", podID.Namespace, "-c", "agent", podID.Name, "--", repoSandboxBinary, "threads", "agent")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to launch repo-sandbox agent via kubectl: %w", err)
	}

	var threads []ThreadInfo
	if err := json.Unmarshal(stdout.Bytes(), &threads); err != nil {
		return nil, fmt.Errorf("failed to parse threads agent output: %w", err)
	}

	sort.Slice(threads, func(i, j int) bool {
		return threads[i].StartTime.Before(threads[j].StartTime)
	})

	// Group by session (TODO: move server side)
	threadsBySession := make(map[string]ThreadInfo)
	for _, thread := range threads {
		key := fmt.Sprintf("%s|%s|%s", thread.ProjectRoot, thread.SessionID, thread.ProjectHash)
		existing, found := threadsBySession[key]
		if !found {
			threadsBySession[key] = thread
		} else {
			// Merge
			existing.TotalTokens += thread.TotalTokens
			existing.Messages = append(existing.Messages, thread.Messages...)
			threadsBySession[key] = existing
		}
	}

	return slices.Collect(maps.Values(threadsBySession)), nil
}
