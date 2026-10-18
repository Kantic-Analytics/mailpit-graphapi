package graphapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

type deletedItemsMailpit struct {
	messages   []mailpit.MessageSummary
	tags       map[string][]string
	read       map[string]bool
	listErr    error
	getErr     error
	tagsErr    error
	headErr    error
	writes     int
	readWrites int
	sends      int
}

func (f *deletedItemsMailpit) List(_ context.Context, start, limit int) (mailpit.ListResponse, error) {
	if f.listErr != nil {
		return mailpit.ListResponse{}, f.listErr
	}
	messages := append([]mailpit.MessageSummary(nil), f.messages...)
	for i := range messages {
		messages[i].Tags = append([]string(nil), f.tags[messages[i].ID]...)
		messages[i].Read = f.read[messages[i].ID]
	}
	if start >= len(messages) {
		messages = nil
	} else {
		end := min(start+limit, len(messages))
		messages = messages[start:end]
	}
	return mailpit.ListResponse{Messages: messages, MessagesCount: len(f.messages)}, nil
}

func (f *deletedItemsMailpit) Get(_ context.Context, id string) (mailpit.Message, error) {
	if f.getErr != nil {
		return mailpit.Message{}, f.getErr
	}
	for _, summary := range f.messages {
		if summary.ID == id {
			return mailpit.Message{ID: summary.ID, MessageID: summary.MessageID, From: summary.From,
				To: summary.To, Subject: summary.Subject, Tags: append([]string(nil), f.tags[id]...)}, nil
		}
	}
	return mailpit.Message{}, errors.New("message not found")
}

func (*deletedItemsMailpit) Raw(context.Context, string) ([]byte, error) { return nil, nil }
func (f *deletedItemsMailpit) Headers(context.Context, string) (map[string][]string, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return map[string][]string{"Message-ID": {"<stable@example.test>"}}, nil
}
func (f *deletedItemsMailpit) SetRead(_ context.Context, id string, read bool) error {
	f.readWrites++
	f.read[id] = read
	return nil
}
func (f *deletedItemsMailpit) SetTags(_ context.Context, id string, tags []string) error {
	if f.tagsErr != nil {
		return f.tagsErr
	}
	f.tags[id] = append([]string(nil), tags...)
	f.writes++
	return nil
}
func (f *deletedItemsMailpit) Send(context.Context, mailpit.SendRequest) (string, error) {
	f.sends++
	return "", nil
}
func (*deletedItemsMailpit) Ready(context.Context) error { return nil }

func deletedItemSummary(id, mailbox string, read bool, tags []string) mailpit.MessageSummary {
	return mailpit.MessageSummary{
		ID: id, MessageID: "<" + id + "@example.test>", Read: read,
		From: mailpit.Address{Address: "sender@example.test"},
		To:   []mailpit.Address{{Address: mailbox}}, Subject: "Deleted candidate",
		Created: time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC), Tags: tags,
	}
}

func deletedItemsRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDeletedItemsBuiltInFolderRoutesAndMoveRestore(t *testing.T) {
	const mailbox = "box@example.test"
	categories := categoryTags([]string{"À traiter par un humain"})
	priorTags := append(append([]string{}, categories...), "graph-draft", folderTag("SAV"))
	fake := &deletedItemsMailpit{
		messages: []mailpit.MessageSummary{
			deletedItemSummary("incoming", mailbox, false, priorTags),
			deletedItemSummary("other-mailbox", "other@example.test", false, []string{folderTag("Deleted Items")}),
		},
		tags: map[string][]string{"incoming": priorTags, "other-mailbox": {folderTag("Deleted Items")}},
		read: map[string]bool{"incoming": false, "other-mailbox": false},
	}
	h := New(fake, Config{Token: "secret", Folders: []string{"deleteditems", "Deleted Items", "Deleted Archive"}}).Handler()

	list := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"deleteditems"`) || !strings.Contains(list.Body.String(), `"displayName":"Deleted Items"`) {
		t.Fatalf("root folders: %d %s", list.Code, list.Body.String())
	}
	children := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/deleteditems/childFolders", "")
	if children.Code != http.StatusOK || !strings.Contains(children.Body.String(), `"value":[]`) {
		t.Fatalf("Deleted Items children: %d %s", children.Code, children.Body.String())
	}
	for _, alias := range []string{"deleteditems", "Deleted%20Items"} {
		folder := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/"+alias, "")
		if folder.Code != http.StatusOK || !strings.Contains(folder.Body.String(), `"id":"deleteditems"`) ||
			!strings.Contains(folder.Body.String(), `"displayName":"Deleted Items"`) || !strings.Contains(folder.Body.String(), `"unreadItemCount":0`) {
			t.Fatalf("folder alias %q: %d %s", alias, folder.Code, folder.Body.String())
		}
	}

	move := deletedItemsRequest(h, http.MethodPost, "/v1.0/users/"+mailbox+"/messages/incoming/move", `{"destinationId":"Deleted Items"}`)
	if move.Code != http.StatusCreated || !strings.Contains(move.Body.String(), `"id":"incoming"`) || !strings.Contains(move.Body.String(), `"parentFolderId":"deleteditems"`) {
		t.Fatalf("move to Deleted Items: %d %s", move.Code, move.Body.String())
	}
	if fake.writes != 1 || hasTag(fake.tags["incoming"], "graph-draft") || hasTag(fake.tags["incoming"], folderTag("SAV")) ||
		!hasTag(fake.tags["incoming"], folderTag("Deleted Items")) || len(categoriesFromTags(fake.tags["incoming"])) != 1 ||
		categoriesFromTags(fake.tags["incoming"])[0] != "À traiter par un humain" || fake.read["incoming"] {
		t.Fatalf("move did not preserve only message metadata and new folder state: tags=%v read=%v writes=%d", fake.tags["incoming"], fake.read["incoming"], fake.writes)
	}
	count := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/deleteditems", "")
	if count.Code != http.StatusOK || !strings.Contains(count.Body.String(), `"unreadItemCount":1`) {
		t.Fatalf("unread count after move: %d %s", count.Code, count.Body.String())
	}

	deleted := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/deleteditems/messages", "")
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"id":"incoming"`) ||
		strings.Contains(deleted.Body.String(), `"id":"other-mailbox"`) || !strings.Contains(deleted.Body.String(), `"parentFolderId":"deleteditems"`) ||
		!strings.Contains(deleted.Body.String(), `"isRead":false`) || !strings.Contains(deleted.Body.String(), `"categories":["À traiter par un humain"]`) {
		t.Fatalf("Deleted Items listing: %d %s", deleted.Code, deleted.Body.String())
	}
	inbox := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/inbox/messages", "")
	if inbox.Code != http.StatusOK || strings.Contains(inbox.Body.String(), `"id":"incoming"`) {
		t.Fatalf("Inbox still contains moved message: %d %s", inbox.Code, inbox.Body.String())
	}

	restore := deletedItemsRequest(h, http.MethodPost, "/v1.0/users/"+mailbox+"/messages/incoming/move", `{"destinationId":"inbox"}`)
	if restore.Code != http.StatusCreated || !strings.Contains(restore.Body.String(), `"id":"incoming"`) || !strings.Contains(restore.Body.String(), `"parentFolderId":"inbox"`) ||
		hasTag(fake.tags["incoming"], folderTag("Deleted Items")) || len(categoriesFromTags(fake.tags["incoming"])) != 1 || fake.read["incoming"] {
		t.Fatalf("restore to Inbox: %d %s tags=%v read=%v", restore.Code, restore.Body.String(), fake.tags["incoming"], fake.read["incoming"])
	}
	inbox = deletedItemsRequest(h, http.MethodGet, "/v1.0/users/"+mailbox+"/mailFolders/inbox/messages", "")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"id":"incoming"`) {
		t.Fatalf("restored message not in Inbox: %d %s", inbox.Code, inbox.Body.String())
	}
}

func TestDeletedItemsFolderInferenceAndLegacyTagMigration(t *testing.T) {
	legacy := folderTagPrefix + base64.RawURLEncoding.EncodeToString([]byte("Deleted Items"))
	fake := &deletedItemsMailpit{
		messages: []mailpit.MessageSummary{
			deletedItemSummary("human", "box@example.test", false, []string{folderTag("Deleted Items")}),
			deletedItemSummary("legacy", "box@example.test", true, []string{legacy}),
		},
		tags: map[string][]string{"human": {folderTag("Deleted Items")}, "legacy": {legacy}},
		read: map[string]bool{"human": false, "legacy": true},
	}
	h := New(fake, Config{Token: "secret"}).Handler()
	listing := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems/messages", "")
	if listing.Code != http.StatusOK || !strings.Contains(listing.Body.String(), `"id":"human"`) || !strings.Contains(listing.Body.String(), `"id":"legacy"`) ||
		!strings.Contains(listing.Body.String(), `"parentFolderId":"deleteditems"`) {
		t.Fatalf("human/legacy folder inference: %d %s", listing.Code, listing.Body.String())
	}
	folder := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/Deleted%20Items", "")
	if folder.Code != http.StatusOK || !strings.Contains(folder.Body.String(), `"unreadItemCount":1`) {
		t.Fatalf("unread count: %d %s", folder.Code, folder.Body.String())
	}
	updated, err := ReconcileDisplayTags(context.Background(), fake)
	if err != nil || updated != 1 || !hasTag(fake.tags["legacy"], folderTag("Deleted Items")) {
		t.Fatalf("legacy Deleted Items tag did not migrate: updated=%d err=%v tags=%v", updated, err, fake.tags["legacy"])
	}
}

func TestDeletedItemsReservedNameAndMoveFailures(t *testing.T) {
	fake := &deletedItemsMailpit{
		messages: []mailpit.MessageSummary{deletedItemSummary("id", "box@example.test", false, nil)},
		tags:     map[string][]string{"id": {}}, read: map[string]bool{"id": false},
	}
	h := New(fake, Config{Token: "secret", Folders: []string{"Deleted Items", "deleteditems", "Deleted Archive"}}).Handler()
	children := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/inbox/childFolders", "")
	if children.Code != http.StatusOK || strings.Contains(children.Body.String(), `"displayName":"Deleted Items"`) || !strings.Contains(children.Body.String(), `"displayName":"Deleted Archive"`) {
		t.Fatalf("reserved custom folder names: %d %s", children.Code, children.Body.String())
	}

	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{name: "unknown folder", body: `{"destinationId":"Missing"}`, status: http.StatusNotFound},
		{name: "malformed body", body: `{"destinationId":`, status: http.StatusBadRequest},
	} {
		response := deletedItemsRequest(h, http.MethodPost, "/v1.0/users/box@example.test/messages/id/move", tc.body)
		if response.Code != tc.status || fake.writes != 0 {
			t.Errorf("%s: %d %s writes=%d", tc.name, response.Code, response.Body.String(), fake.writes)
		}
	}

	fake.getErr = errors.New("upstream get failed")
	response := deletedItemsRequest(h, http.MethodPost, "/v1.0/users/box@example.test/messages/id/move", `{"destinationId":"deleteditems"}`)
	if response.Code != http.StatusBadGateway || fake.writes != 0 {
		t.Fatalf("upstream get failure: %d %s writes=%d", response.Code, response.Body.String(), fake.writes)
	}
	fake.getErr = nil
	fake.tagsErr = errors.New("upstream tags failed")
	response = deletedItemsRequest(h, http.MethodPost, "/v1.0/users/box@example.test/messages/id/move", `{"destinationId":"deleteditems"}`)
	if response.Code != http.StatusBadGateway || fake.writes != 0 {
		t.Fatalf("upstream tag failure: %d %s writes=%d", response.Code, response.Body.String(), fake.writes)
	}
	fake.tagsErr = nil
	fake.listErr = errors.New("upstream list failed")
	response = deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems", "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upstream unread-count failure: %d %s", response.Code, response.Body.String())
	}
	response = deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems/messages", "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upstream listing failure: %d %s", response.Code, response.Body.String())
	}
	fake.listErr = nil
	fake.tags["id"] = []string{folderTag("Deleted Items")}
	fake.headErr = errors.New("upstream headers failed")
	response = deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems/messages", "")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("upstream headers failure: %d %s", response.Code, response.Body.String())
	}
}

func TestDeletedItemsIncomingAndOutgoingReadStateAndNoPermanentDelete(t *testing.T) {
	for _, read := range []bool{false, true} {
		fake := &deletedItemsMailpit{messages: []mailpit.MessageSummary{
			deletedItemSummary("incoming", "box@example.test", read, nil),
			deletedItemSummary("outgoing", "outside@example.test", read, nil),
			deletedItemSummary("unrelated", "other@example.test", read, []string{folderTag("Deleted Items")}),
		}, tags: map[string][]string{"incoming": categoryTags([]string{"kept"}), "outgoing": categoryTags([]string{"kept"}), "unrelated": {folderTag("Deleted Items")}}, read: map[string]bool{"incoming": read, "outgoing": read, "unrelated": read}}
		fake.messages[1].From = mailpit.Address{Address: "box@example.test"}
		h := New(fake, Config{Token: "secret"}).Handler()
		for _, id := range []string{"incoming", "outgoing"} {
			moved := deletedItemsRequest(h, http.MethodPost, "/v1.0/users/box@example.test/messages/"+id+"/move", "{\"destinationId\":\"deleteditems\"}")
			if moved.Code != http.StatusCreated {
				t.Fatalf("move: %d %s", moved.Code, moved.Body.String())
			}
			get := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/messages/"+id+"?$select=id,parentFolderId,isRead,categories", "")
			if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "\"parentFolderId\":\"deleteditems\"") {
				t.Fatalf("readback: %d %s", get.Code, get.Body.String())
			}
			unsupported := deletedItemsRequest(h, http.MethodDelete, "/v1.0/users/box@example.test/messages/"+id, "")
			if unsupported.Code != http.StatusNotFound {
				t.Fatal("permanent deletion unexpectedly supported")
			}
			if fake.read[id] != read || fake.readWrites != 0 || fake.sends != 0 || len(categoriesFromTags(fake.tags[id])) != 1 || categoriesFromTags(fake.tags[id])[0] != "kept" {
				t.Fatal("move modified read state, categories or sent a message")
			}
		}
		listing := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems/messages", "")
		if listing.Code != http.StatusOK || !strings.Contains(listing.Body.String(), "\"id\":\"incoming\"") || !strings.Contains(listing.Body.String(), "\"id\":\"outgoing\"") || strings.Contains(listing.Body.String(), "\"id\":\"unrelated\"") {
			t.Fatalf("mailbox isolation: %d %s", listing.Code, listing.Body.String())
		}
		folder := deletedItemsRequest(h, http.MethodGet, "/v1.0/users/box@example.test/mailFolders/deleteditems", "")
		wantCount := "\"unreadItemCount\":2"
		if read {
			wantCount = "\"unreadItemCount\":0"
		}
		if folder.Code != http.StatusOK || !strings.Contains(folder.Body.String(), wantCount) {
			t.Fatalf("unread count: %d %s", folder.Code, folder.Body.String())
		}
	}
}
