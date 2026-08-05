// Package traefik_regex_request_and_redirect provides a Traefik middleware
// that reports matching requests to a tracking endpoint before returning a
// regex redirect.
package traefik_regex_request_and_redirect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const defaultTrackTimeout = 2 * time.Second

var absoluteRequestURI = regexp.MustCompile(`^(https?)://(\[[\w:.]+\]|[\w._-]+)?(:\d+)?(.*)$`)

// Config contains the middleware configuration.
type Config struct {
	Regex            string            `json:"regex,omitempty"`
	Replacement      string            `json:"replacement,omitempty"`
	Permanent        bool              `json:"permanent,omitempty"`
	TrackEndpoint    string            `json:"trackEndpoint,omitempty"`
	TrackMethod      string            `json:"trackMethod,omitempty"`
	TrackTimeout     string            `json:"trackTimeout,omitempty"`
	TrackHeaders     map[string]string `json:"trackHeaders,omitempty"`
	FailOnTrackError bool              `json:"failOnTrackError,omitempty"`
}

// CreateConfig returns the default middleware configuration.
func CreateConfig() *Config {
	return &Config{
		TrackMethod:  http.MethodPost,
		TrackTimeout: defaultTrackTimeout.String(),
		TrackHeaders: make(map[string]string),
	}
}

// Middleware reports and redirects matching requests.
type Middleware struct {
	next             http.Handler
	name             string
	regex            *regexp.Regexp
	replacement      string
	permanent        bool
	trackEndpoint    string
	trackMethod      string
	trackHeaders     map[string]string
	failOnTrackError bool
	client           *http.Client
}

// Event is sent as JSON to the configured tracking endpoint.
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	Plugin      string    `json:"plugin"`
	Method      string    `json:"method"`
	SourceURL   string    `json:"sourceUrl"`
	RedirectURL string    `json:"redirectUrl"`
	StatusCode  int       `json:"statusCode"`
	RemoteAddr  string    `json:"remoteAddr,omitempty"`
	UserAgent   string    `json:"userAgent,omitempty"`
}

// New creates a new middleware instance.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("next handler must not be nil")
	}
	if config == nil {
		return nil, errors.New("config must not be nil")
	}
	if config.Regex == "" {
		return nil, errors.New("regex must not be empty")
	}

	redirectRegex, err := regexp.Compile(config.Regex)
	if err != nil {
		return nil, fmt.Errorf("compile regex: %w", err)
	}

	endpoint, err := trackEndpointFromConfig(config)
	if err != nil {
		return nil, err
	}
	trackMethod := strings.ToUpper(strings.TrimSpace(config.TrackMethod))
	if trackMethod == "" {
		trackMethod = http.MethodPost
	}
	if trackMethod != http.MethodGet && trackMethod != http.MethodPost {
		return nil, errors.New("trackMethod must be GET or POST")
	}

	timeout := defaultTrackTimeout
	if config.TrackTimeout != "" {
		timeout, err = time.ParseDuration(config.TrackTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse trackTimeout: %w", err)
		}
		if timeout <= 0 {
			return nil, errors.New("trackTimeout must be greater than zero")
		}
	}

	headers := make(map[string]string, len(config.TrackHeaders))
	for key, value := range config.TrackHeaders {
		if strings.TrimSpace(key) == "" {
			return nil, errors.New("trackHeaders must not contain an empty name")
		}
		headers[key] = value
	}

	return &Middleware{
		next:             next,
		name:             name,
		regex:            redirectRegex,
		replacement:      config.Replacement,
		permanent:        config.Permanent,
		trackEndpoint:    endpoint,
		trackMethod:      trackMethod,
		trackHeaders:     headers,
		failOnTrackError: config.FailOnTrackError,
		client:           &http.Client{Timeout: timeout},
	}, nil
}

func trackEndpointFromConfig(config *Config) (string, error) {
	endpoint := strings.TrimSpace(config.TrackEndpoint)
	if endpoint == "" {
		return "", errors.New("trackEndpoint must not be empty")
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse trackEndpoint: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("trackEndpoint must be an absolute HTTP(S) URL")
	}

	return parsed.String(), nil
}

// ServeHTTP implements http.Handler.
func (m *Middleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	sourceURL := requestURL(req)
	if !m.regex.MatchString(sourceURL) {
		m.next.ServeHTTP(rw, req)
		return
	}

	redirectURL := m.regex.ReplaceAllString(sourceURL, m.replacement)
	if redirectURL == sourceURL {
		m.next.ServeHTTP(rw, req)
		return
	}

	parsedRedirectURL, err := url.Parse(redirectURL)
	if err != nil {
		http.Error(rw, "invalid redirect URL", http.StatusInternalServerError)
		return
	}

	statusCode := redirectStatus(req.Method, m.permanent)
	if err := m.report(req, sourceURL, parsedRedirectURL.String(), statusCode); err != nil {
		if m.failOnTrackError {
			http.Error(rw, "tracking service unavailable", http.StatusBadGateway)
			return
		}
		log.Printf("regex-request-and-redirect %q: tracking request failed: %v", m.name, err)
	}

	rw.Header().Set("Location", parsedRedirectURL.String())
	rw.WriteHeader(statusCode)
	_, _ = rw.Write([]byte(http.StatusText(statusCode)))
}

func (m *Middleware) report(req *http.Request, sourceURL, redirectURL string, statusCode int) error {
	event := Event{
		Timestamp:   time.Now().UTC(),
		Plugin:      m.name,
		Method:      req.Method,
		SourceURL:   sourceURL,
		RedirectURL: redirectURL,
		StatusCode:  statusCode,
		RemoteAddr:  req.RemoteAddr,
		UserAgent:   req.UserAgent(),
	}

	trackRequest, err := m.newTrackRequest(req.Context(), event)
	if err != nil {
		return err
	}
	trackRequest.Header.Set("User-Agent", "traefik-regex-request-and-redirect")
	for key, value := range m.trackHeaders {
		trackRequest.Header.Set(key, value)
	}

	response, err := m.client.Do(trackRequest)
	if err != nil {
		return fmt.Errorf("call tracking endpoint: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32*1024))

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("tracking endpoint returned %s", response.Status)
	}

	return nil
}

func (m *Middleware) newTrackRequest(ctx context.Context, event Event) (*http.Request, error) {
	if m.trackMethod == http.MethodGet {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.trackEndpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("create tracking request: %w", err)
		}
		return request, nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.trackEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create tracking request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

func requestURL(req *http.Request) string {
	scheme := "http"
	host := req.Host
	port := ""
	requestURI := req.RequestURI

	if match := absoluteRequestURI.FindStringSubmatch(req.RequestURI); len(match) > 0 {
		scheme = match[1]
		if match[2] != "" {
			host = match[2]
		}
		port = match[3]
		requestURI = match[4]
	}
	if req.TLS != nil {
		scheme = "https"
	}

	return scheme + "://" + host + port + requestURI
}

func redirectStatus(method string, permanent bool) int {
	if permanent {
		if method == http.MethodGet {
			return http.StatusMovedPermanently
		}
		return http.StatusPermanentRedirect
	}
	if method == http.MethodGet {
		return http.StatusFound
	}
	return http.StatusTemporaryRedirect
}
