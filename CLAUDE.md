# go-dingconnect

A Go client and CLI for the DingConnect mobile top-up API. Consumed by
`fly/dinersclub` as a payment provider.

## Documentation Policy

Durable knowledge lives **in this repo**, not in Claude's persistent memory:
in this file, in `README.md`, or in comments at the relevant call site
explaining *why* (the constraint, the invariant, the bug being guarded
against). Memories are invisible to teammates, to git, and to code review.

## The Wire Contract

Every item below was verified against the live API, and every one of them has
already caused a real outage or wasted a debugging session. Do not "clean up"
code that looks redundant here without re-verifying against the live API.

### Authentication is the `api_key` header

```
api_key: <key>     → HTTP 200, ResultCode 1
X-Api-Key: <key>   → HTTP 401, ResultCode 4, AuthenticationFailed
```

Not `X-Api-Key`, not `Authorization`, not a bearer token. The original
`fly/dinersclub/dingconnect.go` shipped `X-Api-Key` and failed 100% of calls in
production. `TestAuthHeaderIsApiKey` exists to make that unrepeatable.

### Everything on the wire is PascalCase

`SkuCode`, `SendValue`, `ResultCode`, `TransferRecord`, `CurrencyIso`. Never
`sku_code`. This is worse than it sounds: snake_case request fields are
silently *ignored*, and snake_case response fields decode without error into
zero values. Nothing fails loudly — you just get an empty struct. The same fly
integration had this bug too, and its tests encoded the wrong casing, so the
suite passed while production was broken.

### HTTP status does not indicate success

Application errors arrive as **HTTP 200 with a non-1 `ResultCode`**. Read
`ResultCode`, never the status code:

| ResultCode | Meaning |
|---|---|
| 1 | Success — the request did exactly what was asked |
| 2 | Partial — "nearest match"; data is returned but is approximate |
| 3 | Transient — retrying may succeed |
| 4 | Invalid request or bad credentials |
| 5 | Valid request the system refused (e.g. `InsufficientBalance`) |

### `InsufficientBalance` returns HTTP 500

It is a permanent failure delivered with a server-error status. Classifying
retryability by HTTP status alone will retry a transfer that cannot succeed
until the account is funded. `Error.Retryable` therefore consults the HTTP
status **only** when no `ResultCode` was decoded. See
`TestInsufficientBalanceIsNotRetryable`.

### `DistributorRef` is the idempotency key

DingConnect answers a replayed `DistributorRef` with
`DuplicateTransactionPrevented` rather than sending twice. This is the only
thing making a `SendTransfer` retry safe.

**Always reuse the same ref when retrying.** Generating a fresh one on retry
turns a transient failure into a double payment. The CLI refuses to invent a
ref for this reason.

### Endpoint quirks

- `EstimatePrices` takes a **bare JSON array** as its body, not an object
  wrapping one. Each item needs a unique `BatchItemRef` or the whole batch
  fails with `RequestInvalid`; responses are correlated by that ref because
  ordering is not guaranteed.
- `ListTransferRecords` paginates on `Skip`/`Take`, not `PageIndex`/`PageSize`.
  `Take` must be in range or the call fails with `ParameterOutOfRange`.
- Array query parameters repeat the key: `countryIsos=NG&countryIsos=KE`. A
  comma-joined single value is not accepted.
- Batch endpoints return per-item `Status`, so a batch can report overall
  success while individual items failed. Always check item-level `ResultCode`.
- There is **no sandbox environment**. Use `ValidateOnly: true` to exercise the
  transfer path without moving money.

## Design Principles

**Zero dependencies outside the standard library**, for the library and the CLI
alike. `go.mod` has no `require` block and there is no `go.sum`. This is the
main thing that makes the package lighter than `go-reloadly`, which predates
generics and carries `sling`, `viper`, and `cobra`. Adding a dependency needs a
reason that a stdlib approach genuinely cannot meet.

**One generic request helper.** `do[T statusHolder]` in `client.go` is the only
place that builds a request, sets headers, decodes, or maps errors. New
endpoints are a few lines in `api.go` and a struct in `types.go` — they must
not hand-roll HTTP. Any behaviour that should apply everywhere (retries,
instrumentation, logging) belongs in `do`, once.

**Errors carry the decoded payload.** Methods return their value *and* a
non-nil error when `ResultCode != 1`. A declined transfer still has a
`TransferRecord` worth recording. Never discard the value just because the
error is non-nil.

**Partial results are errors.** `ResultCode 2` surfaces as an error so it
cannot pass unnoticed; callers opt in with `IsNearestMatch`. This is the
"fail fast and loud" rule applied to a case the API makes easy to ignore.

**The CLI never spends money by accident.** `send` defaults to
`ValidateOnly` and requires an explicit `--confirm`. Keep it that way.

## Layout

```
client.go    Client, options, the generic do[T] helper, query building
errors.go    Status, Error, result/error code constants, Is* helpers
types.go     Request and response types — mirrors of the wire format
api.go       One method per endpoint, thin wrappers over do[T]
client_test.go            Contract tests against an httptest stub
cmd/dingconnect/main.go   The CLI
```

## Testing

Unit tests use `httptest` and need no credentials — `go test ./...` is always
runnable.

The tests are **contract tests**: each pins a fact about the real API that was
verified live. When something here looks over-specified, that is the point. Do
not relax an assertion to make a change pass; re-verify against the live API
first, and if the API really changed, update the assertion and this file
together.

For live verification, `.env` holds a real `DINGCONNECT_API_KEY` and is
gitignored. The CLI does **not** read it: configuration comes from the
environment, matching every other service in this org, so load it explicitly.

```sh
denv .env dingconnect balance
```

Do not add an implicit `.env` lookup back. Any such lookup is cwd-relative, so
which key a transfer used would depend on where the command was run from, and a
stray `.env` in an unrelated directory would silently take over.

Read-only commands (`balance`, `products`, `providers`, `lookup`) are safe to
run against production. `send` without `--confirm` is also safe. Note the
account balance may be **$0.00**, in which case every real transfer returns
`InsufficientBalance` no matter how correct the request is — check
`dingconnect balance` before concluding a transfer bug exists.

## Relationship to fly

`fly/dinersclub` consumes this package as its DingConnect provider. The
provider there should be a thin adapter: map `PaymentEvent` details to
`SendTransferRequest`, call `SendTransfer`, map the result back to a
`dinersclub.Result`. All HTTP, error classification, and wire-format knowledge
belongs here, not there.

When changing anything in the wire contract above, check whether
`fly/dinersclub` needs the same change.
