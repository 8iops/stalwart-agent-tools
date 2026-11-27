package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	Capability = "urn:ietf:params:jmap:mail"
	maxLimit   = 100
)

type Caller interface {
	Call(context.Context, string, string, any) (json.RawMessage, error)
}

type Service struct {
	caller    Caller
	accountID string
}

type QueryOptions struct {
	Text      string
	MailboxID string
	Position  uint64
	Limit     uint64
}

type QueryResult struct {
	QueryState string
	Position   uint64
	Total      uint64
	Messages   []Message
}

type Message struct {
	ID            string
	ThreadID      string
	BlobID        string
	MailboxIDs    []string
	Keywords      []string
	Size          uint64
	ReceivedAt    time.Time
	SentAt        *time.Time
	From          []Address
	To            []Address
	CC            []Address
	BCC           []Address
	ReplyTo       []Address
	Subject       string
	Preview       string
	HasAttachment bool
	TextBody      []BodyPart
	HTMLBody      []BodyPart
	BodyValues    map[string]BodyValue
}

type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type BodyPart struct {
	PartID      string     `json:"partId"`
	BlobID      string     `json:"blobId"`
	Size        uint64     `json:"size"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Charset     string     `json:"charset"`
	Disposition string     `json:"disposition"`
	CID         string     `json:"cid"`
	Language    []string   `json:"language"`
	Location    string     `json:"location"`
	SubParts    []BodyPart `json:"subParts"`
}

type BodyValue struct {
	Value       string `json:"value"`
	IsEncoding  bool   `json:"isEncodingProblem"`
	IsTruncated bool   `json:"isTruncated"`
}

type queryArguments struct {
	AccountID      string      `json:"accountId"`
	Filter         queryFilter `json:"filter,omitempty"`
	Sort           []sort      `json:"sort,omitempty"`
	Position       uint64      `json:"position"`
	Limit          uint64      `json:"limit"`
	CalculateTotal bool        `json:"calculateTotal"`
}

type queryFilter struct {
	Text      string `json:"text,omitempty"`
	InMailbox string `json:"inMailbox,omitempty"`
}

type sort struct {
	Property    string `json:"property"`
	IsAscending bool   `json:"isAscending"`
}

type queryResponse struct {
	AccountID  string   `json:"accountId"`
	QueryState string   `json:"queryState"`
	Position   uint64   `json:"position"`
	IDs        []string `json:"ids"`
	Total      uint64   `json:"total"`
}

type getArguments struct {
	AccountID  string   `json:"accountId"`
	IDs        []string `json:"ids"`
	Properties []string `json:"properties"`
}

type getResponse struct {
	AccountID string         `json:"accountId"`
	List      []providerMail `json:"list"`
	NotFound  []string       `json:"notFound"`
}

type providerMail struct {
	ID            string               `json:"id"`
	ThreadID      string               `json:"threadId"`
	BlobID        string               `json:"blobId"`
	MailboxIDs    map[string]bool      `json:"mailboxIds"`
	Keywords      map[string]bool      `json:"keywords"`
	Size          uint64               `json:"size"`
	ReceivedAt    string               `json:"receivedAt"`
	SentAt        string               `json:"sentAt"`
	From          []Address            `json:"from"`
	To            []Address            `json:"to"`
	CC            []Address            `json:"cc"`
	BCC           []Address            `json:"bcc"`
	ReplyTo       []Address            `json:"replyTo"`
	Subject       string               `json:"subject"`
	Preview       string               `json:"preview"`
	HasAttachment bool                 `json:"hasAttachment"`
	TextBody      []BodyPart           `json:"textBody"`
	HTMLBody      []BodyPart           `json:"htmlBody"`
	BodyValues    map[string]BodyValue `json:"bodyValues"`
}

var summaryProperties = []string{"id", "threadId", "blobId", "mailboxIds", "keywords", "size", "receivedAt", "sentAt", "from", "to", "cc", "bcc", "replyTo", "subject", "preview", "hasAttachment"}

func New(caller Caller, accountID string) (*Service, error) {
	if caller == nil || strings.TrimSpace(accountID) == "" || strings.ContainsAny(accountID, "\r\n") {
		return nil, errors.New("bound mail account and provider caller are required")
	}
	return &Service{caller: caller, accountID: accountID}, nil
}

func (s *Service) Query(ctx context.Context, options QueryOptions) (QueryResult, error) {
	if options.Limit == 0 || options.Limit > maxLimit {
		return QueryResult{}, fmt.Errorf("mail query limit must be between 1 and %d", maxLimit)
	}
	if len(options.Text) > 512 || len(options.MailboxID) > 255 || strings.ContainsAny(options.MailboxID, "\r\n") {
		return QueryResult{}, errors.New("invalid mail query filter")
	}
	raw, err := s.caller.Call(ctx, Capability, "Email/query", queryArguments{
		AccountID: s.accountID, Filter: queryFilter{Text: options.Text, InMailbox: options.MailboxID},
		Sort: []sort{{Property: "receivedAt", IsAscending: false}}, Position: options.Position,
		Limit: options.Limit, CalculateTotal: true,
	})
	if err != nil {
		return QueryResult{}, fmt.Errorf("query bound mail account: %w", err)
	}
	var query queryResponse
	if json.Unmarshal(raw, &query) != nil || query.AccountID != s.accountID || query.QueryState == "" || len(query.IDs) > int(options.Limit) {
		return QueryResult{}, errors.New("provider returned an invalid Email/query result")
	}
	messages, err := s.get(ctx, query.IDs, summaryProperties)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{QueryState: query.QueryState, Position: query.Position, Total: query.Total, Messages: messages}, nil
}

func (s *Service) Read(ctx context.Context, id string) (Message, error) {
	if strings.TrimSpace(id) == "" || len(id) > 255 || strings.ContainsAny(id, "\r\n") {
		return Message{}, errors.New("valid mail id is required")
	}
	properties := append(append([]string{}, summaryProperties...), "textBody", "htmlBody", "bodyValues")
	messages, err := s.get(ctx, []string{id}, properties)
	if err != nil {
		return Message{}, err
	}
	if len(messages) != 1 || messages[0].ID != id {
		return Message{}, errors.New("mail is unavailable in the bound account")
	}
	return messages[0], nil
}

func (s *Service) get(ctx context.Context, ids, properties []string) ([]Message, error) {
	if len(ids) == 0 {
		return []Message{}, nil
	}
	raw, err := s.caller.Call(ctx, Capability, "Email/get", getArguments{AccountID: s.accountID, IDs: ids, Properties: properties})
	if err != nil {
		return nil, fmt.Errorf("read bound mail account: %w", err)
	}
	var response getResponse
	if json.Unmarshal(raw, &response) != nil || response.AccountID != s.accountID || len(response.NotFound) != 0 || len(response.List) != len(ids) {
		return nil, errors.New("provider returned an invalid Email/get result")
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || wanted[id] {
			return nil, errors.New("provider returned invalid mail ids")
		}
		wanted[id] = true
	}
	messages := make([]Message, 0, len(response.List))
	seen := make(map[string]bool, len(response.List))
	for _, item := range response.List {
		if !wanted[item.ID] || seen[item.ID] {
			return nil, errors.New("provider returned mail outside the requested account selection")
		}
		seen[item.ID] = true
		message, err := normalize(item)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func normalize(item providerMail) (Message, error) {
	received, err := time.Parse(time.RFC3339, item.ReceivedAt)
	if item.ID == "" || err != nil {
		return Message{}, errors.New("provider returned malformed mail data")
	}
	var sent *time.Time
	if item.SentAt != "" {
		parsed, parseErr := time.Parse(time.RFC3339, item.SentAt)
		if parseErr != nil {
			return Message{}, errors.New("provider returned malformed mail data")
		}
		sent = &parsed
	}
	return Message{
		ID: item.ID, ThreadID: item.ThreadID, BlobID: item.BlobID,
		MailboxIDs: trueKeys(item.MailboxIDs), Keywords: trueKeys(item.Keywords), Size: item.Size,
		ReceivedAt: received, SentAt: sent, From: item.From, To: item.To, CC: item.CC, BCC: item.BCC,
		ReplyTo: item.ReplyTo, Subject: item.Subject, Preview: item.Preview, HasAttachment: item.HasAttachment,
		TextBody: item.TextBody, HTMLBody: item.HTMLBody, BodyValues: item.BodyValues,
	}, nil
}

func trueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, present := range values {
		if present {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
