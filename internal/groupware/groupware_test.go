package groupware

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeCaller struct {
	responses []json.RawMessage
	methods   []string
}

func (f *fakeCaller) Call(_ context.Context, _ string, method string, _ any) (json.RawMessage, error) {
	f.methods = append(f.methods, method)
	if len(f.responses) == 0 {
		return nil, errors.New("unexpected call")
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r, nil
}

func TestCalendarAndContactQueriesStayBoundedAndAccountBound(t *testing.T) {
	caller := &fakeCaller{responses: []json.RawMessage{
		json.RawMessage(`{"accountId":"account","position":0,"ids":["event" ]}`),
		json.RawMessage(`{"accountId":"account","state":"s1","list":[{"id":"event","title":"Meeting"}],"notFound":[]}`),
		json.RawMessage(`{"accountId":"account","position":0,"ids":["contact"]}`),
		json.RawMessage(`{"accountId":"account","state":"s2","list":[{"id":"contact","kind":"individual"}],"notFound":[]}`),
	}}
	service, _ := New(caller, "account")
	after := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	events, err := service.QueryCalendar(context.Background(), CalendarWindow{After: after, Before: after.Add(24 * time.Hour), TimeZone: "Asia/Shanghai", Page: Page{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	contacts, err := service.QueryContacts(context.Background(), ContactFilter{Text: "person", Page: Page{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 || events.Items[0].ID != "event" || len(contacts.Items) != 1 || contacts.Items[0].ID != "contact" {
		t.Fatalf("unexpected results: %#v %#v", events, contacts)
	}
	if len(caller.methods) != 4 || caller.methods[0] != "CalendarEvent/query" || caller.methods[2] != "ContactCard/query" {
		t.Fatalf("unexpected methods: %#v", caller.methods)
	}
}

func TestQueriesRejectCrossAccountAndUnboundedWindows(t *testing.T) {
	service, _ := New(&fakeCaller{responses: []json.RawMessage{json.RawMessage(`{"accountId":"other","ids":[]}`)}}, "account")
	now := time.Now()
	if _, err := service.QueryCalendar(context.Background(), CalendarWindow{After: now, Before: now.Add(32 * 24 * time.Hour), TimeZone: "Etc/UTC", Page: Page{Limit: 1}}); err == nil {
		t.Fatal("unbounded calendar window accepted")
	}
	if _, err := service.QueryContacts(context.Background(), ContactFilter{Page: Page{Limit: 1}}); err == nil {
		t.Fatal("cross-account contacts accepted")
	}
}

func TestCalendarResponsePreviewVerifiesAttendeeAndRights(t *testing.T) {
	caller := &fakeCaller{responses: []json.RawMessage{
		json.RawMessage(`{"accountId":"account","state":"state","list":[{"id":"event","calendarIds":{"calendar":true},"participants":{"participant":{"calendarAddress":"me@example.test","roles":{"attendee":true},"participationStatus":"needs-action"}}}],"notFound":[]}`),
		json.RawMessage(`{"accountId":"account","list":[{"id":"identity","calendarAddress":"me@example.test"}]}`),
		json.RawMessage(`{"accountId":"account","list":[{"id":"calendar","myRights":{"mayRSVP":true}}],"notFound":[]}`),
	}}
	service, _ := New(caller, "account")
	plan, err := service.PreviewResponse(context.Background(), "event", "accepted", time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.ParticipantID != "participant" || plan.ExpectedState != "state" || plan.Fingerprint == "" {
		t.Fatalf("unexpected response plan: %#v", plan)
	}
}

type directStore struct{}

func (directStore) Do(ctx context.Context, _, _ string, operation func(context.Context) (ResponseResult, error)) (ResponseResult, error) {
	return operation(ctx)
}

func TestCalendarResponseAppliesExpectedStateAndReadsBack(t *testing.T) {
	event := json.RawMessage(`{"accountId":"account","state":"state","list":[{"id":"event","calendarIds":{"calendar":true},"participants":{"participant":{"calendarAddress":"me@example.test","roles":{"attendee":true},"participationStatus":"needs-action"}}}],"notFound":[]}`)
	verified := json.RawMessage(`{"accountId":"account","state":"new-state","list":[{"id":"event","calendarIds":{"calendar":true},"participants":{"participant":{"calendarAddress":"me@example.test","roles":{"attendee":true},"participationStatus":"accepted"}}}],"notFound":[]}`)
	caller := &fakeCaller{responses: []json.RawMessage{
		event,
		json.RawMessage(`{"accountId":"account","list":[{"id":"identity","calendarAddress":"me@example.test"}]}`),
		json.RawMessage(`{"accountId":"account","list":[{"id":"calendar","myRights":{"mayRSVP":true}}]}`),
		json.RawMessage(`{"accountId":"account","newState":"new-state","updated":{"event":{}},"notUpdated":{}}`),
		verified,
	}}
	service, _ := New(caller, "account")
	now := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	plan := ResponsePlan{AccountID: "account", EventID: "event", ParticipantID: "participant", Response: "accepted", CurrentResponse: "needs-action", ExpectedState: "state", ExpiresAt: now.Add(time.Minute)}
	plan.Fingerprint, _ = responseFingerprint(plan)
	result, err := service.Respond(context.Background(), plan, "idempotency-key-0001", now, directStore{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.State != "new-state" || result.SchedulingState != "submitted" || result.DeliveryVerified {
		t.Fatalf("unexpected response result: %#v", result)
	}
	want := []string{"CalendarEvent/get", "ParticipantIdentity/get", "Calendar/get", "CalendarEvent/set", "CalendarEvent/get"}
	for i := range want {
		if caller.methods[i] != want[i] {
			t.Fatalf("unexpected method sequence: %#v", caller.methods)
		}
	}
}
