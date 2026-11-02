# Managed Remote binding fixes

The managed Remote client uses the native provider and live catalog, with no separate model cache. See [client](../../internal/remotejoin/client.go), [provider fetch](../../internal/provider/models.go), and [catalog](../../internal/catalog/live.go).

A successful managed refresh accepts a complete Remote data array, including an empty array. An empty successful list is recorded in the live catalog so it remains distinguishable from an absent list. Malformed responses, failures and incomplete pagination leave the last successful catalog and synchronization time intact. Quiet refresh invalidates held views without invoking the agent catalog callback. Other provider listing behavior is unchanged.

[tailnetdns](../../internal/tailnetdns/dns.go) tries system DNS first for the configured origin. Local Tailscale status is used only for a uniquely owned node name and address set. Fallback chooses addresses matching the requested network family; TLS retains the original URL hostname and certificate verification. An unmapped input IP remains the requested IP.

Verification: go test -p 1 -tags nogui ./internal/tailnetdns ./internal/remotejoin ./internal/catalog ./internal/provider in an isolated HOME. Actual results and platform limitations are recorded in the S4F delivery report. This slice does not implement the connection runner or agent restoration.
