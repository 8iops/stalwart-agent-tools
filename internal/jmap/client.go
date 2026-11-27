package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	defaultMaxRequestBytes  = 1 << 20
	defaultMaxResponseBytes = 4 << 20
	maxBatchCalls           = 16
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Config struct {
	Origin           string
	BearerToken      string
	AllowedMethods   map[string][]string
	HTTPClient       *http.Client
	MaxRequestBytes  int64
	MaxResponseBytes int64
}

type Client struct {
	origin           *url.URL
	bearerToken      string
	allowedMethods   map[string]map[string]bool
	httpClient       *http.Client
	maxRequestBytes  int64
	maxResponseBytes int64
	session          *Session
}

type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]Account         `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
}

type Account struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

type callRequest struct {
	Using       []string `json:"using"`
	MethodCalls []any    `json:"methodCalls"`
}

type callResponse struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
}

type methodResponse struct {
	Name   string
	Result json.RawMessage
	CallID string
}

type ProtocolError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type BatchCall struct {
	ID               string
	Capability       string
	Method           string
	Arguments        any
	ResultReferences []ResultReference
}

type ResultReference struct {
	Argument string
	ResultOf string
	Name     string
	Path     string
}

type BatchResult struct {
	ID     string
	Method string
	Result json.RawMessage
	Error  *ProtocolError
}

func (e *ProtocolError) Error() string {
	if e.Description == "" {
		return "JMAP protocol error: " + e.Type
	}
	return "JMAP protocol error: " + e.Type + ": " + e.Description
}

func New(config Config) (*Client, error) {
	origin, err := url.Parse(config.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("reviewed HTTPS provider origin is required")
	}
	origin.Path = ""

	if config.BearerToken == "" || strings.ContainsAny(config.BearerToken, "\r\n") {
		return nil, errors.New("provider bearer credential is required")
	}
	allowed := map[string]map[string]bool{}
	for capability, methods := range config.AllowedMethods {
		if capability == "" || len(methods) == 0 {
			return nil, errors.New("capability method allowlist is required")
		}
		allowed[capability] = map[string]bool{}
		for _, method := range methods {
			if method == "" || method == "*" || strings.ContainsAny(method, "\r\n") {
				return nil, errors.New("invalid provider method allowlist")
			}
			allowed[capability][method] = true
		}
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	} else {
		copy := *httpClient
		httpClient = &copy
	}
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 20 * time.Second
	}

	maxRequest := config.MaxRequestBytes
	if maxRequest == 0 {
		maxRequest = defaultMaxRequestBytes
	}
	maxResponse := config.MaxResponseBytes
	if maxResponse == 0 {
		maxResponse = defaultMaxResponseBytes
	}
	if maxRequest < 1 || maxResponse < 1 {
		return nil, errors.New("positive request and response limits are required")
	}

	return &Client{
		origin:           origin,
		bearerToken:      config.BearerToken,
		allowedMethods:   allowed,
		httpClient:       httpClient,
		maxRequestBytes:  maxRequest,
		maxResponseBytes: maxResponse,
	}, nil
}

func (c *Client) Discover(ctx context.Context) (Session, error) {
	target := *c.origin
	target.Path = strings.TrimSuffix(c.origin.Path, "/") + "/jmap/session"
	body, _, err := c.request(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Session{}, fmt.Errorf("discover JMAP session: %w", err)
	}
	var session Session
	if err = json.Unmarshal(body, &session); err != nil {
		return Session{}, errors.New("provider returned an invalid JMAP session")
	}
	if len(session.Capabilities) == 0 || len(session.Accounts) == 0 || session.APIURL == "" {
		return Session{}, errors.New("provider returned an incomplete JMAP session")
	}
	for _, endpoint := range []string{session.APIURL, session.UploadURL, session.DownloadURL, session.EventSourceURL} {
		if endpoint == "" {
			continue
		}
		if _, err = c.sameOrigin(endpoint); err != nil {
			return Session{}, errors.New("provider session endpoint is outside the reviewed origin")
		}
	}
	c.session = &session
	return session, nil
}

func (c *Client) Call(ctx context.Context, capability, method string, arguments any) (json.RawMessage, error) {
	results, err := c.CallBatch(ctx, []BatchCall{{ID: "c1", Capability: capability, Method: method, Arguments: arguments}})
	if err != nil {
		return nil, err
	}
	if results[0].Error != nil {
		return nil, results[0].Error
	}
	return results[0].Result, nil
}

func (c *Client) CallBatch(ctx context.Context, calls []BatchCall) ([]BatchResult, error) {
	if c.session == nil {
		return nil, errors.New("JMAP session discovery is required")
	}
	if len(calls) == 0 || len(calls) > maxBatchCalls {
		return nil, fmt.Errorf("JMAP batch must contain between 1 and %d calls", maxBatchCalls)
	}
	using := []string{"urn:ietf:params:jmap:core"}
	seenCapabilities := map[string]bool{"urn:ietf:params:jmap:core": true}
	seenCalls := make(map[string]string, len(calls))
	methodCalls := make([]any, 0, len(calls))
	for _, call := range calls {
		if !identifierPattern.MatchString(call.ID) || seenCalls[call.ID] != "" {
			return nil, errors.New("JMAP batch call ids must be unique bounded identifiers")
		}
		if _, ok := c.session.Capabilities[call.Capability]; !ok {
			return nil, fmt.Errorf("provider capability %q is unavailable", call.Capability)
		}
		methods, ok := c.allowedMethods[call.Capability]
		if !ok || !methods[call.Method] {
			return nil, fmt.Errorf("provider method %q is not registered for capability", call.Method)
		}
		arguments, err := encodeArguments(call.Arguments, call.ResultReferences, seenCalls)
		if err != nil {
			return nil, fmt.Errorf("encode JMAP call %q: %w", call.ID, err)
		}
		seenCalls[call.ID] = call.Method
		if !seenCapabilities[call.Capability] {
			using = append(using, call.Capability)
			seenCapabilities[call.Capability] = true
		}
		methodCalls = append(methodCalls, []any{call.Method, arguments, call.ID})
	}
	payload, err := json.Marshal(callRequest{
		Using:       using,
		MethodCalls: methodCalls,
	})
	if err != nil {
		return nil, errors.New("encode typed JMAP request")
	}
	if int64(len(payload)) > c.maxRequestBytes {
		return nil, errors.New("typed JMAP request exceeds configured limit")
	}
	body, _, err := c.request(ctx, http.MethodPost, c.session.APIURL, payload)
	if err != nil {
		return nil, fmt.Errorf("call JMAP provider: %w", err)
	}
	var response callResponse
	if err = json.Unmarshal(body, &response); err != nil || len(response.MethodResponses) != len(calls) {
		return nil, errors.New("provider returned an invalid JMAP response")
	}
	results := make([]BatchResult, 0, len(calls))
	responded := make(map[string]bool, len(calls))
	for index, raw := range response.MethodResponses {
		response, parseErr := parseMethodResponse(raw)
		if parseErr != nil {
			return nil, parseErr
		}
		call := calls[index]
		if response.CallID != call.ID || responded[response.CallID] {
			return nil, errors.New("provider returned an unexpected JMAP call order or id")
		}
		responded[response.CallID] = true
		result := BatchResult{ID: call.ID, Method: call.Method, Result: response.Result}
		if response.Name == "error" {
			var providerError ProtocolError
			if json.Unmarshal(response.Result, &providerError) != nil || providerError.Type == "" {
				return nil, errors.New("provider returned an invalid JMAP method error")
			}
			result.Result = nil
			result.Error = &providerError
		} else if response.Name != call.Method {
			return nil, errors.New("provider returned an unexpected JMAP method")
		}
		results = append(results, result)
	}
	return results, nil
}

func encodeArguments(arguments any, references []ResultReference, priorCalls map[string]string) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, errors.New("encode typed JMAP request")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(encoded, &object) != nil || object == nil {
		return nil, errors.New("JMAP arguments must encode as an object")
	}
	for _, reference := range references {
		method, exists := priorCalls[reference.ResultOf]
		if !identifierPattern.MatchString(reference.Argument) || !exists || reference.Name != method || !validResultPath(reference.Path) {
			return nil, errors.New("invalid JMAP result reference")
		}
		key := "#" + reference.Argument
		if _, exists = object[key]; exists {
			return nil, errors.New("duplicate JMAP result reference argument")
		}
		value, marshalErr := json.Marshal(map[string]string{"resultOf": reference.ResultOf, "name": reference.Name, "path": reference.Path})
		if marshalErr != nil {
			return nil, errors.New("encode JMAP result reference")
		}
		object[key] = value
	}
	return object, nil
}

func validResultPath(path string) bool {
	if len(path) < 2 || len(path) > 512 || path[0] != '/' || strings.ContainsAny(path, "\r\n") {
		return false
	}
	segments := strings.Split(path[1:], "/")
	return !slices.Contains(segments, "")
}

func parseMethodResponse(raw json.RawMessage) (methodResponse, error) {
	var tuple []json.RawMessage
	if json.Unmarshal(raw, &tuple) != nil || len(tuple) != 3 {
		return methodResponse{}, errors.New("provider returned an invalid JMAP method response")
	}
	var result methodResponse
	if json.Unmarshal(tuple[0], &result.Name) != nil || json.Unmarshal(tuple[2], &result.CallID) != nil {
		return methodResponse{}, errors.New("provider returned an invalid JMAP method response")
	}
	result.Result = tuple[1]
	return result, nil
}

func (c *Client) request(ctx context.Context, method, target string, payload []byte) ([]byte, http.Header, error) {
	if _, err := c.sameOrigin(target); err != nil {
		return nil, nil, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, errors.New("build provider request")
	}
	req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, errors.New("provider request failed")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, nil, errors.New("provider redirect is forbidden")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return nil, nil, errors.New("read provider response")
	}
	if int64(len(data)) > c.maxResponseBytes {
		return nil, nil, errors.New("provider response exceeds configured limit")
	}
	return data, response.Header.Clone(), nil
}

func (c *Client) sameOrigin(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("invalid provider endpoint")
	}
	if !strings.EqualFold(target.Scheme, c.origin.Scheme) || !strings.EqualFold(target.Host, c.origin.Host) {
		return nil, errors.New("provider endpoint is outside the reviewed origin")
	}
	return target, nil
}
