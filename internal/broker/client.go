package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxResponse = 64 << 10

type Client struct {
	endpoint          *url.URL
	workloadToken     string
	workloadTokenFile string
	http              *http.Client
}

func NewFromTokenFile(endpoint, tokenFile string, httpClient *http.Client) (*Client, error) {
	if !filepath.IsAbs(tokenFile) || strings.ContainsAny(tokenFile, "\x00\r\n") {
		return nil, errors.New("absolute projected workload token file is required")
	}
	client, err := newClient(endpoint, httpClient)
	if err != nil {
		return nil, err
	}
	client.workloadTokenFile = tokenFile
	if _, err = client.workloadCredential(); err != nil {
		return nil, err
	}
	return client, nil
}

type Binding struct {
	ConnectionRef string    `json:"connectionRef"`
	Origin        string    `json:"origin"`
	BearerToken   string    `json:"bearerToken"`
	AccountID     string    `json:"accountId"`
	Scopes        []string  `json:"scopes"`
	FileRoots     []string  `json:"fileRoots"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type resolveRequest struct {
	ConnectionRef  string   `json:"connectionRef"`
	RequiredScopes []string `json:"requiredScopes"`
}

type invocationContextKey struct{}

func WithInvocationToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, invocationContextKey{}, token)
}

func New(endpoint, workloadToken string, httpClient *http.Client) (*Client, error) {
	client, err := newClient(endpoint, httpClient)
	if err != nil {
		return nil, err
	}
	if workloadToken == "" || strings.ContainsAny(workloadToken, "\r\n") {
		return nil, errors.New("workload credential is required")
	}
	client.workloadToken = workloadToken
	return client, nil
}

func newClient(endpoint string, httpClient *http.Client) (*Client, error) {
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.Path != "/v1/collaboration/resolve" {
		return nil, errors.New("reviewed HTTPS broker endpoint is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	} else {
		copied := *httpClient
		httpClient = &copied
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if httpClient.Timeout == 0 {
		httpClient.Timeout = 10 * time.Second
	}
	return &Client{endpoint: target, http: httpClient}, nil
}

func (c *Client) Resolve(ctx context.Context, connectionRef string, requiredScopes ...string) (Binding, error) {
	if !validRef(connectionRef) || len(requiredScopes) > 8 {
		return Binding{}, errors.New("valid opaque binding and bounded required scopes are required")
	}
	seen := map[string]bool{}
	for _, scope := range requiredScopes {
		if !allowedScope(scope) || seen[scope] {
			return Binding{}, errors.New("invalid required collaboration scope")
		}
		seen[scope] = true
	}
	payload, _ := json.Marshal(resolveRequest{ConnectionRef: connectionRef, RequiredScopes: requiredScopes})
	invocation, _ := ctx.Value(invocationContextKey{}).(string)
	if invocation == "" || len(invocation) > 8192 || strings.ContainsAny(invocation, "\r\n") {
		return Binding{}, errors.New("verified invocation token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return Binding{}, errors.New("build binding broker request")
	}
	workloadToken, err := c.workloadCredential()
	if err != nil {
		return Binding{}, err
	}
	request.Header.Set("Authorization", "Bearer "+workloadToken)
	request.Header.Set("X-RE8CH-Invocation", "Bearer "+invocation)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Binding{}, errors.New("binding broker unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Binding{}, errors.New("binding broker rejected resolution")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return Binding{}, errors.New("invalid binding broker response")
	}
	var binding Binding
	if json.Unmarshal(raw, &binding) != nil || binding.ConnectionRef != connectionRef || binding.Origin == "" || binding.BearerToken == "" || binding.AccountID == "" || !binding.ExpiresAt.After(time.Now()) {
		return Binding{}, errors.New("binding broker returned incomplete or expired binding")
	}
	granted := map[string]bool{}
	for _, scope := range binding.Scopes {
		if !allowedScope(scope) {
			return Binding{}, errors.New("binding broker returned unsupported scope")
		}
		granted[scope] = true
	}
	for _, scope := range requiredScopes {
		if !granted[scope] {
			return Binding{}, errors.New("binding broker omitted required scope")
		}
	}
	return binding, nil
}

func (c *Client) workloadCredential() (string, error) {
	if c.workloadTokenFile == "" {
		return c.workloadToken, nil
	}
	raw, err := os.ReadFile(c.workloadTokenFile)
	if err != nil || len(raw) == 0 || len(raw) > 16384 {
		return "", errors.New("projected workload credential is unavailable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("projected workload credential is invalid")
	}
	return token, nil
}

func validRef(ref string) bool {
	return strings.HasPrefix(ref, "collaboration://") && len(ref) <= 256 && !strings.ContainsAny(ref, "\r\n\x00 ")
}

func allowedScope(scope string) bool {
	switch scope {
	case "mail.read", "mail.send", "calendar.read", "calendar.respond", "contacts.read", "files.read":
		return true
	default:
		return false
	}
}
