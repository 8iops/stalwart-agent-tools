package mail

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/8iops/stalwart-agent-tools/internal/jmap"
)

type sendProvider struct {
	mu        sync.Mutex
	batches   [][]jmap.BatchCall
	responses [][]jmap.BatchResult
	err       error
}

func (p *sendProvider) Call(context.Context, string, string, any) (json.RawMessage, error) {
	return nil, errors.New("unexpected single call")
}

func (p *sendProvider) CallBatch(_ context.Context, calls []jmap.BatchCall) ([]jmap.BatchResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, calls)
	if p.err != nil {
		return nil, p.err
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

func previewResponses() []jmap.BatchResult {
	return []jmap.BatchResult{
		{ID: "identities", Result: json.RawMessage(`{"accountId":"account","list":[{"id":"identity","email":"me@example.test"}]}`)},
		{ID: "mailboxes", Result: json.RawMessage(`{"accountId":"account","list":[{"id":"drafts","role":"drafts","myRights":{"mayAddItems":true}}]}`)},
		{ID: "state", Result: json.RawMessage(`{"accountId":"account","state":"email-state"}`)},
	}
}

func sendResponses() []jmap.BatchResult {
	return []jmap.BatchResult{
		{ID: "email", Result: json.RawMessage(`{"created":{"draft":{"id":"email"}}}`)},
		{ID: "submission", Result: json.RawMessage(`{"created":{"send":{"id":"submission"}}}`)},
		{ID: "verify", Result: json.RawMessage(`{"accountId":"account","list":[{"id":"submission","emailId":"email","threadId":"thread","sendAt":"2026-11-27T02:03:04Z"}]}`)},
	}
}

func validDraft() Draft {
	return Draft{From: "me@example.test", To: []Address{{Name: "Them", Email: "them@example.test"}}, Subject: "subject", Text: "body"}
}

func TestPreviewResolvesBoundResourcesAndFingerprintsPlan(t *testing.T) {
	provider := &sendProvider{responses: [][]jmap.BatchResult{previewResponses()}}
	service, _ := New(provider, "account")
	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	plan, err := service.PreviewSend(context.Background(), validDraft())
	if err != nil {
		t.Fatal(err)
	}
	if plan.IdentityID != "identity" || plan.DraftsMailboxID != "drafts" || plan.EmailState != "email-state" || plan.Fingerprint == "" || !plan.ExpiresAt.Equal(now.Add(previewTTL)) {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	for _, call := range provider.batches[0] {
		arguments := call.Arguments.(map[string]any)
		if arguments["accountId"] != "account" {
			t.Fatalf("call escaped bound account: %#v", call)
		}
	}
}

func TestPreviewRejectsUnownedSenderAndDraftsWithoutRights(t *testing.T) {
	tests := [][]jmap.BatchResult{
		{{ID: "identities", Result: json.RawMessage(`{"accountId":"account","list":[]}`)}, previewResponses()[1], previewResponses()[2]},
		{previewResponses()[0], {ID: "mailboxes", Result: json.RawMessage(`{"accountId":"account","list":[{"id":"drafts","role":"drafts","myRights":{"mayAddItems":false}}]}`)}, previewResponses()[2]},
	}
	for _, responses := range tests {
		service, _ := New(&sendProvider{responses: [][]jmap.BatchResult{responses}}, "account")
		if _, err := service.PreviewSend(context.Background(), validDraft()); err == nil {
			t.Fatal("unsafe preview accepted")
		}
	}
}

func TestSendUsesExpectedStateCreationReferencesAndReadBack(t *testing.T) {
	provider := &sendProvider{responses: [][]jmap.BatchResult{previewResponses(), sendResponses()}}
	service, _ := New(provider, "account")
	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	plan, _ := service.PreviewSend(context.Background(), validDraft())
	result, err := service.Send(context.Background(), plan, "idempotency-key-0001", NewMemoryIdempotencyStore())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "provider-submitted" || result.SubmissionID != "submission" || result.EmailID != "email" {
		t.Fatalf("unexpected result: %#v", result)
	}
	calls := provider.batches[1]
	set := calls[0].Arguments.(map[string]any)
	if set["ifInState"] != "email-state" {
		t.Fatalf("expected state missing: %#v", set)
	}
	submit := calls[1].Arguments.(map[string]any)
	create := submit["create"].(map[string]any)["send"].(map[string]any)
	if create["emailId"] != "#draft" || create["identityId"] != "identity" {
		t.Fatalf("creation references missing: %#v", create)
	}
	verify := calls[2].Arguments.(map[string]any)
	if verify["ids"].([]string)[0] != "#send" {
		t.Fatalf("read-back reference missing: %#v", verify)
	}
}

func TestSendRejectsTamperedAndExpiredPlans(t *testing.T) {
	provider := &sendProvider{responses: [][]jmap.BatchResult{previewResponses(), previewResponses()}}
	service, _ := New(provider, "account")
	now := time.Date(2026, 11, 27, 1, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	plan, _ := service.PreviewSend(context.Background(), validDraft())
	plan.Draft.Subject = "tampered"
	if _, err := service.Send(context.Background(), plan, "idempotency-key-0001", NewMemoryIdempotencyStore()); err == nil {
		t.Fatal("tampered plan accepted")
	}
	plan, _ = service.PreviewSend(context.Background(), validDraft())
	service.now = func() time.Time { return now.Add(previewTTL + time.Second) }
	if _, err := service.Send(context.Background(), plan, "idempotency-key-0002", NewMemoryIdempotencyStore()); err == nil {
		t.Fatal("expired plan accepted")
	}
}

func TestMemoryStoreExecutesConcurrentRetryOnce(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	var calls atomic.Int32
	operation := func(context.Context) (SendResult, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return SendResult{SubmissionID: "one"}, nil
	}
	var wg sync.WaitGroup
	results := make(chan SendResult, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := store.Do(context.Background(), "idempotency-key-0001", "fingerprint", operation)
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	if calls.Load() != 1 {
		t.Fatalf("operation executed %d times", calls.Load())
	}
	for result := range results {
		if result.SubmissionID != "one" {
			t.Fatalf("unexpected cached result: %#v", result)
		}
	}
	if _, err := store.Do(context.Background(), "idempotency-key-0001", "different", operation); err == nil {
		t.Fatal("key reuse accepted")
	}
}
