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

`--ref` is the **idempotency key** and is required. Reuse the same value when
retrying a failed transfer — DingConnect answers a replayed ref with
`DuplicateTransactionPrevented` instead of sending twice. A fresh ref on retry
risks paying twice.

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

`CLAUDE.md` documents these in full, with the reasoning.

## Testing

```sh
go test ./...
```

Tests run against an `httptest` stub and need no credentials.
