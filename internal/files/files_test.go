package files

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListAndReadStayInsideAuthorizedRoot(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("credential missing")
		}
		switch r.Method + " " + r.URL.Path {
		case "PROPFIND /dav/file/user/":
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s/dav/file/user/</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>user</d:displayname><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response><d:response><d:href>%s/dav/file/user/note.txt</d:href><d:propstat><d:status>HTTP/1.1 200 OK</d:status><d:prop><d:displayname>note.txt</d:displayname><d:getcontentlength>5</d:getcontentlength><d:getcontenttype>text/plain</d:getcontenttype><d:getetag>e1</d:getetag><d:resourcetype/></d:prop></d:propstat></d:response></d:multistatus>`, server.URL, server.URL)
		case "HEAD /dav/file/user/note.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", "e1")
		case "GET /dav/file/user/note.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", "e1")
			fmt.Fprint(w, "hello")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{Origin: server.URL, BearerToken: "token", Roots: []string{"/dav/file/user/"}, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	listing, err := client.List(context.Background(), "/dav/file/user/")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) != 1 || listing.Items[0].Name != "note.txt" || listing.Partial {
		t.Fatalf("unexpected listing: %#v", listing)
	}
	read, err := client.Read(context.Background(), listing.Items[0].Href)
	if err != nil {
		t.Fatal(err)
	}
	if read.Text != "hello" || read.ETag != "e1" {
		t.Fatalf("unexpected read: %#v", read)
	}
	for _, href := range []string{"/dav/file/other/secret", "/dav/file/user/../other", "https://evil.example/dav/file/user/x"} {
		if _, err = client.Read(context.Background(), href); err == nil {
			t.Fatalf("unsafe href accepted: %s", href)
		}
	}
}
