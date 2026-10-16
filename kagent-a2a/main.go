// kagent-a2a: tool A2A sobre o kgateway (a2a-kagent.localhost) para
// invocar agentes do kagent sem port-forward.
//
// Nova API do kagent (controller >= main-bf8afa56): AgentInstanceService
// virou SessionService e o A2A e o JSON-RPC por agente em
// POST /agents/{namespace}/{name} (a2a-go v2, metodos PascalCase).
//
// Fluxo (conversa nova por invocacao, nunca reutiliza instancia):
//  1. A2A SendMessage  (JSON-RPC "SendMessage" em /agents/{ns}/{agent};
//                       a gateway cria a sessao automaticamente, dedup por
//                       message id)
//  2. SessionService/DeleteSession (gRPC-Web, evita sessao zumbi)
//
// Uso:
//   kagent-a2a [agent] [harness] "task..."
//   kagent-a2a loki-specialist adk "ping"
//   kagent-a2a --keep pi-default pi "que horas sao?"   (nao descarta a sessao;
//                                   reporta o contextId para retomar com --context)
//
// Obs.: o argumento "harness" e aceito por compatibilidade com a CLI antiga,
// mas ignorado: no novo modelo o Agent (template+harness) e um unico
// recurso, e o harness ja vem embutido na definicao do Agent.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	flagKeep      bool
	flagHost      = "http://127.0.0.1"
	flagHostHdr   = "a2a-kagent.localhost"
	flagUser      = "admin@kagent.dev"
	flagContext   = ""
	flagQuiet     bool
)

// --- DTOs A2A JSON-RPC (a2a-go v2: metodo "SendMessage", role ROLE_USER) ---

type jsonrpcReq struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Params  sendParams  `json:"params"`
}

type sendParams struct {
	Message a2aMsg `json:"message"`
}

type a2aMsg struct {
	Role      string   `json:"role"`
	MessageID string   `json:"messageId"`
	ContextID string   `json:"contextId,omitempty"`
	Parts     []a2aPt `json:"parts"`
}

type a2aPt struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type jsonrpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonrpcErr     `json:"error"`
}

type jsonrpcErr struct {
	Code int    `json:"code"`
	Msg  string `json:"message"`
}

type a2aResult struct {
	Task    *a2aTask    `json:"task"`
	Message *a2aResultM `json:"message"`
}

type a2aTask struct {
	ID        string    `json:"id"`
	ContextID string    `json:"contextId"`
	Status    a2aStatus `json:"status"`
	Artifacts []a2aArt  `json:"artifacts"`
}

type a2aStatus struct {
	State string `json:"state"`
}

type a2aArt struct {
	Parts []a2aPt `json:"parts"`
}

type a2aResultM struct {
	ContextID string  `json:"contextId"`
	Parts     []a2aPt `json:"parts"`
}

func main() {
	args := os.Args[1:]
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--keep":
			flagKeep = true
		case args[i] == "--quiet":
			flagQuiet = true
		case args[i] == "--host":
			i++
			if i < len(args) {
				flagHost = args[i]
			}
		case args[i] == "--host-header":
			i++
			if i < len(args) {
				flagHostHdr = args[i]
			}
		case args[i] == "--user":
			i++
			if i < len(args) {
				flagUser = args[i]
			}
		case args[i] == "--context":
			i++
			if i < len(args) {
				flagContext = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}

	agent := "loki-specialist"
	task := "ping"
	if len(positional) >= 1 {
		agent = positional[0]
	}
	// positional[1] era o harness; mantido por compatibilidade, agora ignorado
	if len(positional) >= 3 {
		task = strings.Join(positional[2:], " ")
	} else if len(positional) == 2 && positional[1] != "adk" {
		// ambiguidade resolvida: sem harness explicito o 2o argumento e a task
		task = positional[1]
	}

	// agente pode vir como "ns/name"; padrao do namespace e kagent
	namespace, name := "kagent", agent
	if i := strings.IndexByte(agent, '/'); i > 0 {
		namespace, name = agent[:i], agent[i+1:]
	}

	client := &http.Client{Timeout: 30 * time.Minute}

	// 1) A2A SendMessage (JSON-RPC); sessao nova por invocacao
	resp, raw, err := callSendMessage(client, namespace, name, flagContext, task)
	if err != nil {
		fatal("A2A SendMessage: %v", err)
	}
	contextID := ""
	if raw != nil {
		printA2AResponse(raw)
		contextID = raw.ContextID()
	}

	// 2) descarta a sessao (nao virar zumbi) ou reporta o contextId
	if contextID == "" {
		contextID = flagContext
	}
	if !flagKeep {
		if contextID != "" {
			if err := deleteSession(client, contextID); err != nil {
				fmt.Printf("\naviso: DeleteSession: %v (sessao %s pode virar zumbi)\n", err, contextID)
			} else if !flagQuiet {
				fmt.Printf("\nsessao descartada (%s)\n", contextID)
			}
		}
	} else {
		if !flagQuiet {
			fmt.Printf("\n--keep: sessao %s mantida (retome com --context %s)\n", contextID, contextID)
		}
	}
	_ = resp
}

// --- A2A JSON-RPC (transporte HTTP) ---

func callSendMessage(client *http.Client, namespace, agent, contextID, task string) (*a2aResult, *a2aResultFull, error) {
	reqBody := jsonrpcReq{
		JSONRPC: "2.0",
		ID:      uuid.NewString(),
		Method:  "SendMessage",
		Params: sendParams{Message: a2aMsg{
			Role:      "ROLE_USER",
			MessageID: uuid.NewString(),
			ContextID: contextID,
			Parts:     []a2aPt{{Kind: "text", Text: task}},
		}},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/agents/%s/%s", flagHost, namespace, agent), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if flagHostHdr != "" {
		httpReq.Host = flagHostHdr
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-user-id", flagUser)

	if os.Getenv("KAGENT_A2A_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] POST /agents/%s/%s\n", namespace, agent)
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, nil, err
	}

	if os.Getenv("KAGENT_A2A_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] %s (%d bytes)\n", httpResp.Status, len(raw))
	}

	if httpResp.StatusCode >= 500 {
		preview, _ := io.ReadAll(io.LimitReader(bytes.NewReader(raw), 512))
		return nil, nil, fmt.Errorf("HTTP %s: %s", httpResp.Status, strings.TrimSpace(string(preview)))
	}

	var parsed jsonrpcResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// resposta nao-JSON: mostra como text
		parsed = jsonrpcResp{}
		if !flagQuiet {
			fmt.Printf("%s\n", strings.TrimSpace(string(raw)))
		}
		return nil, nil, nil
	}
	if parsed.Error != nil {
		return nil, nil, fmt.Errorf("JSON-RPC %d: %s", parsed.Error.Code, parsed.Error.Msg)
	}
	var result a2aResult
	if len(parsed.Result) == 0 {
		return &result, nil, nil
	}
	if err := json.Unmarshal(parsed.Result, &result); err != nil {
		return nil, nil, fmt.Errorf("unmarshal result: %w", err)
	}
	full := &a2aResultFull{task: result.Task, message: result.Message}
	return &result, full, nil
}

// a2aResultFull carrega o contextId para limpeza, independente do shape
type a2aResultFull struct {
	task    *a2aTask
	message *a2aResultM
}

func (r *a2aResultFull) ContextID() string {
	switch {
	case r.task != nil:
		return r.task.ContextID
	case r.message != nil:
		return r.message.ContextID
	default:
		return ""
	}
}

func printA2AResponse(resp *a2aResultFull) {
	switch {
	case resp.task != nil:
		for _, art := range resp.task.Artifacts {
			for _, part := range art.Parts {
				if part.Text != "" {
					fmt.Println(part.Text)
				}
			}
		}
		if st := resp.task.Status.State; st != "" {
			fmt.Printf("\n[task %s: %s]\n", resp.task.ID, st)
		}
	case resp.message != nil:
		for _, part := range resp.message.Parts {
			if part.Text != "" {
				fmt.Println(part.Text)
			}
		}
	default:
		fmt.Println("(resposta vazia)")
	}
}

// --- gRPC-Web: SessionService/DeleteSession (protobuf manual, field 1) ---

func deleteSession(client *http.Client, sessionID string) error {
	inner := append([]byte{0x0a, byte(len(sessionID))}, sessionID...)
	var buf bytes.Buffer
	buf.WriteByte(0) // flag: uncompressed
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(inner)))
	buf.Write(lenBuf[:])
	buf.Write(inner)

	httpReq, err := http.NewRequest(http.MethodPost,
		flagHost+"/kagent.api.v1alpha1.SessionService/DeleteSession", &buf)
	if err != nil {
		return err
	}
	if flagHostHdr != "" {
		httpReq.Host = flagHostHdr
	}
	httpReq.Header.Set("x-grpc-web", "1")
	httpReq.Header.Set("Content-Type", "application/grpc-web+proto")
	httpReq.Header.Set("Accept", "application/grpc-web+proto")
	httpReq.Header.Set("x-user-id", flagUser)

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)

	if os.Getenv("KAGENT_A2A_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] DeleteSession -> HTTP %s (%d bytes)\n", httpResp.Status, len(raw))
	}

	// trailing frame gRPC-Web: flag 0x80 + trailers
	for len(raw) > 0 {
		if len(raw) < 5 {
			break
		}
		flag, length := raw[0], int(binary.BigEndian.Uint32(raw[1:5]))
		if 5+length > len(raw) {
			break
		}
		payload := raw[5 : 5+length]
		raw = raw[5+length:]
		if flag&0x80 != 0 { // frame de trailers: chave:valor separados por \n
			for _, kv := range strings.Split(string(payload), "\n") {
				k, v, found := strings.Cut(kv, ":")
				if !found {
					continue
				}
				switch k {
				case "grpc-status":
					var code int
					fmt.Sscanf(v, "%d", &code)
					if code != 0 {
						return fmt.Errorf("grpc-status %d", code)
					}
				}
			}
		}
	}
	return nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "erro: "+format+"\n", args...)
	os.Exit(1)
}
