package telegram

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testToken = "123456:secret-token"

func TestRedactingClientHidesToken(t *testing.T) {
	// A server that is already closed, so the request fails like a network error.
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()

	client := &redactingClient{client: server.Client(), token: testToken}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/bot"+testToken+"/getMe", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error leaks the bot token: %v", err)
	}
	if !strings.Contains(err.Error(), "/bot"+redactedToken+"/getMe") {
		t.Errorf("error should keep the redacted URL: %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("error should still be a *url.Error, got %T", err)
	}
}

func TestRedactToken(t *testing.T) {
	if err := redactToken(nil, testToken); err != nil {
		t.Errorf("nil error: got %v", err)
	}
	plain := errors.New("no token here")
	if err := redactToken(plain, testToken); err != plain {
		t.Errorf("error without the token should be returned unchanged, got %v", err)
	}
	err := redactToken(errors.New("bad request for "+testToken), testToken)
	if err.Error() != "bad request for "+redactedToken {
		t.Errorf("got %q", err.Error())
	}
}
