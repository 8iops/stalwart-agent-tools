package groupware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CalendarCapability = "urn:ietf:params:jmap:calendars"
	ContactsCapability = "urn:ietf:params:jmap:contacts"
	maxPage            = 50
)

type Caller interface {
	Call(context.Context, string, string, any) (json.RawMessage, error)
}

type Service struct {
	caller    Caller
	accountID string
}

type Page struct {
	Position uint64
	Limit    uint64
}

type PageResult[T any] struct {
	Items        []T
	Position     uint64
	NextPosition uint64
	MayHaveMore  bool
	State        string
}

type Rights struct {
	MayReadItems   bool `json:"mayReadItems"`
	MayAddItems    bool `json:"mayAddItems"`
	MayModifyItems bool `json:"mayModifyItems"`
	MayRemoveItems bool `json:"mayRemoveItems"`
	MayRename      bool `json:"mayRename"`
	MayDelete      bool `json:"mayDelete"`
	MayRSVP        bool `json:"mayRSVP"`
}

type Calendar struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	TimeZone  string `json:"timeZone"`
	IsDefault bool   `json:"isDefault"`
	Rights    Rights `json:"myRights"`
}

type CalendarWindow struct {
	After    time.Time
	Before   time.Time
	TimeZone string
	Page     Page
}

type Event struct {
	ID           string                 `json:"id"`
	UID          string                 `json:"uid"`
	CalendarIDs  map[string]bool        `json:"calendarIds"`
	Title        string                 `json:"title"`
	Description  string                 `json:"description"`
	Start        string                 `json:"start"`
	Duration     string                 `json:"duration"`
	TimeZone     string                 `json:"timeZone"`
	UTCStart     string                 `json:"utcStart"`
	UTCEnd       string                 `json:"utcEnd"`
	RecurrenceID string                 `json:"recurrenceId"`
	Participants map[string]Participant `json:"participants"`
	IsOrigin     bool                   `json:"isOrigin"`
	IsDraft      bool                   `json:"isDraft"`
}

type Participant struct {
	Name                string            `json:"name"`
	CalendarAddress     string            `json:"calendarAddress"`
	SendTo              map[string]string `json:"sendTo"`
	Roles               map[string]bool   `json:"roles"`
	ParticipationStatus string            `json:"participationStatus"`
	ExpectReply         bool              `json:"expectReply"`
}

type AddressBook struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	SortOrder    uint64 `json:"sortOrder"`
	IsDefault    bool   `json:"isDefault"`
	IsSubscribed bool   `json:"isSubscribed"`
	Rights       Rights `json:"myRights"`
}

type Contact struct {
	ID             string          `json:"id"`
	UID            string          `json:"uid"`
	AddressBookIDs map[string]bool `json:"addressBookIds"`
	Kind           string          `json:"kind"`
	Name           json.RawMessage `json:"name"`
	Emails         json.RawMessage `json:"emails"`
	Phones         json.RawMessage `json:"phones"`
	OnlineServices json.RawMessage `json:"onlineServices"`
	Organizations  json.RawMessage `json:"organizations"`
	Titles         json.RawMessage `json:"titles"`
	Addresses      json.RawMessage `json:"addresses"`
	Notes          json.RawMessage `json:"notes"`
	Members        json.RawMessage `json:"members"`
	Created        string          `json:"created"`
	Updated        string          `json:"updated"`
}

type ContactFilter struct {
	Text          string
	AddressBookID string
	Page          Page
}

type queryResponse struct {
	AccountID string   `json:"accountId"`
	Position  uint64   `json:"position"`
	IDs       []string `json:"ids"`
}

type getResponse[T any] struct {
	AccountID string   `json:"accountId"`
	State     string   `json:"state"`
	List      []T      `json:"list"`
	NotFound  []string `json:"notFound"`
}

func New(caller Caller, accountID string) (*Service, error) {
	if caller == nil || !validID(accountID) {
		return nil, errors.New("bound groupware account and provider caller are required")
	}
	return &Service{caller: caller, accountID: accountID}, nil
}

func (s *Service) Calendars(ctx context.Context) ([]Calendar, error) {
	var result getResponse[Calendar]
	err := s.call(ctx, CalendarCapability, "Calendar/get", map[string]any{"accountId": s.accountID, "properties": []string{"id", "name", "color", "timeZone", "isDefault", "myRights"}}, &result)
	if err != nil || result.AccountID != s.accountID || len(result.NotFound) != 0 || len(result.List) > maxPage {
		return nil, invalidProvider("calendars", err)
	}
	if err = uniqueIDs(result.List, func(v Calendar) string { return v.ID }); err != nil {
		return nil, err
	}
	return result.List, nil
}

func (s *Service) QueryCalendar(ctx context.Context, window CalendarWindow) (PageResult[Event], error) {
	if err := validPage(window.Page); err != nil {
		return PageResult[Event]{}, err
	}
	if window.After.IsZero() || !window.Before.After(window.After) || window.Before.Sub(window.After) > 31*24*time.Hour {
		return PageResult[Event]{}, errors.New("calendar window must be positive and at most 31 days")
	}
	loc, err := time.LoadLocation(window.TimeZone)
	if err != nil {
		return PageResult[Event]{}, errors.New("valid IANA time zone is required")
	}
	var query queryResponse
	err = s.call(ctx, CalendarCapability, "CalendarEvent/query", map[string]any{
		"accountId": s.accountID, "filter": map[string]any{"after": window.After.In(loc).Format("2006-01-02T15:04:05"), "before": window.Before.In(loc).Format("2006-01-02T15:04:05")},
		"timeZone": window.TimeZone, "expandRecurrences": true, "position": window.Page.Position, "limit": window.Page.Limit,
	}, &query)
	if err != nil || query.AccountID != s.accountID || len(query.IDs) > int(window.Page.Limit) {
		return PageResult[Event]{}, invalidProvider("calendar query", err)
	}
	return s.getEvents(ctx, query.IDs, window.TimeZone, query.Position, window.Page.Limit)
}

func (s *Service) ReadEvent(ctx context.Context, id, timeZone string) (Event, string, error) {
	if !validID(id) {
		return Event{}, "", errors.New("valid event id is required")
	}
	if _, err := time.LoadLocation(timeZone); err != nil {
		return Event{}, "", errors.New("valid IANA time zone is required")
	}
	result, err := s.getEvents(ctx, []string{id}, timeZone, 0, 1)
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != id {
		return Event{}, "", invalidProvider("calendar event", err)
	}
	return result.Items[0], result.State, nil
}

func (s *Service) getEvents(ctx context.Context, ids []string, timeZone string, position, limit uint64) (PageResult[Event], error) {
	if len(ids) == 0 {
		return PageResult[Event]{Items: []Event{}, Position: position, NextPosition: position}, nil
	}
	var result getResponse[Event]
	err := s.call(ctx, CalendarCapability, "CalendarEvent/get", map[string]any{"accountId": s.accountID, "ids": ids, "properties": []string{"id", "uid", "calendarIds", "title", "description", "start", "duration", "timeZone", "utcStart", "utcEnd", "recurrenceId", "participants", "isOrigin", "isDraft"}, "timeZone": timeZone}, &result)
	if err != nil || result.AccountID != s.accountID || len(result.NotFound) != 0 || len(result.List) != len(ids) {
		return PageResult[Event]{}, invalidProvider("calendar events", err)
	}
	if err = requestedIDs(ids, result.List, func(v Event) string { return v.ID }); err != nil {
		return PageResult[Event]{}, err
	}
	return PageResult[Event]{Items: result.List, Position: position, NextPosition: position + uint64(len(ids)), MayHaveMore: uint64(len(ids)) == limit, State: result.State}, nil
}

func (s *Service) AddressBooks(ctx context.Context) ([]AddressBook, error) {
	var result getResponse[AddressBook]
	err := s.call(ctx, ContactsCapability, "AddressBook/get", map[string]any{"accountId": s.accountID, "properties": []string{"id", "name", "description", "sortOrder", "isDefault", "isSubscribed", "myRights"}}, &result)
	if err != nil || result.AccountID != s.accountID || len(result.NotFound) != 0 || len(result.List) > maxPage {
		return nil, invalidProvider("address books", err)
	}
	if err = uniqueIDs(result.List, func(v AddressBook) string { return v.ID }); err != nil {
		return nil, err
	}
	return result.List, nil
}

func (s *Service) QueryContacts(ctx context.Context, filter ContactFilter) (PageResult[Contact], error) {
	if err := validPage(filter.Page); err != nil {
		return PageResult[Contact]{}, err
	}
	if len(filter.Text) > 2048 || (filter.AddressBookID != "" && !validID(filter.AddressBookID)) {
		return PageResult[Contact]{}, errors.New("invalid contact filter")
	}
	providerFilter := map[string]any{}
	if filter.Text != "" {
		providerFilter["text"] = filter.Text
	}
	if filter.AddressBookID != "" {
		providerFilter["inAddressBook"] = filter.AddressBookID
	}
	var query queryResponse
	err := s.call(ctx, ContactsCapability, "ContactCard/query", map[string]any{"accountId": s.accountID, "filter": providerFilter, "position": filter.Page.Position, "limit": filter.Page.Limit, "sort": []map[string]any{{"property": "updated", "isAscending": false}}}, &query)
	if err != nil || query.AccountID != s.accountID || len(query.IDs) > int(filter.Page.Limit) {
		return PageResult[Contact]{}, invalidProvider("contact query", err)
	}
	return s.getContacts(ctx, query.IDs, false, query.Position, filter.Page.Limit)
}

func (s *Service) ReadContact(ctx context.Context, id string) (Contact, string, error) {
	if !validID(id) {
		return Contact{}, "", errors.New("valid contact id is required")
	}
	result, err := s.getContacts(ctx, []string{id}, true, 0, 1)
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != id {
		return Contact{}, "", invalidProvider("contact", err)
	}
	return result.Items[0], result.State, nil
}

func (s *Service) getContacts(ctx context.Context, ids []string, detail bool, position, limit uint64) (PageResult[Contact], error) {
	if len(ids) == 0 {
		return PageResult[Contact]{Items: []Contact{}, Position: position, NextPosition: position}, nil
	}
	properties := []string{"id", "uid", "addressBookIds", "kind", "name", "emails", "phones", "organizations", "titles", "updated"}
	if detail {
		properties = append(properties, "onlineServices", "addresses", "notes", "members", "created")
	}
	var result getResponse[Contact]
	err := s.call(ctx, ContactsCapability, "ContactCard/get", map[string]any{"accountId": s.accountID, "ids": ids, "properties": properties}, &result)
	if err != nil || result.AccountID != s.accountID || len(result.NotFound) != 0 || len(result.List) != len(ids) {
		return PageResult[Contact]{}, invalidProvider("contacts", err)
	}
	if err = requestedIDs(ids, result.List, func(v Contact) string { return v.ID }); err != nil {
		return PageResult[Contact]{}, err
	}
	return PageResult[Contact]{Items: result.List, Position: position, NextPosition: position + uint64(len(ids)), MayHaveMore: uint64(len(ids)) == limit, State: result.State}, nil
}

func (s *Service) call(ctx context.Context, capability, method string, arguments any, target any) error {
	raw, err := s.caller.Call(ctx, capability, method, arguments)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, target) != nil {
		return errors.New("provider returned invalid JSON")
	}
	return nil
}

func validPage(page Page) error {
	if page.Position > 100000 || page.Limit == 0 || page.Limit > maxPage {
		return fmt.Errorf("position must be 0..100000 and limit 1..%d", maxPage)
	}
	return nil
}

func validID(id string) bool {
	return id != "" && len(id) <= 1024 && !strings.ContainsAny(id, "\r\n\x00")
}

func invalidProvider(operation string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("provider returned invalid %s data", operation)
}

func uniqueIDs[T any](values []T, id func(T) string) error {
	seen := map[string]bool{}
	for _, value := range values {
		key := id(value)
		if !validID(key) || seen[key] {
			return errors.New("provider returned invalid or duplicate ids")
		}
		seen[key] = true
	}
	return nil
}

func requestedIDs[T any](wanted []string, values []T, id func(T) string) error {
	allowed := map[string]bool{}
	for _, key := range wanted {
		if !validID(key) || allowed[key] {
			return errors.New("invalid requested ids")
		}
		allowed[key] = true
	}
	seen := map[string]bool{}
	for _, value := range values {
		key := id(value)
		if !allowed[key] || seen[key] {
			return errors.New("provider returned resources outside the requested selection")
		}
		seen[key] = true
	}
	return nil
}
