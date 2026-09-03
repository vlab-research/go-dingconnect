package dingconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for Pay: resolving a top-up by delivered amount.
//
// MOST OF THESE ASSERT WHICH TRANSFERS WERE SENT, not just what Pay returned.
// Every "never advanced past" guarantee is a claim about a send that must NOT
// happen, and a test reading only the return value cannot tell "stopped
// correctly" apart from "charged three operators and reported the first
// failure". payAPI records every request for that reason.

// payAPI is a stub DingConnect that records what it was asked for.
type payAPI struct {
	products  []Product
	lookup    string // GetAccountLookup body; empty means "no operator resolved"
	transfers map[string]stubResponse
	fallback  *stubResponse

	sends       []SendTransferRequest
	productCall int
	lookupCall  int
}

type stubResponse struct {
	status int
	body   string
}

func newPayAPI() *payAPI {
	return &payAPI{transfers: map[string]stubResponse{}}
}

// sentSkus lists the SKUs actually charged, in order.
func (a *payAPI) sentSkus() []string {
	out := make([]string, 0, len(a.sends))
	for _, s := range a.sends {
		out = append(out, s.SkuCode)
	}
	return out
}

// client wires the stub to a Client. Caching is left at its default so the
// cache is exercised; TestPayCachesCatalogue pins the fetch count explicitly.
func (a *payAPI) client(t *testing.T, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "/GetProducts"):
			a.productCall++
			body, _ := json.Marshal(map[string]any{"Items": a.products, "ResultCode": 1, "ErrorCodes": []string{}})
			w.WriteHeader(200)
			w.Write(body)

		case strings.HasSuffix(r.URL.Path, "/GetAccountLookup"):
			a.lookupCall++
			w.WriteHeader(200)
			if a.lookup == "" {
				fmt.Fprint(w, `{"CountryIso":"AR","Items":[],"ResultCode":1,"ErrorCodes":[]}`)
				return
			}
			fmt.Fprint(w, a.lookup)

		case strings.HasSuffix(r.URL.Path, "/SendTransfer"):
			var req SendTransferRequest
			json.NewDecoder(r.Body).Decode(&req)
			a.sends = append(a.sends, req)

			resp, ok := a.transfers[req.SkuCode]
			if !ok {
				if a.fallback == nil {
					w.WriteHeader(500)
					fmt.Fprint(w, `{"ResultCode":5,"ErrorCodes":[{"Code":"OtherError"}]}`)
					return
				}
				resp = *a.fallback
			}
			w.WriteHeader(resp.status)
			fmt.Fprint(w, resp.body)

		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"ResultCode":4,"ErrorCodes":[{"Code":"RequestInvalid"}]}`)
		}
	}))
	t.Cleanup(srv.Close)

	return New("test-key", append([]Option{WithBaseURL(srv.URL)}, opts...)...)
}

// --- fixtures ---------------------------------------------------------------

func fixedProduct(sku, provider string, send, receive float64, currency string) Product {
	p := Price{SendValue: send, ReceiveValue: receive, ReceiveCurrencyIso: currency, SendCurrencyIso: "USD"}
	return Product{SkuCode: sku, ProviderCode: provider, Minimum: p, Maximum: p}
}

func rangeProduct(sku, provider string, minSend, minRecv, maxSend, maxRecv float64, currency string) Product {
	return Product{
		SkuCode:      sku,
		ProviderCode: provider,
		Minimum:      Price{SendValue: minSend, ReceiveValue: minRecv, ReceiveCurrencyIso: currency, SendCurrencyIso: "USD"},
		Maximum:      Price{SendValue: maxSend, ReceiveValue: maxRecv, ReceiveCurrencyIso: currency, SendCurrencyIso: "USD"},
	}
}

// A three-operator catalogue where every product delivers the same local
// amount at a different send value, because commission rates differ. This is
// the case Pay exists for.
func threeOperatorCatalogue() []Product {
	return []Product{
		fixedProduct("SKU_A", "OPA", 0.79, 1000, "ARS"),
		fixedProduct("SKU_B", "OPB", 0.85, 1000, "ARS"),
		fixedProduct("SKU_C", "OPC", 0.93, 1000, "ARS"),
	}
}

func pinnedRequest() PayRequest {
	return PayRequest{
		AccountNumber:  "5491112345678",
		DistributorRef: "ref1",
		Amount:         1000,
		AmountCurrency: "ARS",
		Tolerance:      200,
		Operators: map[string]OperatorPin{
			"OPA": {SkuCode: "SKU_A", SendValue: 0.79},
			"OPB": {SkuCode: "SKU_B", SendValue: 0.85},
			"OPC": {SkuCode: "SKU_C", SendValue: 0.93},
		},
	}
}

func okTransfer(sku string, receive float64, currency string) stubResponse {
	return stubResponse{200, fmt.Sprintf(`{
		"TransferRecord": {
			"TransferId": {"DistributorRef":"r","TransferRef":"DC1"},
			"SkuCode": %q,
			"Price": {"ReceiveValue": %v, "ReceiveCurrencyIso": %q, "SendCurrencyIso":"USD"},
			"ProcessingState": "Completed"
		},
		"ResultCode": 1, "ErrorCodes": []
	}`, sku, receive, currency)}
}

func errTransfer(code string) stubResponse {
	return stubResponse{200, fmt.Sprintf(`{"TransferRecord":null,"ResultCode":5,"ErrorCodes":[{"Code":%q}]}`, code)}
}

func lookupBody(provider string) string {
	return fmt.Sprintf(`{"CountryIso":"AR","AccountNumberNormalized":"5491112345678",
		"Items":[{"ProviderCode":%q,"SkuCodes":["X"]}],"ResultCode":1,"ErrorCodes":[]}`, provider)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The verified path
// ---------------------------------------------------------------------------

// TestPayPinnedOperatorSendsOnce is the primary path: detect the operator, look
// its pin up, verify it, send once. One transfer, and it is the detected
// operator's.
func TestPayPinnedOperatorSendsOnce(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = lookupBody("OPB")
	api.transfers["SKU_B"] = okTransfer("SKU_B", 1000, "ARS")

	res, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Transfer == nil || !res.Transfer.Completed() {
		t.Fatalf("want a completed transfer, got %+v", res.Transfer)
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"SKU_B"}) {
		t.Errorf("sent %v, want only the detected operator's product", got)
	}
	if res.Resolution.Path != PathPinned {
		t.Errorf("Path = %q, want %q", res.Resolution.Path, PathPinned)
	}
	if res.Resolution.Expected != 1000 || res.Resolution.Delivered != 1000 {
		t.Errorf("Expected/Delivered = %v/%v, want 1000/1000", res.Resolution.Expected, res.Resolution.Delivered)
	}
}

// TestPaySendsEachOperatorsOwnValue is the defect this API exists to prevent,
// guarded directly. Each operator delivers the same local amount at a different
// send value; a shared value would deliver the wrong amount while reporting
// success.
func TestPaySendsEachOperatorsOwnValue(t *testing.T) {
	for _, tc := range []struct {
		operator string
		sku      string
		send     float64
	}{
		{"OPA", "SKU_A", 0.79},
		{"OPB", "SKU_B", 0.85},
		{"OPC", "SKU_C", 0.93},
	} {
		t.Run(tc.operator, func(t *testing.T) {
			api := newPayAPI()
			api.products = threeOperatorCatalogue()
			api.lookup = lookupBody(tc.operator)
			api.transfers[tc.sku] = okTransfer(tc.sku, 1000, "ARS")

			if _, err := api.client(t).Pay(context.Background(), pinnedRequest()); err != nil {
				t.Fatalf("Pay: %v", err)
			}
			if len(api.sends) != 1 {
				t.Fatalf("sent %d transfers, want 1", len(api.sends))
			}
			if api.sends[0].SkuCode != tc.sku {
				t.Errorf("SkuCode = %q, want %q", api.sends[0].SkuCode, tc.sku)
			}
			if api.sends[0].SendValue != tc.send {
				t.Errorf("SendValue = %v, want %v -- each operator carries its own price",
					api.sends[0].SendValue, tc.send)
			}
		})
	}
}

// TestPaySingleSendKeepsTheGivenRef: only discovery derives. A caller retrying
// a payment it already submitted must reuse the exact reference DingConnect saw.
func TestPaySingleSendKeepsTheGivenRef(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = lookupBody("OPA")
	api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

	if _, err := api.client(t).Pay(context.Background(), pinnedRequest()); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if got := api.sends[0].DistributorRef; got != "ref1" {
		t.Errorf("DistributorRef = %q, want the unmodified %q", got, "ref1")
	}
}

// TestPayHonoursAPinThatIsNoLongerCheapest. A cheaper product delivers the same
// amount and the pin still wins, silently. A pin overridden over pennies is not
// a pin, and payments would become nondeterministic for a saving nobody asked
// for.
func TestPayHonoursAPinThatIsNoLongerCheapest(t *testing.T) {
	api := newPayAPI()
	api.products = append(threeOperatorCatalogue(), fixedProduct("SKU_CHEAP", "OPA", 0.50, 1000, "ARS"))
	api.lookup = lookupBody("OPA")
	api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

	res, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"SKU_A"}) {
		t.Errorf("sent %v, want the pinned product even though a cheaper one exists", got)
	}
	if res.Resolution.SendValue != 0.79 {
		t.Errorf("SendValue = %v, want the pinned 0.79", res.Resolution.SendValue)
	}
}

// TestPayCachesCatalogue: verifying a pin must be a local lookup, not a round
// trip, or every payment carries a GetProducts call.
func TestPayCachesCatalogue(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = lookupBody("OPA")
	api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

	c := api.client(t)
	for i := 0; i < 3; i++ {
		if _, err := c.Pay(context.Background(), pinnedRequest()); err != nil {
			t.Fatalf("Pay %d: %v", i, err)
		}
	}
	if api.productCall != 1 {
		t.Errorf("GetProducts called %d times, want 1", api.productCall)
	}
	if len(api.sends) != 3 {
		t.Errorf("sent %d transfers, want 3", len(api.sends))
	}
}

// TestPayCatalogueTTLZeroDisablesCaching keeps the escape hatch honest.
func TestPayCatalogueTTLZeroDisablesCaching(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = lookupBody("OPA")
	api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

	c := api.client(t, WithCatalogueTTL(0))
	for i := 0; i < 2; i++ {
		if _, err := c.Pay(context.Background(), pinnedRequest()); err != nil {
			t.Fatalf("Pay %d: %v", i, err)
		}
	}
	if api.productCall != 2 {
		t.Errorf("GetProducts called %d times, want 2 with caching off", api.productCall)
	}
}

// ---------------------------------------------------------------------------
// Refusing to send
// ---------------------------------------------------------------------------

// TestPayRefusesToSendOnDrift. Every case here must fail BEFORE any transfer:
// the whole value of declaring an intent is that a stale pin is caught rather
// than paid.
func TestPayRefusesToSendOnDrift(t *testing.T) {
	tests := []struct {
		name       string
		products   []Product
		wantReason ResolutionReason
		wantMsg    string
	}{
		{
			name:       "pinned sku is gone from the catalogue",
			products:   []Product{fixedProduct("SOMETHING_ELSE", "OPA", 0.79, 1000, "ARS")},
			wantReason: ReasonPinSkuMissing,
			wantMsg:    "no longer in the catalogue",
		},
		{
			// The silent-wrong-amount failure, made loud: the send value is
			// still valid and no longer buys what was intended.
			name:       "commission moved so the pin delivers too little",
			products:   []Product{fixedProduct("SKU_A", "OPA", 0.79, 800, "ARS")},
			wantReason: ReasonPinOutOfWindow,
			wantMsg:    "now delivers 800",
		},
		{
			name:       "pin delivers above the tolerance ceiling",
			products:   []Product{fixedProduct("SKU_A", "OPA", 0.79, 1500, "ARS")},
			wantReason: ReasonPinOutOfWindow,
			wantMsg:    "outside the declared window",
		},
		{
			// Never inferred from country: the receive currency is not always
			// the local one, so this is what stops us paying 1000 of the wrong
			// unit and calling it success.
			name:       "product now delivers a different currency",
			products:   []Product{fixedProduct("SKU_A", "OPA", 0.79, 1000, "USD")},
			wantReason: ReasonCurrencyMismatch,
			wantMsg:    "delivers USD but AmountCurrency is ARS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newPayAPI()
			api.products = tt.products
			api.lookup = lookupBody("OPA")
			api.fallback = &stubResponse{200, okTransfer("X", 1000, "ARS").body}

			_, err := api.client(t).Pay(context.Background(), pinnedRequest())
			re, ok := IsResolutionError(err)
			if !ok {
				t.Fatalf("err = %v, want a *ResolutionError", err)
			}
			if re.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", re.Reason, tt.wantReason)
			}
			if !strings.Contains(re.Message, tt.wantMsg) {
				t.Errorf("Message = %q, want it to contain %q", re.Message, tt.wantMsg)
			}
			if len(api.sends) != 0 {
				t.Errorf("sent %v; drift must be caught before any money moves", api.sentSkus())
			}
		})
	}
}

// TestPayNoPinForDetectedOperator names both halves so the caller can fix the
// configuration: what was detected, and what they actually pinned.
func TestPayNoPinForDetectedOperator(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = lookupBody("OPZ")

	_, err := api.client(t).Pay(context.Background(), pinnedRequest())
	re, ok := IsResolutionError(err)
	if !ok {
		t.Fatalf("err = %v, want a *ResolutionError", err)
	}
	if re.Reason != ReasonNoPinForOperator {
		t.Errorf("Reason = %q, want %q", re.Reason, ReasonNoPinForOperator)
	}
	for _, want := range []string{"OPZ", "OPA"} {
		if !strings.Contains(re.Message, want) {
			t.Errorf("Message = %q, want it to name %q", re.Message, want)
		}
	}
	if len(api.sends) != 0 {
		t.Errorf("sent %v, want nothing", api.sentSkus())
	}
}

// TestPayImpossibleAmount pins that the failure names the window AND what was
// available. "Could not resolve" alone is not actionable.
func TestPayImpossibleAmount(t *testing.T) {
	api := newPayAPI()
	api.products = []Product{
		fixedProduct("TOO_SMALL", "OPA", 0.05, 50, "ARS"),
		fixedProduct("TOO_BIG", "OPA", 9.99, 20000, "ARS"),
	}
	api.lookup = lookupBody("OPA")

	req := pinnedRequest()
	req.Operators = nil // no pins: resolve from the catalogue

	_, err := api.client(t).Pay(context.Background(), req)
	re, ok := IsResolutionError(err)
	if !ok {
		t.Fatalf("err = %v, want a *ResolutionError", err)
	}
	if re.Reason != ReasonImpossibleAmount {
		t.Errorf("Reason = %q, want %q", re.Reason, ReasonImpossibleAmount)
	}
	for _, want := range []string{"between 1000 and 1200 ARS", "TOO_SMALL delivers 50 ARS", "TOO_BIG delivers 20000 ARS"} {
		if !strings.Contains(re.Message, want) {
			t.Errorf("Message = %q, want it to contain %q", re.Message, want)
		}
	}
	if len(api.sends) != 0 {
		t.Errorf("sent %v; never deliver outside the window", api.sentSkus())
	}
}

// TestPayOperatorNotDeterminedAndNoPins: with neither a detected operator nor
// pins there is nothing to try, and saying so beats guessing.
func TestPayOperatorNotDeterminedAndNoPins(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()

	req := pinnedRequest()
	req.Operators = nil

	_, err := api.client(t).Pay(context.Background(), req)
	re, ok := IsResolutionError(err)
	if !ok {
		t.Fatalf("err = %v, want a *ResolutionError", err)
	}
	if re.Reason != ReasonOperatorNotDetermined {
		t.Errorf("Reason = %q, want %q", re.Reason, ReasonOperatorNotDetermined)
	}
	if len(api.sends) != 0 {
		t.Errorf("sent %v, want nothing", api.sentSkus())
	}
}

// ---------------------------------------------------------------------------
// Catalogue resolution
// ---------------------------------------------------------------------------

func TestPayResolvesCheapestFixedProduct(t *testing.T) {
	api := newPayAPI()
	api.products = []Product{
		fixedProduct("EXPENSIVE", "OPA", 0.99, 1100, "ARS"),
		fixedProduct("CHEAPEST", "OPA", 0.79, 1000, "ARS"),
		fixedProduct("MIDDLE", "OPA", 0.89, 1050, "ARS"),
		// A cheaper product on a DIFFERENT network is not payable here.
		fixedProduct("OTHER_NET", "OPB", 0.10, 1000, "ARS"),
	}
	api.lookup = lookupBody("OPA")
	api.transfers["CHEAPEST"] = okTransfer("CHEAPEST", 1000, "ARS")

	req := pinnedRequest()
	req.Operators = nil

	res, err := api.client(t).Pay(context.Background(), req)
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"CHEAPEST"}) {
		t.Errorf("sent %v, want the cheapest in-window product on the right network", got)
	}
	if res.Resolution.Path != PathCatalogue {
		t.Errorf("Path = %q, want %q", res.Resolution.Path, PathCatalogue)
	}
}

// TestPayResolvesRangeProduct solves a variable-value product for the declared
// amount rather than sending one of its bounds.
func TestPayResolvesRangeProduct(t *testing.T) {
	api := newPayAPI()
	api.products = []Product{rangeProduct("RANGE", "OPA", 0.10, 1, 10.0, 100, "BOB")}
	api.lookup = lookupBody("OPA")
	api.transfers["RANGE"] = okTransfer("RANGE", 5, "BOB")

	res, err := api.client(t).Pay(context.Background(), PayRequest{
		AccountNumber: "59171234567", DistributorRef: "bo1",
		Amount: 5, AmountCurrency: "BOB", Tolerance: 1,
	})
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if len(api.sends) != 1 {
		t.Fatalf("sent %d transfers, want 1", len(api.sends))
	}
	if got := api.sends[0].SendValue; got < 0.48 || got > 0.52 {
		t.Errorf("SendValue = %v, want ~0.50 to deliver 5 BOB", got)
	}
	if res.Resolution.Path != PathCatalogue {
		t.Errorf("Path = %q, want %q", res.Resolution.Path, PathCatalogue)
	}
}

// TestPayClampsRangeProductUpToItsMinimum: a product whose floor exceeds the
// request is usable only if the overshoot stays inside the tolerance.
func TestPayClampsRangeProductUpToItsMinimum(t *testing.T) {
	api := newPayAPI()
	api.products = []Product{rangeProduct("RANGE", "OPA", 1.0, 100, 10.0, 1000, "BOB")}
	api.lookup = lookupBody("OPA")
	api.transfers["RANGE"] = okTransfer("RANGE", 100, "BOB")

	_, err := api.client(t).Pay(context.Background(), PayRequest{
		AccountNumber: "59171234567", DistributorRef: "bo1",
		Amount: 80, AmountCurrency: "BOB", Tolerance: 50,
	})
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if api.sends[0].SendValue != 1.0 {
		t.Errorf("SendValue = %v, want the product minimum 1.0", api.sends[0].SendValue)
	}

	// ...and unusable when the overshoot exceeds it.
	api2 := newPayAPI()
	api2.products = []Product{rangeProduct("RANGE", "OPA", 1.0, 100, 10.0, 1000, "BOB")}
	api2.lookup = lookupBody("OPA")

	_, err = api2.client(t).Pay(context.Background(), PayRequest{
		AccountNumber: "59171234567", DistributorRef: "bo1",
		Amount: 10, AmountCurrency: "BOB", Tolerance: 5,
	})
	if re, ok := IsResolutionError(err); !ok || re.Reason != ReasonImpossibleAmount {
		t.Errorf("err = %v, want ImpossibleAmount when the clamp overshoots the window", err)
	}
	if len(api2.sends) != 0 {
		t.Errorf("sent %v, want nothing", api2.sentSkus())
	}
}

// ---------------------------------------------------------------------------
// Discovery and the cascade
// ---------------------------------------------------------------------------

// TestPayDiscoveryAdvancesOnRechargeNotAllowed: detection was inconclusive, so
// the pinned candidates are tried until one accepts the account.
func TestPayDiscoveryAdvancesOnRechargeNotAllowed(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.transfers["SKU_A"] = errTransfer(CodeRechargeNotAllowed)
	api.transfers["SKU_B"] = errTransfer(CodeRechargeNotAllowed)
	api.transfers["SKU_C"] = okTransfer("SKU_C", 1000, "ARS")

	res, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"SKU_A", "SKU_B", "SKU_C"}) {
		t.Errorf("sent %v, want all three tried in pin order", got)
	}
	if res.Resolution.Path != PathDiscovery {
		t.Errorf("Path = %q, want %q", res.Resolution.Path, PathDiscovery)
	}
	if len(res.Resolution.Attempts) != 3 {
		t.Fatalf("Attempts = %d, want 3 recorded outcomes", len(res.Resolution.Attempts))
	}
	if !res.Resolution.Attempts[2].Completed {
		t.Error("the last attempt should be recorded as completed")
	}
	if got := res.Resolution.Attempts[0].Codes; len(got) != 1 || got[0] != CodeRechargeNotAllowed {
		t.Errorf("Attempts[0].Codes = %v, want [%s]", got, CodeRechargeNotAllowed)
	}
}

// TestPayDiscoveryDerivesADeterministicRefPerCandidate. Whether a rejected
// transfer consumes its ref is unverified; deriving is correct either way, and
// determinism is what keeps a retry from paying twice.
func TestPayDiscoveryDerivesADeterministicRefPerCandidate(t *testing.T) {
	send := func() []string {
		api := newPayAPI()
		api.products = threeOperatorCatalogue()
		api.transfers["SKU_A"] = errTransfer(CodeRechargeNotAllowed)
		api.transfers["SKU_B"] = errTransfer(CodeRechargeNotAllowed)
		api.transfers["SKU_C"] = okTransfer("SKU_C", 1000, "ARS")

		if _, err := api.client(t).Pay(context.Background(), pinnedRequest()); err != nil {
			t.Fatalf("Pay: %v", err)
		}
		refs := make([]string, 0, len(api.sends))
		for _, s := range api.sends {
			refs = append(refs, s.DistributorRef)
		}
		return refs
	}

	want := []string{"ref1_SKU_A", "ref1_SKU_B", "ref1_SKU_C"}
	first := send()
	if !eqStrings(first, want) {
		t.Errorf("refs = %v, want %v", first, want)
	}
	if second := send(); !eqStrings(first, second) {
		t.Errorf("refs are not deterministic: %v then %v", first, second)
	}
}

// TestPayDiscoveryStopConditions is the heart of the safety contract.
//
// EVERY ASSERTION IS ABOUT A SEND THAT MUST NOT HAPPEN. The other two products
// are rigged to succeed, so a leaked advance shows up as an unexpected success
// rather than as a subtle difference in the returned error.
func TestPayDiscoveryStopConditions(t *testing.T) {
	for _, tt := range []struct {
		name string
		code string
	}{
		{"RateLimited may be a per-account rule, never advance", CodeRateLimited},
		{"AccountNumberInvalid: no other product can help", CodeAccountNumberInvalid},
		{"an unrecognised code stops, because advance is an allow-list", "SomethingIntroducedTomorrow"},
		{"InsufficientBalance stops", CodeInsufficientBalance},
		{"AuthenticationFailed stops", CodeAuthenticationFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := newPayAPI()
			api.products = threeOperatorCatalogue()
			api.transfers["SKU_A"] = errTransfer(tt.code)
			api.transfers["SKU_B"] = okTransfer("SKU_B", 1000, "ARS")
			api.transfers["SKU_C"] = okTransfer("SKU_C", 1000, "ARS")

			res, err := api.client(t).Pay(context.Background(), pinnedRequest())
			if err == nil {
				t.Fatal("want an error, got success -- the cascade advanced past a stop code")
			}
			if res.Transfer != nil {
				t.Error("no transfer should have completed")
			}
			if got := api.sentSkus(); !eqStrings(got, []string{"SKU_A"}) {
				t.Errorf("sent %v, want only [SKU_A]: %s must stop the cascade", got, tt.code)
			}
			// The code must be surfaced verbatim, never swallowed.
			if !HasCode(err, tt.code) {
				t.Errorf("err = %v, want it to carry %s", err, tt.code)
			}
		})
	}
}

// TestPayStopsOnTransportFault. A fault means we do not know whether money
// moved -- the request may have executed and the response been lost -- so
// advancing to a different product risks paying twice.
func TestPayStopsOnTransportFault(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.transfers["SKU_A"] = stubResponse{502, `<html>bad gateway</html>`}
	api.transfers["SKU_B"] = okTransfer("SKU_B", 1000, "ARS")
	api.transfers["SKU_C"] = okTransfer("SKU_C", 1000, "ARS")

	_, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err == nil {
		t.Fatal("want an error")
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"SKU_A"}) {
		t.Errorf("sent %v, want only [SKU_A]: we do not know whether money moved", got)
	}
}

// TestPayExhaustedReturnsTheLastRealFailure: three refusals in a row is the
// honest answer -- no pinned product accepted this account -- and needs no
// synthesised error code of its own.
func TestPayExhaustedReturnsTheLastRealFailure(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.fallback = &stubResponse{200, errTransfer(CodeRechargeNotAllowed).body}

	res, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err == nil {
		t.Fatal("want an error")
	}
	if !HasCode(err, CodeRechargeNotAllowed) {
		t.Errorf("err = %v, want the real last failure (%s)", err, CodeRechargeNotAllowed)
	}
	if len(api.sends) != 3 {
		t.Errorf("sent %d transfers, want all 3 candidates tried", len(api.sends))
	}
	if len(res.Resolution.Attempts) != 3 {
		t.Errorf("Attempts = %d, want 3 -- the trace survives a total failure", len(res.Resolution.Attempts))
	}
}

// TestPayDiscoveryDriftFailsBeforeAnySend pins the strict reading: one stale pin
// fails the payment even though the account's real operator may be fine.
//
// The alternative pays this account while leaving a catalogue change
// undetected, which is the silent-wrong-amount failure Pay exists to remove.
func TestPayDiscoveryDriftFailsBeforeAnySend(t *testing.T) {
	api := newPayAPI()
	api.products = []Product{
		fixedProduct("SKU_A", "OPA", 0.79, 1000, "ARS"),
		fixedProduct("SKU_B", "OPB", 0.85, 1000, "ARS"),
		// SKU_C has vanished.
	}
	api.fallback = &stubResponse{200, okTransfer("X", 1000, "ARS").body}

	_, err := api.client(t).Pay(context.Background(), pinnedRequest())
	re, ok := IsResolutionError(err)
	if !ok {
		t.Fatalf("err = %v, want a *ResolutionError", err)
	}
	if re.Reason != ReasonPinSkuMissing {
		t.Errorf("Reason = %q, want %q", re.Reason, ReasonPinSkuMissing)
	}
	if len(api.sends) != 0 {
		t.Errorf("sent %v; no money moves while a pin is known to be stale", api.sentSkus())
	}
}

// TestPayNamedOperatorSkipsDetection: naming the operator is the point of the
// field, so it must not spend a lookup.
func TestPayNamedOperatorSkipsDetection(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.transfers["SKU_C"] = okTransfer("SKU_C", 1000, "ARS")

	req := pinnedRequest()
	req.Operator = "OPC"

	if _, err := api.client(t).Pay(context.Background(), req); err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if api.lookupCall != 0 {
		t.Errorf("GetAccountLookup called %d times, want 0 when the operator is named", api.lookupCall)
	}
	if got := api.sentSkus(); !eqStrings(got, []string{"SKU_C"}) {
		t.Errorf("sent %v, want [SKU_C]", got)
	}
}

// TestPayTreatsMultipleLookupItemsAsInconclusive. An account belongs to one
// operator, so several Items is a data quirk rather than a choice to make;
// declining to arbitrate and letting a transfer settle it cannot pick wrong.
func TestPayTreatsMultipleLookupItemsAsInconclusive(t *testing.T) {
	api := newPayAPI()
	api.products = threeOperatorCatalogue()
	api.lookup = `{"CountryIso":"AR","Items":[
		{"ProviderCode":"OPA","SkuCodes":["X"]},
		{"ProviderCode":"OPB","SkuCodes":["Y"]}],"ResultCode":1,"ErrorCodes":[]}`
	api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

	res, err := api.client(t).Pay(context.Background(), pinnedRequest())
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Resolution.Path != PathDiscovery {
		t.Errorf("Path = %q, want %q -- an ambiguous lookup falls back to discovery",
			res.Resolution.Path, PathDiscovery)
	}
}

// ---------------------------------------------------------------------------
// The cascade policy in isolation
// ---------------------------------------------------------------------------

// TestDecideCascade is the whole policy, pinned without a server.
func TestDecideCascade(t *testing.T) {
	for _, tt := range []struct {
		name    string
		o       outcome
		hasNext bool
		want    cascadeAction
	}{
		{"success returns", outcome{completed: true}, true, actionReturn},
		{"fault returns", outcome{fault: true}, true, actionReturn},
		{"RechargeNotAllowed advances", outcome{codes: []string{CodeRechargeNotAllowed}}, true, actionAdvance},
		{"RechargeNotAllowed returns when last", outcome{codes: []string{CodeRechargeNotAllowed}}, false, actionReturn},
		{"AccountNumberInvalid returns", outcome{codes: []string{CodeAccountNumberInvalid}}, true, actionReturn},
		{"RateLimited returns", outcome{codes: []string{CodeRateLimited}}, true, actionReturn},

		// A stop code anywhere in the array wins. Reading Codes[0] would
		// advance past a rate limit in the second of these.
		{"RateLimited first", outcome{codes: []string{CodeRateLimited, CodeRechargeNotAllowed}}, true, actionReturn},
		{"RateLimited second", outcome{codes: []string{CodeRechargeNotAllowed, CodeRateLimited}}, true, actionReturn},
		{"AccountNumberInvalid second", outcome{codes: []string{CodeRechargeNotAllowed, CodeAccountNumberInvalid}}, true, actionReturn},

		{"unrecognised returns", outcome{codes: []string{"BrandNewCode"}}, true, actionReturn},
		{"unrecognised alongside RechargeNotAllowed still advances",
			outcome{codes: []string{CodeRechargeNotAllowed, "BrandNewCode"}}, true, actionAdvance},
		{"no codes returns", outcome{}, true, actionReturn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideCascade(tt.o, tt.hasNext); got != tt.want {
				t.Errorf("decideCascade(%+v, %v) = %q, want %q", tt.o, tt.hasNext, got, tt.want)
			}
		})
	}
}

// TestOnlyRechargeNotAllowedEverAdvances states the safety property directly
// rather than sampling it: over every code this package defines, exactly one
// may cause another send.
//
// A table can drift by omission. This cannot: adding a code to errors.go and
// forgetting to consider it leaves this passing only because the default is
// stop, which IS the guarantee.
func TestOnlyRechargeNotAllowedEverAdvances(t *testing.T) {
	every := []string{
		CodeNearestMatch, CodeTransientProviderError, CodeProviderError,
		CodeRechargeNotAllowed, CodeRateLimited, CodeInsufficientBalance,
		CodeOtherError, CodeParameterMissing, CodeBatchItemRefMustBeUnique,
		CodeAccountNumberInvalid, CodeParameterOutOfRange, CodeRequestInvalid,
		CodeAuthenticationFailed, CodeParameterInvalid, CodeParameterCombinationInvalid,
		CodeTooManyParameters, CodeDuplicateTransactionPrevented,
	}

	for _, code := range every {
		got := decideCascade(outcome{codes: []string{code}}, true)
		want := actionReturn
		if code == CodeRechargeNotAllowed {
			want = actionAdvance
		}
		if got != want {
			t.Errorf("decideCascade(%s) = %q, want %q", code, got, want)
		}
	}
}

// TestRateLimitedIsStricterThanRetryable records a deliberate disagreement
// inside this package.
//
// Error.Retryable says RateLimited is retryable, and it is right about its own
// question: repeating the IDENTICAL request after a wait may well succeed. The
// cascade asks a different question -- may we send a DIFFERENT product now --
// and the answer must be no, because RateLimited can mean a per-account rule
// was breached and the response does not say which.
func TestRateLimitedIsStricterThanRetryable(t *testing.T) {
	e := &Error{ResultCode: ResultCodeFailed, Codes: []ErrorCode{{Code: CodeRateLimited}}}
	if !e.Retryable() {
		t.Fatal("precondition: Error.Retryable is expected to be true for RateLimited")
	}
	if got := decideCascade(outcome{codes: []string{CodeRateLimited}}, true); got != actionReturn {
		t.Errorf("decideCascade = %q, want %q despite Retryable being true", got, actionReturn)
	}
}

// ---------------------------------------------------------------------------
// Request validation
// ---------------------------------------------------------------------------

// TestPayValidationRejectsBeforeAnyCall: an incoherent request never reaches
// the network.
func TestPayValidationRejectsBeforeAnyCall(t *testing.T) {
	valid := pinnedRequest()

	tests := []struct {
		name    string
		mutate  func(*PayRequest)
		wantMsg string
	}{
		{"no account number", func(r *PayRequest) { r.AccountNumber = "" }, "AccountNumber is required"},
		{"no distributor ref", func(r *PayRequest) { r.DistributorRef = "" }, "DistributorRef is required"},
		{"zero amount", func(r *PayRequest) { r.Amount = 0 }, "Amount must be positive"},
		{"negative amount", func(r *PayRequest) { r.Amount = -1 }, "Amount must be positive"},
		{"no currency", func(r *PayRequest) { r.AmountCurrency = "" }, "AmountCurrency is required"},
		{"negative tolerance", func(r *PayRequest) { r.Tolerance = -1 }, "Tolerance must not be negative"},
		{"empty operators map", func(r *PayRequest) { r.Operators = map[string]OperatorPin{} }, "present but empty"},
		{"pin without sku", func(r *PayRequest) {
			r.Operators = map[string]OperatorPin{"OPA": {SendValue: 1}}
		}, "missing SkuCode"},
		// A pin with no SendValue is what "one value applies to every
		// operator" looks like in a config file.
		{"pin without send value", func(r *PayRequest) {
			r.Operators = map[string]OperatorPin{"OPA": {SkuCode: "S"}}
		}, "positive SendValue"},
		{"empty operator code", func(r *PayRequest) {
			r.Operators = map[string]OperatorPin{"": {SkuCode: "S", SendValue: 1}}
		}, "empty operator code"},
		{"too many pins", func(r *PayRequest) {
			r.Operators = map[string]OperatorPin{}
			for _, k := range []string{"A", "B", "C", "D", "E", "F"} {
				r.Operators[k] = OperatorPin{SkuCode: "S" + k, SendValue: 1}
			}
		}, "at most 5 operators"},
		// A derived ref must fit, and it is checked up front rather than
		// mid-cascade with money at stake.
		{"derived ref too long", func(r *PayRequest) {
			r.DistributorRef = strings.Repeat("x", 60)
		}, "over the 64 limit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newPayAPI()
			api.products = threeOperatorCatalogue()
			api.lookup = lookupBody("OPA")
			api.fallback = &stubResponse{200, okTransfer("X", 1000, "ARS").body}

			req := valid
			tt.mutate(&req)

			_, err := api.client(t).Pay(context.Background(), req)
			re, ok := IsResolutionError(err)
			if !ok {
				t.Fatalf("err = %v, want a *ResolutionError", err)
			}
			if re.Reason != ReasonInvalidRequest {
				t.Errorf("Reason = %q, want %q", re.Reason, ReasonInvalidRequest)
			}
			if !strings.Contains(re.Message, tt.wantMsg) {
				t.Errorf("Message = %q, want it to contain %q", re.Message, tt.wantMsg)
			}
			if api.lookupCall != 0 || api.productCall != 0 || len(api.sends) != 0 {
				t.Errorf("an invalid request must make no calls; got lookup=%d products=%d sends=%d",
					api.lookupCall, api.productCall, len(api.sends))
			}
		})
	}
}

// TestOperatorKeyMatchingIsCaseInsensitive. Operator codes are hand-written by
// whoever researched the pin; exact matching would decline payments for
// configurations that are obviously right, and a key matching nothing still
// fails loudly.
func TestOperatorKeyMatchingIsCaseInsensitive(t *testing.T) {
	for _, pinKey := range []string{"opa", "OPA", "Opa", "  opa  "} {
		t.Run(pinKey, func(t *testing.T) {
			api := newPayAPI()
			api.products = threeOperatorCatalogue()
			api.lookup = lookupBody("OPA")
			api.transfers["SKU_A"] = okTransfer("SKU_A", 1000, "ARS")

			req := pinnedRequest()
			req.Operators = map[string]OperatorPin{pinKey: {SkuCode: "SKU_A", SendValue: 0.79}}

			if _, err := api.client(t).Pay(context.Background(), req); err != nil {
				t.Fatalf("Pay with pin key %q: %v", pinKey, err)
			}
			if got := api.sentSkus(); !eqStrings(got, []string{"SKU_A"}) {
				t.Errorf("sent %v, want [SKU_A]", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Pricing helpers
// ---------------------------------------------------------------------------

func TestDeliveredFor(t *testing.T) {
	fx := fixedProduct("F", "OP", 0.79, 1000, "ARS")
	rg := rangeProduct("R", "OP", 1.0, 100, 10.0, 1000, "BOB")

	if got, ok := deliveredFor(fx, 0.79); !ok || got != 1000 {
		t.Errorf("fixed at its value = (%v, %v), want (1000, true)", got, ok)
	}
	// A fixed product accepts exactly one send value; accepting a near miss
	// would deliver an amount nobody asked for.
	if _, ok := deliveredFor(fx, 0.85); ok {
		t.Error("fixed product must reject any other send value")
	}
	if got, ok := deliveredFor(rg, 5.5); !ok || got < 549.9 || got > 550.1 {
		t.Errorf("range midpoint = (%v, %v), want (~550, true)", got, ok)
	}
	if _, ok := deliveredFor(rg, 0.5); ok {
		t.Error("range product must reject a send value below its minimum")
	}
	if _, ok := deliveredFor(rg, 11); ok {
		t.Error("range product must reject a send value above its maximum")
	}
}

func TestSendValueFor(t *testing.T) {
	rg := rangeProduct("R", "OP", 1.0, 100, 10.0, 1000, "BOB")

	if got, ok := sendValueFor(rg, 550); !ok || got < 5.4 || got > 5.6 {
		t.Errorf("solve for 550 = (%v, %v), want (~5.5, true)", got, ok)
	}
	if got, ok := sendValueFor(rg, 50); !ok || got != 1.0 {
		t.Errorf("clamp up = (%v, %v), want (1.0, true)", got, ok)
	}
	if _, ok := sendValueFor(rg, 5000); ok {
		t.Error("a product that cannot reach the amount must not be a candidate")
	}
}

// TestRoundUpCents pins the direction. Rounding down could land the delivered
// value a hair under the floor the caller declared, and Amount is a floor.
func TestRoundUpCents(t *testing.T) {
	for _, tt := range []struct{ in, want float64 }{
		{1.2301, 1.24},
		{1.23, 1.23},
		{5.5, 5.5},
	} {
		if got := roundUpCents(tt.in); got != tt.want {
			t.Errorf("roundUpCents(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
