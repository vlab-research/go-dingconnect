package dingconnect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve spins up a stub API and returns a Client pointed at it, plus a pointer
// to the last request the handler saw.
func serve(t *testing.T, h http.HandlerFunc) (*Client, *http.Request, *[]byte) {
	t.Helper()
	var gotReq http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		gotReq = *r
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return New("test-key", WithBaseURL(srv.URL)), &gotReq, &gotBody
}

func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// TestAuthHeaderIsApiKey pins the single most load-bearing fact about this API.
// DingConnect authenticates on the lowercase "api_key" header; X-Api-Key and
// Authorization are both rejected with HTTP 401 / AuthenticationFailed. A
// previous integration in this org shipped X-Api-Key and failed every call.
func TestAuthHeaderIsApiKey(t *testing.T) {
	c, req, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"Balance":5,"CurrencyIso":"USD"}`))

	if _, err := c.Balance(context.Background()); err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got := req.Header.Get("api_key"); got != "test-key" {
		t.Errorf("api_key header = %q, want %q", got, "test-key")
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("X-Api-Key should not be sent, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization should not be sent, got %q", got)
	}
}

// TestWireFormatIsPascalCase guards the second defect that broke the previous
// integration: snake_case field names decode to zero values silently.
func TestWireFormatIsPascalCase(t *testing.T) {
	t.Run("response decodes PascalCase", func(t *testing.T) {
		c, _, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"Balance":12.34,"CurrencyIso":"USD"}`))
		b, err := c.Balance(context.Background())
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		if b.Balance != 12.34 || b.CurrencyIso != "USD" {
			t.Errorf("got %+v, want Balance=12.34 CurrencyIso=USD", b)
		}
	})

	t.Run("snake_case response yields zero values", func(t *testing.T) {
		// Demonstrates the failure mode rather than endorsing it: a
		// snake_case body decodes without error into an empty struct,
		// which is exactly why the bug went unnoticed for so long.
		c, _, _ := serve(t, jsonHandler(200, `{"result_code":1,"balance":12.34,"currency_iso":"USD"}`))
		_, err := c.Balance(context.Background())
		if err == nil {
			t.Fatal("want an error: ResultCode decodes to 0, not 1")
		}
	})

	t.Run("request marshals PascalCase", func(t *testing.T) {
		c, _, body := serve(t, jsonHandler(200,
			`{"ResultCode":1,"ErrorCodes":[],"TransferRecord":{"ProcessingState":"Completed"}}`))
		_, err := c.SendTransfer(context.Background(), SendTransferRequest{
			SkuCode: "X1", SendValue: 5, AccountNumber: "234800", DistributorRef: "r1",
		})
		if err != nil {
			t.Fatalf("SendTransfer: %v", err)
		}
		var sent map[string]any
		if err := json.Unmarshal(*body, &sent); err != nil {
			t.Fatalf("request body was not JSON: %v", err)
		}
		for _, key := range []string{"SkuCode", "SendValue", "AccountNumber", "DistributorRef"} {
			if _, ok := sent[key]; !ok {
				t.Errorf("request missing PascalCase key %q; got %v", key, keys(sent))
			}
		}
		for _, key := range []string{"sku_code", "send_value", "account_number", "distributor_ref"} {
			if _, ok := sent[key]; ok {
				t.Errorf("request must not use snake_case key %q", key)
			}
		}
	})
}

// TestResultCodeBeatsHTTPStatus covers DingConnect reporting application errors
// with a 200, which means the HTTP status alone never determines success.
func TestResultCodeBeatsHTTPStatus(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(200,
		`{"ResultCode":4,"ErrorCodes":[{"Code":"AuthenticationFailed","Context":null}]}`))

	_, err := c.Balance(context.Background())
	if err == nil {
		t.Fatal("want an error for ResultCode 4 despite HTTP 200")
	}
	if !IsAuthError(err) {
		t.Errorf("IsAuthError = false, want true for %v", err)
	}
}

// TestInsufficientBalanceIsNotRetryable is the regression test for a bug found
// against the live API: DingConnect answers InsufficientBalance with HTTP 500,
// so classifying retryability by HTTP status alone marks a permanently
// doomed transfer as worth retrying.
func TestInsufficientBalanceIsNotRetryable(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(500,
		`{"TransferRecord":{"SkuCode":"2ANG44349","ProcessingState":"Failed"},
		  "ResultCode":5,"ErrorCodes":[{"Code":"InsufficientBalance"}]}`))

	res, err := c.SendTransfer(context.Background(), SendTransferRequest{
		SkuCode: "2ANG44349", SendValue: 12.08, AccountNumber: "234800", DistributorRef: "r1",
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if IsRetryable(err) {
		t.Errorf("InsufficientBalance must not be retryable, got retryable: %v", err)
	}
	if !HasCode(err, CodeInsufficientBalance) {
		t.Errorf("HasCode(InsufficientBalance) = false for %v", err)
	}
	// The record must survive the error so callers can log what was attempted.
	if res.TransferRecord == nil {
		t.Fatal("TransferRecord must be returned alongside the error")
	}
	if res.TransferRecord.SkuCode != "2ANG44349" {
		t.Errorf("SkuCode = %q, want 2ANG44349", res.TransferRecord.SkuCode)
	}
	if res.TransferRecord.Completed() {
		t.Error("Completed() must be false for a Failed record")
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"transient provider error", 200,
			`{"ResultCode":3,"ErrorCodes":[{"Code":"TransientProviderError"}]}`, true},
		{"rate limited", 200,
			`{"ResultCode":4,"ErrorCodes":[{"Code":"RateLimited"}]}`, true},
		{"insufficient balance over 500", 500,
			`{"ResultCode":5,"ErrorCodes":[{"Code":"InsufficientBalance"}]}`, false},
		{"auth failure", 401,
			`{"ResultCode":4,"ErrorCodes":[{"Code":"AuthenticationFailed"}]}`, false},
		{"invalid account", 200,
			`{"ResultCode":4,"ErrorCodes":[{"Code":"AccountNumberInvalid"}]}`, false},
		{"duplicate prevented", 200,
			`{"ResultCode":5,"ErrorCodes":[{"Code":"DuplicateTransactionPrevented"}]}`, false},
		{"unparseable 502 has no ResultCode", 502, `<html>bad gateway</html>`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _ := serve(t, jsonHandler(tt.status, tt.body))
			_, err := c.Balance(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsRetryable(err); got != tt.want {
				t.Errorf("IsRetryable = %v, want %v (err: %v)", got, tt.want, err)
			}
		})
	}
}

// TestNearestMatchIsAnErrorButCarriesData covers the partial-result contract:
// callers must opt in to accepting an approximate answer, and the data is
// there when they do.
func TestNearestMatchIsAnErrorButCarriesData(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(200,
		`{"CountryIso":"NG","AccountNumberNormalized":"2348012345678","Items":[],
		  "ResultCode":2,"ErrorCodes":[{"Code":"NearestMatch"}]}`))

	res, err := c.AccountLookup(context.Background(), "+2348012345678")
	if err == nil {
		t.Fatal("NearestMatch must surface as an error so it cannot pass unnoticed")
	}
	if !IsNearestMatch(err) {
		t.Errorf("IsNearestMatch = false for %v", err)
	}
	if IsRetryable(err) {
		t.Error("NearestMatch is not retryable")
	}
	if res.CountryIso != "NG" || res.AccountNumberNormalized != "2348012345678" {
		t.Errorf("partial result must still be populated, got %+v", res)
	}
}

// TestArrayParamsRepeatKey pins how DingConnect expects list parameters:
// countryIsos=NG&countryIsos=KE, not a comma-joined single value.
func TestArrayParamsRepeatKey(t *testing.T) {
	c, req, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"Items":[]}`))

	_, err := c.Products(context.Background(), ProductFilter{CountryISOs: []string{"NG", "KE"}})
	if err != nil {
		t.Fatalf("Products: %v", err)
	}
	got := req.URL.Query()["countryIsos"]
	if len(got) != 2 || got[0] != "NG" || got[1] != "KE" {
		t.Errorf("countryIsos = %v, want [NG KE] as repeated keys", got)
	}
}

// TestEmptyFiltersAreOmitted keeps blank values out of the query string, which
// DingConnect rejects with ParameterInvalid rather than ignoring.
func TestEmptyFiltersAreOmitted(t *testing.T) {
	c, req, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"Items":[]}`))

	_, err := c.Providers(context.Background(), ProviderFilter{CountryISOs: []string{"NG"}})
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	if _, ok := req.URL.Query()["accountNumber"]; ok {
		t.Error("empty accountNumber must be omitted from the query")
	}
	if _, ok := req.URL.Query()["providerCodes"]; ok {
		t.Error("empty providerCodes must be omitted from the query")
	}
}

// TestEstimatePricesSendsBareArray pins the one endpoint whose body is a
// top-level JSON array rather than an object.
func TestEstimatePricesSendsBareArray(t *testing.T) {
	c, _, body := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"Items":[]}`))

	_, err := c.EstimatePrices(context.Background(), []EstimateRequest{
		{BatchItemRef: "a1", SkuCode: "X1", SendValue: 5},
	})
	if err != nil {
		t.Fatalf("EstimatePrices: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(*body)), "[") {
		t.Errorf("body must be a bare JSON array, got %s", *body)
	}
}

// TestValidateOnlyAlwaysSent pins the opposite of what this test used to
// assert. The field was `omitempty`, so a real send (ValidateOnly false)
// carried no ValidateOnly at all, and DingConnect refuses such a body with
// ResultCode 4 / ParameterInvalid / context "ValidateOnly". Validate-only
// calls (true) were unaffected, which is why every test passed and every real
// payment failed. Measured live 2026-09-07; see SendTransferRequest.
func TestValidateOnlyAlwaysSent(t *testing.T) {
	c, _, body := serve(t, jsonHandler(200,
		`{"ResultCode":1,"ErrorCodes":[],"TransferRecord":{"ProcessingState":"Completed"}}`))

	_, err := c.SendTransfer(context.Background(), SendTransferRequest{
		SkuCode: "X1", SendValue: 5, AccountNumber: "234800", DistributorRef: "r1",
	})
	if err != nil {
		t.Fatalf("SendTransfer: %v", err)
	}
	var sent map[string]any
	json.Unmarshal(*body, &sent)
	v, ok := sent["ValidateOnly"]
	if !ok {
		t.Fatal("ValidateOnly must be present on every SendTransfer body; DingConnect rejects its absence")
	}
	if v != false {
		t.Errorf("ValidateOnly = %v, want false on a real send", v)
	}
}

// TestAccountNumberLeadingPlusStripped: survey forms hand us E.164 ("+591..."),
// DingConnect's account regex rejects the "+". Measured live 2026-09-07:
// "+59172690398" -> AccountNumberInvalid / AccountNumberFailedRegex,
// "59172690398" -> Complete.
func TestAccountNumberLeadingPlusStripped(t *testing.T) {
	c, _, body := serve(t, jsonHandler(200,
		`{"ResultCode":1,"ErrorCodes":[],"TransferRecord":{"ProcessingState":"Completed"}}`))

	_, err := c.SendTransfer(context.Background(), SendTransferRequest{
		SkuCode: "X1", SendValue: 5, AccountNumber: " +59172690398 ", DistributorRef: "r1",
	})
	if err != nil {
		t.Fatalf("SendTransfer: %v", err)
	}
	var sent map[string]any
	json.Unmarshal(*body, &sent)
	if sent["AccountNumber"] != "59172690398" {
		t.Errorf("AccountNumber = %q, want %q", sent["AccountNumber"], "59172690398")
	}
	// Only a leading "+" and whitespace: anything else is the caller's to see.
	if got := normalizeAccountNumber("59 17-26"); got != "59 17-26" {
		t.Errorf("normalizeAccountNumber must not rewrite interior characters, got %q", got)
	}
	if got := normalizeAccountNumber("++591"); got != "+591" {
		t.Errorf("only one leading + is removed, got %q", got)
	}
}

func TestNoAPIKeyFailsBeforeRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := New("", WithBaseURL(srv.URL))
	if _, err := c.Balance(context.Background()); err == nil {
		t.Fatal("want an error when no API key is configured")
	}
	if called {
		t.Error("must not issue a request without an API key")
	}
}

func TestContextCancellation(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[]}`))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Balance(ctx); err == nil {
		t.Fatal("want an error for a cancelled context")
	}
}

func TestErrorMessageIsActionable(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(200,
		`{"ResultCode":4,"ErrorCodes":[{"Code":"ParameterInvalid","Context":"SkuCode"}]}`))

	_, err := c.Balance(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"GetBalance", "ResultCode=4", "ParameterInvalid", "SkuCode"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing %q", msg, want)
		}
	}
}

func TestProductFixedValue(t *testing.T) {
	fixed := Product{
		Minimum: Price{SendValue: 12.08},
		Maximum: Price{SendValue: 12.08},
	}
	variable := Product{
		Minimum: Price{SendValue: 2},
		Maximum: Price{SendValue: 105},
	}
	if !fixed.FixedValue() {
		t.Error("equal min and max must report FixedValue")
	}
	if variable.FixedValue() {
		t.Error("differing min and max must not report FixedValue")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTransferRecordsUnwrapsEnvelope pins the real wire shape of
// ListTransferRecords, captured live 2026-09-07: every item is wrapped in a
// SendTransfer-style envelope. Decoding items as bare records gave a page of
// empty structs and `dingconnect transfers` printed blank rows.
func TestTransferRecordsUnwrapsEnvelope(t *testing.T) {
	c, _, _ := serve(t, jsonHandler(200, `{"ResultCode":1,"ErrorCodes":[],"ThereAreMoreItems":false,"Items":[
	  {"TransferRecord":{"TransferId":{"TransferRef":"863785357","DistributorRef":"lacbo_+59172690398_p1_t5"},
	    "SkuCode":"BO_EN_TopUp","Price":{"ReceiveValue":11.0,"ReceiveCurrencyIso":"BOB","SendValue":1.15,"SendCurrencyIso":"USD"},
	    "ProcessingState":"Complete","AccountNumber":"59172690398"},"ResultCode":1,"ErrorCodes":[]},
	  {"TransferId":{"TransferRef":"bare","DistributorRef":"r2"},"SkuCode":"X","ProcessingState":"Complete"}
	]}`))
	res, err := c.TransferRecords(context.Background(), TransferFilter{Take: 5})
	if err != nil {
		t.Fatalf("TransferRecords: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("Items = %d, want 2", len(res.Items))
	}
	if res.Items[0].TransferId.TransferRef != "863785357" || res.Items[0].AccountNumber != "59172690398" || !res.Items[0].Completed() {
		t.Errorf("wrapped item not unwrapped: %+v", res.Items[0])
	}
	if res.Items[1].TransferId.TransferRef != "bare" {
		t.Errorf("bare item not accepted: %+v", res.Items[1])
	}
}
