// kagent-a2a: tool A2A sobre gRPC-Web, exposto no kgateway como
// a2a-kagent.localhost. Sem port-forward: todo o trafego passa pelo
// HTTPRoute a2a-kagent (kgateway -> kagent-controller:8083).
//
// Fluxo (conversa nova por invocacao, nunca reutiliza instancia):
//  1. CreateAgentInstance  (gRPC-Web POST /kagent.api.v1alpha1.AgentInstanceService/CreateAgentInstance)
//  2. A2A SendMessage      (gRPC-Web POST /lf.a2a.v1.A2AService/SendMessage)
//  3. DeleteAgentInstance  (gRPC-Web POST /kagent.api.v1alpha1.AgentInstanceService/DeleteAgentInstance)
//
// Uso:
//   kagent-a2a [template] [harness] "task..."
//   kagent-a2a loki-specialist adk "ping"
//   kagent-a2a --keep pi-default pi "que horas sao?"   (nao descarta a instancia)
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

var (
	flagKeep    bool
	flagHost    = "http://127.0.0.1"
	flagHostHdr = "a2a-kagent.localhost"
	flagUser    = "admin@kagent.dev"
	flagQuiet   bool
)

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
		default:
			positional = append(positional, args[i])
		}
	}

	template := "loki-specialist"
	harness := "adk"
	task := "ping"
	if len(positional) >= 1 {
		template = positional[0]
	}
	if len(positional) >= 2 {
		harness = positional[1]
	}
	if len(positional) >= 3 {
		task = strings.Join(positional[2:], " ")
	}

	client := &http.Client{Timeout: 30 * time.Minute}

	// 1) cria conversa nova
	created, err := callUnary(client, flagHost,
		"/kagent.api.v1alpha1.AgentInstanceService/CreateAgentInstance",
		&apiv1alpha1.CreateAgentInstanceRequest{
			AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: template},
			Harness:       &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: harness},
			RequestId:     uuid.NewString(),
		},
		&apiv1alpha1.CreateAgentInstanceResponse{},
		headerUser(flagUser))
	if err != nil {
		fatal("CreateAgentInstance: %v", err)
	}
	createdResp := created.(*apiv1alpha1.CreateAgentInstanceResponse)
	inst := createdResp.GetAgentInstance()
	if inst == nil {
		fatal("CreateAgentInstance nao retornou instancia")
	}
	instanceID := inst.GetId()
	if !flagQuiet {
		fmt.Printf("instancia: %s (%s via %s)\n", instanceID, template, harness)
	}

	// 2) A2A SendMessage roteado por instancia
	sendResp, err := callUnary(client, flagHost,
		"/lf.a2a.v1.A2AService/SendMessage",
		&a2apb.SendMessageRequest{
			Message: &a2apb.Message{
				MessageId: uuid.NewString(),
				Role:     a2apb.Role_ROLE_USER,
				Parts: []*a2apb.Part{{
					Content: &a2apb.Part_Text{Text: task},
				}},
			},
		},
		&a2apb.SendMessageResponse{},
		headerUserInstance(flagUser, instanceID))
	if err != nil {
		if !flagKeep {
			_, _ = callUnary(client, flagHost,
				"/kagent.api.v1alpha1.AgentInstanceService/DeleteAgentInstance",
				&apiv1alpha1.DeleteAgentInstanceRequest{AgentInstanceId: instanceID},
				&apiv1alpha1.DeleteAgentInstanceResponse{},
				headerUser(flagUser))
		}
		fatal("A2A SendMessage: %v", err)
	}
	fmt.Println()
	fmt.Println("=== resposta A2A ===")
	printSendResponse(sendResp.(*a2apb.SendMessageResponse))

	// 3) descarta a conversa (nao virar instancia zumbi)
	if !flagKeep {
		deleted, err := callUnary(client, flagHost,
			"/kagent.api.v1alpha1.AgentInstanceService/DeleteAgentInstance",
			&apiv1alpha1.DeleteAgentInstanceRequest{AgentInstanceId: instanceID},
			&apiv1alpha1.DeleteAgentInstanceResponse{},
			headerUser(flagUser))
		if err != nil {
			fmt.Printf("\naviso: DeleteAgentInstance: %v (instancia %s pode virar zumbi)\n", err, instanceID)
		} else if !flagQuiet {
			delResp := deleted.(*apiv1alpha1.DeleteAgentInstanceResponse)
			delID := delResp.GetAgentInstance().GetId()
			if delID == "" {
				delID = instanceID
			}
			fmt.Printf("\ninstancia descartada (%s) ✅\n", delID)
		}
	} else if !flagQuiet {
		fmt.Printf("\n--keep: instancia %s mantida\n", instanceID)
	}
}

func printSendResponse(resp *a2apb.SendMessageResponse) {
	switch payload := resp.GetPayload().(type) {
	case *a2apb.SendMessageResponse_Message:
		for _, part := range payload.Message.GetParts() {
			if text := part.GetText(); text != "" {
				fmt.Println(text)
			}
		}
	case *a2apb.SendMessageResponse_Task:
		task := payload.Task
		if status := task.GetStatus(); status != nil {
			if msg := status.GetMessage(); msg != nil {
				for _, part := range msg.GetParts() {
					if text := part.GetText(); text != "" {
						fmt.Println(text)
					}
				}
			}
			fmt.Printf("\n[task %s: %s]\n", task.GetId(), status.GetState().String())
		}
		for _, artifact := range task.GetArtifacts() {
			for _, part := range artifact.GetParts() {
				if text := part.GetText(); text != "" {
					fmt.Println(text)
				}
			}
		}
	default:
		fmt.Println("(resposta vazia)")
	}
}

// --- cliente gRPC-Web minimo (framing binario) ---

func callUnary(client *http.Client, host, path string, req, resp proto.Message, headers map[string]string) (proto.Message, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte(0) // flag: uncompressed
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	buf.Write(lenBuf[:])
	buf.Write(body)

	httpReq, err := http.NewRequest(http.MethodPost, host+path, &buf)
	if err != nil {
		return nil, err
	}
	if flagHostHdr != "" {
		httpReq.Host = flagHostHdr
	}
	httpReq.Header.Set("x-grpc-web", "1")
	httpReq.Header.Set("Content-Type", "application/grpc-web+proto")
	httpReq.Header.Set("Accept", "application/grpc-web+proto")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	if os.Getenv("KAGENT_A2A_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] %s %s -> HTTP %s\n", httpReq.Method, path, httpResp.Status)
	}

	if httpResp.StatusCode >= 500 {
		preview, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
		return nil, fmt.Errorf("HTTP %s: %s", httpResp.Status, strings.TrimSpace(string(preview)))
	}

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	if os.Getenv("KAGENT_A2A_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] corpo (%d bytes): %x\n", len(raw), raw)
	}

	statusCode, statusMsg, err := parseGrpcWebResponse(raw, resp)
	if err != nil {
		return nil, err
	}
	if statusCode != 0 {
		return nil, fmt.Errorf("grpc-status %d: %s", statusCode, statusMsg)
	}
	return resp, nil
}

// parseGrpcWebResponse decoda os frames do corpo gRPC-Web:
// frames de mensagem (flag 0x00) + frame de trailers (flag 0x80).
func parseGrpcWebResponse(raw []byte, resp proto.Message) (int, string, error) {
	var status int
	var statusMsg string

	for len(raw) > 0 {
		if len(raw) < 5 {
			break
		}
		flag := raw[0]
		length := int(binary.BigEndian.Uint32(raw[1:5]))
		raw = raw[5:]
		if length > len(raw) {
			return status, statusMsg, errors.New("frame truncado na resposta gRPC-Web")
		}
		payload := raw[:length]
		raw = raw[length:]

		switch {
		case flag&0x80 != 0: // frame de trailers: chave:valor separados por \n
			for _, kv := range strings.Split(string(payload), "\n") {
				k, v, found := strings.Cut(kv, ":")
				if !found {
					continue
				}
				switch k {
				case "grpc-status":
					fmt.Sscanf(v, "%d", &status)
				case "grpc-message":
					statusMsg = v
				}
			}
		default: // frame de mensagem (unary: exatamente um)
			if err := proto.Unmarshal(payload, resp); err != nil {
				return status, statusMsg, fmt.Errorf("unmarshal resposta: %w", err)
			}
		}
	}

	// HTTP 200 sem trailers explicitos = sucesso implicito
	if status == 0 && statusMsg == "" {
		return 0, "", nil
	}
	if status == 0 && statusMsg != "" {
		return 0, statusMsg, errors.New("grpc reportou status mas sem grpc-status")
	}
	return status, statusMsg, nil
}

func headerUser(userID string) map[string]string {
	return map[string]string{"x-user-id": userID}
}

func headerUserInstance(userID, instanceID string) map[string]string {
	return map[string]string{
		"x-user-id":                   userID,
		"x-kagent-agent-instance-id": instanceID,
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "erro: "+format+"\n", args...)
	os.Exit(1)
}
