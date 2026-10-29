# Microsoft Graph Mail compatibility

The compatibility target is the common application-only mail workflow. The API
base is `/v1.0`; `{user}` is an email address and `{id}` is a Mailpit database ID.

| Graph route | Status | Mailpit mapping |
|---|---|---|
| `POST /{tenant}/oauth2/v2.0/token` | Supported | local static bearer token |
| `GET /users/{user}/mailFolders` | Supported | well-known root folder list, including Deleted Items |
| `GET /users/{user}/mailFolders/{folder}` | Supported | computed unread count |
| `GET /users/{user}/mailFolders/{folder}/childFolders` | Supported | configured direct children of Inbox |
| `GET /users/{user}/mailFolders/{folder}/messages` | Supported | messages/tags |
| `GET /users/{user}/messages` | Supported | messages addressed to/from user |
| `GET /users/{user}/messages/{id}` | Supported | message + headers |
| `GET /users/{user}/messages/{id}/$value` | Supported | raw RFC 822 source |
| `PATCH /users/{user}/messages/{id}` | Supported | read state and UTF-8-safe category tags |
| `POST /users/{user}/messages/{id}/move` | Supported | inbox, drafts, sentitems, deleteditems, configured folders |
| `POST /users/{user}/messages` | Supported | captured message tagged `graph-draft` |
| `POST /users/{user}/sendMail` | Supported | captured message tagged `graph-sent` |
| `POST /users/{user}/messages/{id}/reply` | Supported | captured threaded reply tagged `graph-sent` |
| `POST /subscriptions` | Supported | Inbox `created` notifications, in memory |
| `GET /subscriptions` | Supported | active subscriptions |
| `GET /subscriptions/{id}` | Supported | one active subscription |
| `PATCH /subscriptions/{id}` | Supported | renews `expirationDateTime` |
| `DELETE /subscriptions/{id}` | Supported | stops notifications |

Built-in root folders are `inbox`, `drafts`, `sentitems`, and `deleteditems`;
`deleted items` is accepted as an alias for `deleteditems`. Additional folders
are configured with `MAILPIT_GRAPH_FOLDERS` and exposed as direct children of
Inbox. Built-in names are reserved and ignored in custom folder configuration.
Each configured folder has a deterministic, URL-safe opaque ID distinct from
its display name; display names may contain literal `/` characters. Folder
state is stored as readable ASCII Mailpit tags. Lifecycle categories use the
same representation and are mapped back to their exact Graph spelling. On
startup, legacy Base64URL tags are migrated to readable tags and removed. The
message list honors
`$top`, `$skip`, ascending `$orderby`, `receivedDateTime ge ...`, and
`conversationId eq '...'`. Other OData expressions are ignored.

Moving a message to Deleted Items only updates its Mailpit tags. Moving it to
another folder, including Inbox, removes the Deleted Items tag while preserving
category tags, read state, and the Mailpit message ID. This is a folder-state
simulation: it does not permanently delete the Mailpit message.

## Change notifications

A subscription watches one mailbox's Inbox for new messages. The supported
`resource` is `users/{user}/mailFolders('inbox')/messages`; the
`users/{user}/mailFolders/inbox/messages` form, a leading `/` and any letter
case are accepted too. `changeType` must be `created`. Any other resource or
change type is rejected with `400 InvalidRequest`.

Creating a subscription follows Graph's handshake: the sidecar POSTs to
`notificationUrl?validationToken=...`, then to `lifecycleNotificationUrl` when
set, and each must answer within 10 seconds with `200 OK`, a `text/plain` body
and the URL-decoded token. Otherwise creation fails with `400 InvalidRequest`.
`expirationDateTime` is required, must be in the future and at most 10080
minutes ahead; `PATCH` renews it. An expired subscription is dropped and stops
notifying. `clientState` is limited to 128 characters and echoed in every
notification. A second subscription with the same mailbox and change type is
rejected with `409 Conflict`, as Graph rejects duplicates.

While at least one subscription is active, the sidecar polls Mailpit every
`--notification-poll-interval`. Each message that entered the subscribed Inbox
after the creation request started (the same matching as the Inbox listing)
triggers exactly one POST of this shape to `notificationUrl`:

```json
{
  "value": [{
    "subscriptionId": "…",
    "subscriptionExpirationDateTime": "2026-10-30T10:00:00Z",
    "changeType": "created",
    "resource": "Users/box@example.test/Messages/{id}",
    "resourceData": {
      "@odata.type": "#Microsoft.Graph.Message",
      "@odata.id": "Users/box@example.test/Messages/{id}",
      "id": "{id}"
    },
    "clientState": "…",
    "tenantId": "00000000-0000-0000-0000-000000000000"
  }]
}
```

A delivery that fails (no `2xx` within 3 seconds) is logged and counted, and is
never retried. If Mailpit is unreachable, the poll is skipped and the next one
catches up.

### Test-only routes

These routes sit under `/local/`, like the token endpoint, and require the
bearer token. They let a test exercise what real Graph only does on failure.

| Route | Effect |
|---|---|
| `GET /local/subscriptions/{id}` | delivery counters: `deliveredNotifications`, `failedNotifications`, `droppedNotifications`, `pendingDrops` |
| `POST /local/subscriptions/{id}/dropNotifications` with `{"count": N}` | silently drops the next N change notifications of that subscription, to prove a backup poll catches up |
| `POST /local/subscriptions/{id}/lifecycleNotifications` with `{"lifecycleEvent": "…"}` | posts a Graph lifecycle notification (`reauthorizationRequired`, `missed` or `subscriptionRemoved`) to the subscription's `lifecycleNotificationUrl`; `subscriptionRemoved` also deletes the subscription |

The lifecycle route answers `204` once the endpoint accepted the notification,
`502 NotificationDeliveryFailed` if it did not, and `400 InvalidRequest` for a
subscription created without `lifecycleNotificationUrl`. `subscriptionRemoved`
deletes the subscription even when the delivery fails.

## Field mapping

| Microsoft Graph | Mailpit |
|---|---|
| `id` | database `ID` |
| `internetMessageId` | `MessageID` |
| `receivedDateTime` | `Created` / `Date` |
| `isRead` | `Read` |
| `categories` | `Tags` |
| `bodyPreview` | `Snippet` or first 250 text characters |
| `hasAttachments` | attachment count |

`conversationId` is deterministic: it uses the explicit
`X-Graph-Conversation-ID`, otherwise the root `References`/`In-Reply-To` ID,
otherwise the message ID, hashed with SHA-256 and truncated to 128 bits. Messages
sent through this sidecar preserve their supplied conversation ID.

## Intentional differences

- The OAuth endpoint does not validate scopes, sign JWTs, or emulate Entra ID.
- There are no permissions, delta queries, throttling, mailbox rules, attachment
  CRUD, or multi-tenant isolation.
- Subscriptions live in memory only: a restart loses them, and clients must
  create them again.
- Only basic notifications for new Inbox messages exist: no `updated` or
  `deleted` change types, no other resources, no `includeResourceData` and no
  encryption. Request fields other than `changeType`, `notificationUrl`,
  `lifecycleNotificationUrl`, `resource`, `expirationDateTime`, `clientState`,
  `latestSupportedTlsVersion` and `includeResourceData` are rejected.
- Notification URLs may use plain `http` and must be loopback unless
  `--allow-remote-notification-urls` is set; Graph requires public `https`.
- An `expirationDateTime` under 45 minutes ahead is kept as given, where Graph
  extends it to 45 minutes, so expiry can be tested quickly.
- `PATCH /subscriptions/{id}` only renews `expirationDateTime`; changing
  `notificationUrl` is rejected. `POST /subscriptions/{id}/reauthorize` is not
  implemented.
- Failed deliveries are not retried, where Graph retries for up to 4 hours.
  Notifications are posted one message at a time, never batched.
- A message that was in the Inbox when the subscription was created, or was
  already notified, is not notified again when it is moved back into the Inbox;
  Graph posts a `created` notification for each move into the folder.
- A message that arrives while the creation request is being processed (during
  the validation handshake) is notified, as the baseline of existing messages is
  taken when the request starts.
- Deliveries are sent one at a time from the single polling goroutine, each with
  a 3 s timeout and no retry: an endpoint that accepts the connection without
  answering delays the notifications of every subscription.
- Lifecycle notifications are sent only through the test-only route, never on
  their own.
- `tenantId` is always `00000000-0000-0000-0000-000000000000`, `resource` names
  the mailbox by its address rather than its user ID, and notifications carry no
  `@odata.etag`. Error messages follow Graph's codes but not always its wording.
- A Mailpit database is one shared test store. The `{user}` path filters messages
  by recipient or sender; it is not an access-control boundary.
- `sendMail` captures a message in Mailpit. It does not deliver to Microsoft 365.
- `PATCH` operations are sequential and are not transactionally atomic.
- Configured folders model one level below Inbox; arbitrary nested custom folder
  hierarchies are not implemented.
