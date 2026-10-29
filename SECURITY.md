# Security policy

## Intended use

`mailpit-graphapi` is a local test double. Bind it and Mailpit to loopback or an
isolated container network. Do not expose either service directly to the internet.

The built-in OAuth endpoint returns a static bearer token. It is a compatibility
feature, not an identity provider. Set a unique token and client secret whenever
other users or processes can reach the listener.

The service rejects non-loopback Mailpit URLs unless
`--allow-remote-mailpit` is explicitly set, caps JSON bodies at 1 MiB, applies
HTTP timeouts, avoids logging message bodies, and does not enable CORS.

Subscriptions make the service send outbound HTTP POST requests to the
`notificationUrl` and `lifecycleNotificationUrl` a client registers. Those URLs
must be loopback unless `--allow-remote-notification-urls` is explicitly set;
plain `http` is accepted and redirects are not followed. With the opt-in, any
caller holding the bearer token can make the sidecar post to any address it
reaches: use it only on an isolated container network. Notifications carry
message IDs and the subscription's `clientState`, never message content. Logs
never include these URLs, which may carry a secret in their query: a failed
validation or delivery is logged with its resource or subscription ID and a
failure class.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository. Do not
include real access tokens, customer email, or message bodies in a report.

Supported versions are the latest published release and the default branch.
