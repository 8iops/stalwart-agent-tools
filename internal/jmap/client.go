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
	"strings"
	"time"
)

const (
	defaultMaxRequestBytes  = 1 << 20
	defaultMaxResponseBytes = 4 << 20
)

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
	Type        string
	Description string
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
	if c.session == nil {
		return nil, errors.New("JMAP session discovery is required")
	}
	if _, ok := c.session.Capabilities[capability]; !ok {
		return nil, fmt.Errorf("provider capability %q is unavailable", capability)
	}
	methods, ok := c.allowedMethods[capability]
	if !ok || !methods[method] {
		return nil, fmt.Errorf("provider method %q is not registered for capability", method)
	}
	payload, err := json.Marshal(callRequest{
		Using:       []string{"urn:ietf:params:jmap:core", capability},
		MethodCalls: []any{[]any{method, arguments, "c1"}},
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
	if err = json.Unmarshal(body, &response); err != nil || len(response.MethodResponses) != 1 {
		return nil, errors.New("provider returned an invalid JMAP response")
	}
	result, err := parseMethodResponse(response.MethodResponses[0])
	if err != nil {
		return nil, err
	}
	if result.CallID != "c1" {
		return nil, errors.New("provider returned an unexpected JMAP call id")
	}
	if result.Name == "error" {
		var providerError struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		if json.Unmarshal(result.Result, &providerError) != nil || providerError.Type == "" {
			return nil, errors.New("provider returned an invalid JMAP method error")
		}
		return nil, &ProtocolError{Type: providerError.Type, Description: providerError.Description}
	}
	if result.Name != method {
		return nil, errors.New("provider returned an unexpected JMAP method")
	}
	return result.Result, nil
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
