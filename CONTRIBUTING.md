# Contributing

1. Open an issue describing the Graph behavior and the Mailpit mapping.
2. Keep runtime dependencies minimal; standard library solutions are preferred.
3. Add tests for success, malformed input, and upstream failure cases.
4. Run `make check` before opening a pull request.

Compatibility changes must also update `docs/compatibility.md`. Never commit real
mailboxes, credentials, access tokens, or production message content.
