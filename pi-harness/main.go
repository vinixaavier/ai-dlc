package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

type executor struct {
	pi *piProcess
}

func (e *executor) Execute(ctx context.Context, request *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(request, request.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateWorking, nil), nil) {
			return
		}
		var artifactID a2a.ArtifactID
		streamed := false
		answer, err := e.pi.Run(ctx, request.Message, func(delta string) error {
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
			streamed = true
			if !yield(event, nil) {
				return errStreamStopped
			}
			return nil
		}, func(activity piToolActivity) error {
			// Tool activity is a first-class A2A artifact. This keeps Pi's
			// RPC tool events visible to the kagent UI instead of silently
			// dropping them in the custom harness.
			artifactID = ""
			data := map[string]any{
				"id":   activity.ID,
				"name": activity.Name,
			}
			partType := "function_call"
			if activity.Kind == "result" {
				partType = "function_response"
				response := map[string]any{"result": activity.Result}
				if activity.IsError {
					response["isError"] = true
				}
				data["response"] = response
			} else if activity.Kind == "update" {
				partType = "function_response"
				data["response"] = map[string]any{"result": activity.Result}
			} else {
				data["args"] = activity.Args
			}
			part := a2a.NewDataPart(data)
			part.Metadata = map[string]any{"kagent_type": partType}
			update := a2a.NewArtifactEvent(request, part)
			update.LastChunk = true
			if !yield(update, nil) {
				return errStreamStopped
			}
			return nil
		})
		if err != nil {
			if err == errStreamStopped {
				return
			}
			message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(err.Error()))
			message.ContextID, message.TaskID = request.ContextID, request.TaskID
			yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateFailed, message), nil)
			return
		}
		if streamed {
			yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateCompleted, nil), nil)
			return
		}
		message := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(answer))
		message.ContextID, message.TaskID = request.ContextID, request.TaskID
		yield(a2a.NewStatusUpdateEvent(request, a2a.TaskStateCompleted, message), nil)
	}
}

var errStreamStopped = fmt.Errorf("A2A stream stopped")

func (executor) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

type piProcess struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr strings.Builder
	nextID uint64
}

type piToolActivity struct {
	Kind    string
	ID      string
	Name    string
	Args    map[string]any
	Result  any
	IsError bool
}

func (p *piProcess) startLocked() error {
	if p.cmd != nil && p.cmd.ProcessState == nil {
		return nil
	}
	if p.cmd != nil {
		_ = p.cmd.Wait()
	}
	if err := os.MkdirAll(env("PI_SESSION_DIR", "/data/pi-sessions"), 0o700); err != nil {
		return fmt.Errorf("create Pi session directory: %w", err)
	}
	cmd := exec.Command("pi", "--no-skills", "--offline",
		"--no-extensions", "--extension", "/root/.pi/agent/npm/node_modules/pi-otel/dist/index.js",
		"--provider", env("PI_PROVIDER", "agentgateway"),
		"--model", env("PI_MODEL", "qwen3.8-27b-nvfp4"),
		"--api-key", env("PI_API_KEY", "sglang-local"),
		"--thinking", "off",
		"--session-dir", env("PI_SESSION_DIR", "/data/pi-sessions"),
		"--mode", "rpc")
	cmd.Stderr = &p.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Pi stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Pi stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Pi RPC: %w", err)
	}
	p.cmd, p.stdin, p.stdout = cmd, stdin, bufio.NewReader(stdout)
	p.stderr.Reset()
	return nil
}

func (p *piProcess) stopLocked() {
	if p.cmd == nil {
		return
	}
	_ = p.stdin.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd, p.stdin, p.stdout = nil, nil, nil
}

// Pi's pi-otel extension flushes its BatchSpanProcessor during
// session_shutdown. ATE suspends the actor immediately after the A2A turn,
// so terminate Pi gracefully at the turn boundary instead of freezing the
// process before that lifecycle event runs.
func (p *piProcess) shutdownLocked() {
	if p.cmd == nil {
		return
	}
	_ = p.stdin.Close()
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	p.cmd, p.stdin, p.stdout = nil, nil, nil
}

func (p *piProcess) readLine(ctx context.Context) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() { line, err := p.stdout.ReadBytes('\n'); ch <- result{line, err} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		return result.line, result.err
	}
}

func (p *piProcess) Run(ctx context.Context, message *a2a.Message, onDelta func(string) error, onTool func(piToolActivity) error) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.startLocked(); err != nil {
		return "", err
	}
	var prompt strings.Builder
	for _, part := range message.Parts {
		if part.Text() != "" {
			prompt.WriteString(part.Text())
		}
	}
	if prompt.Len() == 0 {
		return "", fmt.Errorf("text message required")
	}
	piCtx, cancel := context.WithTimeout(ctx, 90e9)
	defer cancel()
	p.nextID++
	requestID := fmt.Sprintf("a2a-%d", p.nextID)
	request := map[string]any{"id": requestID, "type": "prompt", "message": prompt.String()}
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	if _, err := p.stdin.Write(append(encoded, '\n')); err != nil {
		p.stopLocked()
		return "", fmt.Errorf("write Pi RPC prompt: %w", err)
	}
	accepted := false
	var answer string
	for {
		line, err := p.readLine(piCtx)
		if err != nil {
			p.stopLocked()
			return "", fmt.Errorf("Pi RPC failed: %w: %s", err, tail(p.stderr.String(), 800))
		}
		var event struct {
			Type                  string         `json:"type"`
			ID                    string         `json:"id"`
			Success               *bool          `json:"success"`
			Command               string         `json:"command"`
			Delta                 string         `json:"delta"`
			ToolCallID            string         `json:"toolCallId"`
			ToolName              string         `json:"toolName"`
			Args                  map[string]any `json:"args"`
			Result                any            `json:"result"`
			IsError               bool           `json:"isError"`
			PartialResult         any            `json:"partialResult"`
			AssistantMessageEvent struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.Type == "response" && event.ID == requestID {
			accepted = event.Success != nil && *event.Success
			if !accepted {
				return "", fmt.Errorf("Pi rejected prompt")
			}
			continue
		}
		if event.Type == "message_update" && event.AssistantMessageEvent.Type == "text_delta" {
			if err := onDelta(event.AssistantMessageEvent.Delta); err != nil {
				p.stopLocked()
				return "", err
			}
			continue
		}
		if event.Type == "tool_execution_start" {
			if err := onTool(piToolActivity{Kind: "call", ID: event.ToolCallID, Name: event.ToolName, Args: event.Args}); err != nil {
				p.stopLocked()
				return "", err
			}
			continue
		}
		if event.Type == "tool_execution_update" {
			if err := onTool(piToolActivity{Kind: "update", ID: event.ToolCallID, Name: event.ToolName, Result: event.PartialResult}); err != nil {
				p.stopLocked()
				return "", err
			}
			continue
		}
		if event.Type == "tool_execution_end" {
			if err := onTool(piToolActivity{Kind: "result", ID: event.ToolCallID, Name: event.ToolName, Result: event.Result, IsError: event.IsError}); err != nil {
				p.stopLocked()
				return "", err
			}
			continue
		}
		if event.Type == "bash_execution_update" {
			if err := onTool(piToolActivity{Kind: "update", ID: event.ID, Name: "bash", Result: map[string]any{"output": event.Delta}}); err != nil {
				p.stopLocked()
				return "", err
			}
			continue
		}
		if event.Type == "agent_settled" {
			if answer == "" {
				return "", fmt.Errorf("Pi returned no assistant text")
			}
			p.shutdownLocked()
			return answer, nil
		}
		if event.Type != "agent_end" {
			continue
		}
		if !accepted {
			return "", fmt.Errorf("Pi ended before accepting prompt")
		}
		answer = ""
		for i := len(event.Messages) - 1; i >= 0; i-- {
			if event.Messages[i].Role == "assistant" {
				for _, part := range event.Messages[i].Content {
					if part.Type == "text" {
						answer += part.Text
					}
				}
				break
			}
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func tail(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[len(value)-n:]
}

func main() {
	logger, err := logging.NewFromEnv(os.Stderr)
	if err != nil {
		panic(err)
	}
	application, err := app.New(app.AppConfig{
		AgentCard: a2a.AgentCard{Name: "pi-harness", Version: "v1", Capabilities: a2a.AgentCapabilities{Streaming: true}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}},
		Port:      env("PORT", "80"), AppName: "pi-harness", Logger: logger,
	}, &executor{pi: &piProcess{}})
	if err != nil {
		panic(err)
	}
	if err := application.Run(); err != nil {
		panic(err)
	}
}
