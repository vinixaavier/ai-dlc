package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	runtimeconfig "github.com/kagent-dev/kagent/go/adk/pkg/config"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

type executor struct{ process *openCodeProcess }

func (e *executor) Execute(ctx context.Context, request *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(request, request.Message), nil) ||
			!yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var artifactID a2a.ArtifactID
		var answer strings.Builder
		err := e.process.Run(ctx, request.Message, func(delta string) error {
			if delta == "" {
				return nil
			}
			var event *a2a.TaskArtifactUpdateEvent
			if artifactID == "" {
				event = a2a.NewArtifactEvent(request, a2a.NewTextPart(delta))
				artifactID = event.Artifact.ID
			} else {
				event = a2a.NewArtifactUpdateEvent(request, artifactID, a2a.NewTextPart(delta))
			}
			answer.WriteString(delta)
			if !yield(event, nil) {
				return errStreamStopped
			}
			return nil
		}, func(kind, id, name string, value any, call bool) error {
			data := map[string]any{"id": id, "name": name}
			partType := "function_call"
			if call {
				data["args"] = value
			} else {
				partType = "function_response"
				data["response"] = map[string]any{"result": value}
			}
			part := a2a.NewDataPart(data)
			part.Metadata = map[string]any{"kagent_type": partType}
			update := a2a.NewArtifactEvent(request, part)
			update.LastChunk = true
			artifactID = ""
			if !yield(update, nil) {
				return errStreamStopped
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, errStreamStopped) {
				return
			}
			message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))
			message.ContextID, message.TaskID = request.ContextID, request.TaskID
			yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateFailed, message), nil)
			return
		}
		if answer.Len() == 0 {
			message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("OpenCode returned no assistant text"))
			message.ContextID, message.TaskID = request.ContextID, request.TaskID
			yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateFailed, message), nil)
			return
		}
		yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateCompleted, nil), nil)
	}
}

var errStreamStopped = errors.New("A2A stream stopped")

func (executor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

type openCodeProcess struct {
	mu sync.Mutex
}

type cliEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Delta     string `json:"delta"`
	Error     struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	Part struct {
		Type   string          `json:"type"`
		Text   string          `json:"text"`
		CallID string          `json:"callID"`
		Tool   string          `json:"tool"`
		State  json.RawMessage `json:"state"`
	} `json:"part"`
}

type toolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

func (p *openCodeProcess) Run(ctx context.Context, message *a2a.Message, onDelta func(string) error, onTool func(string, string, string, any, bool) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var prompt strings.Builder
	for _, part := range message.Parts {
		if part.Text() != "" {
			prompt.WriteString(part.Text())
		}
	}
	if prompt.Len() == 0 {
		return fmt.Errorf("text message required")
	}
	workspace := env("OPENCODE_WORKSPACE", "/data/workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create OpenCode workspace: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(sessionPath()), 0o700); err != nil {
		return fmt.Errorf("create OpenCode state directory: %w", err)
	}

	args := []string{"run", "--format", "json", "--print-logs", "--log-level", env("OPENCODE_LOG_LEVEL", "ERROR"), "--auto", "--pure", "--dir", workspace, "--model", env("OPENCODE_MODEL", "agentgateway/qwen3.8-27b-nvfp4")}
	if sessionID, err := os.ReadFile(sessionPath()); err == nil && strings.TrimSpace(string(sessionID)) != "" {
		args = append(args, "--session", strings.TrimSpace(string(sessionID)))
	}
	args = append(args, "--", prompt.String())
	callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, env("OPENCODE_EXECUTABLE", "opencode"), args...)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open OpenCode stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start OpenCode: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event cliEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.SessionID != "" {
			if err := persistSession(event.SessionID); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
		}
		switch event.Type {
		case "error":
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			logs := tail(strings.TrimSpace(stderr.String()), 1000)
			if event.Error.Data.Message != "" {
				if logs != "" {
					return fmt.Errorf("OpenCode %s: %s (logs: %s)", event.Error.Name, event.Error.Data.Message, logs)
				}
				return fmt.Errorf("OpenCode %s: %s", event.Error.Name, event.Error.Data.Message)
			}
			return fmt.Errorf("OpenCode error: %s", string(scanner.Bytes()))
		case "text":
			delta := event.Part.Text
			if delta == "" {
				delta = event.Delta
			}
			if err := onDelta(delta); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
		case "tool_use", "tool":
			state := toolState{}
			_ = json.Unmarshal(event.Part.State, &state)
			if state.Status == "running" || event.Type == "tool_use" {
				var input any
				_ = json.Unmarshal(state.Input, &input)
				if err := onTool("call", event.Part.CallID, event.Part.Tool, input, true); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return err
				}
			}
			if state.Status == "completed" || state.Status == "error" {
				var output any
				_ = json.Unmarshal(state.Output, &output)
				if state.Error != "" {
					output = map[string]any{"error": state.Error}
				}
				if err := onTool("result", event.Part.CallID, event.Part.Tool, output, false); err != nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					return err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("read OpenCode JSON stream: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if callCtx.Err() != nil {
			return fmt.Errorf("OpenCode timed out or was cancelled: %w", callCtx.Err())
		}
		return fmt.Errorf("OpenCode failed: %w: %s", err, tail(stderr.String(), 1000))
	}
	return nil
}

func sessionPath() string { return env("OPENCODE_SESSION_FILE", "/data/opencode/session-id") }

func persistSession(sessionID string) error {
	target := sessionPath()
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create OpenCode session directory: %w", err)
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(sessionID), 0o600); err != nil {
		return fmt.Errorf("write OpenCode session id: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("persist OpenCode session id: %w", err)
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func tail(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[len(value)-max:]
}

func main() {
	configDir := "/data/kagent-config"
	if err := runtimeconfig.MaterializeFromEnv(configDir); err != nil {
		panic(err)
	}
	compiled, err := runtimeconfig.LoadAgentConfig(filepath.Join(configDir, "config.json"))
	if err != nil {
		panic(err)
	}
	runtime := runtimeconfig.ProcessConfig(compiled)
	if runtime.ControlPlaneUrl == "" {
		panic("runtime.control_plane_url is required")
	}
	agentName := runtime.Name
	if agentName == "" {
		agentName = "opencode"
	}
	logger, err := logging.New(os.Stderr, "info")
	if err != nil {
		panic(err)
	}
	application, err := app.New(app.AppConfig{
		AgentCard:       a2a.AgentCard{Name: "opencode-harness", Version: env("OPENCODE_VERSION", "1.19.2"), Capabilities: a2a.AgentCapabilities{Streaming: true}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}},
		ControlPlaneURL: runtime.ControlPlaneUrl,
		Port:            env("PORT", "80"), AppName: agentName, Logger: logger,
	}, &executor{process: &openCodeProcess{}})
	if err != nil {
		panic(err)
	}
	if err := application.Run(); err != nil {
		panic(err)
	}
}
