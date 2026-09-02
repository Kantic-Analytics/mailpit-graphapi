package graphapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

type fakeMailpit struct {
	messages  []mailpit.MessageSummary
	sent      mailpit.SendRequest
	read      *bool
	tags      []string
	tagWrites int
}

func (f *fakeMailpit) List(context.Context, int, int) (mailpit.ListResponse, error) {
	messages := append([]mailpit.MessageSummary(nil), f.messages...)
	for i := range messages {
		messages[i].Tags = append([]string(nil), f.tags...)
	}
	return mailpit.ListResponse{Messages: messages, MessagesCount: len(messages)}, nil
}
func (f *fakeMailpit) Get(_ context.Context, id string) (mailpit.Message, error) {
	return mailpit.Message{ID: id, MessageID: "<one@example.test>", From: mailpit.Address{Address: "sender@example.test"}, To: []mailpit.Address{{Address: "box@example.test"}}, Subject: "Hello", Text: "Body", Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Tags: f.tags}, nil
}
func (f *fakeMailpit) Raw(context.Context, string) ([]byte, error) {
	return []byte("From: sender@example.test\r\n\r\nBody"), nil
}
func (f *fakeMailpit) Headers(context.Context, string) (map[string][]string, error) {
	return map[string][]string{"Message-ID": {"<one@example.test>"}}, nil
}
func (f *fakeMailpit) SetRead(_ context.Context, _ string, value bool) error {
	f.read = &value
	return nil
}
func (f *fakeMailpit) SetTags(_ context.Context, _ string, tags []string) error {
	f.tags = tags
	f.tagWrites++
	return nil
}
func (f *fakeMailpit) Send(_ context.Context, m mailpit.SendRequest) (string, error) {
	f.sent = m
	return "new-id", nil
}
func (f *fakeMailpit) Ready(context.Context) error { return nil }

func TestOAuthAndList(t *testing.T) {
	fake := &fakeMailpit{messages: []mailpit.MessageSummary{{ID: "one", MessageID: "<one@example.test>", To: []mailpit.Address{{Address: "box@example.test"}}, Created: time.Now(), Subject: "Hello"}}}
	h := New(fake, Config{Token: "secret", ClientID: "client", ClientSecret: "password"}).Handler()

	tokenReq := httptest.NewRequest(http.MethodPost, "/tenant/oauth2/v2.0/token", strings.NewReader("grant_type=client_credentials&client_id=client&client_secret=password"))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRes := httptest.NewRecorder()
	h.ServeHTTP(tokenRes, tokenReq)
	if tokenRes.Code != http.StatusOK || !strings.Contains(tokenRes.Body.String(), `"access_token":"secret"`) {
		t.Fatalf("token response: %d %s", tokenRes.Code, tokenRes.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/v1.0/users/box@example.test/mailFolders/inbox/messages?$top=10", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list response: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Value []map[string]any `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil || len(body.Value) != 1 || body.Value[0]["id"] != "one" {
		t.Fatalf("unexpected list: %#v, %v", body, err)
	}
}

func TestUnauthorized(t *testing.T) {
	h := New(&fakeMailpit{}, Config{Token: "secret"}).Handler()
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.0/users/a/messages", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", response.Code)
	}
}

func TestSendMail(t *testing.T) {
	fake := &fakeMailpit{}
	h := New(fake, Config{Token: "secret"}).Handler()
	body := `{"message":{"subject":"Reply","body":{"contentType":"Text","content":"OK"},"toRecipients":[{"emailAddress":{"address":"to@example.test"}}],"conversationId":"thread-1"}}`
	r := httptest.NewRequest(http.MethodPost, "/v1.0/users/from@example.test/sendMail", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		b, _ := io.ReadAll(w.Body)
		t.Fatalf("got %d: %s", w.Code, b)
	}
	if fake.sent.From.Email != "from@example.test" || fake.sent.To[0].Email != "to@example.test" || fake.sent.Headers["X-Graph-Conversation-ID"] != "thread-1" || !hasTag(fake.sent.Tags, "graph-sent") {
		t.Fatalf("unexpected send: %#v", fake.sent)
	}
}

func TestPatchReadAndCategories(t *testing.T) {
	fake := &fakeMailpit{messages: []mailpit.MessageSummary{{ID: "id-1", MessageID: "<id-1@example.test>", To: []mailpit.Address{{Address: "box@example.test"}}}}}
	h := New(fake, Config{Token: "secret"}).Handler()
	r := httptest.NewRequest(http.MethodPatch, "/v1.0/users/box@example.test/messages/id-1", strings.NewReader(`{"isRead":true,"categories":["done"]}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || fake.read == nil || !*fake.read || len(categoriesFromTags(fake.tags)) != 1 || categoriesFromTags(fake.tags)[0] != "done" {
		t.Fatalf("response=%d read=%v tags=%v", w.Code, fake.read, fake.tags)
	}
}

func TestCategoriesRoundTripUnicodeThroughMailpitSafeTags(t *testing.T) {
	categories := []string{"Traité par IA", "À traiter par un humain"}
	tags := replaceCategoryTags([]string{"graph-draft", "operator-tag"}, categories)
	got := categoriesFromTags(tags)
	if len(got) != len(categories) || got[0] != categories[0] || got[1] != categories[1] {
		t.Fatalf("categories = %v, want %v (tags=%v)", got, categories, tags)
	}
	if !hasTag(tags, "graph-draft") || !hasTag(tags, "operator-tag") {
		t.Fatalf("non-category tags were lost: %v", tags)
	}
	if !hasTag(tags, "Categorie - Traite par IA") || !hasTag(tags, "Categorie - A traiter par un humain") {
		t.Fatalf("human-readable category tags missing: %v", tags)
	}
}

func TestReconcileDisplayTagsUpgradesExistingMessages(t *testing.T) {
	encodedCategory := categoryTagPrefix + base64.RawURLEncoding.EncodeToString([]byte("À traiter par un humain"))
	fake := &fakeMailpit{
		messages: []mailpit.MessageSummary{{ID: "id-1"}},
		tags:     []string{encodedCategory, folderTag("SAV")},
	}

	updated, err := ReconcileDisplayTags(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if updated != 1 || fake.tagWrites != 1 {
		t.Fatalf("updated=%d writes=%d", updated, fake.tagWrites)
	}
	if !hasTag(fake.tags, "Categorie - A traiter par un humain") || !hasTag(fake.tags, "Dossier - SAV") {
		t.Fatalf("display tags missing after reconciliation: %v", fake.tags)
	}
	if got := categoriesFromTags(fake.tags); len(got) != 1 || got[0] != "À traiter par un humain" {
		t.Fatalf("Graph category changed during reconciliation: %v", got)
	}
}

func TestMoveMessageToInboxPreservesCategories(t *testing.T) {
	fake := &fakeMailpit{tags: []string{"Traitement IA en cours", "graph-draft"}}
	h := New(fake, Config{Token: "secret"}).Handler()
	r := httptest.NewRequest(http.MethodPost, "/v1.0/users/box@example.test/messages/id-1/move", strings.NewReader(`{"destinationId":"inbox"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("response=%d body=%s", w.Code, w.Body.String())
	}
	if len(fake.tags) != 1 || fake.tags[0] != "Traitement IA en cours" {
		t.Fatalf("folder tags were not isolated from categories: %v", fake.tags)
	}
	if !strings.Contains(w.Body.String(), `"id":"id-1"`) || !strings.Contains(w.Body.String(), `"parentFolderId":"inbox"`) {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
}

func TestMoveMessageRejectsUnsupportedFolder(t *testing.T) {
	h := New(&fakeMailpit{}, Config{Token: "secret"}).Handler()
	r := httptest.NewRequest(http.MethodPost, "/v1.0/users/box@example.test/messages/id-1/move", strings.NewReader(`{"destinationId":"SAV"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestCustomFoldersAreListedAndMessagesCanMoveIntoThem(t *testing.T) {
	fake := &fakeMailpit{messages: []mailpit.MessageSummary{{
		ID: "id-1", MessageID: "<id-1@example.test>",
		To: []mailpit.Address{{Address: "box@example.test"}},
	}}}
	h := New(fake, Config{Token: "secret", Folders: []string{"SUIVI", "SAV"}}).Handler()

	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	folders := request(http.MethodGet, "/v1.0/users/box@example.test/mailFolders", "")
	if folders.Code != http.StatusOK || !strings.Contains(folders.Body.String(), `"displayName":"SAV"`) {
		t.Fatalf("folder list: %d %s", folders.Code, folders.Body.String())
	}

	moved := request(http.MethodPost, "/v1.0/users/box@example.test/messages/id-1/move", `{"destinationId":"SAV"}`)
	if moved.Code != http.StatusCreated || !hasTag(fake.tags, folderTag("SAV")) || !hasTag(fake.tags, "Dossier - SAV") {
		t.Fatalf("move: %d %s tags=%v", moved.Code, moved.Body.String(), fake.tags)
	}

	listed := request(http.MethodGet, "/v1.0/users/box@example.test/mailFolders/SAV/messages", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"id-1"`) ||
		!strings.Contains(listed.Body.String(), `"parentFolderId":"SAV"`) {
		t.Fatalf("custom folder listing: %d %s", listed.Code, listed.Body.String())
	}
}

func TestMovingBetweenCustomFoldersPreservesCategories(t *testing.T) {
	fake := &fakeMailpit{
		messages: []mailpit.MessageSummary{{ID: "id-1", To: []mailpit.Address{{Address: "box@example.test"}}}},
		tags:     append(categoryTags([]string{"Traitement IA en cours"}), folderTag("SUIVI")),
	}
	h := New(fake, Config{Token: "secret", Folders: []string{"SUIVI", "SAV"}}).Handler()
	r := httptest.NewRequest(http.MethodPost, "/v1.0/users/box@example.test/messages/id-1/move", strings.NewReader(`{"destinationId":"SAV"}`))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusCreated || !hasTag(fake.tags, folderTag("SAV")) || hasTag(fake.tags, folderTag("SUIVI")) {
		t.Fatalf("move response=%d tags=%v", w.Code, fake.tags)
	}
	if !hasTag(fake.tags, "Dossier - SAV") || hasTag(fake.tags, "Dossier - SUIVI") {
		t.Fatalf("readable folder tags were not replaced: %v", fake.tags)
	}
	got := categoriesFromTags(fake.tags)
	if len(got) != 1 || got[0] != "Traitement IA en cours" {
		t.Fatalf("categories lost after move: %v", got)
	}
}
