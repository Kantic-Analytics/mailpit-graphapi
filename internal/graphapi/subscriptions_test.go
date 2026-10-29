package graphapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

var testStart = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

type subscriptionMailpit struct {
	mu        sync.Mutex
	messages  []mailpit.MessageSummary
	listErr   error
	listCalls int
}

func (f *subscriptionMailpit) List(context.Context, int, int) (mailpit.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return mailpit.ListResponse{}, f.listErr
	}
	messages := append([]mailpit.MessageSummary(nil), f.messages...)
	return mailpit.ListResponse{Messages: messages, MessagesCount: len(messages)}, nil
}

func (*subscriptionMailpit) Get(context.Context, string) (mailpit.Message, error) {
	return mailpit.Message{}, errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) Raw(context.Context, string) ([]byte, error) {
	return nil, errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) Headers(context.Context, string) (map[string][]string, error) {
	return nil, errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) SetRead(context.Context, string, bool) error {
	return errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) SetTags(context.Context, string, []string) error {
	return errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) Send(context.Context, mailpit.SendRequest) (string, error) {
	return "", errors.New("not used by subscription tests")
}
func (*subscriptionMailpit) Ready(context.Context) error { return nil }

func (f *subscriptionMailpit) add(m mailpit.MessageSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, m)
}

func (f *subscriptionMailpit) setListErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *subscriptionMailpit) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func inboxMessage(id, mailbox string, created time.Time) mailpit.MessageSummary {
	return mailpit.MessageSummary{
		ID: id, MessageID: "<" + id + "@example.test>",
		From: mailpit.Address{Address: "sender@example.test"},
		To:   []mailpit.Address{{Address: mailbox}}, Subject: id, Created: created,
	}
}

// logSink collects the server's log output so tests can assert on it.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type validationRequest struct {
	token       string
	contentType string
}

// hookReceiver answers validation requests according to mode and records
// notifications, which it answers with notifyStatus.
type hookReceiver struct {
	server *httptest.Server
	signal chan struct{}

	mu               sync.Mutex
	mode             string
	notifyStatus     int
	onNotify         func()
	onValidate       func()
	validationTokens []validationRequest
	bodies           [][]byte
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	h := &hookReceiver{mode: "echo", notifyStatus: http.StatusAccepted, signal: make(chan struct{}, 64)}
	h.server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hookReceiver) URL() string { return h.server.URL + "/hook" }

func (h *hookReceiver) setMode(mode string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mode = mode
}

func (h *hookReceiver) setOnNotify(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onNotify = fn
}

func (h *hookReceiver) setOnValidate(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onValidate = fn
}

func (h *hookReceiver) setNotifyStatus(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifyStatus = status
}

func (h *hookReceiver) serve(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("validationToken")
	if token == "" {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		status := h.notifyStatus
		onNotify := h.onNotify
		h.mu.Unlock()
		if onNotify != nil {
			onNotify()
		}
		select {
		case h.signal <- struct{}{}:
		default:
		}
		w.WriteHeader(status)
		return
	}
	h.mu.Lock()
	mode := h.mode
	onValidate := h.onValidate
	h.validationTokens = append(h.validationTokens, validationRequest{token: token, contentType: r.Header.Get("Content-Type")})
	h.mu.Unlock()
	if onValidate != nil {
		onValidate()
	}
	switch mode {
	case "status500":
		w.WriteHeader(http.StatusInternalServerError)
	case "wrongToken":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "nope")
	case "json":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, token)
	case "hang":
		<-r.Context().Done()
	default:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, token)
	}
}

func (h *hookReceiver) validations() []validationRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]validationRequest(nil), h.validationTokens...)
}

func (h *hookReceiver) notifications() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.bodies...)
}

func (h *hookReceiver) waitNotification(t *testing.T) {
	t.Helper()
	select {
	case <-h.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no notification received within 2s")
	}
}

func newSubscriptionServer(t *testing.T, fake *subscriptionMailpit) (*Server, http.Handler, *testClock) {
	t.Helper()
	s, h, clock, _ := newLoggedSubscriptionServer(t, fake)
	return s, h, clock
}

func newLoggedSubscriptionServer(t *testing.T, fake *subscriptionMailpit) (*Server, http.Handler, *testClock, *logSink) {
	t.Helper()
	clock := &testClock{t: testStart}
	logs := &logSink{}
	s := New(fake, Config{Token: "secret", ClientID: "app-id", Logger: slog.New(slog.NewTextHandler(logs, nil))})
	s.now = clock.now
	s.validationTimeout = 50 * time.Millisecond
	return s, s.Handler(), clock, logs
}

func createFields(notifyURL, lifecycleURL string, exp time.Time) map[string]any {
	fields := map[string]any{
		"changeType":         "created",
		"notificationUrl":    notifyURL,
		"resource":           "users/box@example.test/mailFolders('inbox')/messages",
		"expirationDateTime": exp.Format(time.RFC3339Nano),
		"clientState":        "secret-state",
	}
	if lifecycleURL != "" {
		fields["lifecycleNotificationUrl"] = lifecycleURL
	}
	return fields
}

func createBody(notifyURL, lifecycleURL string, exp time.Time) string {
	return jsonBody(createFields(notifyURL, lifecycleURL, exp))
}

func jsonBody(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

type subscriptionResponse struct {
	ODataContext              string    `json:"@odata.context"`
	ID                        string    `json:"id"`
	Resource                  string    `json:"resource"`
	ApplicationID             *string   `json:"applicationId"`
	ChangeType                string    `json:"changeType"`
	ClientState               *string   `json:"clientState"`
	NotificationURL           string    `json:"notificationUrl"`
	LifecycleNotificationURL  *string   `json:"lifecycleNotificationUrl"`
	ExpirationDateTime        time.Time `json:"expirationDateTime"`
	LatestSupportedTLSVersion string    `json:"latestSupportedTlsVersion"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body.String())
	}
}

func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	assertStatus(t, w, status)
	var body errorResponse
	decodeResponse(t, w, &body)
	if body.Error.Code != code {
		t.Fatalf("error code = %q, want %q; body %s", body.Error.Code, code, w.Body.String())
	}
	if !strings.Contains(body.Error.Message, message) {
		t.Fatalf("error message = %q, want it to contain %q", body.Error.Message, message)
	}
}

func createTestSubscription(t *testing.T, h http.Handler, notifyURL string, exp time.Time) string {
	t.Helper()
	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(notifyURL, "", exp))
	assertStatus(t, w, http.StatusCreated)
	var created subscriptionResponse
	decodeResponse(t, w, &created)
	return created.ID
}

func localStats(t *testing.T, h http.Handler, id string) subscriptionStats {
	t.Helper()
	w := authorizedRequest(h, http.MethodGet, "/local/subscriptions/"+id, "")
	assertStatus(t, w, http.StatusOK)
	var stats subscriptionStats
	decodeResponse(t, w, &stats)
	return stats
}

func decodeNotification(t *testing.T, body []byte) changeNotification {
	t.Helper()
	var payload notificationCollection[changeNotification]
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode notification %s: %v", body, err)
	}
	if len(payload.Value) != 1 {
		t.Fatalf("notification value length = %d, want 1", len(payload.Value))
	}
	return payload.Value[0]
}

func TestCreateSubscriptionValidatesBothEndpoints(t *testing.T) {
	fake := &subscriptionMailpit{}
	_, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	exp := testStart.Add(time.Hour)
	resource := "users/box@example.test/mailFolders('inbox')/messages"

	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(receiver.URL(), receiver.URL(), exp))
	assertStatus(t, w, http.StatusCreated)
	var created subscriptionResponse
	decodeResponse(t, w, &created)

	if created.ID == "" {
		t.Fatal("created subscription has no id")
	}
	if created.Resource != resource || created.ChangeType != "created" || created.LatestSupportedTLSVersion != "v1_2" {
		t.Fatalf("unexpected subscription %+v", created)
	}
	if created.ClientState == nil || *created.ClientState != "secret-state" {
		t.Fatalf("clientState = %v, want secret-state", created.ClientState)
	}
	if created.ApplicationID == nil || *created.ApplicationID != "app-id" {
		t.Fatalf("applicationId = %v, want app-id", created.ApplicationID)
	}
	if !created.ExpirationDateTime.Equal(exp) {
		t.Fatalf("expirationDateTime = %s, want %s", created.ExpirationDateTime, exp)
	}
	if created.LifecycleNotificationURL == nil || *created.LifecycleNotificationURL != receiver.URL() {
		t.Fatalf("lifecycleNotificationUrl = %v, want %s", created.LifecycleNotificationURL, receiver.URL())
	}
	if created.ODataContext != subscriptionEntityContext {
		t.Fatalf("@odata.context = %q, want %q", created.ODataContext, subscriptionEntityContext)
	}

	validations := receiver.validations()
	if len(validations) != 2 {
		t.Fatalf("validation requests = %d, want 2", len(validations))
	}
	for _, v := range validations {
		if !strings.HasPrefix(v.token, "Validation: ") {
			t.Fatalf("validation token = %q, want the Graph prefix", v.token)
		}
		if !strings.HasPrefix(v.contentType, "text/plain") {
			t.Fatalf("validation Content-Type = %q, want text/plain", v.contentType)
		}
	}

	list := authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", "")
	assertStatus(t, list, http.StatusOK)
	var listed struct {
		Value []subscriptionResponse `json:"value"`
	}
	decodeResponse(t, list, &listed)
	if len(listed.Value) != 1 || listed.Value[0].ID != created.ID {
		t.Fatalf("list = %+v, want the created subscription", listed.Value)
	}

	byID := authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions/"+created.ID, "")
	assertStatus(t, byID, http.StatusOK)
	var got subscriptionResponse
	decodeResponse(t, byID, &got)
	if got.ID != created.ID {
		t.Fatalf("get id = %q, want %q", got.ID, created.ID)
	}
}

func TestCreateSubscriptionAcceptsInboxResourceForms(t *testing.T) {
	resources := []string{
		"users/box@example.test/mailFolders('Inbox')/messages",
		"/Users/box@example.test/mailfolders('inbox')/messages",
		"users/box%40example.test/mailFolders/inbox/messages",
	}
	for _, resource := range resources {
		t.Run(resource, func(t *testing.T) {
			_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
			receiver := newHookReceiver(t)
			fields := createFields(receiver.URL(), "", testStart.Add(time.Hour))
			fields["resource"] = resource
			w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", jsonBody(fields))
			assertStatus(t, w, http.StatusCreated)
		})
	}
}

func TestCreateSubscriptionRejectsInvalidInput(t *testing.T) {
	_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	exp := testStart.Add(time.Hour)
	hook := receiver.URL()

	edit := func(change func(map[string]any)) string {
		fields := createFields(hook, "", exp)
		change(fields)
		return jsonBody(fields)
	}
	cases := []struct {
		name, body, code, message string
	}{
		{"malformed JSON", "{", "BadRequest", ""},
		{"unknown field", edit(func(f map[string]any) { f["foo"] = 1 }), "BadRequest", ""},
		{"missing changeType", edit(func(f map[string]any) { delete(f, "changeType") }), "InvalidRequest", ""},
		{"changeType updated", edit(func(f map[string]any) { f["changeType"] = "updated" }), "InvalidRequest", ""},
		{"changeType created,updated", edit(func(f map[string]any) { f["changeType"] = "created,updated" }), "InvalidRequest", ""},
		{"resource without mailFolder", edit(func(f map[string]any) { f["resource"] = "users/box@example.test/messages" }), "InvalidRequest", ""},
		{"resource on me", edit(func(f map[string]any) { f["resource"] = "me/mailFolders('inbox')/messages" }), "InvalidRequest", ""},
		{"resource on drafts", edit(func(f map[string]any) { f["resource"] = "users/box@example.test/mailFolders('drafts')/messages" }), "InvalidRequest", ""},
		{"missing expiration", edit(func(f map[string]any) { delete(f, "expirationDateTime") }), "InvalidRequest", ""},
		{"expiration not a date", edit(func(f map[string]any) { f["expirationDateTime"] = "tomorrow" }), "InvalidRequest", ""},
		{"expiration in the past", edit(func(f map[string]any) { f["expirationDateTime"] = testStart.Add(-time.Minute).Format(time.RFC3339Nano) }), "InvalidRequest", ""},
		{"expiration beyond lifetime", edit(func(f map[string]any) {
			f["expirationDateTime"] = testStart.Add(10081 * time.Minute).Format(time.RFC3339Nano)
		}), "InvalidRequest", ""},
		{"clientState of 129 characters", edit(func(f map[string]any) { f["clientState"] = strings.Repeat("x", 129) }), "InvalidRequest", ""},
		{"includeResourceData true", edit(func(f map[string]any) { f["includeResourceData"] = true }), "InvalidRequest", ""},
		{"unknown TLS version", edit(func(f map[string]any) { f["latestSupportedTlsVersion"] = "v2" }), "InvalidRequest", ""},
		{"missing notificationUrl", edit(func(f map[string]any) { delete(f, "notificationUrl") }), "InvalidRequest", ""},
		{"ftp notificationUrl", edit(func(f map[string]any) { f["notificationUrl"] = "ftp://127.0.0.1/x" }), "InvalidRequest", ""},
		{"remote notificationUrl", edit(func(f map[string]any) { f["notificationUrl"] = "http://example.test/hook" }), "InvalidRequest", "loopback"},
		{"remote lifecycleNotificationUrl", edit(func(f map[string]any) { f["lifecycleNotificationUrl"] = "http://example.test/hook" }), "InvalidRequest", "loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", tc.body)
			assertError(t, w, http.StatusBadRequest, tc.code, tc.message)
		})
	}
	if n := len(receiver.validations()); n != 0 {
		t.Fatalf("validation requests = %d, want none for rejected input", n)
	}
}

func TestCreateSubscriptionAcceptsExpirationAtLifetimeLimit(t *testing.T) {
	_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	exp := testStart.Add(10080 * time.Minute)
	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(receiver.URL(), "", exp))
	assertStatus(t, w, http.StatusCreated)
}

func TestCreateSubscriptionAcceptsClientStateAtLimit(t *testing.T) {
	_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	fields := createFields(receiver.URL(), "", testStart.Add(time.Hour))
	fields["clientState"] = strings.Repeat("é", 128)
	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", jsonBody(fields))
	assertStatus(t, w, http.StatusCreated)
}

func TestCreateSubscriptionKeepsNotificationURLQueryOnValidation(t *testing.T) {
	cases := []struct {
		name, query, wantPrefix string
	}{
		{"no query", "", "validationToken="},
		{"existing pairs kept verbatim", "b=2&a=1&flag", "b=2&a=1&flag&validationToken="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var rawQuery string
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := r.URL.Query().Get("validationToken")
				mu.Lock()
				rawQuery = r.URL.RawQuery
				mu.Unlock()
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, token)
			}))
			t.Cleanup(receiver.Close)
			_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
			notifyURL := receiver.URL + "/hook"
			if tc.query != "" {
				notifyURL += "?" + tc.query
			}
			w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(notifyURL, "", testStart.Add(time.Hour)))
			assertStatus(t, w, http.StatusCreated)
			mu.Lock()
			defer mu.Unlock()
			if !strings.HasPrefix(rawQuery, tc.wantPrefix) {
				t.Fatalf("validation query = %q, want prefix %q", rawQuery, tc.wantPrefix)
			}
		})
	}
}

func TestCreateSubscriptionAllowsRemoteURLsWhenOptedIn(t *testing.T) {
	s := New(&subscriptionMailpit{}, Config{Token: "secret", AllowRemoteNotificationURLs: true})
	s.now = (&testClock{t: testStart}).now
	s.validationTimeout = 50 * time.Millisecond
	h := s.Handler()
	// 192.0.2.0/24 is reserved for documentation: the URL passes the host
	// check, then the validation request cannot succeed.
	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions",
		createBody("http://192.0.2.1/hook", "", testStart.Add(time.Hour)))
	assertError(t, w, http.StatusBadRequest, "InvalidRequest", "Subscription validation request")
}

func TestCreateSubscriptionFailsWhenValidationFails(t *testing.T) {
	cases := []struct {
		name, notifyMode, lifecycleMode, message string
	}{
		{"server error", "status500", "echo", "Subscription validation request failed"},
		{"wrong token", "wrongToken", "echo", "Subscription validation request failed"},
		{"json content type", "json", "echo", "Subscription validation request failed"},
		{"hang", "hang", "echo", "timed out"},
		{"lifecycle server error", "echo", "status500", "Subscription validation request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
			notify := newHookReceiver(t)
			notify.setMode(tc.notifyMode)
			lifecycle := newHookReceiver(t)
			lifecycle.setMode(tc.lifecycleMode)

			start := time.Now()
			w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(notify.URL(), lifecycle.URL(), testStart.Add(time.Hour)))
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("create took %s, want a bounded validation", elapsed)
			}
			assertError(t, w, http.StatusBadRequest, "InvalidRequest", tc.message)

			list := authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", "")
			var listed struct {
				Value []subscriptionResponse `json:"value"`
			}
			decodeResponse(t, list, &listed)
			if len(listed.Value) != 0 {
				t.Fatalf("list after failed validation = %+v, want empty", listed.Value)
			}
		})
	}
}

func TestCreateSubscriptionReportsMailpitFailure(t *testing.T) {
	fake := &subscriptionMailpit{listErr: errors.New("mailpit down")}
	_, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(receiver.URL(), "", testStart.Add(time.Hour)))
	assertStatus(t, w, http.StatusBadGateway)
	if n := len(receiver.validations()); n != 0 {
		t.Fatalf("validation requests = %d, want none when Mailpit is down", n)
	}
}

func TestDuplicateSubscriptionConflicts(t *testing.T) {
	_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	body := createBody(receiver.URL(), "", testStart.Add(time.Hour))
	first := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", body)
	assertStatus(t, first, http.StatusCreated)
	var created subscriptionResponse
	decodeResponse(t, first, &created)

	second := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", body)
	assertError(t, second, http.StatusConflict, "Conflict", created.ID)
}

func TestCreateSubscriptionRejectsExpirationPassedDuringValidation(t *testing.T) {
	_, h, clock := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	receiver.setOnValidate(func() { clock.advance(2 * time.Minute) })

	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(receiver.URL(), "", testStart.Add(time.Minute)))
	assertError(t, w, http.StatusBadRequest, "InvalidRequest", "expirationDateTime must be in the future")

	var listed struct {
		Value []subscriptionResponse `json:"value"`
	}
	decodeResponse(t, authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", ""), &listed)
	if len(listed.Value) != 0 {
		t.Fatalf("list = %+v, want empty", listed.Value)
	}
}

func TestCreateSubscriptionReplacesOneExpiredDuringValidation(t *testing.T) {
	_, h, clock := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	old := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Minute))
	receiver.setOnValidate(func() { clock.advance(2 * time.Minute) })

	w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(receiver.URL(), "", testStart.Add(time.Hour)))
	assertStatus(t, w, http.StatusCreated)
	var created subscriptionResponse
	decodeResponse(t, w, &created)

	var listed struct {
		Value []subscriptionResponse `json:"value"`
	}
	decodeResponse(t, authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", ""), &listed)
	if len(listed.Value) != 1 || listed.Value[0].ID != created.ID || created.ID == old {
		t.Fatalf("list = %+v, want only the new subscription", listed.Value)
	}
}

func TestNotifiesOnceForNewInboxMessages(t *testing.T) {
	fake := &subscriptionMailpit{}
	fake.add(inboxMessage("old", "box@example.test", testStart))
	s, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	exp := testStart.Add(time.Hour)
	id := createTestSubscription(t, h, receiver.URL(), exp)

	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))
	fake.add(inboxMessage("other", "other@example.test", testStart.Add(time.Minute)))
	draft := inboxMessage("draft", "box@example.test", testStart.Add(time.Minute))
	draft.From = mailpit.Address{Address: "box@example.test"}
	draft.Tags = []string{"graph-draft"}
	fake.add(draft)

	s.pollOnce(context.Background())
	s.pollOnce(context.Background())

	notifications := receiver.notifications()
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want exactly 1", len(notifications))
	}
	n := decodeNotification(t, notifications[0])
	resource := "Users/box@example.test/Messages/new"
	if n.SubscriptionID != id || n.ChangeType != "created" || n.TenantID != simulatedTenantID {
		t.Fatalf("unexpected notification %+v", n)
	}
	if n.ClientState == nil || *n.ClientState != "secret-state" {
		t.Fatalf("clientState = %v, want secret-state", n.ClientState)
	}
	if n.Resource != resource || n.ResourceData.ID != "new" || n.ResourceData.ODataID != resource {
		t.Fatalf("unexpected resource in notification %+v", n)
	}
	if n.ResourceData.ODataType != "#Microsoft.Graph.Message" {
		t.Fatalf("@odata.type = %q", n.ResourceData.ODataType)
	}
	if !n.SubscriptionExpirationDateTime.Equal(exp) {
		t.Fatalf("subscriptionExpirationDateTime = %s, want %s", n.SubscriptionExpirationDateTime, exp)
	}
	if stats := localStats(t, h, id); stats.DeliveredNotifications != 1 {
		t.Fatalf("delivered = %d, want 1", stats.DeliveredNotifications)
	}
}

func TestPollRecoversFromMailpitFailure(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))

	fake.setListErr(errors.New("mailpit down"))
	s.pollOnce(context.Background())
	if n := len(receiver.notifications()); n != 0 {
		t.Fatalf("notifications during outage = %d, want 0", n)
	}

	fake.setListErr(nil)
	s.pollOnce(context.Background())
	if n := len(receiver.notifications()); n != 1 {
		t.Fatalf("notifications after recovery = %d, want 1", n)
	}
}

func TestFailedDeliveryIsCountedNotRetried(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	receiver.setNotifyStatus(http.StatusInternalServerError)
	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))

	s.pollOnce(context.Background())
	s.pollOnce(context.Background())

	if n := len(receiver.notifications()); n != 1 {
		t.Fatalf("deliveries = %d, want exactly 1", n)
	}
	stats := localStats(t, h, id)
	if stats.FailedNotifications != 1 || stats.DeliveredNotifications != 0 {
		t.Fatalf("stats = %+v, want 1 failed and 0 delivered", stats)
	}
}

func TestExpiredSubscriptionStopsNotifying(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, clock := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))
	clock.advance(2 * time.Hour)

	calls := fake.calls()
	s.pollOnce(context.Background())

	if n := len(receiver.notifications()); n != 0 {
		t.Fatalf("notifications after expiry = %d, want 0", n)
	}
	assertStatus(t, authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions/"+id, ""), http.StatusNotFound)
	var listed struct {
		Value []subscriptionResponse `json:"value"`
	}
	decodeResponse(t, authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", ""), &listed)
	if len(listed.Value) != 0 {
		t.Fatalf("list after expiry = %+v, want empty", listed.Value)
	}
	if fake.calls() != calls {
		t.Fatal("Mailpit was polled with no active subscription")
	}
}

func TestSubscriptionExpiringDuringPollStopsDelivery(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, clock := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	fake.add(inboxMessage("first", "box@example.test", testStart.Add(time.Minute)))
	fake.add(inboxMessage("second", "box@example.test", testStart.Add(2*time.Minute)))
	receiver.setOnNotify(func() { clock.advance(2 * time.Hour) })

	s.pollOnce(context.Background())

	if n := len(receiver.notifications()); n != 1 {
		t.Fatalf("notifications = %d, want 1: the expired subscription must not deliver the second message", n)
	}
}

func TestRenewSubscription(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, clock := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	path := "/v1.0/subscriptions/" + id

	renewed := testStart.Add(2 * time.Hour)
	w := authorizedRequest(h, http.MethodPatch, path, jsonBody(map[string]any{"expirationDateTime": renewed.Format(time.RFC3339Nano)}))
	assertStatus(t, w, http.StatusOK)
	var sub subscriptionResponse
	decodeResponse(t, w, &sub)
	if !sub.ExpirationDateTime.Equal(renewed) {
		t.Fatalf("expirationDateTime = %s, want %s", sub.ExpirationDateTime, renewed)
	}

	clock.advance(90 * time.Minute)
	fake.add(inboxMessage("new", "box@example.test", clock.now()))
	s.pollOnce(context.Background())
	if n := len(receiver.notifications()); n != 1 {
		t.Fatalf("notifications after renewal = %d, want 1", n)
	}

	invalid := []struct {
		name string
		body string
	}{
		{"past", jsonBody(map[string]any{"expirationDateTime": clock.now().Add(-time.Minute).Format(time.RFC3339Nano)})},
		{"beyond lifetime", jsonBody(map[string]any{"expirationDateTime": clock.now().Add(10081 * time.Minute).Format(time.RFC3339Nano)})},
		{"notificationUrl", jsonBody(map[string]any{"expirationDateTime": renewed.Format(time.RFC3339Nano), "notificationUrl": "http://127.0.0.1/x"})},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			assertError(t, authorizedRequest(h, http.MethodPatch, path, tc.body), http.StatusBadRequest, "InvalidRequest", "")
		})
	}

	missing := jsonBody(map[string]any{"expirationDateTime": renewed.Format(time.RFC3339Nano)})
	assertStatus(t, authorizedRequest(h, http.MethodPatch, "/v1.0/subscriptions/unknown", missing), http.StatusNotFound)
}

// readHook runs onRead once, when the request body is first read.
type readHook struct {
	io.Reader
	onRead func()
}

func (h *readHook) Read(p []byte) (int, error) {
	if h.onRead != nil {
		h.onRead()
		h.onRead = nil
	}
	return h.Reader.Read(p)
}

func TestRenewRejectsSubscriptionExpiredDuringRequest(t *testing.T) {
	_, h, clock := newSubscriptionServer(t, &subscriptionMailpit{})
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	path := "/v1.0/subscriptions/" + id
	body := jsonBody(map[string]any{"expirationDateTime": testStart.Add(3 * time.Hour).Format(time.RFC3339Nano)})

	r := httptest.NewRequest(http.MethodPatch, path, &readHook{Reader: strings.NewReader(body), onRead: func() { clock.advance(2 * time.Hour) }})
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	assertError(t, w, http.StatusNotFound, "ResourceNotFound", "Subscription not found")
	assertError(t, authorizedRequest(h, http.MethodPatch, "/v1.0/subscriptions/unknown", body), http.StatusNotFound, "ResourceNotFound", "Subscription not found")
	assertStatus(t, authorizedRequest(h, http.MethodGet, path, ""), http.StatusNotFound)
	var listed struct {
		Value []subscriptionResponse `json:"value"`
	}
	decodeResponse(t, authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions", ""), &listed)
	if len(listed.Value) != 0 {
		t.Fatalf("list after expiry = %+v, want empty", listed.Value)
	}
}

func TestDeleteSubscription(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	path := "/v1.0/subscriptions/" + id

	assertStatus(t, authorizedRequest(h, http.MethodDelete, path, ""), http.StatusNoContent)
	assertStatus(t, authorizedRequest(h, http.MethodGet, path, ""), http.StatusNotFound)
	assertStatus(t, authorizedRequest(h, http.MethodDelete, path, ""), http.StatusNotFound)

	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))
	s.pollOnce(context.Background())
	if n := len(receiver.notifications()); n != 0 {
		t.Fatalf("notifications after delete = %d, want 0", n)
	}
}

func TestDropNotifications(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, _ := newSubscriptionServer(t, fake)
	receiver := newHookReceiver(t)
	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	dropPath := "/local/subscriptions/" + id + "/dropNotifications"

	w := authorizedRequest(h, http.MethodPost, dropPath, `{"count":1}`)
	assertStatus(t, w, http.StatusOK)
	var pending subscriptionStats
	decodeResponse(t, w, &pending)
	if pending.PendingDrops != 1 {
		t.Fatalf("pendingDrops = %d, want 1", pending.PendingDrops)
	}

	created := testStart.Add(time.Minute)
	fake.add(inboxMessage("first", "box@example.test", created))
	fake.add(inboxMessage("second", "box@example.test", created.Add(time.Second)))
	s.pollOnce(context.Background())

	notifications := receiver.notifications()
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifications))
	}
	if n := decodeNotification(t, notifications[0]); n.ResourceData.ID != "second" {
		t.Fatalf("notified message = %q, want second", n.ResourceData.ID)
	}
	stats := localStats(t, h, id)
	if stats.DroppedNotifications != 1 || stats.DeliveredNotifications != 1 || stats.PendingDrops != 0 {
		t.Fatalf("stats = %+v, want 1 dropped, 1 delivered, 0 pending", stats)
	}

	for _, body := range []string{`{"count":0}`, `{"count":-1}`, `{}`} {
		assertError(t, authorizedRequest(h, http.MethodPost, dropPath, body), http.StatusBadRequest, "BadRequest", "count must be a positive integer")
	}
	assertStatus(t, authorizedRequest(h, http.MethodPost, "/local/subscriptions/unknown/dropNotifications", `{"count":1}`), http.StatusNotFound)

	r := httptest.NewRequest(http.MethodPost, dropPath, strings.NewReader(`{"count":1}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertStatus(t, w, http.StatusUnauthorized)
}

func TestLifecycleNotifications(t *testing.T) {
	cases := []struct {
		event   string
		removed bool
	}{
		{"reauthorizationRequired", false},
		{"missed", false},
		{"subscriptionRemoved", true},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
			lifecycle := newHookReceiver(t)
			exp := testStart.Add(time.Hour)
			w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(newHookReceiver(t).URL(), lifecycle.URL(), exp))
			assertStatus(t, w, http.StatusCreated)
			var created subscriptionResponse
			decodeResponse(t, w, &created)

			w = authorizedRequest(h, http.MethodPost, "/local/subscriptions/"+created.ID+"/lifecycleNotifications",
				jsonBody(map[string]string{"lifecycleEvent": tc.event}))
			assertStatus(t, w, http.StatusNoContent)

			notifications := lifecycle.notifications()
			if len(notifications) != 1 {
				t.Fatalf("lifecycle deliveries = %d, want 1", len(notifications))
			}
			var payload notificationCollection[lifecycleNotification]
			if err := json.Unmarshal(notifications[0], &payload); err != nil || len(payload.Value) != 1 {
				t.Fatalf("decode lifecycle payload %s: %v", notifications[0], err)
			}
			got := payload.Value[0]
			if got.SubscriptionID != created.ID || got.LifecycleEvent != tc.event || got.TenantID != simulatedTenantID {
				t.Fatalf("unexpected lifecycle notification %+v", got)
			}
			if got.ClientState == nil || *got.ClientState != "secret-state" {
				t.Fatalf("clientState = %v, want secret-state", got.ClientState)
			}
			if !got.SubscriptionExpirationDateTime.Equal(exp) {
				t.Fatalf("subscriptionExpirationDateTime = %s, want %s", got.SubscriptionExpirationDateTime, exp)
			}

			after := authorizedRequest(h, http.MethodGet, "/v1.0/subscriptions/"+created.ID, "")
			if tc.removed {
				assertStatus(t, after, http.StatusNotFound)
			} else {
				assertStatus(t, after, http.StatusOK)
			}
		})
	}

	t.Run("errors", func(t *testing.T) {
		_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
		lifecycle := newHookReceiver(t)
		w := authorizedRequest(h, http.MethodPost, "/v1.0/subscriptions", createBody(newHookReceiver(t).URL(), lifecycle.URL(), testStart.Add(time.Hour)))
		assertStatus(t, w, http.StatusCreated)
		var created subscriptionResponse
		decodeResponse(t, w, &created)
		path := "/local/subscriptions/" + created.ID + "/lifecycleNotifications"

		assertError(t, authorizedRequest(h, http.MethodPost, path, `{"lifecycleEvent":"expired"}`), http.StatusBadRequest, "BadRequest", "lifecycleEvent must be")

		_, plainHandler, _ := newSubscriptionServer(t, &subscriptionMailpit{})
		plain := createTestSubscription(t, plainHandler, newHookReceiver(t).URL(), testStart.Add(time.Hour))
		assertError(t, authorizedRequest(plainHandler, http.MethodPost, "/local/subscriptions/"+plain+"/lifecycleNotifications",
			`{"lifecycleEvent":"missed"}`), http.StatusBadRequest, "InvalidRequest", "no lifecycleNotificationUrl")

		lifecycle.setNotifyStatus(http.StatusInternalServerError)
		assertError(t, authorizedRequest(h, http.MethodPost, path, `{"lifecycleEvent":"missed"}`),
			http.StatusBadGateway, "NotificationDeliveryFailed", "Lifecycle notification delivery failed")

		assertStatus(t, authorizedRequest(h, http.MethodPost, "/local/subscriptions/unknown/lifecycleNotifications",
			`{"lifecycleEvent":"missed"}`), http.StatusNotFound)
	})
}

func TestWebhookFailuresKeepURLSecretsOutOfLogs(t *testing.T) {
	const secret = "s3cr3t-query-token"
	withSecret := func(u string) string { return u + "?code=" + secret }
	type env struct {
		s                 *Server
		h                 http.Handler
		fake              *subscriptionMailpit
		notify, lifecycle *hookReceiver
	}
	create := func(t *testing.T, e env) *httptest.ResponseRecorder {
		t.Helper()
		return authorizedRequest(e.h, http.MethodPost, "/v1.0/subscriptions",
			createBody(withSecret(e.notify.URL()), withSecret(e.lifecycle.URL()), testStart.Add(time.Hour)))
	}
	createOK := func(t *testing.T, e env) string {
		t.Helper()
		w := create(t, e)
		assertStatus(t, w, http.StatusCreated)
		var created subscriptionResponse
		decodeResponse(t, w, &created)
		return created.ID
	}
	deliver := func(t *testing.T, e env, breakHook func()) *httptest.ResponseRecorder {
		t.Helper()
		createOK(t, e)
		breakHook()
		e.fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))
		e.s.pollOnce(context.Background())
		return nil
	}
	lifecycle := func(t *testing.T, e env, breakHook func()) *httptest.ResponseRecorder {
		t.Helper()
		id := createOK(t, e)
		breakHook()
		return authorizedRequest(e.h, http.MethodPost, "/local/subscriptions/"+id+"/lifecycleNotifications", `{"lifecycleEvent":"missed"}`)
	}
	cases := []struct {
		name, want string
		run        func(*testing.T, env) *httptest.ResponseRecorder
	}{
		{"validation status", "unexpected status 500", func(t *testing.T, e env) *httptest.ResponseRecorder {
			e.notify.setMode("status500")
			return create(t, e)
		}},
		{"validation timeout", "timeout", func(t *testing.T, e env) *httptest.ResponseRecorder {
			e.notify.setMode("hang")
			return create(t, e)
		}},
		{"validation connection failed", "connection failed", func(t *testing.T, e env) *httptest.ResponseRecorder {
			e.notify.server.Close()
			return create(t, e)
		}},
		{"lifecycle validation status", "unexpected status 500", func(t *testing.T, e env) *httptest.ResponseRecorder {
			e.lifecycle.setMode("status500")
			return create(t, e)
		}},
		{"notification status", "unexpected status 500", func(t *testing.T, e env) *httptest.ResponseRecorder {
			return deliver(t, e, func() { e.notify.setNotifyStatus(http.StatusInternalServerError) })
		}},
		{"notification connection failed", "connection failed", func(t *testing.T, e env) *httptest.ResponseRecorder {
			return deliver(t, e, e.notify.server.Close)
		}},
		{"lifecycle status", "unexpected status 500", func(t *testing.T, e env) *httptest.ResponseRecorder {
			return lifecycle(t, e, func() { e.lifecycle.setNotifyStatus(http.StatusInternalServerError) })
		}},
		{"lifecycle connection failed", "connection failed", func(t *testing.T, e env) *httptest.ResponseRecorder {
			return lifecycle(t, e, e.lifecycle.server.Close)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &subscriptionMailpit{}
			s, h, _, logs := newLoggedSubscriptionServer(t, fake)
			w := tc.run(t, env{s: s, h: h, fake: fake, notify: newHookReceiver(t), lifecycle: newHookReceiver(t)})
			if w != nil && w.Code < 400 {
				t.Fatalf("status = %d, want a failure", w.Code)
			}
			if w != nil && strings.Contains(w.Body.String(), secret) {
				t.Fatalf("response leaks the URL secret: %s", w.Body.String())
			}
			out := logs.String()
			if strings.Contains(out, secret) {
				t.Fatalf("logs leak the URL secret:\n%s", out)
			}
			if !strings.Contains(out, "error="+strconv.Quote(tc.want)) && !strings.Contains(out, "error="+tc.want) {
				t.Fatalf("logs lack error class %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestRunNotificationsPollsOnlyWhileSubscribed(t *testing.T) {
	fake := &subscriptionMailpit{}
	s, h, _ := newSubscriptionServer(t, fake)
	s.pollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.RunNotifications(ctx)

	receiver := newHookReceiver(t)
	time.Sleep(20 * time.Millisecond)
	if calls := fake.calls(); calls != 0 {
		t.Fatalf("Mailpit polled %d times with no subscription", calls)
	}

	id := createTestSubscription(t, h, receiver.URL(), testStart.Add(time.Hour))
	fake.add(inboxMessage("new", "box@example.test", testStart.Add(time.Minute)))
	receiver.waitNotification(t)

	assertStatus(t, authorizedRequest(h, http.MethodDelete, "/v1.0/subscriptions/"+id, ""), http.StatusNoContent)
	time.Sleep(5 * time.Millisecond)
	calls := fake.calls()
	time.Sleep(20 * time.Millisecond)
	if fake.calls() != calls {
		t.Fatalf("Mailpit polled after the last subscription was deleted: %d -> %d", calls, fake.calls())
	}
}

func TestSubscriptionRoutesRequireBearer(t *testing.T) {
	_, h, _ := newSubscriptionServer(t, &subscriptionMailpit{})
	for _, path := range []string{"/v1.0/subscriptions", "/local/subscriptions/x"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assertStatus(t, w, http.StatusUnauthorized)
	}
}
