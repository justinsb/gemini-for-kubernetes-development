package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/clients"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/github"
	gogithub "github.com/google/go-github/v39/github"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// GithubAutopollOptions holds options for the autopoll function.
type GithubAutopollOptions struct {
	Repos        []string
	Allowlist    []string
	PollInterval time.Duration
	AssignedTo   string
}

// BuildGithubAutopollCommand creates a new cobra command for autopolling github issues
func BuildGithubAutopollCommand() *cobra.Command {
	var opt GithubAutopollOptions

	cmd := &cobra.Command{
		Use:   "github-autopoll",
		Short: "Continuously poll GitHub for issues assigned to a bot and automatically create sandboxes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("command does not take positional arguments")
			}

			return RunGithubAutopoll(cmd.Context(), opt)
		},
	}

	cmd.Flags().StringSliceVar(&opt.Repos, "repos", nil, "GitHub repositories to monitor (e.g., GoogleCloudPlatform/k8s-config-connector)")
	cmd.Flags().StringSliceVar(&opt.Allowlist, "allowlist", []string{"justinsb", "cheftako"}, "Comma-separated list of GitHub users whose issues will be processed")
	cmd.Flags().DurationVar(&opt.PollInterval, "poll-interval", 60*time.Second, "How often to poll GitHub")
	cmd.Flags().StringVar(&opt.AssignedTo, "assigned-to", "codebot-robot", "GitHub user to check for assigned issues")

	return cmd
}

// RunGithubAutopoll continuously polls GitHub for issues and creates sandboxes as needed.
func RunGithubAutopoll(ctx context.Context, opt GithubAutopollOptions) error {
	log := klog.FromContext(ctx)

	if len(opt.Repos) == 0 {
		return fmt.Errorf("--repos is required (e.g., --repos=GoogleCloudPlatform/k8s-config-connector)")
	}

	githubAPI, err := github.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create github client: %w", err)
	}

	kube, err := clients.NewKubernetesClient()
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Convert allowlist to map for faster lookups
	allowlistMap := make(map[string]bool)
	for _, user := range opt.Allowlist {
		allowlistMap[strings.TrimSpace(user)] = true
	}

	log.Info("Starting GitHub autopoll", "repos", opt.Repos, "allowlist", opt.Allowlist, "assignedTo", opt.AssignedTo, "pollInterval", opt.PollInterval)

	ticker := time.NewTicker(opt.PollInterval)
	defer ticker.Stop()

	// Track processed issues to avoid reprocessing in the same run
	poller := &AutoPoller{
		githubAPI:       githubAPI,
		kube:            kube,
		opt:             opt,
		allowlistMap:    allowlistMap,
		processedIssues: make(map[string]bool),
	}

	// Do an initial poll immediately
	if err := poller.pollOnce(ctx); err != nil {
		log.Error(err, "error during initial poll")
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("Context cancelled, stopping autopoll")
			return ctx.Err()
		case <-ticker.C:
			if err := poller.pollOnce(ctx); err != nil {
				log.Error(err, "error during poll")
			}
		}
	}
}

type AutoPoller struct {
	githubAPI       *github.Client
	kube            *clients.KubernetesClient
	opt             GithubAutopollOptions
	allowlistMap    map[string]bool
	processedIssues map[string]bool
}

func (p *AutoPoller) pollOnce(ctx context.Context) error {
	log := klog.FromContext(ctx)

	for _, repoStr := range p.opt.Repos {
		repo, err := github.ParseRepo(repoStr)
		if err != nil {
			log.Error(err, "failed to parse repo", "repo", repoStr)
			continue
		}

		log.V(2).Info("Polling repository", "repo", repoStr)

		// Query GitHub for issues assigned to the bot
		issues, _, err := p.githubAPI.Issues.ListByRepo(ctx, repo.Owner, repo.Name, &gogithub.IssueListByRepoOptions{
			State:     "open",
			Assignee:  p.opt.AssignedTo,
			Sort:      "updated",
			Direction: "desc",
		})
		if err != nil {
			return fmt.Errorf("failed to list issues for %s: %w", repoStr, err)
		}

		log.Info("Found issues assigned to bot", "repo", repoStr, "count", len(issues))

		for _, issue := range issues {
			// Skip pull requests (GitHub API returns PRs as issues)
			if issue.PullRequestLinks != nil {
				continue
			}

			issueKey := fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, issue.GetNumber())

			// Skip if already processed in this run
			if p.processedIssues[issueKey] {
				log.Info("Skipping issue, already processed", "issue", issueKey)
				continue
			}

			// Check if issue author is in allowlist
			author := issue.GetUser().GetLogin()
			if !p.allowlistMap[author] {
				log.Info("Skipping issue, author not in allowlist", "issue", issueKey, "author", author)
				continue
			}

			log.Info("Checking issue for processing", "issue", issueKey, "author", author)

			// Check if issue should be processed
			shouldProcess, reason := shouldProcessIssue(ctx, p.githubAPI, p.kube, repo, issue)
			if !shouldProcess {
				log.Info("Skipping issue", "issue", issueKey, "reason", reason)
				continue
			}

			log.Info("Processing issue", "issue", issueKey)

			// Mark as processed
			p.processedIssues[issueKey] = true

			// Create the issue URL and invoke github-fix-issue logic
			issueURL := fmt.Sprintf("https://github.com/%s/%s/issues/%d", repo.Owner, repo.Name, issue.GetNumber())

			go func(issueURL string) {
				fixIssueOpt := GithubFixIssueOptions{
					URL: issueURL,
				}

				if err := RunGithubFixIssue(ctx, fixIssueOpt); err != nil {
					log.Error(err, "failed to process issue", "issue", issueKey)
					// Don't return error, continue processing other issues
				}
			}(issueURL)
		}
	}

	return nil
}

// shouldProcessIssue checks if an issue should be processed based on:
// 1. Whether a PR is already linked
// 2. Whether a sandbox already exists
func shouldProcessIssue(ctx context.Context, githubAPI *github.Client, kube *clients.KubernetesClient, repo *github.Repo, issue *gogithub.Issue) (bool, string) {
	log := klog.FromContext(ctx)

	// Check if a PR is linked to this issue
	hasLinkedPR, err := hasLinkedPR(ctx, githubAPI, repo, issue)
	if err != nil {
		log.Error(err, "failed to check for linked PR", "issue", issue.GetNumber())
		// If we can't check, skip this issue for now
		return false, fmt.Sprintf("error checking linked PR: %v", err)
	}
	if hasLinkedPR {
		return false, "issue already has a linked PR"
	}

	// Check if a sandbox already exists for this issue
	sandboxName := fmt.Sprintf("github-%s-%s-%d", repo.Owner, repo.Name, issue.GetNumber())
	sandboxName = strings.ToLower(sandboxName)

	podID, err := findSandboxPod(ctx, sandboxName)
	if err != nil {
		log.Error(err, "failed to check for existing sandbox", "sandboxName", sandboxName)
		// If we can't check, skip this issue for now
		return false, fmt.Sprintf("error checking sandbox: %v", err)
	}
	if podID != nil {
		return false, "sandbox already exists for this issue"
	}

	return true, ""
}

// hasLinkedPR checks if the issue has any linked pull requests
func hasLinkedPR(ctx context.Context, githubAPI *github.Client, repo *github.Repo, issue *gogithub.Issue) (bool, error) {
	// Use the timeline API to check for linked PRs
	// GitHub's timeline API shows cross-references including linked PRs
	timeline, _, err := githubAPI.Issues.ListIssueTimeline(ctx, repo.Owner, repo.Name, issue.GetNumber(), nil)
	if err != nil {
		return false, fmt.Errorf("failed to get issue timeline: %w", err)
	}

	for _, event := range timeline {
		// Check for cross-referenced events that link to PRs
		if event.GetEvent() == "cross-referenced" && event.Source != nil {
			if event.Source.Issue != nil && event.Source.Issue.PullRequestLinks != nil {
				// This is a PR that references this issue
				return true, nil
			}
		}
		// Also check for connected events (newer GitHub feature for linking issues/PRs)
		if event.GetEvent() == "connected" {
			return true, nil
		}
	}

	return false, nil
}
