// a2a-ping: conversa A2A direto no gateway do kagent, sem passar pela CLI.
//
// Fluxo (mesmo que `kagent invoke` faz por baixo, só que exposto aqui):
//  1. port-forward svc/kagent-controller -> localhost:8083
//  2. CreateAgentInstance via gRPC API (nova conversa, sem pegar instância existente)
//  3. A2A SendMessage no gateway (gRPC), roteado por header x-kagent-agent-instance-id
//  4. imprime a resposta e deleta a instância
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"net"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/api/client"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

const (
	gatewayURL = "http://localhost:8083"
	userID     = "admin@kagent.dev"
)

func main() {
	template := "loki-specialist"
	harness := "adk"
	task := "ping"
	if len(os.Args) > 1 {
		template = os.Args[1]
	}
	if len(os.Args) > 2 {
		harness = os.Args[2]
	}
	if len(os.Args) > 3 {
		task = strings.Join(os.Args[3:], " ")
	}

	// 1) port-forward
	pf := exec.Command("kubectl", "-n", "kagent", "port-forward", "svc/kagent-controller", "8083:8083")
	pf.Stdout, pf.Stderr = nil, nil
	if err := pf.Start(); err != nil {
		log.Fatalf("port-forward: %v", err)
	}
	defer pf.Process.Kill()

	// espera o gateway aceitar conexões (dial TCP real — o NewAPI é lazy)
	deadline := time.Now().Add(20 * time.Second)
	for {
		var conn net.Conn
		var dialErr error
		conn, dialErr = net.DialTimeout("tcp", "127.0.0.1:8083", 500*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("gateway não ficou pronto em 20s (último erro: %v)", dialErr)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 2) control plane: nova instância
	api, err := client.NewAPI(gatewayURL, client.WithUserID(userID))
	if err != nil {
		log.Fatalf("api: %v", err)
	}
	defer api.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req := &apiv1alpha1.CreateAgentInstanceRequest{
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: template},
		Harness:       &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: harness},
		RequestId:     uuid.NewString(),
	}
	created, err := api.AgentInstance.CreateAgentInstance(ctx, req)
	if err != nil {
		log.Fatalf("create instance: %v", err)
	}
	inst := created.GetAgentInstance()
	fmt.Printf("nova instância: %s (%s via %s)\n", inst.Id, template, harness)

	// 3) A2A gateway: SendMessage roteado pela instância
	gw, err := client.NewGateway(gatewayURL, client.WithUserID(userID))
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	defer gw.Close()

	a2aClient, err := gw.A2A.ForAgentInstance(ctx, inst.Id)
	if err != nil {
		log.Fatalf("a2a client: %v", err)
	}
	_ = metadata.AppendToOutgoingContext(ctx, "x-kagent-agent-instance-id", inst.Id)

	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart(task))
	result, err := a2aClient.SendMessage(ctx, &a2atype.SendMessageRequest{Message: message})
	if err != nil {
		log.Fatalf("send message: %v", err)
	}

	// 4) imprime o texto da resposta
	var out strings.Builder
	switch r := result.(type) {
	case *a2atype.Message:
		for _, p := range r.Parts {
			out.WriteString(p.Text())
		}
	case *a2atype.Task:
		if r.Status.Message != nil {
			for _, p := range r.Status.Message.Parts {
				out.WriteString(p.Text())
			}
		}
		for _, art := range r.Artifacts {
			if art == nil {
				continue
			}
			for _, p := range art.Parts {
				out.WriteString(p.Text())
			}
		}
	}
	fmt.Printf("\n=== resposta A2A ===\n%s\n", out.String())

	// 5) descarta a instância (conversa nova, não vira zumbi)
	if _, err := api.AgentInstance.DeleteAgentInstance(ctx, &apiv1alpha1.DeleteAgentInstanceRequest{AgentInstanceId: inst.Id}); err != nil {
		fmt.Printf("aviso: delete instance: %v\n", err)
	} else {
		fmt.Println("instância descartada ✅")
	}
}
