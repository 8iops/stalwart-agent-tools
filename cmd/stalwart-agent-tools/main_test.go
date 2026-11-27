package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthenticateRequiresBoundedInvocationBearer(t *testing.T) {
	handler := authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, header := range []string{"", "Basic x", "Bearer "} {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("invalid bearer accepted: %q", header)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer signed-invocation")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("bounded bearer rejected: %d", response.Code)
	}
}
