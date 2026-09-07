# go-dingconnect

A Go client and CLI for the [DingConnect](https://www.dingconnect.com) mobile
top-up API.

No dependencies outside the standard library.

```go
import dingconnect "github.com/vlab-research/go-dingconnect"
```

The module path carries a `go-` prefix to match the rest of the
`vlab-research` org; the package itself is `dingconnect`.

There is no official DingConnect SDK for Go — their guidance is to generate one
from their Swagger definition. This is a hand-written alternative, with the
API's several undocumented sharp edges handled and pinned by tests.

## Install

```sh
go get github.com/vlab-research/go-dingconnect            # library
go install github.com/vlab-research/go-dingconnect/cmd/dingconnect@latest   # CLI
```

## CLI

The API key is read from `DINGCONNECT_API_KEY` or `--api-key`. Nothing is
loaded from disk — configuration comes from the environment, as it does in
every other service in this org. To load a file for a single command, use
`denv`:

```sh
denv .env dingconnect balance
```

```sh
dingconnect balance
dingconnect providers --country NG
dingconnect products --country NG --provider MTNG
dingconnect lookup +2348031234567
dingconnect estimate --sku 2ANG44349 --value 12.08
dingconnect transfers --take 25
```

Every command takes `--json` for raw output, so it composes with `jq`:

```sh
dingconnect products --country KE --json | jq '.[] | select(.Minimum.SendValue < 5) | .SkuCode'
```

### Checking prices and carriers

```
$ dingconnect providers --country NG
CODE  COUNTRY  NAME                        PAYMENT
ETNG  NG       9Mobile (Etisalat) Nigeria  Prepaid
ZANG  NG       Airtel Nigeria              Prepaid
2ANG  NG       Amazon Nigeria              Prepaid
...

$ dingconnect products --country NG
SKU          PROVIDER  SEND             RECEIVE                COMM  MODE     REQUIRES
NG_4X_TopUp  4XNG      2.00-105.00 USD  2246.29-117930.31 NGN  3.0%  Instant  MeterId
2ANG44349    2ANG      12.08 USD        10.00 USD              3.0%  Instant
```

`SEND` is what you are charged, `RECEIVE` is what lands in the subscriber's
account; a range means the SKU takes a variable amount, a single figure means
it is fixed. `REQUIRES` lists mandatory extra settings — a SKU listing
`MeterId` needs `--setting MeterId=<value>` on `send` or it will be rejected.

### Sending

`send` validates without moving money unless you pass `--confirm`:

```sh
# dry run — full validation and balance check, no money moves
dingconnect send --sku 2ANG44349 --value 12.08 --account 2348031234567 --ref order-123

# for real
dingconnect send --sku 2ANG44349 --value 12.08 --account 2348031234567 --ref order-123 --confirm
```

`--ref` is your reference for the transfer and is required. Reuse the same
value when retrying a failed transfer; it is what DingConnect support works
from. It is **not** a reliable idempotency key: this README used to say a
replayed ref answers `DuplicateTransactionPrevented`, but a live replay on
2026-09-07 was accepted and paid a second time. Only retry a transfer you know
did not complete.

Three wire facts, all measured live on 2026-09-07 and all pinned by tests:
`ValidateOnly` must be present on every `SendTransfer` body (its absence is
`ParameterInvalid`, context `ValidateOnly`); `AccountNumber` must not carry a
leading `+` (the client strips one); and a completed transfer reports
`ProcessingState` `"Complete"`, not `"Completed"`.

## Library

```go
c := dingconnect.New(os.Getenv("DINGCONNECT_API_KEY"))

bal, err := c.Balance(ctx)
fmt.Printf("%.2f %s\n", bal.Balance, bal.CurrencyIso)

products, err := c.Products(ctx, dingconnect.ProductFilter{
    CountryISOs: []string{"NG"},
})

res, err := c.SendTransfer(ctx, dingconnect.SendTransferRequest{
    SkuCode:        "2ANG44349",
    SendValue:      12.08,
    AccountNumber:  "2348031234567",
    DistributorRef: "order-123",   // idempotency key
})
```

### Paying by delivered amount

`SendTransfer` takes a `SkuCode` and a `SendValue`. Both are per-operator: a SKU
belongs to one network, and the send value that delivers a given local amount
differs between networks because commission rates differ. Three products can all
deliver ARS 1,000 while costing 0.79, 0.85 and 0.93 USD.

That makes a bare `SendValue` an uninterpretable number. Nothing records what it
was *for*, so nothing can check it — and when a commission rate moves, the old
value is still a perfectly valid request. The transfer completes, `ResultCode`
is 1, and the recipient quietly gets less than you intended.

`Pay` closes that hole. You declare what you want delivered, and optionally pin
the products you believe satisfy it:

```go
res, err := c.Pay(ctx, dingconnect.PayRequest{
    AccountNumber:  "5491112345678",
    DistributorRef: "order-123",

    Amount:         1000,     // MINIMUM DELIVERED, not a cap on spend
    AmountCurrency: "ARS",    // validated against the product's ReceiveCurrencyIso
    Tolerance:      200,      // headroom on the delivered amount

    Operators: map[string]dingconnect.OperatorPin{
        "CLAR": {SkuCode: "CLAR5046",  SendValue: 0.79},
        "TFAR": {SkuCode: "TFAR58291", SendValue: 0.85},
        "PRAR": {SkuCode: "PRAR13725", SendValue: 0.93},
    },
})
```

The declared amount is what makes the pinned value verifiable — that is the
whole reason for carrying both. `Operators` is optional; without it `Pay`
resolves the cheapest in-window product from the live catalogue.

How a payment resolves:

1. **Operator known** (from `Operator`, or looked up from the account number)
   **and pinned** — verify the pin against the window, send. One transfer.
2. **Operator known, pins given, none for it** — `ReasonNoPinForOperator`.
3. **Operator known, no pins** — cheapest in-window product from the catalogue.
4. **Operator not determined, pins given** — try each pinned candidate until one
   accepts the account. This is *discovery*: an account belongs to exactly one
   operator, and sending is what settles which.
5. **Operator not determined, no pins** — `ReasonOperatorNotDetermined`.

The catalogue is cached per client (`WithCatalogueTTL`, 6h by default), so
verifying a pin is a local lookup and the common path costs one `SendTransfer`.

**The context bounds the whole resolution**, not each call within it — a lookup,
a catalogue fetch and several transfers all share it. Set your deadline there.

#### When a pin stops being true

`Pay` refuses to send rather than deliver an amount you did not ask for:

```go
if re, ok := dingconnect.IsResolutionError(err); ok {
    switch re.Reason {
    case dingconnect.ReasonPinSkuMissing:    // the pinned SKU is gone
    case dingconnect.ReasonPinOutOfWindow:   // a commission rate moved
    case dingconnect.ReasonCurrencyMismatch: // it now delivers another currency
    case dingconnect.ReasonImpossibleAmount: // nothing lands in the window
    }
    // No money moved. re.Message names the window and what was available.
}
```

A pin that still satisfies the window but is **no longer the cheapest** option is
honoured silently. A pin overridden over pennies is not a pin.

#### Recording what happened

`Pay` returns a `Resolution` whether or not it succeeded, so a failed cascade
still tells you what was tried:

```go
res, err := c.Pay(ctx, req)

log.Printf("path=%s operator=%s sku=%s sent=%v delivered=%v",
    res.Resolution.Path, res.Resolution.Operator, res.Resolution.SkuCode,
    res.Resolution.SendValue, res.Resolution.Delivered)

for _, a := range res.Resolution.Attempts { // discovery only
    log.Printf("  %s ref=%s ok=%v codes=%v", a.SkuCode, a.DistributorRef, a.Completed, a.Codes)
}
```

`Expected` is what the catalogue predicted and `Delivered` is what the transfer
reported. A difference between them means the catalogue and the realised price
disagree — worth alerting on, even though the money has already moved.

This package deliberately owns no metrics client and no logger. It reports the
facts; you record them.

### Errors

DingConnect reports application errors in the body with HTTP 200, so the HTTP
status never tells you whether a call succeeded. Any `ResultCode` other than 1
comes back as a `*dingconnect.Error`:

```go
res, err := c.SendTransfer(ctx, req)
if err != nil {
    // The response is populated even on failure — a declined transfer
    // still carries a TransferRecord worth recording.
    if res.TransferRecord != nil {
        log.Printf("state=%s sku=%s", res.TransferRecord.ProcessingState, res.TransferRecord.SkuCode)
    }

    switch {
    case dingconnect.IsRetryable(err):
        // Reuse the SAME DistributorRef.
        return retry(req)
    case dingconnect.HasCode(err, dingconnect.CodeInsufficientBalance):
        return fmt.Errorf("top up the distributor account: %w", err)
    case dingconnect.IsAuthError(err):
        return fmt.Errorf("bad API key: %w", err)
    }
    return err
}
```

Helpers: `IsRetryable`, `IsAuthError`, `IsNearestMatch`, `HasCode`. For detail,
type-assert to `*dingconnect.Error` and read `ResultCode`, `Codes`, and
`StatusCode`.

**Partial results are errors.** `ResultCode 2` ("nearest match") means
DingConnect returned the closest valid answer rather than what you asked for.
It surfaces as an error so it cannot pass unnoticed; opt in with
`IsNearestMatch`, and the returned value is still populated:

```go
lookup, err := c.AccountLookup(ctx, "+2348012345678")
if err != nil && !dingconnect.IsNearestMatch(err) {
    return err
}
// lookup.AccountNumberNormalized and lookup.CountryIso are usable either way.
```

## Coverage

| Method | Endpoint |
|---|---|
| `Balance` | GetBalance |
| `Countries` | GetCountries |
| `Currencies` | GetCurrencies |
| `Regions` | GetRegions |
| `Providers` | GetProviders |
| `ProviderStatuses` | GetProviderStatus |
| `Products` | GetProducts |
| `ProductDescriptions` | GetProductDescriptions |
| `Promotions` | GetPromotions |
| `PromotionDescriptions` | GetPromotionDescriptions |
| `ErrorCodeDescriptions` | GetErrorCodeDescriptions |
| `AccountLookup` | GetAccountLookup |
| `EstimatePrices` | EstimatePrices |
| `SendTransfer` | SendTransfer |
| `Pay` | GetAccountLookup + GetProducts + SendTransfer |
| `TransferRecords` | ListTransferRecords |
| `CancelTransfers` | CancelTransfers |

## Notes on the API

Behaviour worth knowing, all verified against the live API:

- **Auth is the `api_key` header.** Not `X-Api-Key`, not `Authorization`.
- **The wire format is PascalCase.** snake_case request fields are silently
  ignored and snake_case response fields decode to zero values without error.
- **`InsufficientBalance` returns HTTP 500** despite being permanent. Do not
  classify retryability by HTTP status.
- **There is no sandbox.** Use `ValidateOnly` to exercise the transfer path.

Three things `Pay` depends on are **not** verified, and are documented as such:
whether `RechargeNotAllowed` really signals a wrong-operator SKU, whether
`DistributorRef` has a length limit (64 is assumed), and whether range products
are priced linearly between their bounds. Each is designed so that being wrong
degrades a fallback path rather than misdirecting money.

`CLAUDE.md` documents all of this in full, with the reasoning and with how to
confirm the unverified items.

## Testing

```sh
go test ./...
```

Tests run against an `httptest` stub and need no credentials.
