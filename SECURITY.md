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

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting for this repository. Do not
include real access tokens, customer email, or message bodies in a report.

Supported versions are the latest published release and the default branch.
