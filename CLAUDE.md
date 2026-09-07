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

### `DistributorRef` is NOT an idempotency key (measured 2026-09-07)

This section used to say DingConnect answers a replayed `DistributorRef` with
`DuplicateTransactionPrevented`. It does not, or not reliably: a second
`SendTransfer` with the ref of a transfer that had completed one minute
earlier was accepted and paid again (TransferRefs 863784492 and 863784573,
Bs 11 each, same account). Nothing in this package makes a retry safe on its
own; only the caller's knowledge that the first attempt did not complete does.

**Still reuse the same ref when retrying.** It is the reference support works
from and the handle `ListTransferRecords` searches by. The CLI refuses to
invent a ref for that reason, not for idempotency.

### Three wire facts that broke every real payment (measured 2026-09-07)

All three passed every validate-only test and every unit test, because each
only bites on a real send or on the real response. Each is now pinned by a
test that carries the live evidence in its comment.

- **`ValidateOnly` must be present on every `SendTransfer` body.** It was
  `omitempty`, so a real send (false) carried no field and DingConnect refused
  it: ResultCode 4, `ParameterInvalid`, context `ValidateOnly`. Validate-only
  calls (true) were unaffected. `TestValidateOnlyAlwaysSent`.
- **`AccountNumber` must not carry a leading `+`.** E.164 (`+59172690398`)
  fails `AccountNumberInvalid / AccountNumberFailedRegex`; `59172690398`
  completes. `SendTransfer` strips one leading `+` and nothing else.
  `TestAccountNumberLeadingPlusStripped`.
- **A completed transfer reports `ProcessingState: "Complete"`**, not
  `"Completed"`. With the wrong constant `Pay` returned no `Transfer` for a
  transfer that had paid. `StateCompleted` is now `"Complete"`.

Also measured: `ListTransferRecords` wraps each item in a SendTransfer-style
envelope (`{"TransferRecord": {...}, "ResultCode": 1, ...}`).
`TransferRecords.UnmarshalJSON` unwraps it; `TestTransferRecordsUnwrapsEnvelope`.

### Unverified: what `RechargeNotAllowed` actually means

`Pay`'s discovery path advances to the next candidate on `RechargeNotAllowed`,
treating it as "wrong operator for this account number". **This is inferred, not
confirmed.** DingConnect documents the code against "product/send amount", not
explicitly against an operator mismatch; the phrasing fits and no better
candidate exists, but nobody has watched it come back from a deliberately
wrong-operator send.

The design is built so that being wrong is survivable: `RechargeNotAllowed` is
only the *advance* signal, and advancing is an allow-list. If it turns out to
mean something else, discovery degrades into a single attempt and every other
path is untouched. Nothing sends money somewhere it should not.

**To confirm:** `SendTransfer` with `ValidateOnly: true`, a known-good account
number, and a SKU belonging to a different operator in the same country.
`ValidateOnly` runs the full validation without moving money or assigning a
TransferId, and `Product.UatNumber` supplies the known-good half. If the
wrong-operator SKU comes back `RechargeNotAllowed`, this is settled — update
this section and `decideCascade`'s comment together.

### Unverified: `DistributorRef` length and charset

`MaxDistributorRefLen` is **64, and that number is a guess.** DingConnect
documents no limit, and none has been observed. It matters because `Pay` derives
a per-candidate ref (`<ref>_<SkuCode>`) on the discovery path, which is longer
than what the caller supplied.

It is enforced up front, before any money moves, rather than by truncating: a
truncated ref is unsearchable in `ListTransferRecords`, which is exactly when
you need it. Raising the constant is cheap once someone can ask DingConnect or
watch a long ref be rejected.

Related: whether a rejected transfer consumes its `DistributorRef` no longer
matters, since even a *completed* transfer does not (see the DistributorRef
section above). `Pay` still derives a distinct ref per candidate; the refs are
merely more unique than they needed to be, and they stay searchable.

### Range products are assumed to be priced linearly

`Product` exposes only `Minimum` and `Maximum`, so `Pay` interpolates linearly
between them to answer "what does this deliver at this send value". This is
implied by `CommissionRate` being a single per-product number, but it is not
documented. Fixed products interpolate nothing and are unaffected.
`EstimatePrices` prices a prospective transfer exactly and is the way to check a
range product before trusting a tight tolerance against it.

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

**No metrics registry, no logger.** `Pay` returns a `Resolution` describing what
it did -- which path, which operator, which product, every attempt -- and the
caller records it however it likes. A library that reaches for a metrics client
forces its choice of one on every consumer, and a library that logs decides
where a consumer's output goes. Hand back the facts instead.

**The CLI never spends money by accident.** `send` defaults to
`ValidateOnly` and requires an explicit `--confirm`. Keep it that way.

## Layout

```
client.go    Client, options, the generic do[T] helper, query building
errors.go    Status, Error, result/error code constants, Is* helpers
types.go     Request and response types — mirrors of the wire format
api.go       One method per endpoint, thin wrappers over do[T]
payment.go   Pay: resolving a top-up by delivered amount rather than by SKU
client_test.go            Contract tests against an httptest stub
payment_test.go           Pay's resolution, verification and cascade tests
cmd/dingconnect/main.go   The CLI
```

`payment.go` is the one file that is not a thin wrapper over an endpoint. It
composes `GetAccountLookup`, `GetProducts` and `SendTransfer` into a single
operation, and it is where amount selection and the advance/stop policy live.
That policy is error-code semantics, which is this package's job by the rule
below.

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

`fly/dinersclub` consumes this package as its DingConnect provider. The provider
there is a thin adapter: unmarshal its own payment config, call `Pay` (or
`SendTransfer` when a caller named a product explicitly), map the outcome back
to a `dinersclub.Result`, and record the returned `Resolution` as metrics.

**All HTTP, error classification, amount selection, and wire-format knowledge
belongs here, not there.** That line was already the rule and was briefly
crossed: an earlier version of the amount-resolution work lived entirely in
`dinersclub`, which put cascade error-code semantics in a consumer. It moved
here, which is what `payment.go` is.

The split, stated so the next person does not have to re-derive it:

| here | there |
|---|---|
| operator detection, catalogue fetch and caching | unmarshalling the consumer's own config format |
| amount selection, pin verification | credentials and secret storage |
| the advance/stop cascade policy | mapping outcomes onto the consumer's result type |
| `DistributorRef` derivation (idempotency is a wire property) | recovery classification, retry policy, metrics |

When changing anything in the wire contract above, check whether
`fly/dinersclub` needs the same change.
