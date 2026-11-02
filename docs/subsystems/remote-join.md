# Remote join

`internal/remotejoin` provides peer resolution and an HTTP registration
handler for remote devices. It does not replace gateway authentication or
issue the GUI's Web cookie. At this commit the handler and trust marker are
available to a host but **no production listener or route installs them**.
The protected listener and browser-login bridge belong to the subsequent GUI
integration; their real Linux and Tailscale behavior has not been tested here.

## Trust and identity

[`TrustedServeConnContext`](../../internal/remotejoin/peer.go) marks a
connection's context; it does not inspect the connection or verify its peer.
The host must install it only on a dedicated Tailscale Serve listener after
ensuring the listener and accepted connections are protected. It must never
mark ordinary TCP or loopback traffic. The marker cannot be supplied by an
HTTP header. Client-supplied identity and user headers are ignored.

`LocalAPIResolver.Resolve` accepts one IP in `X-Forwarded-For` only after the
marker, then queries the local tailscaled whois API. The default client uses
the Unix socket without a proxy; the socket path, timeout, or HTTP client can
be injected. The call has a timeout, bounded response, and no redirects.
`Peer` contains StableID, decimal Node ID and a display name from whois; the
name is for display, not identity. Tagged nodes do not require a user header.
See [`peer.go`](../../internal/remotejoin/peer.go) and
[`contracts.go`](../../internal/remotejoin/contracts.go).

## Registration and stored state

[`NewRegistrationHandler`](../../internal/remotejoin/server.go) accepts a
configured HTTPS root origin, actual server version, resolver, device store,
and a callback that checks the current native Web signer. It handles POST
`/_magpie/remote/v1/registration` with a JSON object up to 4 KiB and only an
optional `clientVersion`. Host and forwarded host do not determine returned
URLs; a supplied Origin must match the configured origin. Successful responses
are `no-store` and contain the verified device, its gateway credential, a
clean browser-login URL and protocol/server versions.

`AccessDeviceStore` adapts [`access.EnsureDevice`](../../internal/access/device.go),
which stores device metadata with the gateway key in the existing key store.
Repeated registration of a stable node reuses its credential; list results do
not contain the secret. The gateway key is distinct from Web authentication.
Only the trusted registration response is intended to deliver it; ordinary
logs, status, errors and device lists must not include it. See
[Providers and accounts](providers-accounts.md) for the existing store.

The handler checks `WebSessionReady` before and after storage; nil or failure
returns `WEB_SESSION_UNAVAILABLE` without delivering the credential. A failure
on the second check can leave a device record for an idempotent retry. This
check does not lock the signer against a subsequent change: the host must
coordinate and validate its real cookie issuance and key rotation. The handler
does not implement browser login. `ValidateReturnPath` is available to the GUI
bridge to validate a clean site path before redirecting.

## Failures and verification

Malformed input or ambiguous XFF returns 400 `INVALID_REQUEST`; missing trust,
unknown node or incomplete identity returns 401 `TAILSCALE_IDENTITY_REQUIRED`.
LocalAPI failures return 503 `TAILSCALE_IDENTITY_UNAVAILABLE`, and store or
Web signer failures return 503 `STORE_UNAVAILABLE` or
`WEB_SESSION_UNAVAILABLE`. Errors use fixed messages and `no-store`; method
errors return 405 with `Allow: POST`. The underlying error or response body is
not included.

The package tests exercise fake LocalAPI and local HTTP listeners, including
unmarked TCP, ambiguous forwarding, tagged fixtures, failures, origin checks,
retries and concurrent requests. Run in an isolated test home with pinned Go
caches and dependencies available offline:

```sh
go test -tags nogui ./internal/remotejoin
go test -race -tags nogui ./internal/remotejoin -run 'Peer|Registration'
```

These tests do not prove the security of a real Serve socket, native cookie
issuance or cross-process key-store locking. The inherited access file lock can
continue without a lock on failure or timeout; its independent fix and
integration validation remain necessary before relying on cross-process
uniqueness. Real tagged-node, complete management and end-to-end model use
remain unverified by this slice.
