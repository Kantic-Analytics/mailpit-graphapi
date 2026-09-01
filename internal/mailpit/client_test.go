package mailpit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetReadUsesMailpitContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/messages" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			IDs  []string `json:"IDs"`
			Read bool     `json:"Read"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.IDs) != 1 || body.IDs[0] != "abc" || !body.Read {
			t.Fatalf("unexpected body: %#v", body)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	client, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRead(context.Background(), "abc", true); err != nil {
		t.Fatal(err)
	}
}

func TestSendDecodesID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ID":"mailpit-id"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, nil)
	id, err := client.Send(context.Background(), SendRequest{From: SendAddress{Email: "sender@example.test"}})
	if err != nil || id != "mailpit-id" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}
