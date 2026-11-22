package graphmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// fakeGraph serves the token endpoint and answers each Graph request with
// the next handler; the last handler repeats.
type fakeGraph struct {
	handlers   []http.HandlerFunc
	failTokens int // answer this many token requests with 503
	tokens     int
	posts      int
	slept      []time.Duration
}

func (f *fakeGraph) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			f.tokens++
			if f.tokens <= f.failTokens {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		f.posts++
		f.handlers[min(f.posts, len(f.handlers))-1](w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(Config{TenantID: "t", ClientID: "app", ClientSecret: "abc~secret", From: "noreply@zuidwest.nl"}, srv.URL+"/token", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(_ context.Context, d time.Duration) error {
		f.slept = append(f.slept, d)
		return nil
	}
	return c
}

func status(code int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(code)
	}
}

func TestSendBody(t *testing.T) {
	for _, tc := range []struct {
		msg  Message
		want string
	}{
		{Message{To: []string{"a@x.nl", "b@x.nl"}, Subject: "s", Body: "b"},
			`{"message":{"body":{"content":"b","contentType":"Text"},"subject":"s","toRecipients":[{"emailAddress":{"address":"a@x.nl"}},{"emailAddress":{"address":"b@x.nl"}}]}}`},
		{Message{To: []string{"a@x.nl"}, Body: "h", HTML: true, Attachments: []Attachment{{Name: "dump.mp3", ContentType: "audio/mpeg", Data: []byte{0xff, 0xfb}}}},
			`{"message":{"attachments":[{"@odata.type":"#microsoft.graph.fileAttachment","contentBytes":"//s=","contentType":"audio/mpeg","name":"dump.mp3"}],"body":{"content":"h","contentType":"HTML"},"subject":"","toRecipients":[{"emailAddress":{"address":"a@x.nl"}}]}}`},
	} {
		var got string
		f := &fakeGraph{handlers: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			got = r.URL.Path + " " + string(body)
			w.WriteHeader(http.StatusAccepted)
		}}}
		if err := f.client(t).Send(t.Context(), &tc.msg); err != nil {
			t.Fatal(err)
		}
		if want := "/users/noreply@zuidwest.nl/sendMail " + tc.want; got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

func TestSendRetries(t *testing.T) {
	dropConnection := func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}
	for name, tc := range map[string]struct {
		handlers   []http.HandlerFunc
		failTokens int
		posts      int
		slept      []time.Duration
		fails      bool
	}{
		"Retry-After on 429":   {handlers: []http.HandlerFunc{status(429, "7"), status(202, "")}, posts: 2, slept: []time.Duration{7 * time.Second}},
		"Retry-After on 503":   {handlers: []http.HandlerFunc{status(503, "3"), status(202, "")}, posts: 2, slept: []time.Duration{3 * time.Second}},
		"network error":        {handlers: []http.HandlerFunc{dropConnection, status(202, "")}, posts: 2, slept: []time.Duration{time.Second}},
		"backoff then give up": {handlers: []http.HandlerFunc{status(500, "")}, posts: 4, slept: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, fails: true},
		"long Retry-After":     {handlers: []http.HandlerFunc{status(429, "120"), status(202, "")}, posts: 2, slept: []time.Duration{2 * time.Minute}},
		"client error":         {handlers: []http.HandlerFunc{status(400, "")}, posts: 1, fails: true},
		"token request 503":    {handlers: []http.HandlerFunc{status(202, "")}, failTokens: 1, posts: 1, slept: []time.Duration{time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGraph{handlers: tc.handlers, failTokens: tc.failTokens}
			err := f.client(t).Send(t.Context(), &Message{To: []string{"a@x.nl"}})
			tokens := 1 + tc.failTokens // one token serves every attempt
			if (err != nil) != tc.fails || f.tokens != tokens || f.posts != tc.posts || !slices.Equal(f.slept, tc.slept) {
				t.Errorf("err=%v tokens=%d posts=%d slept=%v, want fails=%v tokens=%d posts=%d slept=%v", err, f.tokens, f.posts, f.slept, tc.fails, tokens, tc.posts, tc.slept)
			}
		})
	}
}

func TestSendCancelledDuringWait(t *testing.T) {
	f := &fakeGraph{handlers: []http.HandlerFunc{status(429, "30")}}
	c := f.client(t)
	ctx, cancel := context.WithCancel(t.Context())
	c.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return sleep(ctx, d) }
	start := time.Now()
	if err := c.Send(ctx, &Message{To: []string{"a@x.nl"}}); !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Errorf("err=%v after %v, want context.Canceled without waiting 30s", err, time.Since(start))
	}
}

func TestSendRenewsTokenExpiredDuringWait(t *testing.T) {
	f := &fakeGraph{handlers: []http.HandlerFunc{status(503, "3"), status(202, "")}}
	c := f.client(t)
	c.sleep = func(context.Context, time.Duration) error {
		c.tok.Expiry = time.Now()
		return nil
	}
	if err := c.Send(t.Context(), &Message{To: []string{"a@x.nl"}}); err != nil || f.tokens != 2 {
		t.Errorf("err=%v tokens=%d, want a new token for the retry", err, f.tokens)
	}
}

func TestSendCancelledWhileWaitingForToken(t *testing.T) {
	f := &fakeGraph{handlers: []http.HandlerFunc{status(202, "")}}
	c := f.client(t)
	c.lock <- struct{}{} // another call is fetching a token
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Send(ctx, &Message{To: []string{"a@x.nl"}}); !errors.Is(err, context.DeadlineExceeded) || f.tokens+f.posts != 0 {
		t.Errorf("err=%v tokens=%d posts=%d, want context.DeadlineExceeded without requests", err, f.tokens, f.posts)
	}
}

func TestSendCancelledDuringTokenFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only after the body is read
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(Config{TenantID: "t", ClientID: "app", ClientSecret: "abc~secret", From: "noreply@zuidwest.nl"}, srv.URL+"/token", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ctx, &Message{To: []string{"a@x.nl"}}); !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

func TestSendCancelled(t *testing.T) {
	f := &fakeGraph{handlers: []http.HandlerFunc{status(202, "")}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.client(t).Send(ctx, &Message{To: []string{"a@x.nl"}}); err == nil || f.tokens+f.posts != 0 || len(f.slept) != 0 {
		t.Errorf("err=%v tokens=%d posts=%d slept=%v, want an error without requests", err, f.tokens, f.posts, f.slept)
	}
}

func TestSecretExpiry(t *testing.T) {
	day := func(n int) time.Time {
		return time.Now().Add(time.Duration(n) * 24 * time.Hour).UTC().Truncate(time.Second)
	}
	for name, tc := range map[string]struct {
		creds []map[string]any
		want  time.Time
	}{
		"earliest unexpired with our hint": {creds: []map[string]any{
			{"hint": "xyz", "endDateTime": day(1)},
			{"endDateTime": day(2)},
			{"hint": "abc", "endDateTime": day(-1)},
			{"hint": "abc", "endDateTime": day(60)},
			{"hint": "abc", "endDateTime": day(30)},
		}, want: day(30)},
		"no secret with our hint": {creds: []map[string]any{{"hint": "xyz", "endDateTime": day(30)}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeGraph{handlers: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
				if want := "/applications(appId='app')"; r.URL.Path != want {
					t.Errorf("path = %s, want %s", r.URL.Path, want)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"passwordCredentials": tc.creds})
			}}}
			got, err := f.client(t).SecretExpiry(t.Context())
			if !got.Equal(tc.want) || (err != nil) != tc.want.IsZero() {
				t.Errorf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	for i, cfg := range []Config{
		{TenantID: "t", ClientID: "c", ClientSecret: "s"},
		{ClientID: "c", ClientSecret: "s"},
		{TenantID: "t", ClientSecret: "s"},
		{TenantID: "t", ClientID: "c"},
	} {
		if _, err := New(cfg); (err == nil) != (i == 0) {
			t.Errorf("New(%+v) error = %v", cfg, err)
		}
	}
}

func TestSendRequiresFrom(t *testing.T) {
	f := &fakeGraph{handlers: []http.HandlerFunc{status(202, "")}}
	c := f.client(t)
	c.cfg.From = ""
	if err := c.Send(t.Context(), &Message{To: []string{"a@x.nl"}}); err == nil || f.tokens+f.posts != 0 {
		t.Errorf("err=%v tokens=%d posts=%d, want an error without requests", err, f.tokens, f.posts)
	}
}
