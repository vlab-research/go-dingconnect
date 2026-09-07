package dingconnect

import (
	"fmt"
	"net/http"
	"strings"
)

// Result codes returned in the ResultCode field of every DingConnect response.
// Only ResultCodeSuccess means "the request did exactly what you asked".
const (
	ResultCodeSuccess   = 1 // request fulfilled exactly
	ResultCodePartial   = 2 // fulfilled approximately; see the NearestMatch error code
	ResultCodeTransient = 3
	ResultCodeInvalid   = 4 // malformed request or bad credentials
	ResultCodeFailed    = 5 // valid request the system refused (e.g. InsufficientBalance)
)

// Error codes returned in the ErrorCodes array. The full authoritative list is
// available at runtime via Client.ErrorCodeDescriptions; these are the ones
// callers branch on.
const (
	CodeNearestMatch                  = "NearestMatch"
	CodeTransientProviderError        = "TransientProviderError"
	CodeProviderError                 = "ProviderError"
	CodeRechargeNotAllowed            = "RechargeNotAllowed"
	CodeRateLimited                   = "RateLimited"
	CodeInsufficientBalance           = "InsufficientBalance"
	CodeOtherError                    = "OtherError"
	CodeParameterMissing              = "ParameterMissing"
	CodeBatchItemRefMustBeUnique      = "BatchItemRefMustBeUnique"
	CodeAccountNumberInvalid          = "AccountNumberInvalid"
	CodeParameterOutOfRange           = "ParameterOutOfRange"
	CodeRequestInvalid                = "RequestInvalid"
	CodeAuthenticationFailed          = "AuthenticationFailed"
	CodeParameterInvalid              = "ParameterInvalid"
	CodeParameterCombinationInvalid   = "ParameterCombinationInvalid"
	CodeTooManyParameters             = "TooManyParameters"
	CodeDuplicateTransactionPrevented = "DuplicateTransactionPrevented"
)

// ErrorCode is a single entry in a response's ErrorCodes array. Context names
// the offending parameter for the Parameter* codes and is empty otherwise.
type ErrorCode struct {
	Code    string `json:"Code"`
	Context string `json:"Context"`
}

func (e ErrorCode) String() string {
	if e.Context == "" {
		return e.Code
	}
	return e.Code + " (" + e.Context + ")"
}

// Status is embedded in every DingConnect response. DingConnect reports
// application-level failure in the body with HTTP 200, so ResultCode -- not the
// HTTP status -- is the real signal.
type Status struct {
	ResultCode int         `json:"ResultCode"`
	ErrorCodes []ErrorCode `json:"ErrorCodes"`
}

func (s Status) status() Status { return s }

// statusHolder constrains the generic request helper to types that carry a
// Status, which every response in this package does by embedding it.
type statusHolder interface {
	status() Status
}

// Error is returned whenever a call does not come back with
// ResultCodeSuccess, including the ResultCodePartial "nearest match" case.
// Partial results are surfaced as errors on purpose: a caller that asked for
// one thing and silently received another is a bug waiting to happen. Use
// IsNearestMatch to opt into accepting them.
//
// The decoded response value is still returned alongside a non-nil Error, so
// failed SendTransfer calls can be inspected for their TransferRecord.
type Error struct {
	// ResultCode is the body's ResultCode. Zero when the request failed
	// before a body was decoded (transport error, non-JSON response).
	ResultCode int
	// Codes holds the body's ErrorCodes array.
	Codes []ErrorCode
	// StatusCode is the HTTP status.
	StatusCode int
	Method     string
	Path       string
	// Err wraps an underlying transport or decode failure, if any.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dingconnect: %s %s", e.Method, e.Path)
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " [HTTP %d]", e.StatusCode)
	}
	if e.ResultCode != 0 {
		fmt.Fprintf(&b, " ResultCode=%d", e.ResultCode)
	}
	if len(e.Codes) > 0 {
		codes := make([]string, len(e.Codes))
		for i, c := range e.Codes {
			codes[i] = c.String()
		}
		fmt.Fprintf(&b, ": %s", strings.Join(codes, ", "))
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Code returns the first error code, or "" if there are none.
func (e *Error) Code() string {
	if len(e.Codes) == 0 {
		return ""
	}
	return e.Codes[0].Code
}

// Has reports whether the given error code appears in the response.
func (e *Error) Has(code string) bool {
	for _, c := range e.Codes {
		if c.Code == code {
			return true
		}
	}
	return false
}

// Retryable reports whether retrying the identical request could plausibly
// succeed. It is true for provider-side transient failures and rate limiting,
// and never for validation, authentication, or balance failures, which will
// fail identically forever.
//
// The HTTP status is deliberately only consulted when no ResultCode was
// decoded. DingConnect answers InsufficientBalance with HTTP 500 despite it
// being permanent, so trusting 5xx would retry a transfer that can never
// succeed until the account is funded. Whenever the body parsed, the body
// decides.
//
// Always reuse the same DistributorRef on retry: it is the reference support
// works from. Do NOT assume it prevents a double payment. This comment used to
// say DingConnect answers a replayed reference with
// DuplicateTransactionPrevented; a live replay on 2026-09-07 was paid a second
// time (see SendTransferRequest.DistributorRef). A retry is only safe when the
// first attempt is known not to have completed.
func (e *Error) Retryable() bool {
	if e.ResultCode != 0 {
		return e.ResultCode == ResultCodeTransient ||
			e.Has(CodeTransientProviderError) ||
			e.Has(CodeRateLimited)
	}
	// No usable body: fall back to transport-level signals.
	return e.StatusCode >= http.StatusInternalServerError || e.StatusCode == http.StatusTooManyRequests
}

// asError converts a *Error out of an error interface, or nil.
func asError(err error) *Error {
	e, _ := err.(*Error)
	return e
}

// IsNearestMatch reports whether err is the partial-result signal: DingConnect
// could not fulfil the request exactly and returned the closest valid answer
// instead. The response value returned alongside such an error is populated
// and usable, so callers who accept approximate results can ignore it.
func IsNearestMatch(err error) bool {
	e := asError(err)
	return e != nil && (e.ResultCode == ResultCodePartial || e.Has(CodeNearestMatch))
}

// IsRetryable reports whether err represents a transient failure worth retrying.
func IsRetryable(err error) bool {
	e := asError(err)
	return e != nil && e.Retryable()
}

// IsAuthError reports whether err was caused by a missing or invalid API key.
func IsAuthError(err error) bool {
	e := asError(err)
	return e != nil && (e.Has(CodeAuthenticationFailed) || e.StatusCode == http.StatusUnauthorized)
}

// HasCode reports whether err is a *Error carrying the given DingConnect error code.
func HasCode(err error, code string) bool {
	e := asError(err)
	return e != nil && e.Has(code)
}
