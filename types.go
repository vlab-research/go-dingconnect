package dingconnect

// All field names below mirror the wire format exactly. DingConnect uses
// PascalCase everywhere, which happens to match Go's exported-field convention,
// so most fields need no struct tag. Tags are written out anyway so that the
// contract is explicit and survives a rename.

// Price is DingConnect's pricing breakdown, used both for product bounds and
// for realised transfer prices.
//
// SendValue is what the distributor is charged; ReceiveValue is what lands in
// the subscriber's account. They differ by fees, tax, and FX, and for many
// products SendValue > ReceiveValue even in a single currency.
type Price struct {
	CustomerFee              float64 `json:"CustomerFee"`
	DistributorFee           float64 `json:"DistributorFee"`
	ReceiveValue             float64 `json:"ReceiveValue"`
	ReceiveCurrencyIso       string  `json:"ReceiveCurrencyIso"`
	ReceiveValueExcludingTax float64 `json:"ReceiveValueExcludingTax"`
	TaxRate                  float64 `json:"TaxRate"`
	SendValue                float64 `json:"SendValue"`
	SendCurrencyIso          string  `json:"SendCurrencyIso"`
}

// Balance is the distributor account balance.
type Balance struct {
	Status
	Balance     float64 `json:"Balance"`
	CurrencyIso string  `json:"CurrencyIso"`
}

// DialingInfo describes a valid phone number shape for a country.
type DialingInfo struct {
	Prefix        string `json:"Prefix"`
	MinimumLength int    `json:"MinimumLength"`
	MaximumLength int    `json:"MaximumLength"`
}

// Country is a supported destination country.
type Country struct {
	CountryIso                      string        `json:"CountryIso"`
	CountryName                     string        `json:"CountryName"`
	InternationalDialingInformation []DialingInfo `json:"InternationalDialingInformation"`
	RegionCodes                     []string      `json:"RegionCodes"`
}

// Currency is a currency DingConnect can quote in.
type Currency struct {
	CurrencyIso  string `json:"CurrencyIso"`
	CurrencyName string `json:"CurrencyName"`
}

// Region is a sub-national billing region. Countries with a single region
// report one region whose code equals the country ISO.
type Region struct {
	RegionCode string `json:"RegionCode"`
	RegionName string `json:"RegionName"`
	CountryIso string `json:"CountryIso"`
}

// Provider is a carrier or merchant that can be topped up.
//
// ValidationRegex is the provider's own account-number pattern. Matching an
// account number against it locally is much cheaper than a round trip, but it
// is not a substitute for AccountLookup, which resolves which provider a
// number actually belongs to.
type Provider struct {
	ProviderCode       string   `json:"ProviderCode"`
	CountryIso         string   `json:"CountryIso"`
	Name               string   `json:"Name"`
	ValidationRegex    string   `json:"ValidationRegex"`
	CustomerCareNumber string   `json:"CustomerCareNumber"`
	RegionCodes        []string `json:"RegionCodes"`
	PaymentTypes       []string `json:"PaymentTypes"`
	LogoUrl            string   `json:"LogoUrl"`
}

// ProviderStatus reports whether a provider is currently processing transfers.
// Checking this before a batch avoids burning retries against a provider that
// is knowingly down.
type ProviderStatus struct {
	ProviderCode          string `json:"ProviderCode"`
	IsProcessingTransfers bool   `json:"IsProcessingTransfers"`
	Message               string `json:"Message"`
}

// SettingDefinition describes an extra field a product requires beyond the
// account number -- a utility meter ID, for example. Mandatory settings must
// be supplied in SendTransferRequest.Settings or the transfer is rejected.
type SettingDefinition struct {
	Name        string `json:"Name"`
	Description string `json:"Description"`
	IsMandatory bool   `json:"IsMandatory"`
}

// Product is a sellable SKU.
//
// Minimum and Maximum bound the accepted SendValue. For fixed-price products
// the two are identical, and any other SendValue is rejected -- so a product
// is variable-value only when Minimum.SendValue != Maximum.SendValue.
type Product struct {
	ProviderCode        string              `json:"ProviderCode"`
	SkuCode             string              `json:"SkuCode"`
	LocalizationKey     string              `json:"LocalizationKey"`
	SettingDefinitions  []SettingDefinition `json:"SettingDefinitions"`
	Maximum             Price               `json:"Maximum"`
	Minimum             Price               `json:"Minimum"`
	CommissionRate      float64             `json:"CommissionRate"`
	ProcessingMode      string              `json:"ProcessingMode"`
	RedemptionMechanism string              `json:"RedemptionMechanism"`
	Benefits            []string            `json:"Benefits"`
	ValidityPeriodIso   string              `json:"ValidityPeriodIso"`
	UatNumber           string              `json:"UatNumber"`
	DefaultDisplayText  string              `json:"DefaultDisplayText"`
	RegionCode          string              `json:"RegionCode"`
	PaymentTypes        []string            `json:"PaymentTypes"`
	LookupBillsRequired bool                `json:"LookupBillsRequired"`
}

// FixedValue reports whether the product accepts exactly one SendValue.
func (p Product) FixedValue() bool {
	return p.Minimum.SendValue == p.Maximum.SendValue
}

// ProductDescription is human-readable copy for a SKU, keyed by
// LocalizationKey rather than SkuCode.
type ProductDescription struct {
	DisplayText         string `json:"DisplayText"`
	DescriptionMarkdown string `json:"DescriptionMarkdown"`
	ReadMoreMarkdown    string `json:"ReadMoreMarkdown"`
	LocalizationKey     string `json:"LocalizationKey"`
	LanguageCode        string `json:"LanguageCode"`
}

// Promotion is an active promotional offer.
type Promotion struct {
	Status
	ProviderCode string `json:"ProviderCode"`
	CountryIso   string `json:"CountryIso"`
	SkuCode      string `json:"SkuCode"`
	StartDateUtc string `json:"StartDateUtc"`
	EndDateUtc   string `json:"EndDateUtc"`
}

// PromotionDescription is human-readable copy for a promotion.
type PromotionDescription struct {
	LocalizationKey     string `json:"LocalizationKey"`
	LanguageCode        string `json:"LanguageCode"`
	DisplayText         string `json:"DisplayText"`
	DescriptionMarkdown string `json:"DescriptionMarkdown"`
}

// ErrorCodeDescription pairs an error code with its official explanation.
type ErrorCodeDescription struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// AccountLookup resolves an account number to the products that can top it up.
//
// A lookup that returns ResultCodePartial with a NearestMatch error code
// normalised the number but could not identify the exact provider; Items is
// then typically empty while AccountNumberNormalized and CountryIso are still
// populated and useful.
type AccountLookup struct {
	Status
	CountryIso              string              `json:"CountryIso"`
	AccountNumberNormalized string              `json:"AccountNumberNormalized"`
	Items                   []AccountLookupItem `json:"Items"`
}

// AccountLookupItem is one provider/SKU combination valid for the account.
type AccountLookupItem struct {
	ProviderCode string   `json:"ProviderCode"`
	SkuCodes     []string `json:"SkuCodes"`
}

// EstimateRequest is one item in an EstimatePrices batch.
//
// BatchItemRef is mandatory and must be unique within the batch -- it is how
// responses are correlated back to requests, since the API does not guarantee
// ordering. Omitting it fails the whole batch with RequestInvalid.
type EstimateRequest struct {
	BatchItemRef    string  `json:"BatchItemRef"`
	SkuCode         string  `json:"SkuCode"`
	SendValue       float64 `json:"SendValue"`
	SendCurrencyIso string  `json:"SendCurrencyIso,omitempty"`
	AccountNumber   string  `json:"AccountNumber,omitempty"`
}

// Estimate is one priced result from an EstimatePrices batch. Items carry
// their own Status: a batch can succeed overall while individual items fail.
type Estimate struct {
	Status
	Price        Price  `json:"Price"`
	SkuCode      string `json:"SkuCode"`
	BatchItemRef string `json:"BatchItemRef"`
}

// TransferID pairs the caller's own reference with DingConnect's.
//
// DistributorRef is supplied by the caller and is the idempotency key; TransferRef
// is assigned by DingConnect and is what support will ask for.
type TransferID struct {
	DistributorRef string `json:"DistributorRef"`
	TransferRef    string `json:"TransferRef"`
}

// Processing states reported in TransferRecord.ProcessingState.
const (
	StateCompleted = "Completed"
	StateFailed    = "Failed"
	StateSubmitted = "Submitted" // deferred transfers only; resolved via notification
)

// TransferRecord is the outcome of a transfer.
//
// It is populated on failure as well as success, so it is worth recording
// regardless -- a Failed record still names the SKU and account involved.
type TransferRecord struct {
	TransferId        TransferID `json:"TransferId"`
	SkuCode           string     `json:"SkuCode"`
	Price             Price      `json:"Price"`
	CommissionApplied float64    `json:"CommissionApplied"`
	StartedUtc        string     `json:"StartedUtc"`
	CompletedUtc      string     `json:"CompletedUtc"`
	ProcessingState   string     `json:"ProcessingState"`
	ReceiptText       string     `json:"ReceiptText"`
	AccountNumber     string     `json:"AccountNumber"`
}

// Completed reports whether the transfer finished successfully.
func (r TransferRecord) Completed() bool { return r.ProcessingState == StateCompleted }

// Setting is a product-specific extra field, matching a SettingDefinition.
type Setting struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// SendTransferRequest is the body of a SendTransfer call.
type SendTransferRequest struct {
	SkuCode         string  `json:"SkuCode"`
	SendValue       float64 `json:"SendValue"`
	SendCurrencyIso string  `json:"SendCurrencyIso,omitempty"`
	AccountNumber   string  `json:"AccountNumber"`
	// DistributorRef is the caller's idempotency key. Reusing one is how a
	// retry is made safe: DingConnect answers a replay with
	// DuplicateTransactionPrevented instead of sending a second time.
	DistributorRef string `json:"DistributorRef"`
	// ValidateOnly runs the full validation and balance check without moving
	// money. The response carries a TransferRecord whose ProcessingState
	// reflects what would have happened.
	ValidateOnly bool      `json:"ValidateOnly,omitempty"`
	Settings     []Setting `json:"Settings,omitempty"`
}

// SendTransferResponse is the result of a SendTransfer call.
type SendTransferResponse struct {
	Status
	TransferRecord *TransferRecord `json:"TransferRecord"`
}

// TransferFilter narrows a TransferRecords query. Skip and Take paginate;
// Take must be within DingConnect's permitted range or the call fails with
// ParameterOutOfRange.
type TransferFilter struct {
	Skip            int      `json:"Skip"`
	Take            int      `json:"Take"`
	DistributorRefs []string `json:"DistributorRefs,omitempty"`
	TransferRefs    []string `json:"TransferRefs,omitempty"`
	AccountNumber   string   `json:"AccountNumber,omitempty"`
	SkuCodes        []string `json:"SkuCodes,omitempty"`
	StartedAtUtc    string   `json:"StartedAtUtc,omitempty"`
	EndedAtUtc      string   `json:"EndedAtUtc,omitempty"`
}

// TransferRecords is a page of transfer history.
type TransferRecords struct {
	Status
	Items             []TransferRecord `json:"Items"`
	ThereAreMoreItems bool             `json:"ThereAreMoreItems"`
}

// CancelRequest asks for one transfer to be cancelled.
type CancelRequest struct {
	BatchItemRef   string `json:"BatchItemRef"`
	DistributorRef string `json:"DistributorRef,omitempty"`
	TransferRef    string `json:"TransferRef,omitempty"`
}

// CancelResult is one outcome from a CancelTransfers batch.
type CancelResult struct {
	Status
	BatchItemRef string     `json:"BatchItemRef"`
	TransferId   TransferID `json:"TransferId"`
}
