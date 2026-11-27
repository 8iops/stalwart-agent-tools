package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/8iops/stalwart-agent-tools/internal/jmap"
)

const previewTTL = 10 * time.Minute

var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

type Draft struct {
	From          string
	To, CC, BCC   []Address
	Subject, Text string
}

type SendPlan struct {
	AccountID, IdentityID, DraftsMailboxID, EmailState string
	Draft                                              Draft
	ExpiresAt                                          time.Time
	Fingerprint                                        string
}

type SendResult struct {
	SubmissionID, EmailID, ThreadID string
	SendAt                          *time.Time
	Outcome                         string
}

type IdempotencyStore interface {
	Do(context.Context, string, string, func(context.Context) (SendResult, error)) (SendResult, error)
}

type memoryEntry struct {
	fingerprint string
	done        chan struct{}
	result      SendResult
	err         error
}

type MemoryIdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*memoryEntry
}

func NewMemoryIdempotencyStore() *MemoryIdempotencyStore {
	return &MemoryIdempotencyStore{entries: map[string]*memoryEntry{}}
}

func (s *MemoryIdempotencyStore) Do(ctx context.Context, key, fingerprint string, operation func(context.Context) (SendResult, error)) (SendResult, error) {
	if !validIdempotencyKey(key) || fingerprint == "" || operation == nil {
		return SendResult{}, errors.New("valid idempotency key, fingerprint, and operation are required")
	}
	s.mu.Lock()
	entry, exists := s.entries[key]
	if exists {
		if entry.fingerprint != fingerprint {
			s.mu.Unlock()
			return SendResult{}, errors.New("idempotency key was already used for a different operation")
		}
		done := entry.done
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return SendResult{}, ctx.Err()
		case <-done:
			return entry.result, entry.err
		}
	}
	entry = &memoryEntry{fingerprint: fingerprint, done: make(chan struct{})}
	s.entries[key] = entry
	s.mu.Unlock()
	entry.result, entry.err = operation(ctx)
	s.mu.Lock()
	close(entry.done)
	s.mu.Unlock()
	return entry.result, entry.err
}

func (s *Service) PreviewSend(ctx context.Context, draft Draft) (SendPlan, error) {
	if s.batch == nil {
		return SendPlan{}, errors.New("provider does not support bounded JMAP batches")
	}
	if err := validateDraft(draft); err != nil {
		return SendPlan{}, err
	}
	results, err := s.batch.CallBatch(ctx, []jmap.BatchCall{
		{ID: "identities", Capability: Capability, Method: "Identity/get", Arguments: map[string]any{"accountId": s.accountID}},
		{ID: "mailboxes", Capability: Capability, Method: "Mailbox/get", Arguments: map[string]any{"accountId": s.accountID, "properties": []string{"id", "role", "myRights"}}},
		{ID: "state", Capability: Capability, Method: "Email/get", Arguments: map[string]any{"accountId": s.accountID, "ids": []string{}, "properties": []string{"id"}}},
	})
	if err != nil {
		return SendPlan{}, fmt.Errorf("preview mail submission: %w", err)
	}
	if err = batchErrors(results, 3); err != nil {
		return SendPlan{}, fmt.Errorf("preview mail submission: %w", err)
	}
	identityID, err := selectIdentity(results[0].Result, s.accountID, draft.From)
	if err != nil {
		return SendPlan{}, err
	}
	mailboxID, err := selectDraftsMailbox(results[1].Result, s.accountID)
	if err != nil {
		return SendPlan{}, err
	}
	state, err := emailState(results[2].Result, s.accountID)
	if err != nil {
		return SendPlan{}, err
	}
	plan := SendPlan{AccountID: s.accountID, IdentityID: identityID, DraftsMailboxID: mailboxID, EmailState: state, Draft: draft, ExpiresAt: s.now().UTC().Add(previewTTL)}
	plan.Fingerprint, err = fingerprint(plan)
	return plan, err
}

func (s *Service) Send(ctx context.Context, plan SendPlan, idempotencyKey string, store IdempotencyStore) (SendResult, error) {
	if s.batch == nil || store == nil || plan.AccountID != s.accountID || s.now().UTC().After(plan.ExpiresAt) {
		return SendResult{}, errors.New("valid unexpired bound send plan and idempotency store are required")
	}
	wanted, err := fingerprint(plan)
	if err != nil || wanted != plan.Fingerprint {
		return SendResult{}, errors.New("send plan fingerprint is invalid")
	}
	if !validIdempotencyKey(idempotencyKey) {
		return SendResult{}, errors.New("valid idempotency key is required")
	}
	return store.Do(ctx, idempotencyKey, plan.Fingerprint, func(ctx context.Context) (SendResult, error) { return s.submit(ctx, plan) })
}

func (s *Service) submit(ctx context.Context, plan SendPlan) (SendResult, error) {
	mail := map[string]any{
		"mailboxIds": map[string]bool{plan.DraftsMailboxID: true}, "keywords": map[string]bool{"$draft": true},
		"from": []Address{{Email: plan.Draft.From}}, "to": plan.Draft.To, "cc": plan.Draft.CC, "bcc": plan.Draft.BCC,
		"subject": plan.Draft.Subject, "bodyValues": map[string]any{"text": map[string]string{"value": plan.Draft.Text}},
		"textBody": []map[string]string{{"partId": "text", "type": "text/plain"}},
	}
	results, err := s.batch.CallBatch(ctx, []jmap.BatchCall{
		{ID: "email", Capability: Capability, Method: "Email/set", Arguments: map[string]any{"accountId": s.accountID, "ifInState": plan.EmailState, "create": map[string]any{"draft": mail}}},
		{ID: "submission", Capability: Capability, Method: "EmailSubmission/set", Arguments: map[string]any{"accountId": s.accountID, "create": map[string]any{"send": map[string]any{"emailId": "#draft", "identityId": plan.IdentityID}}, "onSuccessDestroyEmail": []string{"#send"}}},
		{ID: "verify", Capability: Capability, Method: "EmailSubmission/get", Arguments: map[string]any{"accountId": s.accountID, "ids": []string{"#send"}, "properties": []string{"id", "emailId", "threadId", "sendAt"}}},
	})
	if err != nil {
		return SendResult{}, fmt.Errorf("submit mail: %w", err)
	}
	if err = batchErrors(results, 3); err != nil {
		return SendResult{}, fmt.Errorf("submit mail: %w", err)
	}
	return verifiedSubmission(results[2].Result, s.accountID)
}

func validateDraft(draft Draft) error {
	count := len(draft.To) + len(draft.CC) + len(draft.BCC)
	if !emailPattern.MatchString(draft.From) || count == 0 || count > 100 || len(draft.Subject) > 998 || len(draft.Text) == 0 || len(draft.Text) > 1<<20 {
		return errors.New("invalid or unbounded mail draft")
	}
	all := append(append(append([]Address{}, draft.To...), draft.CC...), draft.BCC...)
	for _, address := range all {
		if !emailPattern.MatchString(address.Email) || len(address.Name) > 255 {
			return errors.New("invalid mail recipient")
		}
	}
	return nil
}

func fingerprint(plan SendPlan) (string, error) {
	plan.Fingerprint = ""
	encoded, err := json.Marshal(plan)
	if err != nil {
		return "", errors.New("encode canonical send plan")
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validIdempotencyKey(key string) bool {
	return len(key) >= 16 && len(key) <= 128 && !strings.ContainsAny(key, "\r\n \t")
}

func batchErrors(results []jmap.BatchResult, wanted int) error {
	if len(results) != wanted {
		return errors.New("provider returned an incomplete JMAP batch")
	}
	for _, result := range results {
		if result.Error != nil {
			return fmt.Errorf("JMAP call %s: %w", result.ID, result.Error)
		}
	}
	return nil
}

func selectIdentity(raw json.RawMessage, accountID, from string) (string, error) {
	var response struct {
		AccountID string `json:"accountId"`
		List      []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"list"`
	}
	if json.Unmarshal(raw, &response) != nil || response.AccountID != accountID {
		return "", errors.New("provider returned invalid identities")
	}
	for _, identity := range response.List {
		if identity.ID != "" && strings.EqualFold(identity.Email, from) {
			return identity.ID, nil
		}
	}
	return "", errors.New("sender is not an identity of the bound account")
}

func selectDraftsMailbox(raw json.RawMessage, accountID string) (string, error) {
	var response struct {
		AccountID string `json:"accountId"`
		List      []struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			MyRights struct {
				MayAddItems bool `json:"mayAddItems"`
			} `json:"myRights"`
		} `json:"list"`
	}
	if json.Unmarshal(raw, &response) != nil || response.AccountID != accountID {
		return "", errors.New("provider returned invalid mailboxes")
	}
	for _, mailbox := range response.List {
		if mailbox.ID != "" && mailbox.Role == "drafts" && mailbox.MyRights.MayAddItems {
			return mailbox.ID, nil
		}
	}
	return "", errors.New("bound account has no writable Drafts mailbox")
}

func emailState(raw json.RawMessage, accountID string) (string, error) {
	var response struct {
		AccountID string `json:"accountId"`
		State     string `json:"state"`
	}
	if json.Unmarshal(raw, &response) != nil || response.AccountID != accountID || response.State == "" {
		return "", errors.New("provider returned invalid email state")
	}
	return response.State, nil
}

func verifiedSubmission(raw json.RawMessage, accountID string) (SendResult, error) {
	var response struct {
		AccountID string `json:"accountId"`
		List      []struct {
			ID       string `json:"id"`
			EmailID  string `json:"emailId"`
			ThreadID string `json:"threadId"`
			SendAt   string `json:"sendAt"`
		} `json:"list"`
	}
	if json.Unmarshal(raw, &response) != nil || response.AccountID != accountID || len(response.List) != 1 || response.List[0].ID == "" || response.List[0].EmailID == "" {
		return SendResult{}, errors.New("provider did not return a verifiable mail submission")
	}
	item := response.List[0]
	result := SendResult{SubmissionID: item.ID, EmailID: item.EmailID, ThreadID: item.ThreadID, Outcome: "provider-submitted"}
	if item.SendAt != "" {
		sendAt, err := time.Parse(time.RFC3339, item.SendAt)
		if err != nil {
			return SendResult{}, errors.New("provider returned an invalid submission time")
		}
		result.SendAt = &sendAt
	}
	return result, nil
}
