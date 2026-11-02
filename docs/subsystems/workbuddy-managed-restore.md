# WorkBuddy managed model-file recovery

`ManagedWorkBuddy` in `internal/agent/managed_workbuddy.go` binds the S5
transaction to the native WorkBuddy connector's exact `models.json` file.
It captures its bytes, existence and permission bits before a generation's
first write. Arrays and objects are supported; invalid or unknown data fails
closed. The snapshot is private journal data, never status or diagnostic text.

S8 owns the entry guard. It must obtain the native WorkBuddy `Agent`, call
`ManagedWorkBuddy`, and pass its target to `BeginManaged` before connecting.
For every original Set or Sync call it must hold the session's
`BeforeManagedWrite` scope and call `BeforeManagedWorkBuddyWrite` immediately
before that original call. An error stops the call. The scope must cover the
whole native write. Captures for the same target in the same generation never
replace the original baseline. Calls are local and must not reenter the store.

Before `RestoreManaged`, S8 must block and drain the agent's native writes.
It must supply an idempotent, non-reentrant `Cleanup` for only the native
WorkBuddy bookkeeping it changes. It must not call `Disconnect` from the
cleanup or roll back the shared stash/applied files. Restoration writes the
original file bytes, then finalizes its original mode, or removes the file
when it was absent. Failed restoration retains the transaction for retry.
The adapter alone does not guard public entry points or provide offline policy.

The real WorkBuddy application may cache configuration; restoring the disk
file does not prove a running task or desktop session immediately reloads it.
Desktop UI and live-session behavior need separate acceptance. Full project
acceptance and S5 parser fixes are tracked outside this isolated slice.

Verification: `go test -tags nogui ./internal/agent -run 'ManagedWorkBuddy|WorkBuddy'`.
