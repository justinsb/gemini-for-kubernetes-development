package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/clients"
	"github.com/gke-labs/gemini-for-kubernetes-development/repo-agent/pkg/github"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	sandboxapi "sigs.k8s.io/agent-sandbox/api/v1alpha1"
)

// CodebotSandbox represents an agent sandbox being used to fix a GitHub issue.
type CodebotSandbox struct {
	kube        *clients.KubernetesClient
	podID       types.NamespacedName
	repo        *github.Repo
	issue       *github.Issue
	pullRequest *github.PullRequest

	resources []string
}

func (s *CodebotSandbox) Resources() []string {
	return s.resources
}

func (s *CodebotSandbox) MkdirAll(ctx context.Context, path string) error {
	opts := execOptions{
		Command: []string{"mkdir", "-p", path},
	}
	if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
		return fmt.Errorf("creating directory %q in pod: %w", path, err)
	}
	return nil
}

func (s *CodebotSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := writeFileInPod(ctx, s.kube, s.podID, path, data); err != nil {
		return fmt.Errorf("writing file %q in pod: %w", path, err)
	}
	return nil
}

func launchSandboxForIssue(ctx context.Context, kube *clients.KubernetesClient, repo *github.Repo, issue *github.Issue) (*CodebotSandbox, error) {
	sandboxName := sandboxNameForIssue(repo, issue)
	issueURL := issue.String()

	sandbox, err := launchSandbox(ctx, kube, sandboxName, repo, issueURL)
	if err != nil {
		return nil, err
	}

	if err := sandbox.setupGit(ctx); err != nil {
		return nil, fmt.Errorf("setting up git in sandbox: %w", err)
	}

	if err := sandbox.SetupGitRepos(ctx); err != nil {
		return nil, fmt.Errorf("cloning repo in sandbox: %w", err)
	}

	return &CodebotSandbox{
		kube:      kube,
		podID:     sandbox.podID,
		repo:      repo,
		issue:     issue,
		resources: sandbox.resources,
	}, nil
}

func launchSandboxForPullRequest(ctx context.Context, kube *clients.KubernetesClient, repo *github.Repo, pr *github.PullRequest) (*CodebotSandbox, error) {
	sandboxName := sandboxNameForPullRequest(repo, pr)
	// TODO: Add support for PR URL?
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", repo.Owner, repo.Name, pr.PullRequestNumber)

	sandbox, err := launchSandbox(ctx, kube, sandboxName, repo, prURL)
	if err != nil {
		return nil, err
	}

	if err := sandbox.setupGit(ctx); err != nil {
		return nil, fmt.Errorf("setting up git in sandbox: %w", err)
	}

	if err := sandbox.SetupGitRepos(ctx); err != nil {
		return nil, fmt.Errorf("cloning repo in sandbox: %w", err)
	}

	return &CodebotSandbox{
		kube:        kube,
		podID:       sandbox.podID,
		repo:        repo,
		pullRequest: pr,
	}, nil
}

func launchSandbox(ctx context.Context, kube *clients.KubernetesClient, sandboxName string, repo *github.Repo, issueURL string) (*CodebotSandbox, error) {
	log := klog.FromContext(ctx)

	cloneRepos := []string{
		fmt.Sprintf("/workspaces/%s=%s", repo.FilesystemName(), repo.GitCloneURL()),
	}

	log.Info("Creating sandbox", "name", sandboxName, "repos", cloneRepos, "issue", issueURL)

	container := v1.Container{}
	container.Name = "agent"
	container.Image = "gcr.io/justinsb-knotai-dev/generic-golang:latest"

	// container.Env = append(container.Env, v1.EnvVar{
	// 	Name:  "CLONE_REPOS",
	// 	Value: strings.Join(cloneRepos, ";"),
	// })

	sandbox := &sandboxapi.Sandbox{}
	sandbox.Name = sandboxName
	sandbox.Namespace = kube.CurrentNamespace

	nodeSelector := make(map[string]string)
	podResourceRequests := make(v1.ResourceList)
	podResourceLimits := make(v1.ResourceList)

	gpu := false
	mountDocker := true
	mountKubeNamespace := false

	var resources []string

	if repo.Name == "generation-ai" {
		gpu = true

		mountKubeNamespace = true
		// 		gcloud beta container --project "justinsb-knotai-dev" node-pools create "rtx6000-gpus" --cluster "ai-us-central1-b" --zone "us-central1-b" --node-version "1.33.5-gke.2172001" \
		//   --machine-type "g4-standard-48" --accelerator "type=nvidia-rtx-pro-6000,count=1,gpu-sharing-strategy=mps,max-shared-clients-per-gpu=48" --image-type "COS_CONTAINERD" \
		//    --disk-type "hyperdisk-balanced" --disk-size "100" --ephemeral-storage-local-ssd count=4 --metadata disable-legacy-endpoints=true --num-nodes "3" --enable-autoscaling \
		//     --min-nodes "0" --max-nodes "3" --location-policy "BALANCED" --enable-autoupgrade --enable-autorepair --max-surge-upgrade 1 --max-unavailable-upgrade 0 \
		//     --shielded-integrity-monitoring --no-shielded-secure-boot --node-locations "us-central1-b","us-central1-f"

		// gcloud beta container --project "justinsb-knotai-dev" node-pools create "l4-gpus-time-sharing" --cluster "ai-us-central1-b" --zone "us-central1-b" --node-version "1.33.5-gke.2172001" --machine-type "g2-standard-32" --accelerator "type=nvidia-l4,count=1,gpu-sharing-strategy=time-sharing,max-shared-clients-per-gpu=48" --image-type "COS_CONTAINERD" --disk-type "pd-balanced" --disk-size "100" --ephemeral-storage-local-ssd count=1 --metadata disable-legacy-endpoints=true --num-nodes "3" --enable-autoscaling --min-nodes "1" --max-nodes "3" --location-policy "BALANCED" --enable-autoupgrade --enable-autorepair --max-surge-upgrade 1 --max-unavailable-upgrade 0 --shielded-integrity-monitoring --no-shielded-secure-boot --node-locations "us-central1-b","us-central1-a","us-central1-c"

		/*

		   gcloud beta container --project "justinsb-knotai-dev" node-pools create "l4-gpus-time-sharing" --cluster "ai-us-central1-b" \
		   --zone "us-central1-b" --node-version "1.33.5-gke.2172001" --machine-type "g2-standard-32" --accelerator "type=nvidia-l4,count=1,gpu-sharing-strategy=time-sharing,max-shared-clients-per-gpu=48,gpu-driver-version=latest" \
		   --image-type "COS_CONTAINERD" --disk-type "pd-balanced" --disk-size "100" --ephemeral-storage-local-ssd count=1 --metadata disable-legacy-endpoints=true --num-nodes "3" --enable-autoscaling --min-nodes "1" --max-nodes "3" \
		   --location-policy "BALANCED" --enable-autoupgrade --enable-autorepair --max-surge-upgrade 1 --max-unavailable-upgrade 0 --shielded-integrity-monitoring --no-shielded-secure-boot --node-locations "us-central1-b","us-central1-a","us-central1-c"

		*/

	}

	if mountKubeNamespace {
		ns := &unstructured.Unstructured{}
		ns.SetName("workspace-" + sandboxName)
		gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
		ns.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
		applyOptions := metav1.ApplyOptions{
			FieldManager: "repo-sandbox",
		}
		if _, err := kube.DynamicClient.Resource(gvr).Apply(ctx, ns.GetName(), ns, applyOptions); err != nil {
			return nil, fmt.Errorf("creating namespace %q: %w", ns.GetName(), err)
		}

		sa := &unstructured.Unstructured{}
		sa.SetName(sandboxName)
		sa.SetNamespace(kube.CurrentNamespace)
		sa.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"})
		if _, err := kube.DynamicClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}).Namespace(sa.GetNamespace()).Apply(ctx, sa.GetName(), sa, applyOptions); err != nil {
			return nil, fmt.Errorf("creating service account %q in namespace %q: %w", sa.GetName(), sa.GetNamespace(), err)
		}

		roleBinding := &unstructured.Unstructured{}
		roleBinding.SetName(sandboxName)
		roleBinding.SetNamespace(ns.GetName())
		roleBinding.SetGroupVersionKind(schema.GroupVersionKind{Version: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"})
		roleBinding.Object["subjects"] = []map[string]any{
			{
				"kind":      "ServiceAccount",
				"name":      sa.GetName(),
				"namespace": sa.GetNamespace(),
			},
		}
		roleBinding.Object["roleRef"] = map[string]any{
			"kind":     "ClusterRole",
			"name":     "admin",
			"apiGroup": "rbac.authorization.k8s.io",
		}

		if _, err := kube.DynamicClient.Resource(schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}).Namespace(roleBinding.GetNamespace()).Apply(ctx, roleBinding.GetName(), roleBinding, applyOptions); err != nil {
			return nil, fmt.Errorf("creating role binding %q in namespace %q: %w", roleBinding.GetName(), roleBinding.GetNamespace(), err)
		}

		sandbox.Spec.PodTemplate.Spec.ServiceAccountName = sa.GetName()

		resources = append(resources, fmt.Sprintf("* You can create resources in the namespace %q using your default (Pod) kubernetes credentials.", ns.GetName()))
	}

	if gpu {
		nodeSelector["cloud.google.com/gke-gpu-sharing-strategy"] = "time-sharing"
		sandbox.Spec.PodTemplate.Spec.NodeSelector = nodeSelector

		// sandbox.Spec.PodTemplate.Spec.Tolerations = []v1.Toleration{
		// 	{
		// 		Key:      "cloud.google.com/gke-gpu-sharing-strategy",
		// 		Operator: v1.TolerationOpEqual,
		// 		Value:    "MPS",
		// 		Effect:   v1.TaintEffectNoSchedule,
		// 	},
		// }
		// container.Resources = v1.ResourceRequirements{
		// 	Limits: v1.ResourceList{
		// 		"nvidia.com/gpu": resource.MustParse("1"),
		// 	},
		// }

		// resourceRequests["nvidia.com/gpu"] = resource.MustParse("1")
		// resourceLimits["nvidia.com/gpu"] = resource.MustParse("1")

		resources = append(resources, fmt.Sprintf("* You have access to a GPU"))
	}

	if mountDocker {
		dind := v1.Container{}
		dind.Name = "dind"
		dind.Image = "docker:dind"
		dind.SecurityContext = &v1.SecurityContext{
			Privileged: ptr.To(true),
		}
		dind.Env = append(dind.Env, v1.EnvVar{
			Name:  "DOCKER_TLS_CERTDIR",
			Value: "", // Disable TLS for simplicity
		})
		dind.VolumeMounts = append(dind.VolumeMounts, v1.VolumeMount{
			Name:      "docker-graph",
			MountPath: "/var/lib/docker",
		})

		sandbox.Spec.PodTemplate.Spec.Volumes = append(sandbox.Spec.PodTemplate.Spec.Volumes, v1.Volume{
			Name: "docker-graph",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		})

		if gpu {
			dind.Resources = v1.ResourceRequirements{
				Limits: v1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("1"),
				},
			}
		}

		sandbox.Spec.PodTemplate.Spec.Containers = append(sandbox.Spec.PodTemplate.Spec.Containers, dind)

		container.Env = append(container.Env, v1.EnvVar{
			Name:  "DOCKER_HOST",
			Value: "tcp://localhost:2375",
		})

		resources = append(resources, fmt.Sprintf("* You can run Docker commands, DOCKER_HOST is pointing to a docker daemon running in the same pod as your agent container."))
	}

	sandbox.Spec.PodTemplate.Spec.Containers = append(sandbox.Spec.PodTemplate.Spec.Containers, container)

	sandbox.Spec.PodTemplate.Spec.Resources = &v1.ResourceRequirements{
		Requests: podResourceRequests,
		Limits:   podResourceLimits,
	}

	sandbox.Spec.PodTemplate.ObjectMeta.Labels = map[string]string{
		// This enables findSandbox to work, even if we are launching the dev sandbox directly
		"sandbox": "devc-" + sandboxName,
	}

	sandbox.Annotations = map[string]string{
		"repo-agent.labs.gke.io/clone-repos": strings.Join(cloneRepos, ";"),
		"repo-agent.labs.gke.io/fix-issue":   issueURL,
	}

	sandboxGVR := sandboxapi.GroupVersion.WithResource("sandboxes")
	sandboxGVK := sandboxapi.GroupVersion.WithKind("Sandbox")

	sandbox.SetGroupVersionKind(sandboxGVK)

	uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(sandbox)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: uObj}
	_, err = kube.DynamicClient.Resource(sandboxGVR).Namespace(sandbox.Namespace).Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", err)
	}

	log.Info("Sandbox created", "name", sandboxName)

	podID := types.NamespacedName{
		Namespace: kube.CurrentNamespace,
		Name:      sandboxName,
	}

	if err := waitForPodReady(ctx, kube, podID); err != nil {
		return nil, err
	}

	s := &CodebotSandbox{
		kube:      kube,
		podID:     podID,
		repo:      repo,
		resources: resources,
	}

	return s, nil
}

func sandboxNameForIssue(repo *github.Repo, issue *github.Issue) string {
	return sandboxNameForRepoAndNumber(repo, issue.IssueNumber)
}

func sandboxNameForPullRequest(repo *github.Repo, pr *github.PullRequest) string {
	return sandboxNameForRepoAndNumber(repo, pr.PullRequestNumber)
}

func sandboxNameForRepoAndNumber(repo *github.Repo, number int) string {
	sandboxName := fmt.Sprintf("github-%s-%s-%d", repo.Owner, repo.Name, number)
	sandboxName = strings.ToLower(sandboxName) // Repos can have capital letters, but k8s names must be lowercase
	return sandboxName
}

func findSandboxForIssue(ctx context.Context, kube *clients.KubernetesClient, repo *github.Repo, issue *github.Issue) (*CodebotSandbox, bool, error) {
	sandboxName := sandboxNameForIssue(repo, issue)

	podIDPtr, err := findSandboxPod(ctx, sandboxName)
	if err != nil {
		return nil, false, err
	}

	if podIDPtr == nil {
		return nil, false, nil
	}

	return &CodebotSandbox{
		kube:  kube,
		podID: *podIDPtr,
		repo:  repo,
		issue: issue,
	}, true, nil
}

func findSandboxForPullRequest(ctx context.Context, kube *clients.KubernetesClient, repo *github.Repo, pr *github.PullRequest) (*CodebotSandbox, bool, error) {
	sandboxName := sandboxNameForPullRequest(repo, pr)

	podIDPtr, err := findSandboxPod(ctx, sandboxName)
	if err != nil {
		return nil, false, err
	}

	if podIDPtr == nil {
		return nil, false, nil
	}

	return &CodebotSandbox{
		kube:        kube,
		podID:       *podIDPtr,
		repo:        repo,
		pullRequest: pr,
	}, true, nil
}

func (s *CodebotSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var stdout bytes.Buffer

	opt := execOptions{
		Command: []string{"cat", path},
		Stdout:  &stdout,
	}

	if err := execInPod(ctx, s.kube, s.podID, opt); err != nil {
		return nil, fmt.Errorf("reading file %q in pod: %w", path, err)
	}

	return stdout.Bytes(), nil
}

func (s *CodebotSandbox) setupGit(ctx context.Context) error {
	// log := klog.FromContext(ctx)

	// Write gh config
	{
		config := `github.com:
    users:
        codebot-robot:
            oauth_token: {{CODEBOT_ROBOT_GITHUB_TOKEN}}
    git_protocol: https
    oauth_token: {{CODEBOT_ROBOT_GITHUB_TOKEN}}
    user: codebot-robot
`

		codebotRobotToken := os.Getenv("CODEBOT_ROBOT_GITHUB_TOKEN")
		if codebotRobotToken == "" {
			return fmt.Errorf("CODEBOT_ROBOT_GITHUB_TOKEN environment variable is not set")
		}

		config = strings.ReplaceAll(config, "{{CODEBOT_ROBOT_GITHUB_TOKEN}}", codebotRobotToken)

		opts := execOptions{
			Command: []string{"mkdir", "-p", "/root/.config/gh"},
		}
		if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
			return fmt.Errorf("creating /root/.config/gh directory: %w", err)
		}

		if err := writeFileInPod(ctx, s.kube, s.podID, "/root/.config/gh/hosts.yml", []byte(config)); err != nil {
			return fmt.Errorf("writing gh config into pod: %w", err)
		}
	}

	// Run git config
	{
		opts := execOptions{
			Command: []string{"git", "config", "--global", "user.email", "codebot-robot@google.com"},
		}
		if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
			return fmt.Errorf("running git config user.email in pod: %w", err)
		}
		opts = execOptions{
			Command: []string{"git", "config", "--global", "user.name", "codebot-robot"},
		}
		if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
			return fmt.Errorf("running git config user.name in pod: %w", err)
		}
	}

	// Run gh auth setup-git
	{
		opts := execOptions{
			Command: []string{"gh", "auth", "setup-git"},
		}
		if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
			return fmt.Errorf("running gh auth setup-git in pod: %w", err)
		}
	}

	return nil
}

func (s *CodebotSandbox) SetupGitRepos(ctx context.Context) error {
	log := klog.FromContext(ctx)

	workdir := fmt.Sprintf("/workspaces/%s", s.repo.FilesystemName())

	// Clone the repo
	log.Info("Cloning repository in pod", "pod", s.podID.Name, "repo", s.repo.GitCloneURL())
	{
		exists := false
		{
			var stdout bytes.Buffer
			opts := execOptions{
				Command: []string{"stat", filepath.Join(workdir, ".git")},
				Stdout:  &stdout,
			}
			if err := execInPod(ctx, s.kube, s.podID, opts); err == nil {
				exists = true
			}
		}
		if !exists {
			repoURL := s.repo.GitCloneURL()
			repoDir := workdir
			if err := execInPod(ctx, s.kube, s.podID, execOptions{
				Command: []string{"git", "clone", repoURL, repoDir},
				Stdout:  os.Stdout,
				Stderr:  os.Stderr,
			}); err != nil {
				return fmt.Errorf("failed to clone repo %q in %q: %w", repoURL, repoDir, err)
			}
		}
	}

	shouldFork := true
	if strings.Contains(s.repo.GitCloneURL(), "/sandboxes") {
		shouldFork = false
	}
	if shouldFork {
		// Run gh repo fork
		log.Info("Forking repository in pod", "pod", s.podID.Name, "repo", s.repo.GitCloneURL())
		{
			// TODO: Does gh support -C ?
			opts := execOptions{
				Command: []string{"sh", "-c", fmt.Sprintf("cd %s && gh repo fork --remote", workdir)},
			}
			if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
				return fmt.Errorf("running gh repo fork in pod: %w", err)
			}
		}
	}

	// Setup default remote
	{
		defaultRepo := s.repo.GitCloneURL()

		// TODO: Does gh support -C ?
		opts := execOptions{
			Command: []string{"sh", "-c", fmt.Sprintf("cd %s && gh repo set-default %s", workdir, defaultRepo)},
		}
		if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
			return fmt.Errorf("running gh repo set-default in pod: %w", err)
		}

	}

	// Wait for checkout to complete
	{
		timeoutAt := time.Now().Add(time.Minute)
		for {
			log.Info("Waiting for checkout to be ready")

			var stdout bytes.Buffer
			opts := execOptions{
				Command: []string{"git", "-C", workdir, "branch", "--show-current"},
				Stdout:  &stdout,
			}
			if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
				klog.Infof("stdout: %v", stdout.String())
				if time.Now().After(timeoutAt) {
					return fmt.Errorf("timed out waiting for initial checkout to complete: %w", err)
				}
			} else {
				klog.Infof("current branch: %v", stdout.String())
				break
			}

			time.Sleep(2 * time.Second)
		}
	}

	return nil
}

func (s *CodebotSandbox) CheckoutNewBranch(ctx context.Context) error {
	log := klog.FromContext(ctx)

	workdir := fmt.Sprintf("/workspaces/%s", s.repo.FilesystemName())

	branchName := fmt.Sprintf("issue_%d", s.issue.IssueNumber)

	// Create a new branch
	log.Info("Creating new branch in pod", "pod", s.podID.Name, "branch", branchName)

	opts := execOptions{
		Command: []string{"git", "-C", workdir, "checkout", "-b", branchName},
	}
	if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
		return fmt.Errorf("creating new branch in pod: %w", err)
	}

	return nil
}

func (s *CodebotSandbox) CheckoutExistingBranch(ctx context.Context, branchName string) error {
	log := klog.FromContext(ctx)

	workdir := fmt.Sprintf("/workspaces/%s", s.repo.FilesystemName())

	log.Info("Fetching from fork in pod", "pod", s.podID.Name)

	opts := execOptions{
		Command: []string{"git", "-C", workdir, "fetch", "origin"},
	}
	if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
		return fmt.Errorf("fetching from fork in pod: %w", err)
	}

	opts = execOptions{
		Command: []string{"git", "-C", workdir, "checkout", branchName},
	}
	if err := execInPod(ctx, s.kube, s.podID, opts); err != nil {
		return fmt.Errorf("checking out branch %q in pod: %w", branchName, err)
	}

	return nil
}

func (s *CodebotSandbox) ListThreads(ctx context.Context) ([]ThreadInfo, error) {
	threads, err := listThreads(ctx, s.podID)
	if err != nil {
		return nil, fmt.Errorf("failed to list threads: %w", err)
	}
	return threads, nil
}

func (s *CodebotSandbox) GetThreadMessages(ctx context.Context, threadID string) ([]ThreadMessage, error) {
	getThreadOptions := GetThreadsOptions{
		ThreadID:        threadID,
		IncludeMessages: true,
	}
	thread, err := getThread(ctx, s.podID, getThreadOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get thread %q: %w", threadID, err)
	}
	return thread.Messages, nil
}

func configureGemini(ctx context.Context, sandbox *CodebotSandbox) error {
	log := klog.FromContext(ctx)

	// Configure gemini
	{
		general := map[string]any{
			"enableAutoUpdate": false,
			"retryFetchErrors": true,
		}

		config := map[string]any{
			"general": general,
		}

		// Maybe:
		// general.checkpointing.enabled
		// output.format
		// general.sessionRetention.enabled (but false is default)
		// model.summarizeToolOutput
		// experimental.enableAgents
		// experimental.plan
		// experimental.codebaseInvestigatorSettings
		// Memory in a shared location?
		// Hooks?
		// telemetry?
		// ui.theme?

		// TODO: Install ripgrep?

		b, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling gemini config: %w", err)
		}

		log.Info("Writing gemini config in pod", "pod", sandbox.podID)

		// if b0, err := sandbox.ReadFile(ctx, "/root/.gemini/settings.json"); err != nil {
		// 	return fmt.Errorf("reading gemini config in pod: %w", err)
		// } else {
		// 	klog.Infof("Existing gemini config: %s", string(b0))
		// }

		if err := sandbox.MkdirAll(ctx, "/root/.gemini"); err != nil {
			return fmt.Errorf("creating /root/.gemini directory in pod: %w", err)
		}

		if err := sandbox.WriteFile(ctx, "/root/.gemini/settings.json", b); err != nil {
			return fmt.Errorf("writing gemini config in pod: %w", err)
		}
	}

	if err := increaseGeminiRetryLimit(ctx, sandbox); err != nil {
		return fmt.Errorf("increasing gemini retry limit in pod: %w", err)
	}

	return nil
}

func increaseGeminiRetryLimit(ctx context.Context, sandbox *CodebotSandbox) error {
	log := klog.FromContext(ctx)

	log.Info("Increasing gemini retry limit in pod", "pod", sandbox.podID)

	return nil
}
