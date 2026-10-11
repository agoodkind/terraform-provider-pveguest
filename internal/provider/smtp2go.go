package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultSMTP2GOAPIURL = "https://api.smtp2go.com/v3"

	smtp2goEnvironmentName = "PVEGUEST_SMTP2GO_API_KEY"
	smtp2goKeyHeader       = "X-Smtp2go-Api-Key"

	smtp2goAddPath  = "/users/smtp/add"
	smtp2goEditPath = "/users/smtp/edit"
	smtp2goViewPath = "/users/smtp/view"

	smtp2goStatusAllowed      = "allowed"
	smtp2goExistingUserMarker = "already have smtp user"

	smtp2goRequestTimeout = 30 * time.Second
	smtp2goResponseLimit  = 1 << 20

	jsonMediaType  = "application/json"
	loopbackName   = "localhost"
	redactedSecret = "[redacted]"
)

type smtp2goClient struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
}

type smtp2goUserBody struct {
	Username    string `json:"username"`
	Generated   string `json:"email_password,omitempty"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status,omitempty"`
}

// smtp2goUser excludes email_password from decoded view responses.
type smtp2goUser struct {
	Username string `json:"username"`
	Status   string `json:"status"`
}

type smtp2goErrorData struct {
	Error string `json:"error"`
}

type smtp2goErrorEnvelope struct {
	Data smtp2goErrorData `json:"data"`
}

type smtp2goViewData struct {
	Results *[]smtp2goUser `json:"results"`
}

type smtp2goViewEnvelope struct {
	Data smtp2goViewData `json:"data"`
}

func newSMTP2GOClient(apiURL string, apiKey string) (*smtp2goClient, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil {
		slog.Error("smtp2go_api_url parsing failed", "err", err)
		return nil, fmt.Errorf("smtp2go_api_url parsing failed: %w", err)
	}
	if !acceptsSMTP2GOAPIURL(parsed) {
		return nil, errors.New("smtp2go_api_url requires a host and HTTPS; HTTP is permitted only for localhost or a loopback IP address")
	}
	// The client disables redirects to avoid sending the API key to another host.
	httpClient := &http.Client{
		Timeout: smtp2goRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &smtp2goClient{
		apiURL:     strings.TrimRight(apiURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
	}, nil
}

func acceptsSMTP2GOAPIURL(parsed *url.URL) bool {
	if parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	hostname := parsed.Hostname()
	if hostname == loopbackName {
		return true
	}
	address := net.ParseIP(hostname)
	return address != nil && address.IsLoopback()
}

func (client *smtp2goClient) setPassword(
	ctx context.Context,
	username string,
	password string,
	description string,
) error {
	addBody := smtp2goUserBody{
		Username:    username,
		Generated:   password,
		Description: description,
		Status:      smtp2goStatusAllowed,
	}
	status, response, err := client.post(ctx, smtp2goAddPath, addBody)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	errorText := smtp2goErrorText(response)
	hasExistingUserMarker := strings.Contains(strings.ToLower(errorText), smtp2goExistingUserMarker)
	userExists := status == http.StatusConflict || (status == http.StatusBadRequest && hasExistingUserMarker)
	if !userExists {
		return client.failure(smtp2goAddPath, status, errorText, password)
	}

	editBody := smtp2goUserBody{
		Username:    username,
		Generated:   password,
		Description: "",
		Status:      smtp2goStatusAllowed,
	}
	status, response, err = client.post(ctx, smtp2goEditPath, editBody)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return client.failure(smtp2goEditPath, status, smtp2goErrorText(response), password)
	}
	return nil
}

// readUser reports a missing user only for an empty data.results list.
func (client *smtp2goClient) readUser(ctx context.Context, username string) (smtp2goUser, bool, error) {
	viewBody := smtp2goUserBody{Username: username, Generated: "", Description: "", Status: ""}
	status, response, err := client.post(ctx, smtp2goViewPath, viewBody)
	if err != nil {
		return smtp2goUser{}, false, err
	}
	if status != http.StatusOK {
		return smtp2goUser{}, false, client.failure(smtp2goViewPath, status, smtp2goErrorText(response), "")
	}
	var envelope smtp2goViewEnvelope
	if err := json.Unmarshal(response, &envelope); err != nil {
		// readUser omits the decoder error because the response may contain email_password.
		return smtp2goUser{}, false, fmt.Errorf("cannot decode SMTP2GO response from %s", smtp2goViewPath)
	}
	if envelope.Data.Results == nil {
		return smtp2goUser{}, false, fmt.Errorf("SMTP2GO response from %s has no data.results", smtp2goViewPath)
	}
	users := *envelope.Data.Results
	for _, user := range users {
		if user.Username == username {
			return user, true, nil
		}
	}
	if len(users) > 0 {
		return smtp2goUser{}, false, fmt.Errorf("SMTP2GO response from %s lists users but none matches username %s", smtp2goViewPath, username)
	}
	return smtp2goUser{}, false, nil
}

func (client *smtp2goClient) post(ctx context.Context, apiPath string, body smtp2goUserBody) (int, []byte, error) {
	if client.apiKey == "" {
		return 0, nil, fmt.Errorf("SMTP2GO API key is missing; smtp2go_api_key or %s must be set", smtp2goEnvironmentName)
	}
	document, err := json.Marshal(&body)
	if err != nil {
		// post omits the encoding error because the request contains the password.
		return 0, nil, fmt.Errorf("SMTP2GO request encoding for %s failed", apiPath)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.apiURL+apiPath, bytes.NewReader(document))
	if err != nil {
		slog.ErrorContext(ctx, "SMTP2GO request construction failed", "path", apiPath, "err", err)
		return 0, nil, fmt.Errorf("SMTP2GO request construction for %s failed: %w", apiPath, err)
	}
	request.Header.Set("Content-Type", jsonMediaType)
	request.Header.Set("Accept", jsonMediaType)
	request.Header.Set(smtp2goKeyHeader, client.apiKey)

	response, err := client.httpClient.Do(request)
	if err != nil {
		slog.ErrorContext(ctx, "The SMTP2GO HTTP request failed", "path", apiPath, "err", err)
		return 0, nil, fmt.Errorf("SMTP2GO request to %s failed: %w", apiPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	content, err := io.ReadAll(io.LimitReader(response.Body, smtp2goResponseLimit))
	if err != nil {
		slog.ErrorContext(ctx, "The SMTP2GO response body read failed", "path", apiPath, "err", err)
		return 0, nil, fmt.Errorf("cannot read SMTP2GO response body for %s: %w", apiPath, err)
	}
	return response.StatusCode, content, nil
}

func (client *smtp2goClient) failure(apiPath string, status int, errorText string, password string) error {
	visibleText := redactSecret(redactSecret(errorText, client.apiKey), password)
	return fmt.Errorf("SMTP2GO API request to %s returned unexpected HTTP status %d: %s", apiPath, status, visibleText)
}

func smtp2goErrorText(response []byte) string {
	var envelope smtp2goErrorEnvelope
	if err := json.Unmarshal(response, &envelope); err != nil {
		return ""
	}
	return envelope.Data.Error
}

func redactSecret(text string, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, redactedSecret)
}
