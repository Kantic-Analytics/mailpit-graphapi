package graphapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/loopback"
	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

const maxSubscriptionLifetime = 10080 * time.Minute
const maxClientStateLength = 128
const simulatedTenantID = "00000000-0000-0000-0000-000000000000"
const defaultNotificationPollInterval = time.Second
const notificationDeliveryTimeout = 3 * time.Second
const subscriptionEntityContext = "https://graph.microsoft.com/v1.0/$metadata#subscriptions/$entity"
const expirationNotInFuture = "expirationDateTime must be in the future"

var inboxResource = regexp.MustCompile(`(?i)^/?users/([^/]+)/mailfolders(?:\('inbox'\)|/inbox)/messages$`)

var tlsVersions = []string{"v1_0", "v1_1", "v1_2", "v1_3"}

type subscription struct {
	ID, Resource, Mailbox, ChangeType, NotificationURL, LatestSupportedTLSVersion string
	ClientState, LifecycleNotificationURL                                         *string
	ExpirationDateTime                                                            time.Time
	seq                                                                           int
	seen                                                                          map[string]bool
	pendingDrops, delivered, failed, dropped                                      int
}

var errSubscriptionExpired = errors.New("subscription expired before it was stored")

type subscriptionStore struct {
	mu   sync.Mutex
	byID map[string]*subscription
	next int
	wake chan struct{}
}

func newSubscriptionStore() *subscriptionStore {
	return &subscriptionStore{byID: map[string]*subscription{}, wake: make(chan struct{}, 1)}
}

func (st *subscriptionStore) prune(now time.Time) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pruneLocked(now)
}

func (st *subscriptionStore) pruneLocked(now time.Time) []string {
	var expired []string
	for id, sub := range st.byID {
		if !now.Before(sub.ExpirationDateTime) {
			expired = append(expired, id)
			delete(st.byID, id)
		}
	}
	return expired
}

func (st *subscriptionStore) active() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.byID) > 0
}

// add stores sub unless it has already expired at now or duplicates an active
// subscription. Subscriptions expired at now are dropped first and returned, so
// one that lapsed during the creation request does not cause a conflict.
func (st *subscriptionStore) add(sub *subscription, now time.Time) (expired []string, conflictID string, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	expired = st.pruneLocked(now)
	if !now.Before(sub.ExpirationDateTime) {
		return expired, "", errSubscriptionExpired
	}
	for _, existing := range st.byID {
		if strings.EqualFold(existing.Mailbox, sub.Mailbox) && existing.ChangeType == sub.ChangeType {
			return expired, existing.ID, nil
		}
	}
	sub.seq = st.next
	st.next++
	st.byID[sub.ID] = sub
	select {
	case st.wake <- struct{}{}:
	default:
	}
	return expired, "", nil
}

func (st *subscriptionStore) get(id string) (subscription, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sub, ok := st.byID[id]
	if !ok {
		return subscription{}, false
	}
	return *sub, true
}

func (st *subscriptionStore) list() []subscription {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]subscription, 0, len(st.byID))
	for _, sub := range st.byID {
		out = append(out, *sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

func (st *subscriptionStore) renew(id string, exp time.Time) (subscription, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sub, ok := st.byID[id]
	if !ok {
		return subscription{}, false
	}
	sub.ExpirationDateTime = exp
	return *sub, true
}

func (st *subscriptionStore) remove(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.byID[id]; !ok {
		return false
	}
	delete(st.byID, id)
	return true
}

func (st *subscriptionStore) dropNext(id string, n int) (subscription, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sub, ok := st.byID[id]
	if !ok {
		return subscription{}, false
	}
	sub.pendingDrops += n
	return *sub, true
}

func (st *subscriptionStore) claim(id, messageID string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	sub, ok := st.byID[id]
	if !ok || !now.Before(sub.ExpirationDateTime) || sub.seen[messageID] {
		return false
	}
	sub.seen[messageID] = true
	if sub.pendingDrops > 0 {
		sub.pendingDrops--
		sub.dropped++
		return false
	}
	return true
}

func (st *subscriptionStore) recordDelivery(id string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sub, found := st.byID[id]
	if !found {
		return
	}
	if ok {
		sub.delivered++
	} else {
		sub.failed++
	}
}

func (s *Server) pruneExpired() {
	s.logExpired(s.subs.prune(s.now()))
}

func (s *Server) logExpired(ids []string) {
	for _, id := range ids {
		s.log.Info("Subscription expired", "subscription", id)
	}
}

// RunNotifications polls Mailpit and delivers change notifications until ctx is done. It polls only while at least one subscription is active.
func (s *Server) RunNotifications(ctx context.Context) {
	for {
		s.pruneExpired()
		if !s.subs.active() {
			select {
			case <-ctx.Done():
				return
			case <-s.subs.wake:
				continue
			}
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.pollOnce(ctx)
		}
	}
}

func (s *Server) pollOnce(ctx context.Context) {
	s.pruneExpired()
	if !s.subs.active() {
		return
	}
	all, err := s.allMessages(ctx)
	if err != nil {
		s.log.Warn("Mailpit poll for change notifications failed", "error", err)
		return
	}
	for _, sub := range s.subs.list() {
		var inbox []mailpit.MessageSummary
		for _, m := range all {
			if s.matchesFolder(m, sub.Mailbox, "inbox") {
				inbox = append(inbox, m)
			}
		}
		sort.SliceStable(inbox, func(i, j int) bool { return inbox[i].Created.Before(inbox[j].Created) })
		for _, m := range inbox {
			if ctx.Err() != nil {
				return
			}
			if !s.subs.claim(sub.ID, m.ID, s.now()) {
				continue
			}
			err := s.postNotification(ctx, sub.NotificationURL, notificationCollection[changeNotification]{
				Value: []changeNotification{newChangeNotification(sub, m.ID)},
			})
			s.subs.recordDelivery(sub.ID, err == nil)
			if err != nil {
				s.log.Warn("Change notification delivery failed", "subscription", sub.ID, "message", m.ID, "error", err)
			}
		}
	}
}

func (s *Server) postNotification(ctx context.Context, rawURL string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, notificationDeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.outbound.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notification endpoint returned %s", resp.Status)
	}
	return nil
}

func (s *Server) validateEndpoint(ctx context.Context, rawURL string) error {
	token := "Validation: Testing client application reachability for subscription Request-Id: " + newGUID()
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	validation := "validationToken=" + url.QueryEscape(token)
	if u.RawQuery == "" {
		u.RawQuery = validation
	} else {
		u.RawQuery += "&" + validation
	}
	ctx, cancel := context.WithTimeout(ctx, s.validationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := s.outbound.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("validation endpoint returned %s", resp.Status)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/plain" {
		return errors.New("validation response is not text/plain")
	}
	if string(body) != token {
		return errors.New("validation response does not echo the token")
	}
	return nil
}

func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func inboxMailbox(resource string) string {
	m := inboxResource.FindStringSubmatch(resource)
	if m == nil {
		return ""
	}
	mailbox, err := url.PathUnescape(m[1])
	if err != nil {
		return ""
	}
	return mailbox
}

func (s *Server) checkHookURL(field, raw string) string {
	if raw == "" {
		return field + " is required"
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return field + " must be an absolute http or https URL"
	}
	if !s.allowRemoteHooks && !loopback.IsHTTPURL(raw) {
		return field + " must be a loopback URL; start the simulator with --allow-remote-notification-urls to allow other hosts"
	}
	return ""
}

func (s *Server) parseExpiration(raw string) (time.Time, string) {
	if raw == "" {
		return time.Time{}, "expirationDateTime is required"
	}
	exp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, "expirationDateTime must be an ISO 8601 date-time with a time zone"
	}
	now := s.now()
	if !exp.After(now) {
		return time.Time{}, expirationNotInFuture
	}
	if exp.After(now.Add(maxSubscriptionLifetime)) {
		return time.Time{}, "Subscription expiration can only be 10080 minutes in the future."
	}
	return exp.UTC(), ""
}

type notificationCollection[T any] struct {
	Value []T `json:"value"`
}

type changeNotification struct {
	SubscriptionID                 string                   `json:"subscriptionId"`
	SubscriptionExpirationDateTime time.Time                `json:"subscriptionExpirationDateTime"`
	ChangeType                     string                   `json:"changeType"`
	Resource                       string                   `json:"resource"`
	ResourceData                   notificationResourceData `json:"resourceData"`
	ClientState                    *string                  `json:"clientState"`
	TenantID                       string                   `json:"tenantId"`
}

type notificationResourceData struct {
	ODataType string `json:"@odata.type"`
	ODataID   string `json:"@odata.id"`
	ID        string `json:"id"`
}

type lifecycleNotification struct {
	SubscriptionID                 string    `json:"subscriptionId"`
	SubscriptionExpirationDateTime time.Time `json:"subscriptionExpirationDateTime"`
	TenantID                       string    `json:"tenantId"`
	ClientState                    *string   `json:"clientState"`
	LifecycleEvent                 string    `json:"lifecycleEvent"`
}

func newChangeNotification(sub subscription, messageID string) changeNotification {
	resource := "Users/" + sub.Mailbox + "/Messages/" + messageID
	return changeNotification{
		SubscriptionID:                 sub.ID,
		SubscriptionExpirationDateTime: sub.ExpirationDateTime,
		ChangeType:                     sub.ChangeType,
		Resource:                       resource,
		ResourceData:                   notificationResourceData{ODataType: "#Microsoft.Graph.Message", ODataID: resource, ID: messageID},
		ClientState:                    sub.ClientState,
		TenantID:                       simulatedTenantID,
	}
}

type subscriptionJSON struct {
	ODataContext              string    `json:"@odata.context,omitempty"`
	ID                        string    `json:"id"`
	Resource                  string    `json:"resource"`
	ApplicationID             *string   `json:"applicationId"`
	ChangeType                string    `json:"changeType"`
	ClientState               *string   `json:"clientState"`
	NotificationURL           string    `json:"notificationUrl"`
	LifecycleNotificationURL  *string   `json:"lifecycleNotificationUrl"`
	ExpirationDateTime        time.Time `json:"expirationDateTime"`
	CreatorID                 *string   `json:"creatorId"`
	IncludeResourceData       bool      `json:"includeResourceData"`
	LatestSupportedTLSVersion string    `json:"latestSupportedTlsVersion"`
	EncryptionCertificate     *string   `json:"encryptionCertificate"`
	EncryptionCertificateID   *string   `json:"encryptionCertificateId"`
	NotificationQueryOptions  *string   `json:"notificationQueryOptions"`
	NotificationURLAppID      *string   `json:"notificationUrlAppId"`
}

func (s *Server) subscriptionJSON(sub subscription, odataContext string) subscriptionJSON {
	var applicationID *string
	if s.client != "" {
		applicationID = &s.client
	}
	return subscriptionJSON{
		ODataContext:              odataContext,
		ID:                        sub.ID,
		Resource:                  sub.Resource,
		ApplicationID:             applicationID,
		ChangeType:                sub.ChangeType,
		ClientState:               sub.ClientState,
		NotificationURL:           sub.NotificationURL,
		LifecycleNotificationURL:  sub.LifecycleNotificationURL,
		ExpirationDateTime:        sub.ExpirationDateTime.UTC(),
		LatestSupportedTLSVersion: sub.LatestSupportedTLSVersion,
	}
}

func invalidRequest(w http.ResponseWriter, message string) {
	graphError(w, http.StatusBadRequest, "InvalidRequest", message)
}

func subscriptionNotFound(w http.ResponseWriter) {
	graphError(w, http.StatusNotFound, "ResourceNotFound", "Subscription not found")
}

func (s *Server) subscriptions(w http.ResponseWriter, r *http.Request, rest []string) {
	s.pruneExpired()
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		s.listSubscriptions(w)
	case len(rest) == 0 && r.Method == http.MethodPost:
		s.createSubscription(w, r)
	case len(rest) == 1 && r.Method == http.MethodGet:
		s.getSubscription(w, rest[0])
	case len(rest) == 1 && r.Method == http.MethodPatch:
		s.renewSubscription(w, r, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		s.deleteSubscription(w, rest[0])
	default:
		graphError(w, http.StatusNotFound, "Request_ResourceNotFound", "Unknown endpoint")
	}
}

func (s *Server) listSubscriptions(w http.ResponseWriter) {
	subs := s.subs.list()
	values := make([]subscriptionJSON, 0, len(subs))
	for _, sub := range subs {
		values = append(values, s.subscriptionJSON(sub, ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{"@odata.context": "https://graph.microsoft.com/v1.0/$metadata#subscriptions", "value": values})
}

func (s *Server) createSubscription(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ChangeType                string  `json:"changeType"`
		NotificationURL           string  `json:"notificationUrl"`
		LifecycleNotificationURL  *string `json:"lifecycleNotificationUrl"`
		Resource                  string  `json:"resource"`
		ExpirationDateTime        string  `json:"expirationDateTime"`
		ClientState               *string `json:"clientState"`
		LatestSupportedTLSVersion *string `json:"latestSupportedTlsVersion"`
		IncludeResourceData       bool    `json:"includeResourceData"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		return
	}
	changeType := strings.TrimSpace(input.ChangeType)
	if changeType == "" {
		invalidRequest(w, "changeType is required")
		return
	}
	if changeType != "created" {
		invalidRequest(w, "Only the created changeType is supported by this simulator")
		return
	}
	mailbox := inboxMailbox(input.Resource)
	if mailbox == "" {
		invalidRequest(w, "Unsupported resource; only users/{user}/mailFolders('inbox')/messages is supported")
		return
	}
	exp, msg := s.parseExpiration(input.ExpirationDateTime)
	if msg != "" {
		invalidRequest(w, msg)
		return
	}
	if input.ClientState != nil && utf8.RuneCountInString(*input.ClientState) > maxClientStateLength {
		invalidRequest(w, "clientState must be at most 128 characters")
		return
	}
	if input.IncludeResourceData {
		invalidRequest(w, "includeResourceData is not supported by this simulator")
		return
	}
	tlsVersion := "v1_2"
	if input.LatestSupportedTLSVersion != nil {
		tlsVersion = *input.LatestSupportedTLSVersion
	}
	if !slices.Contains(tlsVersions, tlsVersion) {
		invalidRequest(w, "latestSupportedTlsVersion must be v1_0, v1_1, v1_2 or v1_3")
		return
	}
	if msg := s.checkHookURL("notificationUrl", input.NotificationURL); msg != "" {
		invalidRequest(w, msg)
		return
	}
	if input.LifecycleNotificationURL != nil {
		if msg := s.checkHookURL("lifecycleNotificationUrl", *input.LifecycleNotificationURL); msg != "" {
			invalidRequest(w, msg)
			return
		}
	}
	all, err := s.allMessages(r.Context())
	if err != nil {
		s.upstreamError(w, err)
		return
	}
	seen := make(map[string]bool)
	for _, m := range all {
		if s.matchesFolder(m, mailbox, "inbox") {
			seen[m.ID] = true
		}
	}
	if err := s.validateEndpoint(r.Context(), input.NotificationURL); err != nil {
		s.validationFailed(w, input.NotificationURL, err)
		return
	}
	if input.LifecycleNotificationURL != nil {
		if err := s.validateEndpoint(r.Context(), *input.LifecycleNotificationURL); err != nil {
			s.validationFailed(w, *input.LifecycleNotificationURL, err)
			return
		}
	}
	sub := &subscription{
		ID:                        newGUID(),
		Resource:                  input.Resource,
		Mailbox:                   mailbox,
		ChangeType:                changeType,
		NotificationURL:           input.NotificationURL,
		LifecycleNotificationURL:  input.LifecycleNotificationURL,
		ClientState:               input.ClientState,
		ExpirationDateTime:        exp,
		LatestSupportedTLSVersion: tlsVersion,
		seen:                      seen,
	}
	body := s.subscriptionJSON(*sub, subscriptionEntityContext)
	expired, conflict, err := s.subs.add(sub, s.now())
	s.logExpired(expired)
	if err != nil {
		invalidRequest(w, expirationNotInFuture)
		return
	}
	if conflict != "" {
		graphError(w, http.StatusConflict, "Conflict", "Subscription Id "+conflict+" already exists for the requested combination")
		return
	}
	s.log.Info("Subscription created", "subscription", sub.ID, "resource", sub.Resource)
	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) validationFailed(w http.ResponseWriter, endpoint string, err error) {
	s.log.Warn("Subscription validation failed", "endpoint", endpoint, "error", err)
	if errors.Is(err, context.DeadlineExceeded) {
		invalidRequest(w, "Subscription validation request timed out.")
		return
	}
	invalidRequest(w, "Subscription validation request failed. Notification endpoint must respond with 200 OK to validation request.")
}

func (s *Server) getSubscription(w http.ResponseWriter, id string) {
	sub, ok := s.subs.get(id)
	if !ok {
		subscriptionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.subscriptionJSON(sub, subscriptionEntityContext))
}

func (s *Server) renewSubscription(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		ExpirationDateTime string  `json:"expirationDateTime"`
		NotificationURL    *string `json:"notificationUrl"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		return
	}
	if input.NotificationURL != nil {
		invalidRequest(w, "Only expirationDateTime can be updated by this simulator")
		return
	}
	exp, msg := s.parseExpiration(input.ExpirationDateTime)
	if msg != "" {
		invalidRequest(w, msg)
		return
	}
	sub, ok := s.subs.renew(id, exp)
	if !ok {
		subscriptionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.subscriptionJSON(sub, subscriptionEntityContext))
}

func (s *Server) deleteSubscription(w http.ResponseWriter, id string) {
	if !s.subs.remove(id) {
		subscriptionNotFound(w)
		return
	}
	s.log.Info("Subscription deleted", "subscription", id)
	w.WriteHeader(http.StatusNoContent)
}

type subscriptionStats struct {
	ID                     string `json:"id"`
	DeliveredNotifications int    `json:"deliveredNotifications"`
	FailedNotifications    int    `json:"failedNotifications"`
	DroppedNotifications   int    `json:"droppedNotifications"`
	PendingDrops           int    `json:"pendingDrops"`
}

func statsOf(sub subscription) subscriptionStats {
	return subscriptionStats{
		ID:                     sub.ID,
		DeliveredNotifications: sub.delivered,
		FailedNotifications:    sub.failed,
		DroppedNotifications:   sub.dropped,
		PendingDrops:           sub.pendingDrops,
	}
}

func (s *Server) local(w http.ResponseWriter, r *http.Request) {
	s.pruneExpired()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 3 && parts[1] == "subscriptions" && r.Method == http.MethodGet:
		sub, ok := s.subs.get(parts[2])
		if !ok {
			subscriptionNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, statsOf(sub))
	case len(parts) == 4 && parts[1] == "subscriptions" && parts[3] == "dropNotifications" && r.Method == http.MethodPost:
		s.dropNotifications(w, r, parts[2])
	case len(parts) == 4 && parts[1] == "subscriptions" && parts[3] == "lifecycleNotifications" && r.Method == http.MethodPost:
		s.lifecycleNotification(w, r, parts[2])
	default:
		graphError(w, http.StatusNotFound, "Request_ResourceNotFound", "Unknown endpoint")
	}
}

func (s *Server) dropNotifications(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Count int `json:"count"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		return
	}
	if input.Count < 1 {
		graphError(w, http.StatusBadRequest, "BadRequest", "count must be a positive integer")
		return
	}
	sub, ok := s.subs.dropNext(id, input.Count)
	if !ok {
		subscriptionNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, statsOf(sub))
}

func (s *Server) lifecycleNotification(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		LifecycleEvent string `json:"lifecycleEvent"`
	}
	if err := decodeBody(w, r, &input); err != nil {
		return
	}
	switch input.LifecycleEvent {
	case "reauthorizationRequired", "missed", "subscriptionRemoved":
	default:
		graphError(w, http.StatusBadRequest, "BadRequest", "lifecycleEvent must be reauthorizationRequired, missed or subscriptionRemoved")
		return
	}
	sub, ok := s.subs.get(id)
	if !ok {
		subscriptionNotFound(w)
		return
	}
	if sub.LifecycleNotificationURL == nil {
		invalidRequest(w, "Subscription has no lifecycleNotificationUrl")
		return
	}
	if input.LifecycleEvent == "subscriptionRemoved" {
		s.subs.remove(id)
		s.log.Info("Subscription removed by lifecycle event", "subscription", id)
	}
	payload := notificationCollection[lifecycleNotification]{Value: []lifecycleNotification{{
		SubscriptionID:                 sub.ID,
		SubscriptionExpirationDateTime: sub.ExpirationDateTime,
		TenantID:                       simulatedTenantID,
		ClientState:                    sub.ClientState,
		LifecycleEvent:                 input.LifecycleEvent,
	}}}
	if err := s.postNotification(r.Context(), *sub.LifecycleNotificationURL, payload); err != nil {
		s.log.Warn("Lifecycle notification delivery failed", "subscription", id, "error", err)
		graphError(w, http.StatusBadGateway, "NotificationDeliveryFailed", "Lifecycle notification delivery failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
