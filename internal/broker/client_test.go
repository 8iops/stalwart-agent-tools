package broker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveUsesWorkloadIdentityAndEnforcesScopes(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/collaboration/resolve" || r.Header.Get("Authorization") != "Bearer workload-token" || r.Header.Get("X-RE8CH-Invocation") != "Bearer invocation-token" {
			t.Fatalf("unexpected broker request")
		}
		fmt.Fprintf(w, `{"connectionRef":"collaboration://owned","origin":"https://mail.example","bearerToken":"provider-secret","accountId":"account","scopes":["mail.read","mail.send"],"expiresAt":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1/collaboration/resolve", "workload-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithInvocationToken(context.Background(), "invocation-token")
	binding, err := client.Resolve(ctx, "collaboration://owned", "mail.send")
	if err != nil {
		t.Fatal(err)
	}
	if binding.AccountID != "account" || binding.BearerToken != "provider-secret" {
		t.Fatalf("unexpected binding: %#v", binding)
	}
	if _, err = client.Resolve(ctx, "collaboration://owned", "files.read"); err == nil || !strings.Contains(err.Error(), "omitted required scope") {
		t.Fatalf("missing scope accepted: %v", err)
	}
}

func TestProjectedWorkloadTokenReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	var authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"connectionRef":"collaboration://owned","origin":"https://mail.example","bearerToken":"provider","accountId":"account","scopes":["mail.read"],"expiresAt":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	client, err := NewFromTokenFile(server.URL+"/v1/collaboration/resolve", path, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithInvocationToken(context.Background(), "invocation")
	if _, err = client.Resolve(ctx, "collaboration://owned", "mail.read"); err != nil || authorization != "Bearer first" {
		t.Fatalf("first token failed: %q %v", authorization, err)
	}
	if err = os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resolve(ctx, "collaboration://owned", "mail.read"); err != nil || authorization != "Bearer second" {
		t.Fatalf("rotated token failed: %q %v", authorization, err)
	}
}

func TestResolveRejectsUnsafeInputsAndRedirects(t *testing.T) {
	client, err := New("https://broker.example/v1/collaboration/resolve", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithInvocationToken(context.Background(), "invocation-token")
	for _, ref := range []string{"", "other://x", "collaboration://x\n"} {
		if _, err = client.Resolve(ctx, ref, "mail.read"); err == nil {
			t.Fatalf("unsafe ref accepted: %q", ref)
		}
	}
	if _, err = client.Resolve(ctx, "collaboration://x", "admin"); err == nil {
		t.Fatal("unsafe scope accepted")
	}
	if _, err = client.Resolve(context.Background(), "collaboration://x", "mail.read"); err == nil {
		t.Fatal("missing invocation token accepted")
	}
}

func TestNewAcceptsOnlyExactInClusterPlaintextBroker(t *testing.T) {
	endpoint := "http://tenant-api.tenant-infrastructure.svc.cluster.local/v1/collaboration/resolve"
	client, err := New(endpoint, "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.endpoint.String(); got != endpoint {
		t.Fatalf("unexpected endpoint %q", got)
	}
	for _, candidate := range []string{
		"http://tenant-api.tenant-infrastructure.svc/v1/collaboration/resolve",
		"http://tenant-api.tenant-infrastructure.svc.cluster.local:80/v1/collaboration/resolve",
		"http://tenant-api.tenant-infrastructure.svc.cluster.local/other",
	} {
		if _, err := New(candidate, "token", nil); err == nil {
			t.Fatalf("accepted unreviewed plaintext endpoint %q", candidate)
		}
	}
}
