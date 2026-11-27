package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const mailCapability = "urn:ietf:params:jmap:mail"

func newTestClient(t *testing.T, handler http.Handler, responseLimit int64) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	client, err := New(Config{
		Origin:           server.URL,
		BearerToken:      "test-provider-token",
		HTTPClient:       server.Client(),
		MaxResponseBytes: responseLimit,
		AllowedMethods:   map[string][]string{mailCapability: {"Email/get"}},
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func session(origin string) string {
	return fmt.Sprintf(`{
      "capabilities":{"urn:ietf:params:jmap:core":{},%q:{}},
      "accounts":{"account":{"name":"Fixture","isPersonal":true,"isReadOnly":false,"accountCapabilities":{%q:{}}}},
      "primaryAccounts":{%q:"account"},
      "apiUrl":%q,
      "uploadUrl":%q,
      "downloadUrl":%q,
      "eventSourceUrl":%q
    }`, mailCapability, mailCapability, mailCapability, origin+"/jmap/", origin+"/upload/{accountId}/", origin+"/download/{accountId}/{blobId}/{name}", origin+"/events")
}

func TestDiscoverAndCallRegisteredMethod(t *testing.T) {
	var server *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-provider-token" {
			t.Error("provider credential was not sent")
		}
		switch r.URL.Path {
		case "/jmap/session":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, session(server.URL))
		case "/jmap/":
			var request struct {
				Using       []string            `json:"using"`
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.MethodCalls) != 1 {
				t.Fatal("invalid request")
			}
			fmt.Fprint(w, `{"methodResponses":[["Email/get",{"state":"s1","list":[]},"c1"]]}`)
		default:
			http.NotFound(w, r)
		}
	})
	client, created := newTestClient(t, handler, 1<<20)
	server = created
	defer server.Close()

	if _, err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := client.Call(context.Background(), mailCapability, "Email/get", map[string]any{"ids": []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), `"state":"s1"`) {
		t.Fatalf("unexpected result: %s", result)
	}
}

func TestRejectsCrossOriginSessionEndpoint(t *testing.T) {
	client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"capabilities":{"x":{}},"accounts":{"a":{}},"apiUrl":"https://evil.example/jmap"}`)
	}), 1<<20)
	defer server.Close()
	if _, err := client.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "outside the reviewed origin") {
		t.Fatalf("cross-origin endpoint was not rejected: %v", err)
	}
}

func TestRejectsRedirect(t *testing.T) {
	client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}), 1<<20)
	defer server.Close()
	if _, err := client.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "redirect is forbidden") {
		t.Fatalf("redirect was not rejected: %v", err)
	}
}

func TestRequiresAdvertisedCapabilityAndRegisteredMethod(t *testing.T) {
	var server *httptest.Server
	client, created := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, session(server.URL))
	}), 1<<20)
	server = created
	defer server.Close()
	if _, err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(context.Background(), "unknown", "Email/get", map[string]any{}); err == nil {
		t.Fatal("unknown capability was accepted")
	}
	if _, err := client.Call(context.Background(), mailCapability, "Email/set", map[string]any{}); err == nil {
		t.Fatal("unregistered method was accepted")
	}
}

func TestBoundsProviderResponseAndDoesNotLeakToken(t *testing.T) {
	client, server := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 65))
	}), 64)
	defer server.Close()
	_, err := client.Discover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds configured limit") {
		t.Fatalf("oversized response was not rejected: %v", err)
	}
	if strings.Contains(err.Error(), "test-provider-token") {
		t.Fatal("provider credential leaked in error")
	}
}

func TestBoundsTypedRequest(t *testing.T) {
	client, err := New(Config{
		Origin:          "https://mail.example",
		BearerToken:     "token",
		MaxRequestBytes: 32,
		AllowedMethods:  map[string][]string{mailCapability: {"Email/get"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.session = &Session{
		Capabilities: map[string]json.RawMessage{mailCapability: json.RawMessage(`{}`)},
		APIURL:       "https://mail.example/jmap/",
	}
	if _, err = client.Call(context.Background(), mailCapability, "Email/get", map[string]any{"ids": []string{strings.Repeat("x", 64)}}); err == nil || !strings.Contains(err.Error(), "request exceeds configured limit") {
		t.Fatalf("oversized request was not rejected: %v", err)
	}
}

func TestNormalizesJMAPMethodError(t *testing.T) {
	var server *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap/session" {
			fmt.Fprint(w, session(server.URL))
			return
		}
		fmt.Fprint(w, `{"methodResponses":[["error",{"type":"accountReadOnly","description":"write denied"},"c1"]]}`)
	})
	client, created := newTestClient(t, handler, 1<<20)
	server = created
	defer server.Close()
	if _, err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := client.Call(context.Background(), mailCapability, "Email/get", map[string]any{})
	var protocolError *ProtocolError
	if !errors.As(err, &protocolError) || protocolError.Type != "accountReadOnly" {
		t.Fatalf("method error was not normalized: %v", err)
	}
}

func TestRejectsUnreviewedOriginAndCredentialInjection(t *testing.T) {
	for _, config := range []Config{
		{Origin: "http://mail.example", BearerToken: "token", AllowedMethods: map[string][]string{"x": {"Y/get"}}},
		{Origin: "https://user@mail.example", BearerToken: "token", AllowedMethods: map[string][]string{"x": {"Y/get"}}},
		{Origin: "https://mail.example/unreviewed", BearerToken: "token", AllowedMethods: map[string][]string{"x": {"Y/get"}}},
		{Origin: "https://mail.example", BearerToken: "token\r\ninjected", AllowedMethods: map[string][]string{"x": {"Y/get"}}},
	} {
		if _, err := New(config); err == nil {
			t.Fatalf("unsafe config was accepted: %#v", config)
		}
	}
}

func TestBatchEncodesReviewedResultReference(t *testing.T) {
	var server *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap/session" {
			fmt.Fprint(w, session(server.URL))
			return
		}
		var envelope struct {
			Using       []string            `json:"using"`
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		if json.NewDecoder(r.Body).Decode(&envelope) != nil || len(envelope.MethodCalls) != 2 {
			t.Fatal("invalid batch request")
		}
		var arguments map[string]json.RawMessage
		if json.Unmarshal(envelope.MethodCalls[1][1], &arguments) != nil {
			t.Fatal("invalid second call arguments")
		}
		var reference map[string]string
		if json.Unmarshal(arguments["#emailId"], &reference) != nil || reference["resultOf"] != "create" || reference["name"] != "Email/set" || reference["path"] != "/created/draft/id" {
			t.Fatalf("result reference not encoded safely: %s", arguments["#emailId"])
		}
		fmt.Fprint(w, `{"methodResponses":[["Email/set",{"created":{"draft":{"id":"m1"}}},"create"],["EmailSubmission/set",{"created":{"submit":{"id":"s1"}}},"submit"]]}`)
	})
	server = httptest.NewTLSServer(handler)
	defer server.Close()
	client, err := New(Config{
		Origin: server.URL, BearerToken: "token", HTTPClient: server.Client(),
		AllowedMethods: map[string][]string{mailCapability: {"Email/set", "EmailSubmission/set"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, err := client.CallBatch(context.Background(), []BatchCall{
		{ID: "create", Capability: mailCapability, Method: "Email/set", Arguments: map[string]any{"accountId": "a"}},
		{ID: "submit", Capability: mailCapability, Method: "EmailSubmission/set", Arguments: map[string]any{"accountId": "a"}, ResultReferences: []ResultReference{{Argument: "emailId", ResultOf: "create", Name: "Email/set", Path: "/created/draft/id"}}},
	})
	if err != nil || len(results) != 2 || results[1].ID != "submit" {
		t.Fatalf("unexpected batch result: %#v, %v", results, err)
	}
}

func TestBatchRejectsInvalidCallsBeforeProvider(t *testing.T) {
	client, err := New(Config{Origin: "https://mail.example", BearerToken: "token", AllowedMethods: map[string][]string{mailCapability: {"Email/set", "EmailSubmission/set"}}})
	if err != nil {
		t.Fatal(err)
	}
	client.session = &Session{Capabilities: map[string]json.RawMessage{mailCapability: json.RawMessage(`{}`)}, APIURL: "https://mail.example/jmap/"}
	tests := [][]BatchCall{
		{},
		{{ID: "same", Capability: mailCapability, Method: "Email/set", Arguments: map[string]any{}}, {ID: "same", Capability: mailCapability, Method: "Email/set", Arguments: map[string]any{}}},
		{{ID: "submit", Capability: mailCapability, Method: "EmailSubmission/set", Arguments: map[string]any{}, ResultReferences: []ResultReference{{Argument: "emailId", ResultOf: "later", Name: "Email/set", Path: "/created/x/id"}}}},
		{{ID: "create", Capability: mailCapability, Method: "Email/set", Arguments: []string{"not-an-object"}}},
	}
	for _, calls := range tests {
		if _, err = client.CallBatch(context.Background(), calls); err == nil {
			t.Fatalf("invalid batch accepted: %#v", calls)
		}
	}
}

func TestBatchReturnsAttributedPartialMethodError(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap/session" {
			fmt.Fprint(w, session(server.URL))
			return
		}
		fmt.Fprint(w, `{"methodResponses":[["Email/get",{"list":[]},"read"],["error",{"type":"notFound","description":"missing"},"read2"]]}`)
	}))
	defer server.Close()
	client, err := New(Config{Origin: server.URL, BearerToken: "token", HTTPClient: server.Client(), AllowedMethods: map[string][]string{mailCapability: {"Email/get"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, err := client.CallBatch(context.Background(), []BatchCall{
		{ID: "read", Capability: mailCapability, Method: "Email/get", Arguments: map[string]any{}},
		{ID: "read2", Capability: mailCapability, Method: "Email/get", Arguments: map[string]any{}},
	})
	if err != nil || results[0].Error != nil || results[1].Error == nil || results[1].Error.Type != "notFound" {
		t.Fatalf("partial error was not attributed: %#v, %v", results, err)
	}
}

func TestBatchRejectsResponseReordering(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jmap/session" {
			fmt.Fprint(w, session(server.URL))
			return
		}
		fmt.Fprint(w, `{"methodResponses":[["Email/get",{},"b"],["Email/get",{},"a"]]}`)
	}))
	defer server.Close()
	client, _ := New(Config{Origin: server.URL, BearerToken: "token", HTTPClient: server.Client(), AllowedMethods: map[string][]string{mailCapability: {"Email/get"}}})
	if _, err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := client.CallBatch(context.Background(), []BatchCall{{ID: "a", Capability: mailCapability, Method: "Email/get", Arguments: map[string]any{}}, {ID: "b", Capability: mailCapability, Method: "Email/get", Arguments: map[string]any{}}})
	if err == nil || !strings.Contains(err.Error(), "order or id") {
		t.Fatalf("response reordering was accepted: %v", err)
	}
}
