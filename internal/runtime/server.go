package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/8iops/stalwart-agent-tools/internal/broker"
	"github.com/8iops/stalwart-agent-tools/internal/files"
	"github.com/8iops/stalwart-agent-tools/internal/groupware"
	"github.com/8iops/stalwart-agent-tools/internal/jmap"
	"github.com/8iops/stalwart-agent-tools/internal/mail"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Resolver interface {
	Resolve(context.Context, string, ...string) (broker.Binding, error)
}

type Server struct {
	resolver       Resolver
	mailPlans      sync.Map
	responsePlans  sync.Map
	mailWrites     *mail.MemoryIdempotencyStore
	responseWrites *memoryResponseStore
	now            func() time.Time
}

type planEntry[T any] struct {
	Plan      T
	ExpiresAt time.Time
}

type connectionInput struct {
	ConnectionRef string `json:"connectionRef"`
}
type queryInput struct {
	ConnectionRef string `json:"connectionRef"`
	Action        string `json:"action,omitempty"`
	Text          string `json:"text,omitempty"`
	MailboxID     string `json:"mailboxId,omitempty"`
	AddressBookID string `json:"addressBookId,omitempty"`
	ContactID     string `json:"contactId,omitempty"`
	EventID       string `json:"eventId,omitempty"`
	After         string `json:"after,omitempty"`
	Before        string `json:"before,omitempty"`
	TimeZone      string `json:"timeZone,omitempty"`
	Position      uint64 `json:"position,omitempty"`
	Limit         uint64 `json:"limit,omitempty"`
}
type readMailInput struct {
	ConnectionRef string `json:"connectionRef"`
	EmailID       string `json:"emailId"`
}
type fileInput struct {
	ConnectionRef string `json:"connectionRef"`
	Href          string `json:"href"`
}
type sendInput struct {
	ConnectionRef  string         `json:"connectionRef"`
	Apply          bool           `json:"apply"`
	From           string         `json:"from,omitempty"`
	To             []mail.Address `json:"to,omitempty"`
	CC             []mail.Address `json:"cc,omitempty"`
	BCC            []mail.Address `json:"bcc,omitempty"`
	Subject        string         `json:"subject,omitempty"`
	Text           string         `json:"text,omitempty"`
	PlanHash       string         `json:"planHash,omitempty"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
}
type respondInput struct {
	ConnectionRef  string `json:"connectionRef"`
	EventID        string `json:"eventId"`
	Response       string `json:"response"`
	PlanHash       string `json:"planHash,omitempty"`
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Apply          bool   `json:"apply,omitempty"`
}

func New(resolver Resolver) (*Server, error) {
	if resolver == nil {
		return nil, errors.New("binding resolver is required")
	}
	return &Server{resolver: resolver, mailWrites: mail.NewMemoryIdempotencyStore(), responseWrites: newMemoryResponseStore(), now: time.Now}, nil
}

func (s *Server) MCP() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "stalwart-agent-tools", Version: "v0.1.0"}, nil)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	write := &mcp.ToolAnnotations{IdempotentHint: true}
	mcp.AddTool(server, &mcp.Tool{Name: "collaboration_capabilities", Description: "Verify an opaque Stalwart binding and report granted user capabilities without returning credentials.", Annotations: readOnly}, s.capabilities)
	mcp.AddTool(server, &mcp.Tool{Name: "mail_query", Description: "List mailboxes or search bounded mail summaries for the bound user.", Annotations: readOnly}, s.mailQuery)
	mcp.AddTool(server, &mcp.Tool{Name: "mail_read", Description: "Read one bounded message for the bound user.", Annotations: readOnly}, s.mailRead)
	mcp.AddTool(server, &mcp.Tool{Name: "mail_send", Description: "Preview or idempotently send mail as a bound identity with expected-state and read-back verification.", Annotations: write}, s.mailSend)
	mcp.AddTool(server, &mcp.Tool{Name: "calendar_query", Description: "List calendars, query a bounded window, or read one event.", Annotations: readOnly}, s.calendarQuery)
	mcp.AddTool(server, &mcp.Tool{Name: "calendar_respond", Description: "Preview or idempotently respond to an invitation owned by the bound attendee.", Annotations: write}, s.calendarRespond)
	mcp.AddTool(server, &mcp.Tool{Name: "contacts_query", Description: "List address books, search contacts, or read one contact.", Annotations: readOnly}, s.contactsQuery)
	mcp.AddTool(server, &mcp.Tool{Name: "files_list", Description: "List one bounded authorized WebDAV collection.", Annotations: readOnly}, s.filesList)
	mcp.AddTool(server, &mcp.Tool{Name: "files_read", Description: "Read one bounded UTF-8 file from an authorized WebDAV collection.", Annotations: readOnly}, s.filesRead)
	return server
}

func (s *Server) capabilities(ctx context.Context, _ *mcp.CallToolRequest, in connectionInput) (*mcp.CallToolResult, any, error) {
	binding, err := s.resolver.Resolve(ctx, in.ConnectionRef)
	if err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"connectionRef": binding.ConnectionRef, "scopes": binding.Scopes, "expiresAt": binding.ExpiresAt, "providerVerified": true}, nil
}

func (s *Server) mailQuery(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "mail.read")
	if err != nil {
		return nil, nil, err
	}
	service, _ := mail.New(client, binding.AccountID)
	switch in.Action {
	case "list":
		items, state, callErr := service.Mailboxes(ctx)
		return nil, map[string]any{"items": items, "state": state, "untrustedContent": true}, callErr
	case "", "search":
		result, callErr := service.Query(ctx, mail.QueryOptions{Text: in.Text, MailboxID: in.MailboxID, Position: in.Position, Limit: boundedLimit(in.Limit)})
		return nil, result, callErr
	default:
		return nil, nil, errors.New("unsupported mail query action")
	}
}

func (s *Server) mailRead(ctx context.Context, _ *mcp.CallToolRequest, in readMailInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "mail.read")
	if err != nil {
		return nil, nil, err
	}
	service, _ := mail.New(client, binding.AccountID)
	value, err := service.Read(ctx, in.EmailID)
	return nil, map[string]any{"item": value, "untrustedContent": true}, err
}

func (s *Server) mailSend(ctx context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "mail.send")
	if err != nil {
		return nil, nil, err
	}
	service, _ := mail.New(client, binding.AccountID)
	if !in.Apply {
		plan, planErr := service.PreviewSend(ctx, mail.Draft{From: in.From, To: in.To, CC: in.CC, BCC: in.BCC, Subject: in.Subject, Text: in.Text})
		if planErr != nil {
			return nil, nil, planErr
		}
		s.mailPlans.Store(planKey(in.ConnectionRef, plan.Fingerprint), planEntry[mail.SendPlan]{Plan: plan, ExpiresAt: plan.ExpiresAt})
		return nil, map[string]any{"planHash": plan.Fingerprint, "expiresAt": plan.ExpiresAt, "from": plan.Draft.From, "to": plan.Draft.To, "cc": plan.Draft.CC, "bcc": plan.Draft.BCC, "subject": plan.Draft.Subject, "bodyBytes": len(plan.Draft.Text), "decision": "preview"}, nil
	}
	entry, ok := loadPlan[mail.SendPlan](&s.mailPlans, planKey(in.ConnectionRef, in.PlanHash), s.now())
	if !ok {
		return nil, nil, errors.New("matching unexpired mail send plan is required")
	}
	result, err := service.Send(ctx, entry.Plan, in.IdempotencyKey, s.mailWrites)
	return nil, result, err
}

func (s *Server) calendarQuery(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "calendar.read")
	if err != nil {
		return nil, nil, err
	}
	service, _ := groupware.New(client, binding.AccountID)
	switch in.Action {
	case "list":
		value, e := service.Calendars(ctx)
		return nil, map[string]any{"items": value, "untrustedContent": true}, e
	case "get":
		value, state, e := service.ReadEvent(ctx, in.EventID, in.TimeZone)
		return nil, map[string]any{"item": value, "state": state, "untrustedContent": true}, e
	case "", "search":
		after, e1 := time.Parse(time.RFC3339, in.After)
		before, e2 := time.Parse(time.RFC3339, in.Before)
		if e1 != nil || e2 != nil {
			return nil, nil, errors.New("RFC3339 calendar window required")
		}
		value, e := service.QueryCalendar(ctx, groupware.CalendarWindow{After: after, Before: before, TimeZone: in.TimeZone, Page: groupware.Page{Position: in.Position, Limit: boundedLimit(in.Limit)}})
		return nil, value, e
	default:
		return nil, nil, errors.New("unsupported calendar query action")
	}
}

func (s *Server) calendarRespond(ctx context.Context, _ *mcp.CallToolRequest, in respondInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "calendar.read", "calendar.respond")
	if err != nil {
		return nil, nil, err
	}
	service, _ := groupware.New(client, binding.AccountID)
	if !in.Apply {
		plan, e := service.PreviewResponse(ctx, in.EventID, in.Response, s.now())
		if e != nil {
			return nil, nil, e
		}
		s.responsePlans.Store(planKey(in.ConnectionRef, plan.Fingerprint), planEntry[groupware.ResponsePlan]{Plan: plan, ExpiresAt: plan.ExpiresAt})
		return nil, map[string]any{"eventId": plan.EventID, "response": plan.Response, "currentResponse": plan.CurrentResponse, "expectedState": plan.ExpectedState, "planHash": plan.Fingerprint, "expiresAt": plan.ExpiresAt, "sendsSchedulingMessage": plan.CurrentResponse != plan.Response, "decision": "preview"}, nil
	}
	entry, ok := loadPlan[groupware.ResponsePlan](&s.responsePlans, planKey(in.ConnectionRef, in.PlanHash), s.now())
	if !ok {
		return nil, nil, errors.New("matching unexpired calendar response plan is required")
	}
	result, e := service.Respond(ctx, entry.Plan, in.IdempotencyKey, s.now(), s.responseWrites)
	return nil, result, e
}

func (s *Server) contactsQuery(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, any, error) {
	binding, client, err := s.jmap(ctx, in.ConnectionRef, "contacts.read")
	if err != nil {
		return nil, nil, err
	}
	service, _ := groupware.New(client, binding.AccountID)
	switch in.Action {
	case "list":
		value, e := service.AddressBooks(ctx)
		return nil, map[string]any{"items": value, "untrustedContent": true}, e
	case "get":
		value, state, e := service.ReadContact(ctx, in.ContactID)
		return nil, map[string]any{"item": value, "state": state, "untrustedContent": true}, e
	case "", "search":
		value, e := service.QueryContacts(ctx, groupware.ContactFilter{Text: in.Text, AddressBookID: in.AddressBookID, Page: groupware.Page{Position: in.Position, Limit: boundedLimit(in.Limit)}})
		return nil, value, e
	default:
		return nil, nil, errors.New("unsupported contacts query action")
	}
}

func (s *Server) filesList(ctx context.Context, _ *mcp.CallToolRequest, in fileInput) (*mcp.CallToolResult, any, error) {
	binding, err := s.resolver.Resolve(ctx, in.ConnectionRef, "files.read")
	if err != nil {
		return nil, nil, err
	}
	client, err := files.New(files.Config{Origin: binding.Origin, BearerToken: binding.BearerToken, Roots: binding.FileRoots})
	if err != nil {
		return nil, nil, err
	}
	if in.Href == "" {
		items := client.Roots()
		if len(items) == 0 {
			items, err = client.DiscoverRoots(ctx)
		}
		return nil, map[string]any{"items": items, "untrustedContent": true}, err
	}
	value, err := client.List(ctx, in.Href)
	return nil, value, err
}
func (s *Server) filesRead(ctx context.Context, _ *mcp.CallToolRequest, in fileInput) (*mcp.CallToolResult, any, error) {
	binding, err := s.resolver.Resolve(ctx, in.ConnectionRef, "files.read")
	if err != nil {
		return nil, nil, err
	}
	client, err := files.New(files.Config{Origin: binding.Origin, BearerToken: binding.BearerToken, Roots: binding.FileRoots})
	if err != nil {
		return nil, nil, err
	}
	value, err := client.Read(ctx, in.Href)
	return nil, value, err
}

func (s *Server) jmap(ctx context.Context, ref string, scopes ...string) (broker.Binding, *jmap.Client, error) {
	binding, err := s.resolver.Resolve(ctx, ref, scopes...)
	if err != nil {
		return broker.Binding{}, nil, err
	}
	allowed := map[string][]string{mail.Capability: {"Mailbox/get", "Email/query", "Email/get", "Identity/get", "Email/set", "EmailSubmission/set", "EmailSubmission/get"}, groupware.CalendarCapability: {"Calendar/get", "CalendarEvent/query", "CalendarEvent/get", "ParticipantIdentity/get", "CalendarEvent/set"}, groupware.ContactsCapability: {"AddressBook/get", "ContactCard/query", "ContactCard/get"}}
	client, err := jmap.New(jmap.Config{Origin: binding.Origin, BearerToken: binding.BearerToken, AllowedMethods: allowed})
	if err != nil {
		return broker.Binding{}, nil, err
	}
	_, err = client.Discover(ctx)
	return binding, client, err
}
func boundedLimit(value uint64) uint64 {
	if value == 0 {
		return 50
	}
	return value
}
func planKey(connectionRef, hash string) string { return connectionRef + ":" + hash }
func loadPlan[T any](store *sync.Map, key string, now time.Time) (planEntry[T], bool) {
	raw, ok := store.Load(key)
	if !ok {
		return planEntry[T]{}, false
	}
	entry, ok := raw.(planEntry[T])
	if !ok || now.After(entry.ExpiresAt) {
		store.Delete(key)
		return planEntry[T]{}, false
	}
	return entry, true
}
