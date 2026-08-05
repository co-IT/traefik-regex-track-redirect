package traefik_regex_track_redirect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCreateConfig(t *testing.T) {
	config := CreateConfig()
	if config.TrackMethod != http.MethodPost {
		t.Fatalf("unexpected default method: %q", config.TrackMethod)
	}
	if config.TrackTimeout != "2s" {
		t.Fatalf("unexpected default timeout: %q", config.TrackTimeout)
	}
	if config.TrackHeaders == nil {
		t.Fatal("expected initialized tracking headers")
	}
}

func TestNewValidation(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	tests := []struct {
		name   string
		config *Config
		want   string
	}{
		{name: "nil config", want: "config must not be nil"},
		{name: "empty regex", config: &Config{}, want: "regex must not be empty"},
		{name: "invalid regex", config: &Config{Regex: "[", TrackEndpoint: "http://tracker"}, want: "compile regex"},
		{name: "missing endpoint", config: &Config{Regex: ".*"}, want: "trackEndpoint must not be empty"},
		{name: "invalid endpoint", config: &Config{Regex: ".*", TrackEndpoint: "/relative"}, want: "absolute HTTP(S) URL"},
		{name: "invalid method", config: &Config{Regex: ".*", TrackEndpoint: "http://tracker", TrackMethod: http.MethodPut}, want: "trackMethod must be GET or POST"},
		{name: "invalid timeout", config: &Config{Regex: ".*", TrackEndpoint: "http://tracker", TrackTimeout: "later"}, want: "parse trackTimeout"},
		{name: "zero timeout", config: &Config{Regex: ".*", TrackEndpoint: "http://tracker", TrackTimeout: "0s"}, want: "greater than zero"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(context.Background(), next, test.config, "test")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestTrackEndpointWithGETKeepsConfiguredQuery(t *testing.T) {
	type capturedRequest struct {
		method      string
		rawQuery    string
		body        string
		contentType string
	}
	captured := make(chan capturedRequest, 1)
	tracker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		captured <- capturedRequest{
			method:      req.Method,
			rawQuery:    req.URL.RawQuery,
			body:        string(body),
			contentType: req.Header.Get("Content-Type"),
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer tracker.Close()

	handler := newTestMiddleware(t, http.NotFoundHandler(), &Config{
		Regex:         `^http://example\.com/old$`,
		Replacement:   "https://example.com/new",
		TrackEndpoint: tracker.URL + "/track?token=external",
		TrackMethod:   "get",
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/old", nil))

	request := <-captured
	if request.method != http.MethodGet {
		t.Fatalf("tracking method = %q, want GET", request.method)
	}
	if request.rawQuery != "token=external" {
		t.Fatalf("tracking query = %q, want unchanged external query", request.rawQuery)
	}
	if request.body != "" || request.contentType != "" {
		t.Fatalf("GET body = %q, Content-Type = %q; want both empty", request.body, request.contentType)
	}
}

func TestNonMatchingRequestCallsNext(t *testing.T) {
	var trackRequests atomic.Int32
	tracker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		trackRequests.Add(1)
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer tracker.Close()

	nextCalled := false
	next := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		rw.WriteHeader(http.StatusTeapot)
	})
	handler := newTestMiddleware(t, next, &Config{
		Regex:         `^https://example\.com/old`,
		Replacement:   "https://example.com/new",
		TrackEndpoint: tracker.URL,
	})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/other", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if !nextCalled || response.Code != http.StatusTeapot {
		t.Fatalf("next called = %v, status = %d", nextCalled, response.Code)
	}
	if trackRequests.Load() != 0 {
		t.Fatalf("unexpected tracking request count: %d", trackRequests.Load())
	}
}

func TestRedirectReportsFirst(t *testing.T) {
	events := make(chan Event, 1)
	tracker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", req.Method)
		}
		if req.Header.Get("X-API-Key") != "secret" {
			t.Errorf("missing configured header")
		}
		var event Event
		if err := json.NewDecoder(req.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
		}
		events <- event
		rw.WriteHeader(http.StatusAccepted)
	}))
	defer tracker.Close()

	handler := newTestMiddleware(t, http.NotFoundHandler(), &Config{
		Regex:         `^http://example\.com/old/(.*)$`,
		Replacement:   `https://example.com/new/${1}`,
		TrackEndpoint: tracker.URL,
		TrackHeaders:  map[string]string{"X-API-Key": "secret"},
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/old/item", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("User-Agent", "test-agent")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	if got := response.Header().Get("Location"); got != "https://example.com/new/item" {
		t.Fatalf("Location = %q", got)
	}
	event := <-events
	if event.Plugin != "test-plugin" || event.SourceURL != "http://example.com/old/item" || event.RedirectURL != "https://example.com/new/item" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.StatusCode != http.StatusFound || event.RemoteAddr != request.RemoteAddr || event.UserAgent != "test-agent" {
		t.Fatalf("unexpected event metadata: %#v", event)
	}
}

func TestPermanentRedirectPreservesNonGETMethod(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer tracker.Close()

	handler := newTestMiddleware(t, http.NotFoundHandler(), &Config{
		Regex:         `^http://example\.com/old$`,
		Replacement:   "https://example.com/new",
		Permanent:     true,
		TrackEndpoint: tracker.URL,
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://example.com/old", nil))

	if response.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusPermanentRedirect)
	}
}

func TestTrackFailureModes(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tracker.Close()

	for _, test := range []struct {
		name       string
		failClosed bool
		wantStatus int
	}{
		{name: "fail open", wantStatus: http.StatusFound},
		{name: "fail closed", failClosed: true, wantStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestMiddleware(t, http.NotFoundHandler(), &Config{
				Regex:            `^http://example\.com/old$`,
				Replacement:      "https://example.com/new",
				TrackEndpoint:    tracker.URL,
				FailOnTrackError: test.failClosed,
			})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/old", nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}

func newTestMiddleware(t *testing.T, next http.Handler, config *Config) http.Handler {
	t.Helper()
	handler, err := New(context.Background(), next, config, "test-plugin")
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return handler
}
