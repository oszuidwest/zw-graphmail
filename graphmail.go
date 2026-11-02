// Package graphmail sends mail through Microsoft Graph as an Entra app
// (client credentials flow).
package graphmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	graphURL    = "https://graph.microsoft.com/v1.0"
	maxAttempts = 4
	maxWait     = 30 * time.Second
)

// Config identifies the Entra app and the mailbox it sends from.
type Config struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// From is the user or shared mailbox the app sends as. The app needs the
	// Mail.Send application permission.
	From string
}

// Message is one mail. Body is plain text unless HTML is set.
type Message struct {
	To          []string
	Subject     string
	Body        string
	HTML        bool
	Attachments []Attachment
}

// Attachment is a file attached to the message.
type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

// Client sends mail. It is safe for concurrent use.
type Client struct {
	cfg     Config
	http    *http.Client
	baseURL string
	sleep   func(context.Context, time.Duration) error
}

// New returns a client for cfg. It does not contact Microsoft.
func New(cfg Config) (*Client, error) {
	tokenURL := "https://login.microsoftonline.com/" + url.PathEscape(cfg.TenantID) + "/oauth2/v2.0/token"
	return newClient(cfg, tokenURL, graphURL)
}

func newClient(cfg Config, tokenURL, baseURL string) (*Client, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.From == "" {
		return nil, errors.New("graphmail: tenant ID, client ID, client secret and from address are required")
	}
	creds := clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     tokenURL,
		AuthStyle:    oauth2.AuthStyleInParams,
		Scopes:       []string{"https://graph.microsoft.com/.default"},
	}
	base := &http.Client{Timeout: 30 * time.Second}
	return &Client{
		cfg:     cfg,
		http:    creds.Client(context.WithValue(context.Background(), oauth2.HTTPClient, base)),
		baseURL: baseURL,
		sleep:   sleep,
	}, nil
}

// Send delivers m. It makes up to four attempts on 429, 500, 502, 503, 504
// and network errors, and gives up when Graph asks to wait longer than 30
// seconds. A retry after a lost response can deliver the mail twice, so
// this favours delivery over avoiding duplicates.
func (c *Client) Send(ctx context.Context, m *Message) error {
	payload, _ := json.Marshal(newMailRequest(m)) // only strings, maps and slices
	endpoint := c.baseURL + "/users/" + url.PathEscape(c.cfg.From) + "/sendMail"
	for attempt := 1; ; attempt++ {
		retry, wait, err := c.post(ctx, endpoint, payload)
		if !retry || attempt == maxAttempts || wait > maxWait {
			return err
		}
		if wait == 0 {
			wait = time.Second << (attempt - 1)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return fmt.Errorf("graphmail: wait for retry: %w", err)
		}
	}
}

// post makes one attempt. wait is the server's Retry-After, or zero.
func (c *Client) post(ctx context.Context, endpoint string, payload []byte) (retry bool, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, 0, fmt.Errorf("graphmail: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ctx.Err() == nil, 0, fmt.Errorf("graphmail: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 == 2 {
		return false, 0, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	err = fmt.Errorf("graphmail: sendMail returned %s: %s", resp.Status, body)
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true, retryAfter(resp.Header.Get("Retry-After")), err
	}
	return false, 0, err
}

// retryAfter parses the delta-seconds form that Graph uses.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(header)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(min(seconds, math.MaxInt32)) * time.Second // no overflow
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// SecretExpiry reports when the configured client secret expires. It needs
// the Application.Read.All application permission. Graph only exposes the
// first three characters of each secret, so it returns the earliest expiry of
// the unexpired secrets that start with those characters.
func (c *Client) SecretExpiry(ctx context.Context) (time.Time, error) {
	endpoint := c.baseURL + "/applications(appId='" + url.PathEscape(c.cfg.ClientID) + "')?$select=passwordCredentials"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return time.Time{}, fmt.Errorf("graphmail: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("graphmail: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return time.Time{}, fmt.Errorf("graphmail: application lookup returned %s: %s", resp.Status, body)
	}
	var app struct {
		PasswordCredentials []struct {
			Hint        string    `json:"hint"`
			EndDateTime time.Time `json:"endDateTime"`
		} `json:"passwordCredentials"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&app); err != nil {
		return time.Time{}, fmt.Errorf("graphmail: decode application: %w", err)
	}
	var earliest time.Time
	for _, s := range app.PasswordCredentials {
		ours := s.Hint != "" && strings.HasPrefix(c.cfg.ClientSecret, s.Hint)
		if ours && s.EndDateTime.After(time.Now()) && (earliest.IsZero() || s.EndDateTime.Before(earliest)) {
			earliest = s.EndDateTime
		}
	}
	if earliest.IsZero() {
		return time.Time{}, errors.New("graphmail: configured client secret not found on the application")
	}
	return earliest, nil
}

// newMailRequest builds the sendMail body. encoding/json writes []byte as
// the base64 that contentBytes expects.
func newMailRequest(m *Message) map[string]any {
	contentType := "Text"
	if m.HTML {
		contentType = "HTML"
	}
	to := make([]any, 0, len(m.To))
	for _, address := range m.To {
		to = append(to, map[string]any{"emailAddress": map[string]string{"address": address}})
	}
	message := map[string]any{
		"subject":      m.Subject,
		"body":         map[string]string{"contentType": contentType, "content": m.Body},
		"toRecipients": to,
	}
	files := make([]any, 0, len(m.Attachments))
	for _, a := range m.Attachments {
		files = append(files, map[string]any{
			"@odata.type": "#microsoft.graph.fileAttachment", "name": a.Name, "contentType": a.ContentType, "contentBytes": a.Data,
		})
	}
	if len(files) > 0 {
		message["attachments"] = files
	}
	return map[string]any{"message": message}
}
