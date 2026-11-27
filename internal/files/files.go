package files

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxDAVResponse = 1 << 20
	maxDownload    = 5 << 20
	maxInlineText  = 256 << 10
	maxEntries     = 50
)

type Config struct {
	Origin      string
	BearerToken string
	Roots       []string
	HTTPClient  *http.Client
}

type Client struct {
	origin *url.URL
	token  string
	roots  []*url.URL
	http   *http.Client
}

type Entry struct {
	Href         string
	Name         string
	Size         int64
	ContentType  string
	ETag         string
	IsCollection bool
}

type ListResult struct {
	Items     []Entry
	Partial   bool
	Truncated bool
}

type ReadResult struct {
	Text        string
	ContentType string
	Bytes       int
	ETag        string
	Truncated   bool
}

type davResponse struct {
	Href   string `xml:"DAV: href"`
	Status string `xml:"DAV: status"`
	Props  []struct {
		Status string `xml:"DAV: status"`
		Prop   struct {
			Name         string `xml:"DAV: displayname"`
			Size         int64  `xml:"DAV: getcontentlength"`
			Type         string `xml:"DAV: getcontenttype"`
			ETag         string `xml:"DAV: getetag"`
			ResourceType struct {
				Collection *struct{} `xml:"DAV: collection"`
			} `xml:"DAV: resourcetype"`
		} `xml:"DAV: prop"`
	} `xml:"DAV: propstat"`
}

func New(config Config) (*Client, error) {
	origin, err := url.Parse(config.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return nil, errors.New("reviewed HTTPS file origin is required")
	}
	if config.BearerToken == "" || strings.ContainsAny(config.BearerToken, "\r\n") || len(config.Roots) == 0 {
		return nil, errors.New("provider credential and authorized roots are required")
	}
	origin.Path = ""
	client := &Client{origin: origin, token: config.BearerToken}
	for _, raw := range config.Roots {
		root, rootErr := client.sameOrigin(raw)
		if rootErr != nil || !strings.HasPrefix(root.Path, "/dav/file/") || !strings.HasSuffix(root.Path, "/") {
			return nil, errors.New("invalid authorized file root")
		}
		client.roots = append(client.roots, root)
	}
	client.http = config.HTTPClient
	if client.http == nil {
		client.http = &http.Client{Timeout: 30 * time.Second}
	} else {
		copied := *client.http
		client.http = &copied
	}
	client.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.http.Timeout == 0 {
		client.http.Timeout = 30 * time.Second
	}
	return client, nil
}

func (c *Client) Roots() []Entry {
	items := make([]Entry, 0, len(c.roots))
	for _, root := range c.roots {
		items = append(items, Entry{Href: root.EscapedPath(), IsCollection: true})
	}
	return items
}

func (c *Client) List(ctx context.Context, href string) (ListResult, error) {
	target, err := c.authorized(href)
	if err != nil {
		return ListResult{}, err
	}
	payload := []byte(`<d:propfind xmlns:d="DAV:"><d:prop><d:displayname/><d:getcontentlength/><d:getcontenttype/><d:getetag/><d:resourcetype/></d:prop></d:propfind>`)
	raw, _, err := c.request(ctx, "PROPFIND", target, payload, map[string]string{"Depth": "1", "Content-Type": "application/xml"}, maxDAVResponse)
	if err != nil {
		return ListResult{}, err
	}
	var document struct {
		XMLName   xml.Name      `xml:"DAV: multistatus"`
		Responses []davResponse `xml:"DAV: response"`
	}
	if xml.Unmarshal(raw, &document) != nil {
		return ListResult{}, errors.New("provider returned invalid DAV multistatus")
	}
	result := ListResult{Items: []Entry{}}
	parent := strings.TrimSuffix(target.Path, "/")
	for _, response := range document.Responses {
		if response.Status != "" && !strings.Contains(response.Status, " 200 ") {
			result.Partial = true
			continue
		}
		reference, parseErr := url.Parse(response.Href)
		if parseErr != nil {
			result.Partial = true
			continue
		}
		resolved, authErr := c.authorized(target.ResolveReference(reference).String())
		if authErr != nil {
			result.Partial = true
			continue
		}
		entryPath := strings.TrimSuffix(resolved.Path, "/")
		if entryPath == parent {
			continue
		}
		if path.Dir(entryPath) != parent {
			result.Partial = true
			continue
		}
		if len(result.Items) >= maxEntries {
			result.Truncated = true
			continue
		}
		accepted := false
		for _, prop := range response.Props {
			if !strings.Contains(prop.Status, " 200 ") {
				result.Partial = true
				continue
			}
			result.Items = append(result.Items, Entry{Href: resolved.EscapedPath(), Name: prop.Prop.Name, Size: prop.Prop.Size, ContentType: prop.Prop.Type, ETag: prop.Prop.ETag, IsCollection: prop.Prop.ResourceType.Collection != nil})
			accepted = true
			break
		}
		if !accepted {
			result.Partial = true
		}
	}
	return result, nil
}

func (c *Client) Read(ctx context.Context, href string) (ReadResult, error) {
	target, err := c.authorized(href)
	if err != nil {
		return ReadResult{}, err
	}
	if _, _, err = c.request(ctx, "HEAD", target, nil, nil, maxDownload); err != nil {
		return ReadResult{}, err
	}
	data, headers, err := c.request(ctx, "GET", target, nil, nil, maxDownload)
	if err != nil {
		return ReadResult{}, err
	}
	mediaType, _, err := mime.ParseMediaType(headers.Get("Content-Type"))
	if err != nil || !(strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || mediaType == "application/xml") || !utf8.Valid(data) {
		return ReadResult{}, errors.New("binary or untyped content requires protected download")
	}
	if len(data) > maxInlineText {
		return ReadResult{}, errors.New("file exceeds inline text limit")
	}
	return ReadResult{Text: string(data), ContentType: mediaType, Bytes: len(data), ETag: headers.Get("ETag")}, nil
}

func (c *Client) authorized(raw string) (*url.URL, error) {
	target, err := c.sameOrigin(raw)
	if err != nil {
		return nil, err
	}
	for _, root := range c.roots {
		if target.Path == strings.TrimSuffix(root.Path, "/") || strings.HasPrefix(target.Path, root.Path) {
			return target, nil
		}
	}
	return nil, errors.New("file is outside authorized roots")
}

func (c *Client) sameOrigin(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("invalid provider file reference")
	}
	target = c.origin.ResolveReference(target)
	if target.Scheme != c.origin.Scheme || !strings.EqualFold(target.Host, c.origin.Host) || strings.ContainsAny(target.Path, "\\\x00\r\n") {
		return nil, errors.New("provider file is outside reviewed origin")
	}
	decoded := target.Path
	for range 3 {
		lower := strings.ToLower(decoded)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return nil, errors.New("encoded path separator denied")
		}
		for _, part := range strings.Split(decoded, "/") {
			if part == "." || part == ".." {
				return nil, errors.New("noncanonical provider path")
			}
		}
		next, decodeErr := url.PathUnescape(decoded)
		if decodeErr != nil {
			return nil, errors.New("invalid provider path")
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	if strings.ContainsAny(decoded, "\\\x00\r\n") || strings.Contains(decoded, "%") {
		return nil, errors.New("noncanonical provider path")
	}
	return target, nil
}

func (c *Client) request(ctx context.Context, method string, target *url.URL, payload []byte, headers map[string]string, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, errors.New("build provider file request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, nil, errors.New("file provider unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, nil, errors.New("provider redirect is forbidden")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("file provider returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return nil, nil, errors.New("file provider response exceeds limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, nil, errors.New("read file provider response")
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("file provider response exceeds limit")
	}
	return data, response.Header.Clone(), nil
}
