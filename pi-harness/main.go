package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	a2a "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/harness/runtime"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/kagent-dev/kagent/go/pkg/telemetry"
	"github.com/kagent-dev/kagent/go/pkg/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// One Actor owns one native Pi conversation. The shared upstream executor
// supplies task admission, cancellation, A2A artifacts and durable TaskStore
// integration; Pi owns its JSONL history in the Actor's /data directory.
type piProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr lockedOutput
	nextID uint64
}

type lockedOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *lockedOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(data)
}

func (o *lockedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func (o *lockedOutput) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.text.Reset()
}

func validateSessionFile(path string) error {
	dir := filepath.Clean(env("PI_SESSION_DIR", "/data/pi-sessions"))
	if !filepath.IsAbs(path) || filepath.Dir(path) != dir || filepath.Ext(path) != ".jsonl" {
		return fmt.Errorf("Pi continuation must be a JSONL file in %s", dir)
	}
	return nil
}

func (p *piProcess) start(ctx context.Context, sessionFile string) error {
	dir := env("PI_SESSION_DIR", "/data/pi-sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create Pi session directory: %w", err)
	}
	args := []string{"--no-skills", "--offline",
		"--no-extensions", "--extension", "/root/.pi/agent/npm/node_modules/pi-otel/dist/index.js",
		"--provider", env("PI_PROVIDER", "agentgateway"),
		"--model", env("PI_MODEL", "qwen3.8-27b-nvfp4"),
		"--api-key", env("PI_API_KEY", "sglang-local"),
		"--thinking", "off", "--session-dir", dir, "--mode", "rpc"}
	if sessionFile != "" {
		if err := validateSessionFile(sessionFile); err != nil {
			return err
		}
		if _, err := os.Stat(sessionFile); err != nil {
			return fmt.Errorf("read durable Pi session: %w", err)
		}
		args = append(args, "--session", sessionFile)
	}
	cmd := exec.Command("pi", args...)
	// Pi starts once per turn, so these headers cannot leak between invocations.
	// The extension extracts this parent when creating its interaction span.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	cmd.Env = append(os.Environ(), "TRACEPARENT="+carrier.Get("traceparent"), "TRACESTATE="+carrier.Get("tracestate"))
	p.stderr.Reset()
	cmd.Stderr = &p.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open Pi stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open Pi stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start Pi RPC: %w", err)
	}
	p.cmd, p.stdin, p.stdout = cmd, stdin, bufio.NewReader(stdout)
	return nil
}

// session_shutdown flushes pi-otel before the TaskStore reports quiescence and
// Substrate freezes the Actor. This also reaps the native process on cancellation.
func (p *piProcess) shutdown() {
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
	reader := p.stdout
	ch := make(chan result, 1)
	go func() { line, err := reader.ReadBytes('\n'); ch <- result{line, err} }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		return result.line, result.err
	}
}

func (p *piProcess) send(command string, fields map[string]any) (string, error) {
	p.nextID++
	id := fmt.Sprintf("a2a-%d", p.nextID)
	fields["id"], fields["type"] = id, command
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	if _, err := p.stdin.Write(append(encoded, '\n')); err != nil {
		return "", fmt.Errorf("write Pi RPC %s: %w", command, err)
	}
	return id, nil
}

type rpcEvent struct {
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Success       bool           `json:"success"`
	Error         string         `json:"error"`
	ToolCallID    string         `json:"toolCallId"`
	ToolName      string         `json:"toolName"`
	Args          map[string]any `json:"args"`
	Result        any            `json:"result"`
	IsError       bool           `json:"isError"`
	PartialResult any            `json:"partialResult"`
	Data          struct {
		SessionFile string `json:"sessionFile"`
	} `json:"data"`
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	Messages []struct {
		Role       string          `json:"role"`
		StopReason string          `json:"stopReason"`
		Content    json.RawMessage `json:"content"`
	} `json:"messages"`
}

func assistantText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	for _, part := range parts {
		if part.Type == "text" {
			text += part.Text
		}
	}
	return text
}

func (p *piProcess) readEvent(ctx context.Context) (rpcEvent, error) {
	line, err := p.readLine(ctx)
	if err != nil {
		return rpcEvent{}, err
	}
	var event rpcEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return rpcEvent{}, fmt.Errorf("decode Pi RPC event: %w", err)
	}
	return event, nil
}

func (p *piProcess) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (outcome runtime.Outcome, runErr error) {
	if turn.InputResponse != nil {
		return runtime.Outcome{}, fmt.Errorf("Pi has no parked native input request")
	}
	piCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// A local deadline is a failed turn, not the supervisor's cancellation
	// path (which deliberately publishes its terminal event from Cancel).
	defer func() {
		if ctx.Err() == nil && errors.Is(runErr, context.DeadlineExceeded) {
			outcome = runtime.Outcome{Failure: &runtime.Failure{Message: "Pi turn timed out"}}
			runErr = nil
		}
	}()
	if err := p.start(piCtx, turn.ContinuationID); err != nil {
		return runtime.Outcome{}, err
	}
	defer p.shutdown()
	stateID, err := p.send("get_state", map[string]any{})
	if err != nil {
		return runtime.Outcome{}, err
	}
	var sessionFile string
	for sessionFile == "" {
		event, err := p.readEvent(piCtx)
		if err != nil {
			return runtime.Outcome{}, err
		}
		if event.Type != "response" || event.ID != stateID {
			continue
		}
		if !event.Success || event.Data.SessionFile == "" {
			return runtime.Outcome{}, fmt.Errorf("Pi did not return a persistent session")
		}
		sessionFile = event.Data.SessionFile
		if err := validateSessionFile(sessionFile); err != nil {
			return runtime.Outcome{}, err
		}
		if turn.ContinuationID != "" && sessionFile != turn.ContinuationID {
			return runtime.Outcome{}, fmt.Errorf("Pi resumed a different native session")
		}
	}
	bound := turn.ContinuationID != ""
	bind := func() error {
		if bound {
			return nil
		}
		// Pi creates the file on its first assistant response, not at startup.
		if _, err := os.Stat(sessionFile); err != nil {
			return fmt.Errorf("Pi did not persist its native session: %w", err)
		}
		if err := sink.SessionStarted(runtime.SessionStarted{ContinuationID: sessionFile}); err != nil {
			return err
		}
		bound = true
		return nil
	}
	requestID, err := p.send("prompt", map[string]any{"message": turn.Prompt})
	if err != nil {
		return runtime.Outcome{}, err
	}
	accepted, streamed := false, false
	answer, failed := "", false
	for {
		event, err := p.readEvent(piCtx)
		if err != nil {
			return runtime.Outcome{}, fmt.Errorf("Pi RPC failed: %w: %s", err, tail(p.stderr.String(), 800))
		}
		switch event.Type {
		case "response":
			if event.ID == requestID {
				if !event.Success {
					return runtime.Outcome{}, fmt.Errorf("Pi rejected prompt: %s", event.Error)
				}
				accepted = true
			}
		case "message_update":
			if event.AssistantMessageEvent.Type == "text_delta" {
				if err := sink.TextDelta(runtime.TextDelta{Text: event.AssistantMessageEvent.Delta}); err != nil {
					return runtime.Outcome{}, err
				}
				streamed = true
			}
		case "tool_execution_start":
			if err := sink.ToolCall(runtime.ToolCall{ID: event.ToolCallID, Name: event.ToolName, Arguments: event.Args}); err != nil {
				return runtime.Outcome{}, err
			}
		case "tool_execution_update", "tool_execution_end":
			result := event.Result
			if event.Type == "tool_execution_update" {
				result = event.PartialResult
			}
			if err := sink.ToolResult(runtime.ToolResult{ID: event.ToolCallID, Name: event.ToolName, Result: result, IsError: event.IsError}); err != nil {
				return runtime.Outcome{}, err
			}
		case "agent_end":
			answer, failed = "", false
			for i := len(event.Messages) - 1; i >= 0; i-- {
				message := event.Messages[i]
				if message.Role == "assistant" {
					// agent_end follows message persistence; message_end is
					// emitted before Pi writes the JSONL file.
					if err := bind(); err != nil {
						return runtime.Outcome{}, err
					}
					failed = message.StopReason == "error" || message.StopReason == "aborted"
					answer = assistantText(message.Content)
					break
				}
			}
		case "agent_settled":
			if !accepted {
				return runtime.Outcome{}, fmt.Errorf("Pi settled before accepting prompt")
			}
			if failed || answer == "" {
				return runtime.Outcome{Failure: &runtime.Failure{Message: "Pi returned no successful assistant text"}}, nil
			}
			if err := bind(); err != nil {
				return runtime.Outcome{}, err
			}
			if !streamed {
				if err := sink.TextDelta(runtime.TextDelta{Text: answer}); err != nil {
					return runtime.Outcome{}, err
				}
			}
			return runtime.Outcome{}, nil
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

func run(ctx context.Context) error {
	var runtimeConfig struct {
		Runtime struct {
			ControlPlaneURL string `json:"control_plane_url"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(os.Getenv("KAGENT_CONFIG_JSON")), &runtimeConfig); err != nil {
		return fmt.Errorf("decode controller runtime config: %w", err)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal([]byte(os.Getenv("KAGENT_AGENT_CARD_JSON")), &card); err != nil {
		return fmt.Errorf("decode controller agent card: %w", err)
	}
	if card.Name == "" {
		return fmt.Errorf("controller agent card name is required")
	}
	// BYO compilers do not assign one of the native RuntimeTelemetry enums.
	// Preserve their SDK environment identity instead of impersonating ADK.
	runtimeTelemetry := tracing.RuntimeTelemetry{}
	providers, err := telemetry.Init(ctx, telemetry.Options{Defaults: runtimeTelemetry.ResourceDefaults(card.Name)})
	if err != nil {
		logging.FromContext(ctx).ErrorContext(ctx, "initialize Pi telemetry", "error", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := providers.Shutdown(shutdownCtx); err != nil {
			logging.FromContext(ctx).ErrorContext(ctx, "shutdown Pi telemetry", "error", err)
		}
	}()
	store, err := continuation.New("/data/adapter", "pi", validateSessionFile)
	if err != nil {
		return err
	}
	executor, err := runtimea2a.New(&piProcess{}, store, runtimeTelemetry)
	if err != nil {
		return err
	}
	application, err := app.New(app.AppConfig{
		AgentCard: card, Port: env("KAGENT_PORT", "80"), AppName: card.Name, ControlPlaneURL: runtimeConfig.Runtime.ControlPlaneURL,
		Logger: logging.FromContext(ctx), Telemetry: runtimeTelemetry, Flush: providers.ForceFlush,
	}, executor)
	if err != nil {
		return err
	}
	return application.Run()
}

func main() {
	logger, err := logging.New(os.Stderr, env("KAGENT_LOG_LEVEL", "info"))
	if err != nil {
		panic(err)
	}
	ctx := logging.IntoContext(context.Background(), logger)
	if err := run(ctx); err != nil {
		logger.ErrorContext(ctx, "Pi harness stopped", "error", err)
		os.Exit(1)
	}
}
