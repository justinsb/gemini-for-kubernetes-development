package prompts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/github"
	githubapi "github.com/google/go-github/v39/github"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// I0118 12:24:07.181415   89423 fix_pr_feedback.go:130] Full comment YAML: author_association: MEMBER
// body: Let's mark this deprecated, and tell callers they should use Normalize instead
// commit_id: dee5d2cef1b6c1fba504d0363ce031a741dda913
// created_at: "2026-01-18T17:05:55Z"
// diff_hunk: ""
// html_url: https://github.com/GoogleCloudPlatform/k8s-config-connector/pull/6150#discussion_r2702572647
// id: 2702572647
// line: 127
// node_id: PRRC_kwDOCrwMCc6hFfxn
// original_commit_id: dee5d2cef1b6c1fba504d0363ce031a741dda913
// original_line: 125
// original_position: 1
// path: apis/refs/v1beta1/organizationref.go
// position: 1
// pull_request_review_id: 3675594793
// pull_request_url: https://api.github.com/repos/GoogleCloudPlatform/k8s-config-connector/pulls/6150
// reactions:
//   "+1": 0
//   "-1": 0
//   confused: 0
//   eyes: 0
//   heart: 0
//   hooray: 0
//   laugh: 0
//   rocket: 0
//   total_count: 0
//   url: https://api.github.com/repos/GoogleCloudPlatform/k8s-config-connector/pulls/comments/2702572647/reactions
// side: RIGHT
// updated_at: "2026-01-18T17:06:03Z"
// url: https://api.github.com/repos/GoogleCloudPlatform/k8s-config-connector/pulls/comments/2702572647
// user:
//   avatar_url: https://avatars.githubusercontent.com/u/100893?v=4
//   events_url: https://api.github.com/users/justinsb/events{/privacy}
//   followers_url: https://api.github.com/users/justinsb/followers
//   following_url: https://api.github.com/users/justinsb/following{/other_user}
//   gists_url: https://api.github.com/users/justinsb/gists{/gist_id}
//   gravatar_id: ""
//   html_url: https://github.com/justinsb
//   id: 100893
//   login: justinsb
//   node_id: MDQ6VXNlcjEwMDg5Mw==
//   organizations_url: https://api.github.com/users/justinsb/orgs
//   received_events_url: https://api.github.com/users/justinsb/received_events
//   repos_url: https://api.github.com/users/justinsb/repos
//   site_admin: false
//   starred_url: https://api.github.com/users/justinsb/starred{/owner}{/repo}
//   subscriptions_url: https://api.github.com/users/justinsb/subscriptions
//   type: User
//   url: https://api.github.com/users/justinsb


// git show dee5d2cef1b6c1fba504d0363ce031a741dda913:apis/refs/v1beta1/organizationref.go | cat -n

//    112  // ResolveOrganizationFromAnnotation resolves the OrganizationID to use for a
//    113  // resource, it should be used for resources which do not have
//    114  // 'spec.organizationRef'.
//    115  func ResolveOrganizationFromAnnotation(ctx context.Context, reader client.Reader, src client.Object) (*OrganizationIdentity, error) {
//    116          if organizationID := src.GetAnnotations()["cnrm.cloud.google.com/organization-id"]; organizationID != "" {
//    117                  return &OrganizationIdentity{OrganizationID: organizationID}, nil
//    118          }
//    119
//    120          return nil, fmt.Errorf("organization-id annotation not set on resource")
//    121  }
//    122
//    123  // ResolveOrganization will resolve an OrganizationRef to an Organization, with
//    124  // the OrganizationID.
//    125  func ResolveOrganization(ctx context.Context, reader client.Reader, src client.Object, ref *OrganizationRef) (*OrganizationIdentity, error) {
//    126          if ref == nil {
//    127                  return nil, nil
//    128          }
//    129
//    130          if ref.External == "" {
//    131                  return nil, fmt.Errorf("must specify 'external' in 'organizationRef'")
//    132          }
//    133
//    134          id := &OrganizationIdentity{}
//    135          if err := id.FromExternal(ref.External); err != nil {
//    136                  return nil, err
//    137          }
//    138          return id, nil
//    139  }
//    140
//    141  func ResolveOrganizationID(ctx context.Context, reader client.Reader, obj *unstructured.Unstructured) (string, error) {


func FixPRFeedbackPrompt(ctx context.Context, githubAPI *github.Client, repoInfo *github.RepoInfo, pullRequest *github.PullRequest, alreadyPostedIDs map[string]bool) ([]byte, error) {
	b := ModelBuilder{
		githubAPI:        githubAPI,
		repoInfo:         repoInfo,
		pullRequest:      pullRequest,
		alreadyPostedIDs: alreadyPostedIDs,
	}

	repo := pullRequest.Repo

	pr, _, err := b.githubAPI.PullRequests.Get(ctx, repo.Owner, repo.Name, pullRequest.PullRequestNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to get github pull request: %w", err)
	}
	b.pullRequestInfo = pr

	b.model.PullRequest = PullRequest{
		URL:    pr.GetHTMLURL(),
		Number: pr.GetNumber(),
		Title:  pr.GetTitle(),
		Body:   pr.GetBody(),
	}

	b.model.Upstream = repoInfo.GitCloneURL()
	b.model.DefaultBranch = repoInfo.DefaultBranch()

	commits, _, err := b.githubAPI.PullRequests.ListCommits(ctx, repo.Owner, repo.Name, pullRequest.PullRequestNumber, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list github pull request commits: %w", err)
	}
	for _, commit := range commits {
		b.model.PullRequest.Commits = append(b.model.PullRequest.Commits, PullRequestCommit{
			SHA:     commit.GetSHA(),
			Message: commit.GetCommit().GetMessage(),
		})
	}

	issueCommentListOptions := &githubapi.IssueListCommentsOptions{}
	issueComments, _, err := b.githubAPI.Issues.ListComments(ctx, repo.Owner, repo.Name, pullRequest.PullRequestNumber, issueCommentListOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list github pull request comments: %w", err)
	}
	for _, comment := range issueComments {
		id := comment.GetNodeID()

		klog.V(2).Infof("Comment: %+v", comment)
		if b.alreadyPostedIDs[id] {
			klog.V(2).Infof("Skipping comment %q as already posted", id)
			continue
		}

		modelComment := PullRequestComment{
			ID:        id,
			Author:    comment.GetUser().GetLogin(),
			Body:      comment.GetBody(),
			Timestamp: comment.GetCreatedAt(),
		}

		b.model.Comments = append(b.model.Comments, modelComment)
	}

	prCommentListOptions := &githubapi.PullRequestListCommentsOptions{}
	prComments, _, err := b.githubAPI.PullRequests.ListComments(ctx, repo.Owner, repo.Name, pullRequest.PullRequestNumber, prCommentListOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list github pull request comments: %w", err)
	}
	comentsByPullRequestReviewID := make(map[int64][]*githubapi.PullRequestComment)
	for _, comment := range prComments {
		comentsByPullRequestReviewID[comment.GetPullRequestReviewID()] = append(comentsByPullRequestReviewID[comment.GetPullRequestReviewID()], comment)
	}

	reviewListOptions := &githubapi.ListOptions{PerPage: 100}
	reviews, _, err := githubAPI.PullRequests.ListReviews(ctx, repo.Owner, repo.Name, pullRequest.PullRequestNumber, reviewListOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list github pull request reviews: %w", err)
	}
	for _, review := range reviews {
		id := review.GetNodeID()

		if alreadyPostedIDs[id] {
			klog.V(2).Infof("Skipping review %q as already posted", id)
			continue
		}

		modelComment := PullRequestComment{
			ID:        id,
			Author:    review.GetUser().GetLogin(),
			Body:      review.GetBody(),
			Timestamp: review.GetSubmittedAt(),
		}

		comments := comentsByPullRequestReviewID[review.GetID()]

		for _, comment := range comments {
			id := comment.GetNodeID()

			out := PullRequestReview{
				ID:       id,
				Author:   comment.GetUser().GetLogin(),
				Body:     comment.GetBody(),
				Path:     comment.GetPath(),
				DiffHunk: AnnotateDiffHunk(comment.GetDiffHunk()),
			}

			if out.DiffHunk == "" {
				klog.Warningf("Empty diff hunk for review comment %q", id)
				y, _ := yaml.Marshal(comment)
				klog.Infof("Full comment YAML: %s", string(y))
			}
			modelComment.Reviews = append(modelComment.Reviews, out)
		}

		b.model.Comments = append(b.model.Comments, modelComment)
	}

	b.addTestFailures(ctx)
	sort.Slice(b.model.Comments, func(i, j int) bool {
		return b.model.Comments[i].Timestamp.Before(b.model.Comments[j].Timestamp)
	})

	if len(b.model.Comments) == 0 {
		return nil, nil
	}

	var tmpl *template.Template
	if len(alreadyPostedIDs) > 0 {
		tmpl, err = getTemplate("fix_pr_feedback_incremental.txt")
		if err != nil {
			return nil, err
		}
	} else {
		tmpl, err = getTemplate("fix_pr_feedback.txt")
		if err != nil {
			return nil, err
		}
	}

	var w bytes.Buffer
	if err := tmpl.Execute(&w, &b.model); err != nil {
		return nil, fmt.Errorf("failed to execute prompt template: %w", err)
	}

	return w.Bytes(), nil
}

type FixPRFeedbackPromptModel struct {
	PullRequest PullRequest
	Comments    []PullRequestComment

	Upstream      string
	DefaultBranch string
}

type PullRequest struct {
	URL     string
	Number  int
	Title   string
	Body    string
	Commits []PullRequestCommit
}

type PullRequestCommit struct {
	SHA     string
	Message string
}

type PullRequestComment struct {
	ID     string
	Author string
	Body   string

	Reviews []PullRequestReview

	Timestamp time.Time
}

type PullRequestReview struct {
	ID       string
	Author   string
	Body     string
	Path     string
	DiffHunk string
}

func AnnotateDiffHunk(diffHunk string) string {
	lines := strings.Split(diffHunk, "\n")
	var annotatedLines []string

	currentOldLine := 0
	currentNewLine := 0
	// Regex to parse the hunk header: @@ -oldStart,oldLen +newStart,newLen @@
	// This is how we get line numbers
	headerRegex := regexp.MustCompile(`^@@ \-(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			matches := headerRegex.FindStringSubmatch(line)
			if len(matches) == 3 {
				oldStart, err := strconv.Atoi(matches[1])
				if err == nil {
					currentOldLine = oldStart
				}
				newStart, err := strconv.Atoi(matches[2])
				if err == nil {
					currentNewLine = newStart
				}
			}
			continue
		}

		if currentNewLine == 0 && currentOldLine == 0 {
			// If we haven't seen a header yet, just print the line
			annotatedLines = append(annotatedLines, line)
			continue
		}

		if strings.HasPrefix(line, " ") {
			// Context line
			annotatedLines = append(annotatedLines, fmt.Sprintf("%4d: %s", currentNewLine, line))
			currentOldLine++
			currentNewLine++
		} else if strings.HasPrefix(line, "+") {
			// Added line
			annotatedLines = append(annotatedLines, fmt.Sprintf("%4d: %s", currentNewLine, line))
			currentNewLine++
		} else if strings.HasPrefix(line, "-") {
			// Deleted line - show placeholder for line number
			annotatedLines = append(annotatedLines, fmt.Sprintf("....: %s", line))
			currentOldLine++
		} else {
			// Other (e.g. \ No newline at end of file)
			annotatedLines = append(annotatedLines, fmt.Sprintf(".... : %s", line))
		}
	}

	if len(annotatedLines) > 4 {
		// Only show last 4 lines to avoid too much verbosity - github seems to end the chunk at the "right spot"
		annotatedLines = annotatedLines[len(annotatedLines)-4:]
	}

	return strings.Join(annotatedLines, "\n")
}

type ModelBuilder struct {
	githubAPI        *github.Client
	repoInfo         *github.RepoInfo
	pullRequest      *github.PullRequest
	pullRequestInfo  *githubapi.PullRequest
	alreadyPostedIDs map[string]bool

	model FixPRFeedbackPromptModel
}

func (b *ModelBuilder) addTestFailures(ctx context.Context) error {
	log := klog.FromContext(ctx)

	repo := b.repoInfo

	// Add information about test failures
	if false {
		options := &githubapi.ListCheckSuiteOptions{}
		suites, _, err := b.githubAPI.Checks.ListCheckSuitesForRef(ctx, repo.Owner, repo.Name, b.pullRequestInfo.GetHead().GetSHA(), options)
		if err != nil {
			return fmt.Errorf("failed to list check suites for pull request: %w", err)
		}

		for _, checkSuite := range suites.CheckSuites {
			log.Info("found check suite", "name", checkSuite.GetApp().GetName(), "conclusion", checkSuite.GetConclusion())

			var allChecks []*githubapi.CheckRun

			listCheckRunOptions := &githubapi.ListCheckRunsOptions{}
			listCheckRunOptions.PerPage = 100
			listCheckRunOptions.Page = 1
			for {
				checks, _, err := b.githubAPI.Checks.ListCheckRunsCheckSuite(ctx, repo.Owner, repo.Name, checkSuite.GetID(), listCheckRunOptions)
				if err != nil {
					return fmt.Errorf("failed to list check runs for pull request: %w", err)
				}

				allChecks = append(allChecks, checks.CheckRuns...)
				if checks.GetTotal() <= len(allChecks) {
					break
				}
				listCheckRunOptions.Page++
			}

			for _, check := range allChecks {
				id := check.GetNodeID()
				ignoreCheck := false
				switch check.GetConclusion() {
				case "success":
					ignoreCheck = true
				}
				if ignoreCheck {
					continue
				}

				log.Info("found check", "name", check.GetName(), "conclusion", check.GetConclusion())

				if b.alreadyPostedIDs[id] {
					klog.V(2).Infof("Skipping check run %q as already posted", id)
					continue
				}

				// Get the logs for this (failed) check
				// run, _, err := githubAPI.Checks.GetCheckRun(ctx, repo.Owner, repo.Name, check.GetID())
				// if err != nil {
				// 	return nil, fmt.Errorf("failed to get check run for pull request: %w", err)
				// }
				body := fmt.Sprintf("Check **%s** concluded with status **%s**.\n\nDetails: %s", check.GetName(), check.GetConclusion(), check.GetHTMLURL())

				// logs, _, err := githubAPI.Actions.GetWorkflowRunLogs(ctx, repo.Owner, repo.Name, workflow.GetAttempt(), true)
				// if err != nil {
				// 	return nil, fmt.Errorf("failed to get check run logs for pull request: %w", err)
				// }
				// body += fmt.Sprintf("\n\nLogs:\n%s", string(logs))

				modelComment := PullRequestComment{
					Author:    check.GetApp().GetName(),
					Body:      body,
					Timestamp: check.GetCompletedAt().Time,
					ID:        id,
				}
				b.model.Comments = append(b.model.Comments, modelComment)
			}
		}

		// TODO: Pagination?
	}

	{
		listWorkflowRunsOptions := &githubapi.ListWorkflowRunsOptions{
			Branch: b.pullRequestInfo.GetHead().GetRef(),
			// CheckSuite: checkSuite.GetID(),
		}

		runs, _, err := b.githubAPI.Actions.ListRepositoryWorkflowRuns(ctx, repo.Owner, repo.Name, listWorkflowRunsOptions)
		if err != nil {
			return fmt.Errorf("failed to get workflow run for pull request: %w", err)
		}

		for _, run := range runs.WorkflowRuns {

			skip := false
			switch run.GetConclusion() {
			case "success":
				skip = true
			}
			if skip {
				continue
			}

			log.Info("found workflow run", "id", run.GetID(), "name", run.GetName(), "conclusion", run.GetConclusion())

			if run.GetHeadSHA() != b.pullRequestInfo.GetHead().GetSHA() {
				// log.Info("skipping run as head SHA does not match PR", "runHeadSHA", run.GetHeadSHA(), "prHeadSHA", pr.GetHead().GetSHA())
				continue
			}

			runID := run.GetID()

			var allJobs []*githubapi.WorkflowJob
			{
				listWorkflowJobsOptions := &githubapi.ListWorkflowJobsOptions{}
				listWorkflowJobsOptions.Page = 1
				listWorkflowJobsOptions.PerPage = 100
				for {
					jobs, _, err := b.githubAPI.Actions.ListWorkflowJobs(ctx, repo.Owner, repo.Name, runID, listWorkflowJobsOptions)
					if err != nil {
						return fmt.Errorf("failed to list workflow jobs for pull request: %w", err)
					}
					allJobs = append(allJobs, jobs.Jobs...)
					if jobs.GetTotalCount() <= len(allJobs) {
						break
					}
					listWorkflowJobsOptions.Page++
				}
			}

			testFailureCount := 0
			for _, job := range allJobs {
				id := job.GetNodeID()
				if b.alreadyPostedIDs[id] {
					klog.V(2).Infof("Skipping workflow run %q as already posted", id)
					continue
				}

				if job.GetHeadSHA() != b.pullRequestInfo.GetHead().GetSHA() {
					// log.Info("skipping job as head SHA does not match PR", "jobHeadSHA", job.GetHeadSHA(), "prHeadSHA", pr.GetHead().GetSHA())
					continue
				}

				skip := false
				switch job.GetConclusion() {
				case "success":
					skip = true
				}
				if skip {
					continue
				}

				// log.Info("found job", "name", job.GetName(), "conclusion", job.GetConclusion(), "status", job.GetStatus(), "headSHA", job.GetHeadSHA(), "jobURL", job.GetHTMLURL())

				body := "Test failed; relevant log lines:\n"

				// body := fmt.Sprintf("Check **%s** concluded with status **%s**.\n\nDetails: %s", check.GetName(), check.GetConclusion(), check.GetHTMLURL())
				// body += fmt.Sprintf("\n\nrun: %v %v %v %v\n", run.GetID(), run.GetName(), run.GetConclusion(), run.GetStatus())

				if job.GetStatus() != "completed" {
					continue
				}

				// if job.GetStatus() == "skipped" {
				// 	klog.Warningf("skipping job %q as status is skipped", job.GetName())
				// 	continue
				// }

				if job.GetConclusion() == "skipped" {
					klog.Warningf("skipping job %q as conclusion is skipped", job.GetName())
					continue
				}

				// Get the logs for this (failed) check
				followRedirects := true
				logsURL, _, err := b.githubAPI.Actions.GetWorkflowJobLogs(ctx, repo.Owner, repo.Name, job.GetID(), followRedirects)
				if err != nil {
					klog.Infof("job conclusion: %q", job.GetConclusion())
					klog.Infof("job status: %q", job.GetStatus())
					klog.Infof("job: %+v", job)
					klog.Warningf("failed to get workflow job logs URL for pull request for job %s: %v", job.GetHTMLURL(), err)
					// continue
					return fmt.Errorf("failed to get workflow run logs for pull request for job %s: %w", job.GetHTMLURL(), err)
				}

				httpClient := http.DefaultClient

				klog.Infof("Downloading logs from URL: %v", logsURL.String())
				logs, err := httpClient.Get(logsURL.String())
				if err != nil {
					return fmt.Errorf("failed to download workflow run logs for pull request for job %s: %w", job.GetHTMLURL(), err)
				}
				defer logs.Body.Close()
				logsData, err := io.ReadAll(logs.Body)
				if err != nil {
					return fmt.Errorf("failed to read workflow run logs for pull request for job %s: %w", job.GetHTMLURL(), err)
				}

				klog.Infof("Downloaded logs for workflow run %q run=%v job=%v: %v", run.GetName(), run.GetID(), job.GetID(), len(logsData))

				logLines := strings.Split(string(logsData), "\n")
				relevantLines := make(map[int]bool)
				for lineNum, line := range logLines {
					// if strings.Contains(line, "ERROR") || strings.Contains(line, "Error") || strings.Contains(line, "error") {
					// 	klog.Infof("Log line with error: %q", line)
					// }
					if strings.Contains(line, "FAIL:") {
						relevantLines[lineNum] = true
					}
					if strings.Contains(line, "<hint_for_agent>") {
						relevantLines[lineNum] = true
					}
				}

				// Fallback to looking for ERROR if no "high confidence lines" found
				if len(relevantLines) == 0 {
					for lineNum, line := range logLines {
						if strings.Contains(line, "ERROR") || strings.Contains(line, "Error") || strings.Contains(line, "error") {
							relevantLines[lineNum] = true
						}
					}
				}

				// TODO: What if still no relevant lines?

				// relevantLineCount := 0
				for relevantLine := range relevantLines {
					// Include some context lines
					for i := 1; i <= 2; i++ {
						if relevantLine-i >= 0 {
							relevantLines[relevantLine-i] = true
						}
						if relevantLine+i < len(logLines) {
							relevantLines[relevantLine+i] = true
						}
					}
					// relevantLineCount++
					// if relevantLineCount >= 5 {
					// 	break
					// }
				}

				for lineNum, line := range logLines {
					if relevantLines[lineNum] {
						// Add some ... indicators if the lines are not contiguous
						if lineNum > 0 && !relevantLines[lineNum-1] {
							body += "...\n"
						}
						body += fmt.Sprintf("%4d: %s\n", lineNum+1, line)
					}
				}

				modelComment := PullRequestComment{
					Author:    "GitHub Actions Test " + run.GetName() + "/" + job.GetName(),
					Body:      body,
					Timestamp: run.GetUpdatedAt().Time,
					ID:        id,
				}
				b.model.Comments = append(b.model.Comments, modelComment)
				testFailureCount++
				if testFailureCount >= 5 {
					break
				}
			}
		}
	}
	return nil
}
