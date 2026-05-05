package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

// ThreadsAgentOptions holds options for the ThreadsAgent function.
type ThreadsAgentOptions struct {
	SandboxName string

	// The ThreadID to operate on
	ThreadID string

	IncludeMessages bool

	// Action determines what action to perform: "list" or "append"
	Action string

	// Cwd is the current working directory for the agent
	Cwd string
}

func (o *ThreadsAgentOptions) InitDefaults() {
	o.Action = "list"
}

// NewThreadsAgentCommand creates a new cobra command for managing LLM threads/chats in the dev sandbox.
func NewThreadsAgentCommand() *cobra.Command {
	var opt ThreadsAgentOptions

	opt.InitDefaults()

	cmd := &cobra.Command{
		Use:   "agent [sandbox-name]",
		Short: "Manage LLM threads/chats in the dev sandbox",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return fmt.Errorf("threads agent command does not take any arguments")
			}
			return RunThreadsAgent(cmd.Context(), opt)
		},
	}
	cmd.Hidden = true

	cmd.Flags().StringVar(&opt.ThreadID, "thread-id", opt.ThreadID, "If specified, filter only for the given thread ID")
	cmd.Flags().StringVar(&opt.Action, "action", opt.Action, "Action to perform: list or append")
	cmd.Flags().BoolVar(&opt.IncludeMessages, "include-messages", opt.IncludeMessages, "If specified, include messages in the output")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", opt.Cwd, "Current working directory for the agent")

	return cmd
}

// RunThreadsAgent runs the threads agent in the specified dev sandbox.
func RunThreadsAgent(ctx context.Context, opt ThreadsAgentOptions) error {
	agent := &threadsAgent{}

	switch opt.Action {
	case "list":
		return agent.List(ctx, opt)
	case "append":
		return agent.Append(ctx, opt)

	default:
		return fmt.Errorf("unknown action %q", opt.Action)
	}
}

func (a *threadsAgent) List(ctx context.Context, opt ThreadsAgentOptions) error {
	threads, err := a.listThreads(ctx, opt)
	if err != nil {
		return fmt.Errorf("failed to list threads: %w", err)
	}

	b, err := json.Marshal(threads)
	if err != nil {
		return fmt.Errorf("failed to marshal threads to JSON: %w", err)
	}
	if _, err := os.Stdout.Write(b); err != nil {
		return fmt.Errorf("failed to write threads to stdout: %w", err)
	}
	return nil
}

func (a *threadsAgent) Append(ctx context.Context, opt ThreadsAgentOptions) error {
	log := klog.FromContext(ctx)

	if opt.ThreadID == "" {
		// TODO: Support creating a new thread?
		return fmt.Errorf("--thread-id is required for append action")
	}

	// Read stdin
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}

	// Run gemini in yolo mode, resuming the specified thread

	// Use an uncancellable context to avoid issues with the connection being lost
	geminiCtx := context.WithoutCancel(ctx)

	cmd := exec.CommandContext(geminiCtx, "gemini", "--yolo",
		"--resume", opt.ThreadID,
	)
	cmd.Stdin = bytes.NewReader(stdin)
	if opt.Cwd != "" {
		cmd.Dir = opt.Cwd
	}

	// Our stdout/stderr go to the kubectl exec command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Info("Running gemini in yolo mode to append to thread", "threadID", opt.ThreadID)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run gemini in yolo mode: %w", err)
	}

	return nil
}

type threadsAgent struct {
}

type geminiSessionInfo struct {
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`

	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`

	Messages []geminiSessionMessage `json:"messages"`
}

type geminiSessionMessage struct {
	ID        string              `json:"id"`
	Timestamp time.Time           `json:"timestamp"`
	Type      string              `json:"type"`
	Content   json.RawMessage     `json:"content"`
	Thoughts  []json.RawMessage   `json:"thoughts"`
	Tokens    geminiSessionTokens `json:"tokens"`
	Model     string              `json:"model"`
	ToolCalls []geminiToolCall    `json:"toolCalls"`
}

func (m *geminiSessionMessage) GetContent() string {
	// Content is usually a string, but can sometimes be an array of text/thoughts. In that case, we concatenate all the text parts.
	var out string
	if m.Content != nil {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			out = s
		} else {
			var parts []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				for _, part := range parts {
					out += part.Text
				}
			} else {
				klog.Warningf("failed to unmarshal message content as string or array of parts: %q", string(m.Content))
				// Unexpected format, return the raw content as a string
				return string(m.Content)
			}
		}
	}

	return out
}

type geminiSessionTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

type geminiToolCall struct {
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	Args                   map[string]any  `json:"args"`
	Result                 json.RawMessage `json:"result"`
	Status                 string          `json:"status"`
	Timestamp              string          `json:"timestamp"`
	ResultDisplay          json.RawMessage `json:"resultDisplay"`
	DisplayName            string          `json:"displayName"`
	Description            string          `json:"description"`
	RenderOutputAsMarkdown bool            `json:"renderOutputAsMarkdown"`
}

func (a *threadsAgent) listThreads(ctx context.Context, opt ThreadsAgentOptions) ([]ThreadInfo, error) {
	log := klog.FromContext(ctx)

	var out []ThreadInfo

	// gemini --list-sessions only does one directory (the cwd), and doesn't output structured information.

	// TODO: Fix identity in dev-sandbox (it will likely trigger some alarms if we keep using root)
	geminiDir := "/root/.gemini/tmp"

	if _, err := os.Stat(geminiDir); os.IsNotExist(err) {
		log.Info("No gemini sessions found (gemini tmp dir does not exist)", "dir", geminiDir)
		return out, nil
	}

	projectRoots := make(map[string][]string)

	// Look for .project_root files, which contain the absolute path to the workspace directory.
	// We use this to infer the workspace for each thread, which is not actually stored in the gemini session files.
	if err := filepath.WalkDir(geminiDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".project_root" {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("failed to read .project_root file %q: %w", path, err)
			}
			workspace := string(bytes.TrimSpace(b))
			projectDir := filepath.Dir(path)
			log.Info("Found .project_root file, inferred workspace", "projectDir", projectDir, "workspace", workspace)
			projectRoots[workspace] = append(projectRoots[workspace], projectDir)
			return nil
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to walk gemini dir %q: %w", geminiDir, err)
	}

	parseGeminiSessionFile := func(projectRoot string, path string) error {
		if filepath.Base(path) == "logs.json" {
			// ignore
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read gemini session file %q: %w", path, err)
		}
		var session geminiSessionInfo
		if err := json.Unmarshal(b, &session); err != nil {
			return fmt.Errorf("failed to unmarshal gemini session file %q: %w", path, err)
		}

		if opt.ThreadID != "" && session.SessionID != opt.ThreadID {
			return nil
		}

		thread := ThreadInfo{
			SessionID:   session.SessionID,
			ProjectHash: session.ProjectHash,
		}
		if t, err := time.Parse(time.RFC3339, session.StartTime); err != nil {
			log.Error(err, "failed to parse start time", "startTime", session.StartTime, "path", path)
		} else {
			thread.StartTime = t
		}

		thread.ProjectRoot = projectRoot

		// Compute token statistics
		for _, msg := range session.Messages {
			thread.TotalTokens += msg.Tokens.Total
		}

		if opt.IncludeMessages {
			for _, msg := range session.Messages {
				msgOut := ThreadMessage{
					ID:        msg.ID,
					Timestamp: msg.Timestamp,
					Type:      msg.Type,
					Content:   msg.GetContent(),
					Model:     msg.Model,
				}
				for _, toolCall := range msg.ToolCalls {
					toolCallOut := ToolCall{
						ID:        toolCall.ID,
						Name:      toolCall.Name,
						Arguments: toolCall.Args,
					}
					msgOut.ToolCalls = append(msgOut.ToolCalls, toolCallOut)
				}

				thread.Messages = append(thread.Messages, msgOut)
			}
		}

		out = append(out, thread)
		return nil
	}

	parseJsonlGeminiSessionFile := func(projectRoot string, path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read gemini session file %q: %w", path, err)
		}

		var thread ThreadInfo
		for i, line := range bytes.Split(b, []byte("\n")) {
			line = bytes.TrimSpace(line)

			if i == 0 {
				var session geminiSessionInfo

				if err := json.Unmarshal(line, &session); err != nil {
					return fmt.Errorf("failed to unmarshal gemini session line %q %q: %w", path, line, err)
				}
				if opt.ThreadID != "" && session.SessionID != opt.ThreadID {
					return nil
				}

				thread = ThreadInfo{
					SessionID:   session.SessionID,
					ProjectHash: session.ProjectHash,
				}

				if t, err := time.Parse(time.RFC3339, session.StartTime); err != nil {
					log.Error(err, "failed to parse start time", "startTime", session.StartTime, "path", path)
				} else {
					thread.StartTime = t
				}

				thread.ProjectRoot = projectRoot

				continue
			}

			if len(line) == 0 {
				continue
			}
			if !opt.IncludeMessages {
				continue
			}

			var msg geminiSessionMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				return fmt.Errorf("failed to unmarshal gemini message line %q %q: %w", path, line, err)
			}

			msgOut := ThreadMessage{
				ID:        msg.ID,
				Timestamp: msg.Timestamp,
				Type:      msg.Type,
				Content:   msg.GetContent(),
				Model:     msg.Model,
			}
			for _, toolCall := range msg.ToolCalls {
				toolCallOut := ToolCall{
					ID:        toolCall.ID,
					Name:      toolCall.Name,
					Arguments: toolCall.Args,
				}
				msgOut.ToolCalls = append(msgOut.ToolCalls, toolCallOut)
			}

			// TODO: Thoughts
			if msgOut.Content == "" && len(msg.ToolCalls) == 0 {
				log.Info("could not extract content or tool calls from message", "message", string(line))
			}

			thread.Messages = append(thread.Messages, msgOut)
			thread.TotalTokens += msg.Tokens.Total
		}

		out = append(out, thread)

		return nil
	}

	for projectRoot, projectDirs := range projectRoots {
		log.Info("Processing project root", "projectRoot", projectRoot, "numSessions", len(projectDirs))
		for _, projectDir := range projectDirs {
			if err := filepath.WalkDir(projectDir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				name := d.Name()
				if filepath.Ext(name) == ".json" {
					if err := parseGeminiSessionFile(projectRoot, path); err != nil {
						log.Error(err, "failed to parse gemini session file", "path", path)
					}
				}
				if filepath.Ext(name) == ".jsonl" {
					if err := parseJsonlGeminiSessionFile(projectRoot, path); err != nil {
						log.Error(err, "failed to parse gemini session file", "path", path)
					}
				}
				return nil
			}); err != nil {
				return nil, fmt.Errorf("failed to walk project dir %q: %w", projectDir, err)
			}
		}
	}

	return out, nil
}

// // inferWorkspace tries to infer the workspace directory from the gemini session file path.
// // Annoyingly, this is not actually directly stored in the tmp directory, so we have to see if we can guess the workspace and generate a matching hash.
// func inferWorkspace(geminiSessionFilePath string) (string, error) {
// 	// gemini session files are stored in /root/.gemini/tmp/<workspace-hash>/chats/session-<session-id>.json
// 	chatsDir := filepath.Dir(geminiSessionFilePath)
// 	if filepath.Base(chatsDir) != "chats" {
// 		return "", fmt.Errorf("unexpected gemini session file path: %q", geminiSessionFilePath)
// 	}
// 	workspaceDir := filepath.Dir(chatsDir)
// 	workspaceHash := filepath.Base(workspaceDir)

// 	// Check all the directories under /workspaces, which is where dev-sandbox workspaces are stored
// 	workspacesDir := "/workspaces"
// 	entries, err := os.ReadDir(workspacesDir)
// 	if err != nil {
// 		return "", fmt.Errorf("failed to read workspaces dir %q: %w", workspacesDir, err)
// 	}
// 	for _, entry := range entries {
// 		if !entry.IsDir() {
// 			continue
// 		}
// 		p := filepath.Join(workspacesDir, entry.Name())

// 		// Check if the hash of this directory matches the workspace hash
// 		hash := computeGeminiWorkspaceHash(p)
// 		if hash == workspaceHash {
// 			return p, nil
// 		}
// 	}

// 	return "", fmt.Errorf("workspace for %q not found", geminiSessionFilePath)
// }

// func computeGeminiWorkspaceHash(dir string) string {
// 	hasher := sha256.New()
// 	hasher.Write([]byte(dir))
// 	hash := hasher.Sum(nil)
// 	return hex.EncodeToString(hash)
// }
