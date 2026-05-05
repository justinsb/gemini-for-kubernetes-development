package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/clients"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/github"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/prompts"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// GithubFixIssueOptions holds options for the RunCode function.
type GithubFixIssueOptions struct {
	URL string

	// Model is the LLM model to use.
	Model string
}

func (o *GithubFixIssueOptions) InitDefaults() {
	o.Model = "gemini-3-pro-preview"
}

// BuildGithubFixIssueCommand creates a new cobra command for using a dev sandbox to solve a github issue
func BuildGithubFixIssueCommand() *cobra.Command {
	var opt GithubFixIssueOptions

	opt.InitDefaults()

	cmd := &cobra.Command{
		Use:   "github-fix-issue",
		Short: "Fix a github issue using an LLM in a dev sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("command does not take positional arguments")
			}

			return RunGithubFixIssue(cmd.Context(), opt)
		},
	}

	cmd.Flags().StringVar(&opt.URL, "url", opt.URL, "GitHub issue URL")
	cmd.Flags().StringVar(&opt.Model, "model", opt.Model, "LLM model to use")

	return cmd
}

// RunGithubFixIssue launches VS Code connected to the specified dev sandbox.
func RunGithubFixIssue(ctx context.Context, opt GithubFixIssueOptions) error {
	log := klog.FromContext(ctx)

	codebotRobotToken := os.Getenv("CODEBOT_ROBOT_GITHUB_TOKEN")
	if codebotRobotToken == "" {
		return fmt.Errorf("CODEBOT_ROBOT_GITHUB_TOKEN environment variable is not set")
	}

	kube, err := clients.NewKubernetesClient()
	if err != nil {
		return err
	}

	githubAPI, err := github.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create github client: %w", err)
	}

	if opt.URL == "" {
		return fmt.Errorf("--url is required")
	}

	issue, err := github.ParseIssueURL(opt.URL)
	if err != nil {
		return err
	}
	repo := issue.Repo

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

	prompt, err := prompts.FixIssuePrompt(ctx, githubAPI, issue, sandbox.Resources())
	if err != nil {
		return fmt.Errorf("failed to generate prompt for issue: %w", err)
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

	// HACK: Avoid git lock issues
	time.Sleep(5 * time.Second)

	if err := sandbox.CheckoutNewBranch(ctx); err != nil {
		return fmt.Errorf("checking out branch: %w", err)
	}

	if err := configureGemini(ctx, sandbox); err != nil {
		return fmt.Errorf("configuring gemini in sandbox: %w", err)
	}

	// Copy the prompt into the pod (for now)
	if len(prompt) > 0 {
		log.Info("copying prompt into sandbox pod", "pod", sandbox.podID)

		path := "/workspaces/prompt.txt"
		if err := writeFileInPod(ctx, kube, sandbox.podID, path, prompt); err != nil {
			return fmt.Errorf("copying prompt into sandbox pod: %w", err)
		}

		log.Info("Copied prompt into sandbox pod", "pod", sandbox.podID, "path", path)
	}

	// Run gemini with API key and prompt
	{
		log.Info("Running gemini in pod", "pod", sandbox.podID)

		workdir := fmt.Sprintf("/workspaces/%s", sandbox.repo.FilesystemName())

		// TODO:
		// export GEMINI_TELEMETRY_ENABLED=true
		// export GEMINI_TELEMETRY_OTLP_ENDPOINT=http://otel-portal.otel-system:4317

		opts := execOptions{
			Command: []string{"sh", "-c", fmt.Sprintf("cd %s && export GEMINI_API_KEY=%s && gemini --yolo --model %s < /workspaces/prompt.txt", workdir, geminiAPIKey, opt.Model)},
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
		}
		opts.Secrets = []string{geminiAPIKey}

		if err := execInPod(ctx, kube, sandbox.podID, opts); err != nil {
			collectLogs(ctx, kube, sandbox.podID)
			return fmt.Errorf("running gemini: %w", err)
		}
		collectLogs(ctx, kube, sandbox.podID)

	}

	return nil
}

// collectLogs will download the logs from the specified pod and write them to the the _logs directory.
func collectLogs(ctx context.Context, kube *clients.KubernetesClient, podID types.NamespacedName) error {
	log := klog.FromContext(ctx)

	logDir := filepath.Join("_logs", podID.Namespace+"_"+podID.Name, time.Now().Format("20060102_150405"))

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("creating logs directory: %w", err)
	}

	downloadKubeLogs := func() error {
		logs, err := kube.Clientset.CoreV1().Pods(podID.Namespace).GetLogs(podID.Name, &v1.PodLogOptions{Container: "agent"}).Do(ctx).Raw()
		if err != nil {
			return fmt.Errorf("failed to get logs from pod: %w", err)
		}

		logFilePath := filepath.Join(logDir, "pod.log")
		if err := os.WriteFile(logFilePath, logs, 0644); err != nil {
			return fmt.Errorf("failed to write logs to file: %w", err)
		}
		return nil
	}

	downloadGeminiLogs := func() error {
		// Run kubectl cp -c agent <pod>:/root/.gemini/tmp <local-dir>
		cmd := exec.CommandContext(ctx, "kubectl", "cp", "-c", "agent", fmt.Sprintf("%s/%s:/root/.gemini", podID.Namespace, podID.Name), filepath.Join(logDir, "gemini-logs"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to copy gemini logs from pod: %w: %s", err, stderr.String())
		}

		return nil
	}

	downloadGeminiStdout := func() error {
		// Run kubectl cp -c agent <pod>:/root/.gemini/tmp <local-dir>
		cmd := exec.CommandContext(ctx, "kubectl", "cp", "-c", "agent", fmt.Sprintf("%s/%s:/workspaces/gemini.log", podID.Namespace, podID.Name), filepath.Join(logDir, "gemini.log"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to copy gemini stdout log from pod: %w: %s", err, stderr.String())
		}

		return nil
	}

	var errs []error
	errs = append(errs, downloadKubeLogs())
	errs = append(errs, downloadGeminiLogs())
	errs = append(errs, downloadGeminiStdout())

	err := errors.Join(errs...)
	if err != nil {
		log.Error(err, "Failed to collect logs from pod", "pod", podID)
	} else {
		log.Info("Collected logs from pod", "pod", podID, "logDir", logDir)
	}
	return err
}

type execOptions struct {
	Command []string
	Secrets []string

	Stdin  []byte
	Stdout io.Writer
	Stderr io.Writer
}

// execInPod writes the specified data to a file in the specified pod.
func execInPod(ctx context.Context, kube *clients.KubernetesClient, podID types.NamespacedName, opts execOptions) error {
	log := klog.FromContext(ctx)

	redactedCommand := strings.Join(opts.Command, " ")
	for _, v := range opts.Secrets {
		redactedCommand = strings.ReplaceAll(redactedCommand, v, "****")
	}

	podExecOptions := &v1.PodExecOptions{
		// Container: containerName,
		Container: "agent",
		Command:   opts.Command,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
	if opts.Stdin == nil {
		podExecOptions.Stdin = false
	}

	req := kube.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podID.Name).
		Namespace(podID.Namespace).
		SubResource("exec").
		VersionedParams(podExecOptions, scheme.ParameterCodec)

	url := req.URL().String()
	exec, err := remotecommand.NewWebSocketExecutor(kube.RestConfig, "POST", url)
	if err != nil {
		return fmt.Errorf("executing command in pod: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	streamOptions := remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	}
	if opts.Stdin != nil {
		streamOptions.Stdin = bytes.NewReader(opts.Stdin)
	}
	if opts.Stdout != nil {
		streamOptions.Stdout = opts.Stdout
	}
	if opts.Stderr != nil {
		streamOptions.Stderr = opts.Stderr
	}

	// Run the command
	if err := exec.StreamWithContext(ctx, streamOptions); err != nil {
		log.Error(err, "executing command", "pod", podID, "command", redactedCommand, "stdout", stdout.String(), "stderr", stderr.String())
		return fmt.Errorf("streaming command in pod: %w", err)
	}

	log.Info("executed command", "pod", podID, "command", redactedCommand, "stdout", stdout.String(), "stderr", stderr.String())
	return nil
}

// writeFileInPod writes the specified data to a file in the specified pod.
func writeFileInPod(ctx context.Context, kube *clients.KubernetesClient, podID types.NamespacedName, path string, data []byte) error {
	// log := klog.FromContext(ctx)

	var stdout bytes.Buffer

	opt := execOptions{
		Command: []string{"/bin/tee", path},
		Stdin:   data,
		Stdout:  &stdout, // To avoid logging to stdout
	}

	return execInPod(ctx, kube, podID, opt)
}

// waitForPodReady waits for the specified pod to be ready.
func waitForPodReady(ctx context.Context, kube *clients.KubernetesClient, podID types.NamespacedName) error {
	log := klog.FromContext(ctx)

	clientset := kube.Clientset

	log.Info("Waiting for sandbox pod to be ready", "pod", podID.Name)

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	for {
		stream, err := clientset.CoreV1().Pods(podID.Namespace).Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + podID.Name, Watch: true})
		if err != nil {
			return err
		}
		defer stream.Stop()
		for event := range stream.ResultChan() {
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				return fmt.Errorf("unexpected type %T when watching pod", event.Object)
			}
			if isPodReady(pod) {
				log.Info("Sandbox pod is ready", "pod", podID.Name)
				return nil
			}
		}
	}
}

func isPodReady(pod *v1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.PodReady {
			return cond.Status == v1.ConditionTrue
		}
	}
	return false
}
