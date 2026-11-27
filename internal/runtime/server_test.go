package runtime

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/8iops/stalwart-agent-tools/internal/broker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type unusedResolver struct{}

func (unusedResolver) Resolve(context.Context, string, ...string) (broker.Binding, error) {
	panic("unexpected resolve")
}

func TestMCPPublishesFullUserSurfaceWithTypedSchemas(t *testing.T) {
	runtime, _ := New(unusedResolver{})
	server := runtime.MCP()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v1"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		raw, _ := json.Marshal(tool.InputSchema)
		if !json.Valid(raw) {
			t.Fatalf("tool %s has invalid schema", tool.Name)
		}
		if tool.Name == "mail_send" || tool.Name == "calendar_respond" {
			if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Fatalf("write annotations missing for %s", tool.Name)
			}
		} else if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("read annotation missing for %s", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{"calendar_query", "calendar_respond", "collaboration_capabilities", "contacts_query", "files_list", "files_read", "mail_query", "mail_read", "mail_send"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("unexpected tools: %#v", names)
		}
	}
}
