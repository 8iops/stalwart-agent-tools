package broker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
