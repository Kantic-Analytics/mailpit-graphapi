# Microsoft Graph Mail compatibility

The compatibility target is the common application-only mail workflow. The API
base is `/v1.0`; `{user}` is an email address and `{id}` is a Mailpit database ID.

| Graph route | Status | Mailpit mapping |
|---|---|---|
| `POST /{tenant}/oauth2/v2.0/token` | Supported | local static bearer token |
| `GET /users/{user}/mailFolders/inbox` | Supported | computed unread count |
| `GET /users/{user}/mailFolders/{folder}/messages` | Supported | messages/tags |
| `GET /users/{user}/messages` | Supported | messages addressed to/from user |
| `GET /users/{user}/messages/{id}` | Supported | message + headers |
| `GET /users/{user}/messages/{id}/$value` | Supported | raw RFC 822 source |
| `PATCH /users/{user}/messages/{id}` | Supported | read state and tags |
| `POST /users/{user}/messages` | Supported | captured message tagged `graph-draft` |
| `POST /users/{user}/sendMail` | Supported | captured message tagged `graph-sent` |

Supported folders are `inbox`, `drafts`, and `sentitems`. The message list honors
`$top`, `$skip`, ascending `$orderby`, `receivedDateTime ge ...`, and
`conversationId eq '...'`. Other OData expressions are ignored.

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
- There are no permissions, subscriptions, delta queries, webhooks, throttling,
  mailbox rules, attachment CRUD, or multi-tenant isolation.
- A Mailpit database is one shared test store. The `{user}` path filters messages
  by recipient or sender; it is not an access-control boundary.
- `sendMail` captures a message in Mailpit. It does not deliver to Microsoft 365.
- `PATCH` operations are sequential and are not transactionally atomic.
