package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/clients"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/github"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/prompts"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// GithubFeedbackOptions holds options for the RunCode function.
type GithubFeedbackOptions struct {
	PullRequest string
	Issue       string
}

// BuildGithubFeedbackCommand creates a new cobra command for using a dev sandbox to address github feedback
func BuildGithubFeedbackCommand() *cobra.Command {
	var opt GithubFeedbackOptions

	cmd := &cobra.Command{
		Use:   "github-feedback",
		Short: "Address github pull request feedback using an LLM in a dev sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("command does not take positional arguments")
			}

			return RunGithubFeedback(cmd.Context(), opt)
		},
	}

	cmd.Flags().StringVar(&opt.Issue, "issue", opt.Issue, "GitHub issue URL")
	cmd.Flags().StringVar(&opt.PullRequest, "pull-request", opt.PullRequest, "GitHub pull request URL")
	return cmd
}

// RunGithubFeedback launches gemini-cli to respond to the specified GitHub pull request feedback.
func RunGithubFeedback(ctx context.Context, opt GithubFeedbackOptions) error {
	log := klog.FromContext(ctx)

	githubAPI, err := github.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create github client: %w", err)
	}

	kube, err := clients.NewKubernetesClient()
	if err != nil {
		return err
	}

	if opt.PullRequest == "" {
		return fmt.Errorf("--pull-request is required")
	}

	pullRequest, err := github.ParsePullRequestURL(opt.PullRequest)
	if err != nil {
		return err
	}

	pullRequestData, _, err := githubAPI.PullRequests.Get(ctx, pullRequest.Repo.Owner, pullRequest.Repo.Name, pullRequest.PullRequestNumber)
	if err != nil {
		return fmt.Errorf("getting pull request data: %w", err)
	}

	issueURL := opt.Issue
	if issueURL == "" {
		issueURL, err = findIssueFromPullRequest(ctx, githubAPI, pullRequestID.Repo, pullRequestData)
		if err != nil {
			return fmt.Errorf("finding issue from pull request: %w", err)
		}
		klog.Infof("inferred issue: %q", issueURL)
		// return fmt.Errorf("--issue is required")
	}

	issue, err := github.ParseIssueURL(issueURL)
	if err != nil {
		return err
	}

	repo := issue.Repo

	repoInfo, err := repo.FetchInfo(ctx, githubAPI)
	if err != nil {
		return fmt.Errorf("getting repo info: %w", err)
	}

	sandbox, found, err := findSandboxForIssue(ctx, kube, repo, issue)
	if err != nil {
		return err
	}

	if !found {
		sandbox, err = launchSandboxForIssue(ctx, kube, repo, issue)
		if err != nil {
			return fmt.Errorf("launching sandbox for issue: %w", err)
		}
	}

	geminiAPIKey, err := GetGeminiAPIKey(sandbox.podID.Namespace + "/" + sandbox.podID.Name)
	if err != nil {
		return err
	}

	if err := sandbox.setupGit(ctx); err != nil {
		return fmt.Errorf("setting up git in sandbox: %w", err)
	}

	if err := sandbox.SetupGitRepos(ctx); err != nil {
		return fmt.Errorf("setting up git branches in sandbox: %w", err)
	}

	branchName := pullRequestData.GetHead().GetRef()

	// HACK: avoid .git/index.lock conflict with checkout
	time.Sleep(5 * time.Second)

	if err := sandbox.CheckoutExistingBranch(ctx, branchName); err != nil {
		return err
	}

	threads, err := sandbox.ListThreads(ctx)
	if err != nil {
		return fmt.Errorf("listing threads in sandbox: %w", err)
	}

	log.Info("found threads in sandbox", "count", len(threads))

	haveIDs := make(map[string]bool)

	appendToThread := ""

	if len(threads) > 0 {
		if len(threads) > 1 {
			return fmt.Errorf("multiple threads found in sandbox %q; not yet supported", sandbox.podID)
		}

		// log.Info("found existing thread in sandbox", "thread", threads[0], "messages_count", len(messages))
		appendToThread = threads[0].SessionID

		messages, err := sandbox.GetThreadMessages(ctx, threads[0].SessionID)
		if err != nil {
			return fmt.Errorf("getting thread messages: %w", err)
		}

		for _, msg := range messages {
			if msg.Type == "gemini" {
				continue
			}
			// log.Info("existing message in thread", "message", msg)
			for _, line := range strings.Split(msg.Content, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "GITHUB_ID: ") {
					tokens := strings.Fields(line)
					if len(tokens) != 2 {
						return fmt.Errorf("unexpected ID line format: %q", line)
					}
					haveIDs[tokens[1]] = true
					continue
				}
			}
		}

		// if len(haveIDs) == 0 {
		// 	return fmt.Errorf("no IDs found in existing thread messages")
		// }

		// klog.Fatalf("have IDs: %+v", haveIDs)

		// // TODO: Extract IDs from messages
		// haveIDs["IC_kwDOCrwMCc7fSY9H"] = true
		// haveIDs["PRR_kwDOCrwMCc7aBBrC"] = true
		// haveIDs["PRR_kwDOCrwMCc7aCH29"] = true
		// haveIDs["PRR_kwDOCrwMCc7aPMBx"] = true
		// haveIDs["PRR_kwDOCrwMCc7aQGTY"] = true
	}

	prompt, err := prompts.FixPRFeedbackPrompt(ctx, githubAPI, pullRequest, haveIDs)
	if err != nil {
		return fmt.Errorf("failed to generate prompt for pull-request: %w", err)
	}

	// klog.Fatalf("generated prompt for pull request feedback:\n%v\n", string(prompt))

	// Copy the prompt into the pod (for now)
	if len(prompt) > 0 {
		path := "/workspaces/prompt.txt"
		if err := writeFileInPod(ctx, kube, sandbox.podID, path, prompt); err != nil {
			return fmt.Errorf("copying prompt into sandbox pod: %w", err)
		}

		log.Info("wrote prompt into sandbox pod", "pod", sandbox.podID, "path", path)
	}

	// Run gemini with API key and prompt
	{
		log.Info("Running gemini in pod", "pod", sandbox.podID)

		workdir := fmt.Sprintf("/workspaces/%s", pullRequest.Repo.FilesystemName())

		// TODO:
		// export GEMINI_TELEMETRY_ENABLED=true
		// export GEMINI_TELEMETRY_OTLP_ENDPOINT=http://otel-portal.otel-system:4317

		opts := execOptions{
			Command: []string{"sh", "-c", fmt.Sprintf("cd %s && export GEMINI_API_KEY=%s && gemini --yolo --model gemini-3-pro-preview < /workspaces/prompt.txt", workdir, geminiAPIKey)},
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
		}

		if appendToThread != "" {
			opts.Command = []string{"sh", "-c", fmt.Sprintf("cd %s && export GEMINI_API_KEY=%s && gemini --yolo --model gemini-3-pro-preview --resume=%s < /workspaces/prompt.txt", workdir, geminiAPIKey, appendToThread)}
		}

		opts.Secrets = []string{geminiAPIKey}

		if err := execInPod(ctx, kube, sandbox.podID, opts); err != nil {
			return fmt.Errorf("running gemini in pod: %w", err)
		}
	}

	return nil
}
