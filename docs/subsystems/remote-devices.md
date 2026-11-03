# Remote device management

The Gateway keys page lists trusted registrations as devices. Device metadata and credentials share the existing caller-key store; there is no separate device table. `access.ListDevices` returns metadata ordered by key ID, and `access.RemoveDevice` removes only a matching device key. Registration after removal creates a new key. Other keys are unaffected.

`remoteDeviceRoutes` in [`remote_devices.go`](../../internal/gui/remote_devices.go) supplies `GET /api/remote-devices` and `DELETE /api/remote-devices/{id}`. It is intended for registration in the existing `Handler` route mux, under the same native Web cookie and origin checks as the rest of the management API. The list accepts a cursor and limit from 1 to 100 (default 50); it contains no credential. An absent device deletion succeeds. Invalid input receives `INVALID_REQUEST`; store failures receive `STORE_UNAVAILABLE` without underlying errors. A failed store read is not treated as an empty store.

The page labels `lastSeenAt` as the most recent registration, not an online or agent status. Removing a device is not a ban: a trusted node may register again. This feature does not collect agent choices or impose device restrictions.

Verification: `go test -p 1 -tags nogui ./internal/gui -run RemoteDevices`; `node --test internal/gui/tests/remote-devices.test.cjs` with Chromium and WebKit. Product acceptance additionally requires real nodes and agents; browser fixtures and Go tests alone do not establish that result.
