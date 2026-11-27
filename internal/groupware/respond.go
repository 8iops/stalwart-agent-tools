package groupware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ResponsePlan struct {
	AccountID       string
	EventID         string
	ParticipantID   string
	Response        string
	CurrentResponse string
	ExpectedState   string
	ExpiresAt       time.Time
	Fingerprint     string
}

type ResponseResult struct {
	EventID          string
	Response         string
	Changed          bool
	State            string
	SchedulingState  string
	DeliveryVerified bool
}

type ResponseStore interface {
	Do(context.Context, string, string, func(context.Context) (ResponseResult, error)) (ResponseResult, error)
}

func (s *Service) PreviewResponse(ctx context.Context, eventID, response string, now time.Time) (ResponsePlan, error) {
	participant, current, state, err := s.responseTarget(ctx, eventID, response)
	if err != nil {
		return ResponsePlan{}, err
	}
	plan := ResponsePlan{AccountID: s.accountID, EventID: eventID, ParticipantID: participant, Response: response, CurrentResponse: current, ExpectedState: state, ExpiresAt: now.UTC().Add(10 * time.Minute)}
	plan.Fingerprint, err = responseFingerprint(plan)
	return plan, err
}

func (s *Service) Respond(ctx context.Context, plan ResponsePlan, key string, now time.Time, store ResponseStore) (ResponseResult, error) {
	if store == nil || plan.AccountID != s.accountID || now.UTC().After(plan.ExpiresAt) || !validMutationKey(key) {
		return ResponseResult{}, errors.New("valid unexpired response plan, idempotency key and store are required")
	}
	wanted, err := responseFingerprint(plan)
	if err != nil || wanted != plan.Fingerprint {
		return ResponseResult{}, errors.New("calendar response plan fingerprint is invalid")
	}
	return store.Do(ctx, key, plan.Fingerprint, func(ctx context.Context) (ResponseResult, error) {
		participant, current, state, err := s.responseTarget(ctx, plan.EventID, plan.Response)
		if err != nil {
			return ResponseResult{}, err
		}
		if participant != plan.ParticipantID || state != plan.ExpectedState {
			return ResponseResult{}, errors.New("calendar event changed; refresh response plan")
		}
		if current == plan.Response {
			return ResponseResult{EventID: plan.EventID, Response: plan.Response, SchedulingState: "unchanged"}, nil
		}
		escaped := strings.ReplaceAll(strings.ReplaceAll(participant, "~", "~0"), "/", "~1")
		arguments := map[string]any{"accountId": s.accountID, "ifInState": plan.ExpectedState, "update": map[string]any{plan.EventID: map[string]any{"participants/" + escaped + "/participationStatus": plan.Response, "participants/" + escaped + "/expectReply": false}}, "sendSchedulingMessages": true}
		var result struct {
			AccountID, NewState string
			Updated             map[string]json.RawMessage `json:"updated"`
			NotUpdated          map[string]json.RawMessage `json:"notUpdated"`
		}
		if err = s.call(ctx, CalendarCapability, "CalendarEvent/set", arguments, &result); err != nil {
			return ResponseResult{}, errors.New("calendar response outcome unconfirmed; read state before retrying")
		}
		if result.AccountID != s.accountID || result.NewState == "" || len(result.NotUpdated) != 0 {
			return ResponseResult{}, errors.New("calendar response was not applied")
		}
		if _, ok := result.Updated[plan.EventID]; !ok {
			return ResponseResult{}, errors.New("calendar response was not applied")
		}
		verified, _, readErr := s.ReadEvent(ctx, plan.EventID, "Etc/UTC")
		if readErr != nil {
			return ResponseResult{}, errors.New("calendar response stored but verification unavailable; read state before retrying")
		}
		actual, ok := verified.Participants[participant]
		if !ok || actual.ParticipationStatus != plan.Response {
			return ResponseResult{}, errors.New("calendar response stored but readback differs; refresh event")
		}
		return ResponseResult{EventID: plan.EventID, Response: plan.Response, Changed: true, State: result.NewState, SchedulingState: "submitted", DeliveryVerified: false}, nil
	})
}

func (s *Service) responseTarget(ctx context.Context, eventID, response string) (string, string, string, error) {
	if response != "accepted" && response != "declined" && response != "tentative" {
		return "", "", "", errors.New("invalid participation status")
	}
	event, state, err := s.ReadEvent(ctx, eventID, "Etc/UTC")
	if err != nil {
		return "", "", "", err
	}
	if state == "" || event.IsDraft {
		return "", "", "", errors.New("event is not a versioned invitation")
	}
	var identities getResponse[struct {
		ID              string            `json:"id"`
		CalendarAddress string            `json:"calendarAddress"`
		SendTo          map[string]string `json:"sendTo"`
	}]
	if err = s.call(ctx, CalendarCapability, "ParticipantIdentity/get", map[string]any{"accountId": s.accountID}, &identities); err != nil || identities.AccountID != s.accountID {
		return "", "", "", invalidProvider("participant identities", err)
	}
	own := map[string]bool{}
	for _, identity := range identities.List {
		address := identity.CalendarAddress
		if address == "" {
			address = identity.SendTo["imip"]
		}
		if address != "" {
			own[address] = true
		}
	}
	participantID, current := "", ""
	for id, participant := range event.Participants {
		address := participant.CalendarAddress
		if address == "" {
			address = participant.SendTo["imip"]
		}
		if own[address] && participant.Roles["attendee"] && !participant.Roles["owner"] {
			if participantID != "" {
				return "", "", "", errors.New("ambiguous attendee identity")
			}
			participantID, current = id, participant.ParticipationStatus
		}
	}
	if participantID == "" {
		return "", "", "", errors.New("binding user is not an attendee")
	}
	ids := []string{}
	for id, present := range event.CalendarIDs {
		if present {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", "", "", errors.New("event calendar rights unavailable")
	}
	var calendars getResponse[Calendar]
	if err = s.call(ctx, CalendarCapability, "Calendar/get", map[string]any{"accountId": s.accountID, "ids": ids, "properties": []string{"id", "myRights"}}, &calendars); err != nil || calendars.AccountID != s.accountID || len(calendars.List) != len(ids) {
		return "", "", "", invalidProvider("calendar RSVP rights", err)
	}
	rights := map[string]bool{}
	for _, calendar := range calendars.List {
		rights[calendar.ID] = calendar.Rights.MayRSVP
	}
	for _, id := range ids {
		if !rights[id] {
			return "", "", "", errors.New("calendar RSVP permission required")
		}
	}
	return participantID, current, state, nil
}

func responseFingerprint(plan ResponsePlan) (string, error) {
	plan.Fingerprint = ""
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode response plan: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validMutationKey(key string) bool {
	return len(key) >= 16 && len(key) <= 128 && !strings.ContainsAny(key, "\r\n \t")
}
