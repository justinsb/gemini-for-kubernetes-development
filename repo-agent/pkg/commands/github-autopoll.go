package commands

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

	// Model is the LLM model to use.
	Model string
}

func (o *GithubAutopollOptions) InitDefaults() {
	o.Model = "gemini-3-pro-preview"
}

// BuildGithubAutopollCommand creates a new cobra command for autopolling github issues
func BuildGithubAutopollCommand() *cobra.Command {
	var opt GithubAutopollOptions

	opt.InitDefaults()

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
	cmd.Flags().StringSliceVar(&opt.Allowlist, "allowlist", opt.Allowlist, "Comma-separated list of GitHub users whose issues will be processed")
	cmd.Flags().DurationVar(&opt.PollInterval, "poll-interval", 60*time.Second, "How often to poll GitHub")
	cmd.Flags().StringVar(&opt.AssignedTo, "assigned-to", "codebot-robot", "GitHub user to check for assigned issues")
	cmd.Flags().StringVar(&opt.Model, "model", opt.Model, "LLM model to use")

	return cmd
}

// RunGithubAutopoll continuously polls GitHub for issues and creates sandboxes as needed.
func RunGithubAutopoll(ctx context.Context, opt GithubAutopollOptions) error {
	log := klog.FromContext(ctx)

	if len(opt.Repos) == 0 {
		return fmt.Errorf("--repos is required (e.g., --repos=GoogleCloudPlatform/k8s-config-connector)")
	}

	if len(opt.Allowlist) == 0 {
		return fmt.Errorf("--allowlist is required (e.g., --allowlist=user1,user2)")
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
		processedIssues: make(map[string]*Info),
		running:         make(map[string]*RunningInfo),
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

type RunningInfo struct {
	Error error
	Done  bool
}

type AutoPoller struct {
	githubAPI       *github.Client
	kube            *clients.KubernetesClient
	opt             GithubAutopollOptions
	allowlistMap    map[string]bool
	processedIssues map[string]*Info

	running      map[string]*RunningInfo
	runningMutex sync.Mutex
}

type Info struct {
	Reason string
}

func (p *AutoPoller) pollOnce(ctx context.Context) error {
	log := klog.FromContext(ctx)

	log.Info("Polling GitHub", "repos", p.opt.Repos, "assignedTo", p.opt.AssignedTo)

	for _, repoStr := range p.opt.Repos {
		if err := p.pollRepo(ctx, repoStr); err != nil {
			log.Error(err, "error polling repo", "repo", repoStr)
			// Don't return error, continue polling other repos
		}
	}

	{
		var running []string
		p.runningMutex.Lock()
		for k, info := range p.running {
			if info.Done {
				continue
			}
			running = append(running, k)
		}
		p.runningMutex.Unlock()

		log.Info("Currently processing", "count", len(running), "prs", running)
	}

	return nil
}

func (p *AutoPoller) pollRepo(ctx context.Context, repoStr string) error {
	log := klog.FromContext(ctx)

	log.V(2).Info("polling repository", "repo", repoStr)

	{
		repo, err := github.ParseRepo(repoStr)
		if err != nil {
			return fmt.Errorf("failed to parse repo %q: %w", repoStr, err)
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

		log.V(2).Info("Found issues assigned to bot", "repo", repoStr, "count", len(issues))

		for _, issue := range issues {
			// Skip pull requests (GitHub API returns PRs as issues)
			if issue.PullRequestLinks != nil {
				continue
			}

			issueKey := fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, issue.GetNumber())

			// Skip if already processed in this run
			if info := p.processedIssues[issueKey]; info != nil {
				log.V(2).Info("Skipping issue, already processed", "issue", issueKey, "reason", info.Reason)
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
			shouldProcess, reason, err := shouldProcessIssue(ctx, p.githubAPI, repo, issue)
			if err != nil {
				log.Error(err, "error checking if issue should be processed", "issue", issueKey)
				continue
			}
			if !shouldProcess {
				log.Info("Skipping issue", "issue", issueKey, "reason", reason)
				p.processedIssues[issueKey] = &Info{Reason: reason}
				continue
			}

			log.Info("Processing issue", "issue", issueKey)

			// Mark as processed
			p.processedIssues[issueKey] = &Info{Reason: "processed"}

			// Create the issue URL and invoke github-fix-issue logic
			issueURL := fmt.Sprintf("https://github.com/%s/%s/issues/%d", repo.Owner, repo.Name, issue.GetNumber())

			running := &RunningInfo{}
			p.runningMutex.Lock()
			p.running[issueKey] = running
			p.runningMutex.Unlock()

			go func(issueURL string) {
				fixIssueOpt := GithubFixIssueOptions{
					URL:   issueURL,
					Model: p.opt.Model,
				}

				if err := RunGithubFixIssue(ctx, fixIssueOpt); err != nil {
					log.Error(err, "failed to process issue", "issue", issueKey)
					// Don't return error, continue processing other issues
					running.Error = err
				}

				running.Done = true
			}(issueURL)
		}

		options := &gogithub.PullRequestListOptions{
			State:     "open",
			Sort:      "updated",
			Direction: "desc",
		}
		options.PerPage = 100

		prs, _, err := p.githubAPI.PullRequests.List(ctx, repo.Owner, repo.Name, options)
		if err != nil {
			return fmt.Errorf("failed to list pull requests for %s: %w", repoStr, err)
		}

		log.V(2).Info("Found pull requests assigned to bot", "repo", repoStr, "count", len(prs))

		for _, pr := range prs {
			log.V(2).Info("Processing pull request", "pr", pr.GetNumber())
			if err := p.processPullRequest(ctx, repo, pr); err != nil {
				log.Error(err, "failed to process pull request", "pr", pr.GetNumber())
			}
		}

	}

	return nil
}

func (p *AutoPoller) processPullRequest(ctx context.Context, repo *github.Repo, pr *gogithub.PullRequest) error {
	log := klog.FromContext(ctx)
	prNumber := pr.GetNumber()
	prKey := fmt.Sprintf("%s/%s#%d", repo.Owner, repo.Name, prNumber)

	// Check allowlist (checking issue author, who is the PR author)
	author := pr.GetUser().GetLogin()
	if author != p.opt.AssignedTo {
		log.V(2).Info("Skipping PR, not created by bot", "pr", prKey, "author", author)
		return nil
	}

	assigned := false
	for _, assignee := range pr.Assignees {
		if assignee.GetLogin() == p.opt.AssignedTo {
			// Assigned to bot, process it
			assigned = true
			break
		}
	}

	if !assigned {
		log.V(2).Info("Skipping PR, not assigned to bot", "pr", prKey)
		return nil
	}

	var runningInfo *RunningInfo
	{
		p.runningMutex.Lock()
		_, found := p.running[prKey]
		if !found {
			runningInfo = &RunningInfo{}
			p.running[prKey] = runningInfo
		}
		p.runningMutex.Unlock()

		if found {
			log.V(2).Info("Skipping PR, already being processed", "pr", prKey)
			return nil
		}
	}

	log.Info("Processing PR", "pr", prKey)

	// // Remove assignment
	// _, _, err := p.githubAPI.Issues.RemoveAssignees(ctx, repo.Owner, repo.Name, prNumber, []string{p.opt.AssignedTo})
	// if err != nil {
	// 	return fmt.Errorf("failed to remove assignee: %w", err)
	// }

	// Trigger RunGithubFeedback
	// Run asynchronously to not block polling
	go func() {
		opt := GithubFeedbackOptions{}

		opt.InitDefaults()

		opt.Model = p.opt.Model
		opt.PullRequest = fmt.Sprintf("https://github.com/%s/%s/pull/%v", repo.Owner, repo.Name, prNumber)
		// Sandbox is empty, let RunGithubFeedback find/create it

		log.Info("doing github feedback for PR", "pr", prKey)
		if err := RunGithubFeedback(ctx, opt); err != nil {
			runningInfo.Error = err
			log.Error(err, "failed to run github feedback", "pr", prKey)
		}

		runningInfo.Done = true

		// Try to unassign the PR from the bot
		_, _, err := p.githubAPI.Issues.RemoveAssignees(ctx, repo.Owner, repo.Name, prNumber, []string{p.opt.AssignedTo})
		if err != nil {
			log.Error(err, "failed to remove assignee", "pr", prKey)
		} else {
			// If we were able to unassign, we can use reassignment as a signal for reprocessing
			p.runningMutex.Lock()
			delete(p.running, prKey)
			p.runningMutex.Unlock()
		}
	}()

	return nil
}

// shouldProcessIssue checks if an issue should be processed based on:
// 1. Whether a PR is already linked
// 2. Whether a sandbox already exists
func shouldProcessIssue(ctx context.Context, githubAPI *github.Client, repo *github.Repo, issue *gogithub.Issue) (bool, string, error) {
	log := klog.FromContext(ctx)

	// Check if a sandbox already exists for this issue
	sandboxName := fmt.Sprintf("github-%s-%s-%d", repo.Owner, repo.Name, issue.GetNumber())
	sandboxName = strings.ToLower(sandboxName)

	podID, err := findSandboxPod(ctx, sandboxName)
	if err != nil {
		log.Error(err, "failed to check for existing sandbox", "sandboxName", sandboxName)
		// If we can't check, skip this issue for now
		return false, "", fmt.Errorf("error checking sandbox: %v", err)
	}
	if podID != nil {
		return false, fmt.Sprintf("sandbox already exists for this issue: %v", podID), nil
	}

	// Check if a PR is linked to this issue
	linkedPR, err := hasLinkedPR(ctx, githubAPI, repo, issue)
	if err != nil {
		return false, "", fmt.Errorf("failed to check for linked PR: %w", err)
	}

	for _, pr := range linkedPR {
		prData, _, err := githubAPI.PullRequests.Get(ctx, pr.Repo.Owner, pr.Repo.Name, pr.PullRequestNumber)
		if err != nil {
			return false, "", fmt.Errorf("error fetching linked PR data: %v", err)
		}
		switch prData.GetState() {
		case "open":
			return false, fmt.Sprintf("issue has an open linked PR %v", prData.GetHTMLURL()), nil
		}
	}

	klog.Infof("no open linked PRs found for issue %s/%s#%d", repo.Owner, repo.Name, issue.GetNumber())

	return true, "", nil
}

// hasLinkedPR checks if the issue has any linked pull requests
func hasLinkedPR(ctx context.Context, githubAPI *github.Client, repo *github.Repo, issue *gogithub.Issue) ([]*github.PullRequest, error) {
	// Use the timeline API to check for linked PRs
	// GitHub's timeline API shows cross-references including linked PRs
	klog.Infof("checking for linked PRs for issue %s/%s#%d", repo.Owner, repo.Name, issue.GetNumber())
	timeline, _, err := githubAPI.Issues.ListIssueTimeline(ctx, repo.Owner, repo.Name, issue.GetNumber(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get issue timeline: %w", err)
	}

	var prs []*github.PullRequest

	for _, event := range timeline {
		klog.Infof("event: %+v", event.GetEvent())
		// Check for cross-referenced events that link to PRs
		if event.GetEvent() == "cross-referenced" && event.Source != nil {
			klog.Infof("found cross-referenced event: %+v", event)
			klog.Infof("found cross-referenced event.event: %+v", ValueOf(event.Event))
			// klog.Infof("found cross-referenced event.source: %+v", event.Source)
			// klog.Infof("found cross-referenced event.source.Issue: %+v", event.Source.Issue)
			klog.Infof("found cross-referenced event.source.Type: %+v", event.GetSource().GetType())
			if event.Source.Issue != nil {
				// We're looking for a PR, not another issue
				// if event.GetSource().GetType() == "issue" {
				// 	continue
				// }
				klog.Infof("found cross-referenced event.source.issue: %+v", ValueOf(event.Source.Type))
				if event.Source.Issue.PullRequestLinks != nil {
					klog.Infof("PullRequestLinks found in cross-referenced event: %+v", ValueOf(event.Source.Issue.PullRequestLinks))
					u := event.Source.Issue.GetHTMLURL()
					parsedPR, err := github.ParsePullRequestURL(u)
					if err != nil {
						return nil, fmt.Errorf("failed to parse linked PR URL %q: %w", u, err)
					}
					prs = append(prs, parsedPR)
				}
			}
		}
		// Also check for connected events (newer GitHub feature for linking issues/PRs)
		if event.GetEvent() == "connected" {
			klog.Infof("Found connected event (not yet handled in hasLinkedPR): %+v", event)
			return nil, fmt.Errorf("connected events not yet supported in hasLinkedPR")
		}
	}

	author := issue.GetUser().GetLogin()
	if author != "justinsb" {
		return prs, nil
	}

	var matches []*github.PullRequest
	for _, pr := range prs {
		prObject, _, err := githubAPI.PullRequests.Get(ctx, pr.Repo.Owner, pr.Repo.Name, pr.PullRequestNumber)
		if err != nil {
			return nil, fmt.Errorf("error fetching PR data: %v", err)
		}
		isOverseer := false
		for _, label := range prObject.Labels {
			if label.GetName() == "overseer" {
				isOverseer = true
			}
		}
		if isOverseer {
			klog.Infof("Found overseer PR: %s", prObject.GetHTMLURL())
			continue
		}
		matches = append(matches, pr)
	}
	return matches, nil
}

func ValueOf[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}
