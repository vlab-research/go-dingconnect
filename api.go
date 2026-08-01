package dingconnect

import (
	"context"
	"net/http"
)

// Every method here returns the decoded payload alongside a non-nil error when
// DingConnect reports a non-success ResultCode, so partial results stay
// inspectable. See Error and IsNearestMatch.

// Balance returns the distributor account balance.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	return do[Balance](ctx, c, http.MethodGet, "/GetBalance", nil, nil)
}

// Countries returns every supported destination country.
func (c *Client) Countries(ctx context.Context) ([]Country, error) {
	r, err := do[List[Country]](ctx, c, http.MethodGet, "/GetCountries", nil, nil)
	return r.Items, err
}

// Currencies returns every currency DingConnect can quote in.
func (c *Client) Currencies(ctx context.Context) ([]Currency, error) {
	r, err := do[List[Currency]](ctx, c, http.MethodGet, "/GetCurrencies", nil, nil)
	return r.Items, err
}

// Regions returns billing regions, optionally narrowed to given countries.
func (c *Client) Regions(ctx context.Context, countryISOs ...string) ([]Region, error) {
	r, err := do[List[Region]](ctx, c, http.MethodGet, "/GetRegions",
		query("countryIsos", countryISOs), nil)
	return r.Items, err
}

// ProviderFilter narrows a Providers query. All fields are optional; an empty
// filter returns every provider DingConnect offers.
type ProviderFilter struct {
	ProviderCodes []string
	CountryISOs   []string
	RegionCodes   []string
	AccountNumber string
}

// Providers returns carriers and merchants matching the filter.
func (c *Client) Providers(ctx context.Context, f ProviderFilter) ([]Provider, error) {
	r, err := do[List[Provider]](ctx, c, http.MethodGet, "/GetProviders", query(
		"providerCodes", f.ProviderCodes,
		"countryIsos", f.CountryISOs,
		"regionCodes", f.RegionCodes,
		"accountNumber", f.AccountNumber,
	), nil)
	return r.Items, err
}

// ProviderStatuses reports whether providers are currently processing
// transfers. With no codes it returns the status of every provider.
func (c *Client) ProviderStatuses(ctx context.Context, providerCodes ...string) ([]ProviderStatus, error) {
	r, err := do[List[ProviderStatus]](ctx, c, http.MethodGet, "/GetProviderStatus",
		query("providerCodes", providerCodes), nil)
	return r.Items, err
}

// ProductFilter narrows a Products query. All fields are optional, but an
// unfiltered call returns the entire global catalogue, which is large --
// filter by country or provider in anything interactive.
type ProductFilter struct {
	CountryISOs   []string
	ProviderCodes []string
	SkuCodes      []string
	RegionCodes   []string
	Benefits      []string
	AccountNumber string
}

// Products returns sellable SKUs matching the filter.
func (c *Client) Products(ctx context.Context, f ProductFilter) ([]Product, error) {
	r, err := do[List[Product]](ctx, c, http.MethodGet, "/GetProducts", query(
		"countryIsos", f.CountryISOs,
		"providerCodes", f.ProviderCodes,
		"skuCodes", f.SkuCodes,
		"regionCodes", f.RegionCodes,
		"benefits", f.Benefits,
		"accountNumber", f.AccountNumber,
	), nil)
	return r.Items, err
}

// ProductDescriptions returns human-readable copy for SKUs. Results are keyed
// by LocalizationKey, which is not always equal to SkuCode.
func (c *Client) ProductDescriptions(ctx context.Context, languageCodes, skuCodes []string) ([]ProductDescription, error) {
	r, err := do[List[ProductDescription]](ctx, c, http.MethodGet, "/GetProductDescriptions", query(
		"languageCodes", languageCodes,
		"skuCodes", skuCodes,
	), nil)
	return r.Items, err
}

// Promotions returns active promotional offers.
func (c *Client) Promotions(ctx context.Context, countryISOs, providerCodes []string, accountNumber string) ([]Promotion, error) {
	r, err := do[List[Promotion]](ctx, c, http.MethodGet, "/GetPromotions", query(
		"countryIsos", countryISOs,
		"providerCodes", providerCodes,
		"accountNumber", accountNumber,
	), nil)
	return r.Items, err
}

// PromotionDescriptions returns human-readable copy for promotions.
func (c *Client) PromotionDescriptions(ctx context.Context, languageCodes ...string) ([]PromotionDescription, error) {
	r, err := do[List[PromotionDescription]](ctx, c, http.MethodGet, "/GetPromotionDescriptions",
		query("languageCodes", languageCodes), nil)
	return r.Items, err
}

// ErrorCodeDescriptions returns the authoritative error-code table. Useful for
// rendering a failure to a human without hardcoding messages.
func (c *Client) ErrorCodeDescriptions(ctx context.Context) ([]ErrorCodeDescription, error) {
	r, err := do[List[ErrorCodeDescription]](ctx, c, http.MethodGet, "/GetErrorCodeDescriptions", nil, nil)
	return r.Items, err
}

// AccountLookup resolves an account number to the providers and SKUs that can
// top it up.
//
// A ResultCodePartial response is common and not necessarily a problem: the
// number was normalised but no exact provider match was found. Test for it
// with IsNearestMatch -- the returned AccountLookup still carries
// CountryIso and AccountNumberNormalized.
func (c *Client) AccountLookup(ctx context.Context, accountNumber string) (AccountLookup, error) {
	return do[AccountLookup](ctx, c, http.MethodGet, "/GetAccountLookup",
		query("accountNumber", accountNumber), nil)
}

// EstimatePrices prices a batch of prospective transfers without sending them.
//
// Each request needs a unique BatchItemRef; responses carry it back so they can
// be correlated, since ordering is not guaranteed. The batch as a whole can
// report success while individual Estimates carry their own failing Status.
func (c *Client) EstimatePrices(ctx context.Context, reqs []EstimateRequest) ([]Estimate, error) {
	// The endpoint takes a bare JSON array, not an object wrapping one.
	r, err := do[List[Estimate]](ctx, c, http.MethodPost, "/EstimatePrices", nil, reqs)
	return r.Items, err
}

// SendTransfer sends a top-up.
//
// This moves real money unless req.ValidateOnly is set. Two properties matter:
//
//   - DistributorRef is the idempotency key. Retrying with the same ref is
//     safe; DingConnect answers a replay with DuplicateTransactionPrevented.
//     Generating a fresh ref on retry risks sending twice.
//   - The response carries a TransferRecord on failure as well as success, so
//     the returned value is worth recording even when err is non-nil.
//
// Transfers are processed instantly; this client does not send the
// X-Option: DeferTransfer header, so a returned record is final rather than
// pending an out-of-band notification.
func (c *Client) SendTransfer(ctx context.Context, req SendTransferRequest) (SendTransferResponse, error) {
	return do[SendTransferResponse](ctx, c, http.MethodPost, "/SendTransfer", nil, req)
}

// TransferRecords returns a page of transfer history.
//
// Take must be within DingConnect's permitted page-size range; zero or an
// out-of-range value fails with ParameterOutOfRange on "Take".
func (c *Client) TransferRecords(ctx context.Context, f TransferFilter) (TransferRecords, error) {
	return do[TransferRecords](ctx, c, http.MethodPost, "/ListTransferRecords", nil, f)
}

// CancelTransfers requests cancellation of one or more transfers. Each request
// needs a unique BatchItemRef and identifies its transfer by either
// DistributorRef or TransferRef.
func (c *Client) CancelTransfers(ctx context.Context, reqs []CancelRequest) ([]CancelResult, error) {
	r, err := do[List[CancelResult]](ctx, c, http.MethodPost, "/CancelTransfers", nil, reqs)
	return r.Items, err
}
