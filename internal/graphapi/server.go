package graphapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

const maxBodyBytes = 1 << 20
const categoryTagPrefix = "graph-category-"
const folderTagPrefix = "graph-folder-"

type Mailpit interface {
	List(ctx context.Context, start, limit int) (mailpit.ListResponse, error)
	Get(ctx context.Context, id string) (mailpit.Message, error)
	Raw(ctx context.Context, id string) ([]byte, error)
	Headers(ctx context.Context, id string) (map[string][]string, error)
	SetRead(ctx context.Context, id string, read bool) error
	SetTags(ctx context.Context, id string, tags []string) error
	Send(ctx context.Context, message mailpit.SendRequest) (string, error)
	Ready(ctx context.Context) error
}

type Config struct {
	Token        string
	ClientID     string
	ClientSecret string
	Folders      []string
	Logger       *slog.Logger
}

type Server struct {
	mp      Mailpit
	token   string
	client  string
	secret  string
	folders []string
	log     *slog.Logger
}

func New(mp Mailpit, cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{mp: mp, token: cfg.Token, client: cfg.ClientID, secret: cfg.ClientSecret,
		folders: normalizedCustomFolders(cfg.Folders), log: logger}
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(http.HandlerFunc(s.serveHTTP))
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "/readyz":
		if err := s.mp.Ready(r.Context()); err != nil {
			graphError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "Mailpit is unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") && r.Method == http.MethodPost {
		s.tokenEndpoint(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1.0/") {
		graphError(w, http.StatusNotFound, "Request_ResourceNotFound", "Unknown endpoint")
		return
	}
	if !s.authorized(r) {
		graphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken", "Access token is missing or invalid")
		return
	}
	s.graph(w, r)
}

func (s *Server) tokenEndpoint(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		graphError(w, http.StatusBadRequest, "invalid_request", "Invalid form body")
		return
	}
	if r.Form.Get("grant_type") != "client_credentials" {
		graphError(w, http.StatusBadRequest, "unsupported_grant_type", "Only client_credentials is supported")
		return
	}
	if (s.client != "" && !secureEqual(r.Form.Get("client_id"), s.client)) || (s.secret != "" && !secureEqual(r.Form.Get("client_secret"), s.secret)) {
		graphError(w, http.StatusUnauthorized, "invalid_client", "Invalid client credentials")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3600, "ext_expires_in": 3600, "access_token": s.token})
}

func (s *Server) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return got != "" && secureEqual(got, s.token)
}

func secureEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1.0" || parts[1] != "users" {
		graphError(w, http.StatusNotFound, "Request_ResourceNotFound", "Unknown endpoint")
		return
	}
	mailbox, err := url.PathUnescape(parts[2])
	if err != nil || mailbox == "" {
		graphError(w, http.StatusBadRequest, "BadRequest", "Invalid mailbox")
		return
	}
	tail := parts[3:]
	switch {
	case len(tail) == 1 && tail[0] == "mailFolders" && r.Method == http.MethodGet:
		s.folderList(w, mailbox)
	case len(tail) == 2 && tail[0] == "mailFolders" && r.Method == http.MethodGet:
		s.folder(w, r, mailbox, tail[1])
	case len(tail) == 3 && tail[0] == "mailFolders" && tail[2] == "messages" && r.Method == http.MethodGet:
		s.list(w, r, mailbox, tail[1])
	case len(tail) == 1 && tail[0] == "messages" && r.Method == http.MethodGet:
		s.list(w, r, mailbox, "all")
	case len(tail) == 2 && tail[0] == "messages" && r.Method == http.MethodGet:
		s.get(w, r, tail[1])
	case len(tail) == 3 && tail[0] == "messages" && tail[2] == "$value" && r.Method == http.MethodGet:
		s.raw(w, r, tail[1])
	case len(tail) == 2 && tail[0] == "messages" && r.Method == http.MethodPatch:
		s.patch(w, r, tail[1])
	case len(tail) == 3 && tail[0] == "messages" && tail[2] == "move" && r.Method == http.MethodPost:
		s.move(w, r, tail[1])
	case len(tail) == 1 && tail[0] == "messages" && r.Method == http.MethodPost:
		s.create(w, r, mailbox, true)
	case len(tail) == 1 && tail[0] == "sendMail" && r.Method == http.MethodPost:
		s.create(w, r, mailbox, false)
	default:
		graphError(w, http.StatusNotFound, "Request_ResourceNotFound", "Unknown endpoint")
	}
}

func (s *Server) move(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		DestinationID string `json:"destinationId"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		return
	}
	folder := s.normalizedFolder(input.DestinationID)
	if folder == "" {
		graphError(w, http.StatusNotFound, "ErrorItemNotFound", "Unknown mail folder")
		return
	}
	message, err := s.mp.Get(r.Context(), id)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	tags := withoutFolderTags(message.Tags)
	switch folder {
	case "drafts":
		tags = append(tags, "graph-draft")
	case "sentitems":
		tags = append(tags, "graph-sent")
	case "inbox":
	default:
		tags = append(tags, folderTag(folder))
	}
	if err := s.mp.SetTags(r.Context(), id, unique(tags)); err != nil {
		s.upstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "parentFolderId": folder})
}

func (s *Server) normalizedFolder(folder string) string {
	switch strings.ToLower(strings.TrimSpace(folder)) {
	case "inbox":
		return "inbox"
	case "drafts":
		return "drafts"
	case "sentitems", "sent items":
		return "sentitems"
	}
	for _, custom := range s.folders {
		if strings.EqualFold(custom, strings.TrimSpace(folder)) {
			return custom
		}
	}
	return ""
}

func withoutFolderTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if hasTag([]string{tag}, "graph-draft") || hasTag([]string{tag}, "graph-sent") ||
			strings.HasPrefix(tag, folderTagPrefix) {
			continue
		}
		out = append(out, tag)
	}
	return out
}

func (s *Server) folder(w http.ResponseWriter, r *http.Request, mailbox, folder string) {
	folder = s.normalizedFolder(folder)
	if folder == "" {
		graphError(w, http.StatusNotFound, "ErrorItemNotFound", "Unknown mail folder")
		return
	}
	list, err := s.mp.List(r.Context(), 0, 1000)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	unread := 0
	for _, m := range list.Messages {
		if s.matchesFolder(m, mailbox, folder) && !m.Read {
			unread++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": folder, "displayName": folderDisplayName(folder), "unreadItemCount": unread})
}

func (s *Server) folderList(w http.ResponseWriter, mailbox string) {
	values := []map[string]any{
		{"id": "inbox", "displayName": "Inbox"},
		{"id": "drafts", "displayName": "Drafts"},
		{"id": "sentitems", "displayName": "Sent Items"},
	}
	for _, folder := range s.folders {
		values = append(values, map[string]any{"id": folder, "displayName": folder})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users('" + mailbox + "')/mailFolders",
		"value":          values,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, mailbox, folder string) {
	all, err := s.allMessages(r)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	filtered := make([]mailpit.MessageSummary, 0, len(all))
	headersByID := make(map[string]map[string][]string)
	conversation := filterConversation(r.URL.Query().Get("$filter"))
	for _, m := range all {
		if !s.matchesFolder(m, mailbox, folder) {
			continue
		}
		if conversation != "" {
			headers, err := s.mp.Headers(r.Context(), m.ID)
			if err != nil {
				s.upstreamError(w, err)
				return
			}
			headersByID[m.ID] = headers
			if conversationIDFor(m.MessageID, headers) != conversation {
				continue
			}
		}
		if after := filterAfter(r.URL.Query().Get("$filter")); !after.IsZero() && m.Created.Before(after) {
			continue
		}
		filtered = append(filtered, m)
	}
	if strings.Contains(strings.ToLower(r.URL.Query().Get("$orderby")), "asc") {
		sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Created.Before(filtered[j].Created) })
	}
	top := queryInt(r, "$top", 50, 1, 1000)
	skip := queryInt(r, "$skip", 0, 0, len(filtered))
	end := min(skip+top, len(filtered))
	values := make([]map[string]any, 0, end-skip)
	for _, m := range filtered[skip:end] {
		headers := headersByID[m.ID]
		if headers == nil {
			var err error
			headers, err = s.mp.Headers(r.Context(), m.ID)
			if err != nil {
				s.upstreamError(w, err)
				return
			}
		}
		values = append(values, summaryToGraph(m, headers, s.folderID(folder)))
	}
	response := map[string]any{"@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users('" + mailbox + "')/messages", "value": values}
	if end < len(filtered) {
		q := r.URL.Query()
		q.Set("$skip", strconv.Itoa(end))
		response["@odata.nextLink"] = requestBase(r) + r.URL.Path + "?" + q.Encode()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) allMessages(r *http.Request) ([]mailpit.MessageSummary, error) {
	const pageSize = 1000
	var out []mailpit.MessageSummary
	for start := 0; ; start += pageSize {
		page, err := s.mp.List(r.Context(), start, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Messages...)
		if len(page.Messages) < pageSize || len(out) >= page.MessagesCount {
			return out, nil
		}
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, id string) {
	// Mailpit's full-message endpoint marks a message as read. Metadata-only
	// Graph requests are answered from the list endpoint to preserve Graph's
	// non-mutating GET semantics.
	if selected := r.URL.Query().Get("$select"); selected != "" && !strings.Contains(strings.ToLower(selected), "body") {
		m, err := s.findSummary(r, id)
		if err != nil {
			s.upstreamError(w, err)
			return
		}
		headers, err := s.mp.Headers(r.Context(), id)
		if err != nil {
			s.upstreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, summaryToGraph(m, headers, s.folderForMessage(m)))
		return
	}
	m, err := s.mp.Get(r.Context(), id)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	headers, _ := s.mp.Headers(r.Context(), id)
	writeJSON(w, http.StatusOK, fullToGraph(m, headers))
}

func (s *Server) findSummary(r *http.Request, id string) (mailpit.MessageSummary, error) {
	all, err := s.allMessages(r)
	if err != nil {
		return mailpit.MessageSummary{}, err
	}
	for _, m := range all {
		if m.ID == id {
			return m, nil
		}
	}
	return mailpit.MessageSummary{}, fmt.Errorf("message %q not found", id)
}

func (s *Server) raw(w http.ResponseWriter, r *http.Request, id string) {
	b, err := s.mp.Raw(r.Context(), id)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *Server) patch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		IsRead     *bool    `json:"isRead"`
		Categories []string `json:"categories"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		return
	}
	if body.IsRead != nil {
		if err := s.mp.SetRead(r.Context(), id, *body.IsRead); err != nil {
			s.upstreamError(w, err)
			return
		}
	}
	if body.Categories != nil {
		current, err := s.findSummary(r, id)
		if err != nil {
			s.upstreamError(w, err)
			return
		}
		if err := s.mp.SetTags(r.Context(), id, replaceCategoryTags(current.Tags, body.Categories)); err != nil {
			s.upstreamError(w, err)
			return
		}
	}
	m, err := s.findSummary(r, id)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	headers, err := s.mp.Headers(r.Context(), id)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summaryToGraph(m, headers, s.folderForMessage(m)))
}

type graphRecipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessageInput struct {
	Subject string `json:"subject"`
	Body    struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	ToRecipients  []graphRecipient `json:"toRecipients"`
	CcRecipients  []graphRecipient `json:"ccRecipients"`
	BccRecipients []graphRecipient `json:"bccRecipients"`
	ReplyTo       []graphRecipient `json:"replyTo"`
	Categories    []string         `json:"categories"`
	Conversation  string           `json:"conversationId"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, mailbox string, draft bool) {
	var envelope struct {
		Message         graphMessageInput `json:"message"`
		SaveToSentItems bool              `json:"saveToSentItems"`
	}
	if draft {
		if err := decodeBody(w, r, &envelope.Message); err != nil {
			return
		}
	} else if err := decodeBody(w, r, &envelope); err != nil {
		return
	}
	m := envelope.Message
	if !draft && len(m.ToRecipients)+len(m.CcRecipients)+len(m.BccRecipients) == 0 {
		graphError(w, http.StatusBadRequest, "ErrorInvalidRecipients", "At least one recipient is required")
		return
	}
	tags := categoryTags(m.Categories)
	if draft {
		tags = append(tags, "graph-draft")
	} else {
		tags = append(tags, "graph-sent")
	}
	headers := map[string]string{}
	if m.Conversation != "" {
		headers["X-Graph-Conversation-ID"] = m.Conversation
	}
	req := mailpit.SendRequest{From: mailpit.SendAddress{Email: mailbox}, To: sendAddresses(m.ToRecipients), Cc: sendAddresses(m.CcRecipients), Bcc: emailStrings(m.BccRecipients), ReplyTo: sendAddresses(m.ReplyTo), Subject: m.Subject, Headers: headers, Tags: unique(tags)}
	if strings.EqualFold(m.Body.ContentType, "html") {
		req.HTML = m.Body.Content
	} else {
		req.Text = m.Body.Content
	}
	id, err := s.mp.Send(r.Context(), req)
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	if draft {
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "isDraft": true, "subject": m.Subject, "categories": m.Categories})
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func sendAddresses(in []graphRecipient) []mailpit.SendAddress {
	out := make([]mailpit.SendAddress, 0, len(in))
	for _, r := range in {
		out = append(out, mailpit.SendAddress{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	return out
}

func emailStrings(in []graphRecipient) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		out = append(out, r.EmailAddress.Address)
	}
	return out
}

func summaryToGraph(m mailpit.MessageSummary, headers map[string][]string, parentFolderID string) map[string]any {
	conversation := conversationIDFor(m.MessageID, headers)
	return map[string]any{
		"id": m.ID, "internetMessageId": m.MessageID, "conversationId": conversation,
		"subject": m.Subject, "bodyPreview": m.Snippet, "receivedDateTime": m.Created,
		"sentDateTime": m.Created, "isRead": m.Read, "hasAttachments": m.Attachments > 0,
		"categories": categoriesFromTags(m.Tags), "parentFolderId": parentFolderID, "flag": map[string]string{"flagStatus": "notFlagged"},
		"internetMessageHeaders": graphHeaderList(headers), "from": graphAddress(m.From), "sender": graphAddress(m.From),
		"toRecipients": graphAddresses(m.To), "ccRecipients": graphAddresses(m.Cc), "bccRecipients": graphAddresses(m.Bcc),
	}
}

func fullToGraph(m mailpit.Message, headers map[string][]string) map[string]any {
	bodyType, body := "text", m.Text
	if m.HTML != "" {
		bodyType, body = "html", m.HTML
	}
	conversation := conversationIDFor(m.MessageID, headers)
	out := map[string]any{"id": m.ID, "internetMessageId": m.MessageID, "conversationId": conversation, "subject": m.Subject, "body": map[string]string{"contentType": bodyType, "content": body}, "bodyPreview": preview(m.Text), "receivedDateTime": m.Date, "sentDateTime": m.Date, "hasAttachments": len(m.Attachments) > 0, "categories": categoriesFromTags(m.Tags), "from": graphAddress(m.From), "sender": graphAddress(m.From), "toRecipients": graphAddresses(m.To), "ccRecipients": graphAddresses(m.Cc), "bccRecipients": graphAddresses(m.Bcc), "replyTo": graphAddresses(m.ReplyTo)}
	out["internetMessageHeaders"] = graphHeaderList(headers)
	return out
}

func graphHeaderList(headers map[string][]string) []map[string]string {
	out := make([]map[string]string, 0, len(headers))
	for name, values := range headers {
		for _, value := range values {
			out = append(out, map[string]string{"name": name, "value": value})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i]["name"] == out[j]["name"] {
			return out[i]["value"] < out[j]["value"]
		}
		return out[i]["name"] < out[j]["name"]
	})
	return out
}

func graphAddress(a mailpit.Address) map[string]any {
	return map[string]any{"emailAddress": map[string]string{"name": a.Name, "address": a.Address}}
}
func graphAddresses(in []mailpit.Address) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, a := range in {
		out = append(out, graphAddress(a))
	}
	return out
}

func belongsTo(m mailpit.MessageSummary, mailbox string) bool {
	for _, list := range [][]mailpit.Address{m.To, m.Cc, m.Bcc} {
		for _, a := range list {
			if strings.EqualFold(a.Address, mailbox) {
				return true
			}
		}
	}
	return false
}

func (s *Server) matchesFolder(m mailpit.MessageSummary, mailbox, folder string) bool {
	actual := s.folderForMessage(m)
	switch strings.ToLower(folder) {
	case "inbox":
		return belongsTo(m, mailbox) && actual == "inbox"
	case "drafts":
		return strings.EqualFold(m.From.Address, mailbox) && actual == "drafts"
	case "sentitems", "sent":
		return strings.EqualFold(m.From.Address, mailbox) && actual == "sentitems"
	case "all":
		return belongsTo(m, mailbox) || strings.EqualFold(m.From.Address, mailbox)
	default:
		return belongsTo(m, mailbox) && strings.EqualFold(actual, folder)
	}
}

func (s *Server) folderID(folder string) string {
	if normalized := s.normalizedFolder(folder); normalized != "" {
		return normalized
	}
	switch strings.ToLower(folder) {
	case "drafts":
		return "drafts"
	case "sentitems", "sent":
		return "sentitems"
	default:
		return "inbox"
	}
}

func (s *Server) folderForMessage(m mailpit.MessageSummary) string {
	if hasTag(m.Tags, "graph-draft") {
		return "drafts"
	}
	if hasTag(m.Tags, "graph-sent") {
		return "sentitems"
	}
	for _, tag := range m.Tags {
		if folder, ok := folderFromTag(tag); ok && s.normalizedFolder(folder) != "" {
			return s.normalizedFolder(folder)
		}
	}
	return "inbox"
}

func folderDisplayName(folder string) string {
	switch strings.ToLower(folder) {
	case "inbox":
		return "Inbox"
	case "drafts":
		return "Drafts"
	case "sentitems":
		return "Sent Items"
	default:
		return folder
	}
}

func normalizedCustomFolders(folders []string) []string {
	var out []string
	for _, folder := range folders {
		folder = strings.TrimSpace(folder)
		if folder == "" || strings.Contains(folder, "/") {
			continue
		}
		switch strings.ToLower(folder) {
		case "inbox", "drafts", "sentitems", "sent items":
			continue
		}
		out = append(out, folder)
	}
	return unique(out)
}

func folderTag(folder string) string {
	return folderTagPrefix + base64.RawURLEncoding.EncodeToString([]byte(folder))
}

func folderFromTag(tag string) (string, bool) {
	if !strings.HasPrefix(tag, folderTagPrefix) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tag, folderTagPrefix))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

func hasTag(tags []string, wanted string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, wanted) {
			return true
		}
	}
	return false
}

func categoryTags(categories []string) []string {
	out := make([]string, 0, len(categories))
	for _, category := range categories {
		if category = strings.TrimSpace(category); category != "" {
			out = append(out, categoryTagPrefix+base64.RawURLEncoding.EncodeToString([]byte(category)))
		}
	}
	return unique(out)
}

func categoriesFromTags(tags []string) []string {
	var out []string
	for _, tag := range tags {
		if !strings.HasPrefix(tag, categoryTagPrefix) {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tag, categoryTagPrefix))
		if err == nil && len(raw) > 0 {
			out = append(out, string(raw))
		}
	}
	return unique(out)
}

func replaceCategoryTags(tags, categories []string) []string {
	out := make([]string, 0, len(tags)+len(categories))
	for _, tag := range tags {
		if !strings.HasPrefix(tag, categoryTagPrefix) {
			out = append(out, tag)
		}
	}
	return unique(append(out, categoryTags(categories)...))
}
func unique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
func conversationID(messageID string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(messageID))))
	return hex.EncodeToString(sum[:16])
}

func conversationIDFor(messageID string, headers map[string][]string) string {
	if id := strings.TrimSpace(firstHeader(headers, "X-Graph-Conversation-ID")); id != "" {
		return id
	}
	if refs := strings.Fields(firstHeader(headers, "References")); len(refs) > 0 {
		return conversationID(refs[0])
	}
	if parent := strings.TrimSpace(firstHeader(headers, "In-Reply-To")); parent != "" {
		return conversationID(strings.Fields(parent)[0])
	}
	return conversationID(messageID)
}

var conversationFilter = regexp.MustCompile(`(?i)conversationId\s+eq\s+'([^']+)'`)
var afterFilter = regexp.MustCompile(`(?i)receivedDateTime\s+ge\s+([^\s]+)`)

func filterConversation(v string) string {
	m := conversationFilter.FindStringSubmatch(v)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}
func filterAfter(v string) time.Time {
	m := afterFilter.FindStringSubmatch(v)
	if len(m) != 2 {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, m[1])
	return t
}
func firstHeader(h map[string][]string, wanted string) string {
	for k, v := range h {
		if strings.EqualFold(k, wanted) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}
func preview(v string) string {
	v = strings.TrimSpace(v)
	r := []rune(v)
	if len(r) > 250 {
		return string(r[:250])
	}
	return v
}
func queryInt(r *http.Request, key string, fallback, low, high int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v == "http" || v == "https" {
		scheme = v
	}
	return scheme + "://" + r.Host
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		graphError(w, http.StatusBadRequest, "BadRequest", "Invalid JSON body")
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		graphError(w, http.StatusBadRequest, "BadRequest", "Only one JSON object is allowed")
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func (s *Server) upstreamError(w http.ResponseWriter, err error) {
	s.log.Warn("Mailpit request failed", "error", err)
	graphError(w, http.StatusBadGateway, "ServiceUnavailable", "Mailpit request failed")
}
func graphError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "innerError": map[string]string{"date": time.Now().UTC().Format(time.RFC3339)}}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
