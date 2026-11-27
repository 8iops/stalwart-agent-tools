package mail

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type call struct {
	capability string
	method     string
	arguments  any
}

type fakeCaller struct {
	responses []json.RawMessage
	err       error
	calls     []call
}

func (f *fakeCaller) Call(_ context.Context, capability, method string, arguments any) (json.RawMessage, error) {
	f.calls = append(f.calls, call{capability: capability, method: method, arguments: arguments})
	if f.err != nil {
		return nil, f.err
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response, nil
}

func TestQueryUsesOnlyBoundAccountAndNormalizesResults(t *testing.T) {
	caller := &fakeCaller{responses: []json.RawMessage{
		json.RawMessage(`{"accountId":"bound-account","queryState":"q1","position":0,"ids":["m1"],"total":1}`),
		json.RawMessage(`{"accountId":"bound-account","list":[{"id":"m1","threadId":"t1","mailboxIds":{"inbox":true,"archive":false},"keywords":{"$seen":true},"size":42,"receivedAt":"2026-11-27T01:02:03Z","from":[{"name":"Sender","email":"sender@example.test"}],"subject":"hello","preview":"world"}],"notFound":[]}`),
	}}
	service, err := New(caller, "bound-account")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Query(context.Background(), QueryOptions{Text: "hello", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Messages) != 1 || result.Messages[0].ID != "m1" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !reflect.DeepEqual(result.Messages[0].MailboxIDs, []string{"inbox"}) || !reflect.DeepEqual(result.Messages[0].Keywords, []string{"$seen"}) {
		t.Fatalf("boolean maps were not normalized: %#v", result.Messages[0])
	}
	if len(caller.calls) != 2 || caller.calls[0].method != "Email/query" || caller.calls[1].method != "Email/get" {
		t.Fatalf("unexpected calls: %#v", caller.calls)
	}
	query := caller.calls[0].arguments.(queryArguments)
	get := caller.calls[1].arguments.(getArguments)
	if query.AccountID != "bound-account" || get.AccountID != "bound-account" || caller.calls[0].capability != Capability {
		t.Fatalf("calls escaped the bound account: %#v %#v", query, get)
	}
}

func TestQueryEnforcesBoundsBeforeCallingProvider(t *testing.T) {
	caller := &fakeCaller{}
	service, _ := New(caller, "bound-account")
	for _, options := range []QueryOptions{{}, {Limit: 101}, {Limit: 1, Text: string(make([]byte, 513))}} {
		if _, err := service.Query(context.Background(), options); err == nil {
			t.Fatalf("expected bounds error for %#v", options)
		}
	}
	if len(caller.calls) != 0 {
		t.Fatal("invalid query reached provider")
	}
}

func TestQueryRejectsCrossAccountAndUnexpectedMail(t *testing.T) {
	tests := []struct {
		name      string
		responses []json.RawMessage
	}{
		{"cross-account query", []json.RawMessage{json.RawMessage(`{"accountId":"other","queryState":"q1","ids":[]}`)}},
		{"unexpected mail", []json.RawMessage{
			json.RawMessage(`{"accountId":"bound-account","queryState":"q1","ids":["m1"],"total":1}`),
			json.RawMessage(`{"accountId":"bound-account","list":[{"id":"m2","receivedAt":"2026-11-27T01:02:03Z"}],"notFound":[]}`),
		}},
		{"duplicate mail", []json.RawMessage{
			json.RawMessage(`{"accountId":"bound-account","queryState":"q1","ids":["m1","m2"],"total":2}`),
			json.RawMessage(`{"accountId":"bound-account","list":[{"id":"m1","receivedAt":"2026-11-27T01:02:03Z"},{"id":"m1","receivedAt":"2026-11-27T01:02:03Z"}],"notFound":[]}`),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, _ := New(&fakeCaller{responses: test.responses}, "bound-account")
			if _, err := service.Query(context.Background(), QueryOptions{Limit: 10}); err == nil {
				t.Fatal("expected provider result rejection")
			}
		})
	}
}

func TestReadRequestsBodiesAndRejectsNotFound(t *testing.T) {
	caller := &fakeCaller{responses: []json.RawMessage{json.RawMessage(`{"accountId":"bound-account","list":[{"id":"m1","receivedAt":"2026-11-27T01:02:03Z","textBody":[{"partId":"p1","type":"text/plain"}],"bodyValues":{"p1":{"value":"hello"}}}],"notFound":[]}`)}}
	service, _ := New(caller, "bound-account")
	message, err := service.Read(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if message.BodyValues["p1"].Value != "hello" {
		t.Fatalf("body missing: %#v", message)
	}
	properties := caller.calls[0].arguments.(getArguments).Properties
	if !contains(properties, "textBody") || !contains(properties, "htmlBody") || !contains(properties, "bodyValues") {
		t.Fatalf("body properties missing: %#v", properties)
	}

	caller.responses = []json.RawMessage{json.RawMessage(`{"accountId":"bound-account","list":[],"notFound":["missing"]}`)}
	if _, err := service.Read(context.Background(), "missing"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestProviderErrorsRemainWrapped(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	service, _ := New(&fakeCaller{err: providerErr}, "bound-account")
	_, err := service.Query(context.Background(), QueryOptions{Limit: 1})
	if !errors.Is(err, providerErr) {
		t.Fatalf("provider error not wrapped: %v", err)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
