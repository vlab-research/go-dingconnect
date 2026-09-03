package dingconnect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Paying by delivered amount rather than by SKU.
//
// WHY THIS EXISTS. A SkuCode identifies one operator's product, and its
// SendValue is a charge in the send currency whose DELIVERED value differs per
// operator because commission rates differ. Three products can all deliver
// ARS 1,000 at 0.79, 0.85 and 0.93 USD. So a bare SendValue is an
// uninterpretable number: nothing records what it was for, which means nothing
// can check it. When a commission rate moves, the old SendValue is still a
// perfectly valid request -- the transfer completes, ResultCode is 1, and the
// recipient quietly gets less than the caller intended.
//
// Pay closes that hole by making the caller declare the intent (deliver at
// least Amount of AmountCurrency, within Tolerance) and optionally pin the
// products it believes satisfy that intent. The declared intent is what makes
// the pinned number verifiable; that is the whole reason for carrying both.
//
// The policy below -- amount selection, pin verification, and which error codes
// may advance a cascade -- is error-code semantics, which is this package's job
// and not its callers'. See CLAUDE.md, "Relationship to fly".

// MaxCandidates caps how many transfers one Pay call may attempt.
//
// This is a timeout bound, not a matter of taste. DefaultTimeout is 90s per
// request, so N sequential sends is N*90s of wall clock inside whatever budget
// the caller has. Pay takes ONE context for the whole resolution precisely so a
// cascade cannot outrun a caller's deadline, and this cap keeps a caller from
// authoring a forty-operator pin in the first place.
const MaxCandidates = 5

// MaxDistributorRefLen bounds a derived per-candidate reference.
//
// UNVERIFIED against the live API. DingConnect documents no length or charset
// limit for DistributorRef and no rejection has been observed, so this is a
// conservative guess with room to spare over realistic refs. It is enforced up
// front, before any money moves, rather than truncating: a truncated ref is
// unsearchable in ListTransferRecords, which is exactly when you need it.
const MaxDistributorRefLen = 64

// DefaultCatalogueTTL is how long Pay trusts a cached product catalogue.
//
// Long on purpose. Verifying a pin has to know what the pinned SKU currently
// delivers, and doing that with a round trip would put a GetProducts call in
// front of every payment. SKUs and commission rates change rarely, and the cost
// of staleness is bounded and loud: a stale pin fails verification instead of
// paying the wrong amount.
const DefaultCatalogueTTL = 6 * time.Hour

// OperatorPin is a caller's record of which product tops up a given operator,
// and at what price.
//
// SendValue is per-operator and never gains a default. Sharing one SendValue
// across operators is the exact bug this API exists to prevent: it would
// deliver a different amount depending on which network the account is on,
// silently, reported as success.
type OperatorPin struct {
	SkuCode   string  `json:"SkuCode"`
	SendValue float64 `json:"SendValue"`
}

// PayRequest asks for a top-up expressed as delivered value.
//
// Amount, AmountCurrency and AccountNumber are required; so is DistributorRef,
// because an unreferenced transfer cannot be safely retried.
type PayRequest struct {
	// AccountNumber is the account to top up.
	AccountNumber string
	// DistributorRef is the idempotency key. On the discovery path each
	// candidate derives its own deterministic ref from this one.
	DistributorRef string

	// Amount is the MINIMUM DELIVERED value: a floor on Price.ReceiveValue,
	// not a cap on spend.
	Amount float64
	// AmountCurrency is required and is validated against the resolved
	// product's ReceiveCurrencyIso.
	//
	// It is never inferred from the account's country. DingConnect's receive
	// currency is not always the local one -- some products receive USD in a
	// non-USD country -- so inferring it would let a catalogue change deliver
	// the right number of the wrong unit and report success.
	AmountCurrency string
	// Tolerance is headroom on the DELIVERED amount. The acceptable window is
	// [Amount, Amount+Tolerance].
	Tolerance float64

	// Operators pins a product per operator code, matched case-insensitively.
	// Optional: with no pins, Pay resolves from the live catalogue.
	//
	// It is a map, not a slice, because it is looked up rather than iterated:
	// an account number belongs to exactly one operator, so there is never a
	// choice between several correct answers and order carries no meaning.
	Operators map[string]OperatorPin
	// Operator names the operator directly, skipping detection. Optional.
	Operator string

	// SendCurrencyIso and Settings are passed through to SendTransfer.
	SendCurrencyIso string
	Settings        []Setting
}

// ResolutionPath records how Pay chose the product it sent.
type ResolutionPath string

const (
	// PathPinned: the operator was known and the caller had pinned it.
	// One transfer.
	PathPinned ResolutionPath = "pinned"
	// PathCatalogue: the operator was known and no pin existed, so the
	// product came from the live catalogue. One transfer.
	PathCatalogue ResolutionPath = "catalogue"
	// PathDiscovery: the operator could not be determined, so the pinned
	// candidates were tried until one accepted the account. Up to N transfers.
	PathDiscovery ResolutionPath = "discovery"
)

// Attempt is one candidate transfer's outcome.
type Attempt struct {
	SkuCode        string  `json:"SkuCode"`
	SendValue      float64 `json:"SendValue"`
	DistributorRef string  `json:"DistributorRef"`
	// Codes are DingConnect's error codes for this attempt, empty on success.
	Codes     []string `json:"Codes,omitempty"`
	Completed bool     `json:"Completed"`
}

// Resolution is the record of how a payment was resolved, returned whether or
// not it succeeded so a caller can instrument it.
//
// This package deliberately does NOT own metrics: a library that reaches for a
// metrics registry forces its choice of one on every consumer. Pay reports the
// facts and the caller records them however it likes.
type Resolution struct {
	Path       ResolutionPath `json:"Path"`
	Operator   string         `json:"Operator,omitempty"`
	CountryIso string         `json:"CountryIso,omitempty"`
	SkuCode    string         `json:"SkuCode,omitempty"`
	SendValue  float64        `json:"SendValue,omitempty"`
	// Expected is what the catalogue said this product would deliver.
	Expected float64 `json:"Expected,omitempty"`
	// Delivered is what the completed transfer actually reported. A difference
	// from Expected means the catalogue and the realised price disagree, which
	// is worth surfacing even though the money has already moved.
	Delivered float64 `json:"Delivered,omitempty"`
	Currency  string  `json:"Currency,omitempty"`
	// Attempts is populated on the discovery path, one entry per candidate.
	Attempts []Attempt `json:"Attempts,omitempty"`
}

// PayResult is a completed payment.
//
// Like every other method here, Pay returns its value populated even alongside
// a non-nil error: Resolution carries the attempts that were made, which is the
// part worth recording when a payment fails.
type PayResult struct {
	// Transfer is the completed transfer record, nil unless the payment
	// succeeded.
	Transfer *TransferRecord
	// Response is the raw final SendTransfer response, success or failure.
	Response   SendTransferResponse
	Resolution Resolution
}

// ResolutionReason names a payment that failed before, or instead of, a
// transfer being accepted -- as opposed to *Error, which is DingConnect
// refusing a request.
type ResolutionReason string

const (
	// ReasonInvalidRequest: the PayRequest itself is not coherent.
	ReasonInvalidRequest ResolutionReason = "InvalidRequest"
	// ReasonPinSkuMissing: a pinned SkuCode is no longer in the catalogue.
	ReasonPinSkuMissing ResolutionReason = "PinSkuMissing"
	// ReasonPinOutOfWindow: a pinned product no longer delivers inside the
	// declared window at its pinned SendValue. A commission rate moved.
	ReasonPinOutOfWindow ResolutionReason = "PinOutOfWindow"
	// ReasonCurrencyMismatch: the resolved product's ReceiveCurrencyIso is not
	// the caller's AmountCurrency.
	ReasonCurrencyMismatch ResolutionReason = "CurrencyMismatch"
	// ReasonNoPinForOperator: an operator was determined, pins were supplied,
	// and none of them covers it.
	ReasonNoPinForOperator ResolutionReason = "NoPinForOperator"
	// ReasonOperatorNotDetermined: the operator could not be determined and no
	// pins were supplied to try.
	ReasonOperatorNotDetermined ResolutionReason = "OperatorNotDetermined"
	// ReasonImpossibleAmount: nothing available delivers inside the window.
	ReasonImpossibleAmount ResolutionReason = "ImpossibleAmount"
)

// ResolutionError is returned when a payment cannot be resolved to a product
// that satisfies the caller's declared intent.
//
// It is distinct from *Error on purpose. *Error means DingConnect refused
// something; ResolutionError means we refused to send, because sending would
// have delivered an amount the caller did not ask for. No money has moved when
// this is returned.
type ResolutionError struct {
	Reason  ResolutionReason
	Message string
	// Resolution is what was known at the point of failure.
	Resolution Resolution
}

func (e *ResolutionError) Error() string {
	return fmt.Sprintf("dingconnect: %s: %s", e.Reason, e.Message)
}

// IsResolutionError reports whether err is a *ResolutionError, and returns it.
func IsResolutionError(err error) (*ResolutionError, bool) {
	e, ok := err.(*ResolutionError)
	return e, ok
}

func resolutionErr(reason ResolutionReason, res Resolution, format string, a ...any) *ResolutionError {
	return &ResolutionError{Reason: reason, Resolution: res, Message: fmt.Sprintf(format, a...)}
}

// ---------------------------------------------------------------------------
// Pay
// ---------------------------------------------------------------------------

// Pay tops an account up by delivered amount.
//
// THE CONTEXT BOUNDS THE WHOLE RESOLUTION, not each call within it. Pay may
// make an account lookup, a catalogue fetch, and several transfers; giving it
// one deadline is what stops a discovery cascade from costing N times a single
// payment's wall clock. Callers running under a queue deadline should set it
// here and not rely on per-request timeouts.
//
// Resolution order:
//
//  1. Operator known (req.Operator, or GetAccountLookup) and pinned: verify the
//     pin against the declared window and send. One transfer.
//  2. Operator known, pins supplied, none for it: ReasonNoPinForOperator.
//  3. Operator known, no pins: resolve the cheapest in-window product from the
//     catalogue and send. One transfer.
//  4. Operator not determined, pins supplied: try each pinned candidate until
//     one accepts the account. This is discovery, not routing -- exactly one of
//     them is the account's operator, and sending is what settles it.
//  5. Operator not determined, no pins: ReasonOperatorNotDetermined.
//
// A completed transfer is returned with a nil error. Every other outcome
// returns a populated PayResult alongside either a *ResolutionError (we
// declined to send) or an *Error (DingConnect refused).
func (c *Client) Pay(ctx context.Context, req PayRequest) (PayResult, error) {
	var out PayResult

	pins, err := req.validate()
	if err != nil {
		return out, err
	}

	operator, country := req.Operator, ""
	if operator == "" {
		operator, country = c.detectOperator(ctx, req.AccountNumber)
	}
	res := Resolution{Operator: operator, CountryIso: country, Currency: req.AmountCurrency}

	candidates, err := c.resolveCandidates(ctx, req, pins, operator, &res)
	if err != nil {
		out.Resolution = res
		return out, err
	}

	return c.sendCandidates(ctx, req, candidates, res)
}

// validate checks a PayRequest and normalises its pin keys.
//
// Every rejection here happens before any network call, so an incoherent
// request is reported as such rather than as an opaque API refusal.
func (r PayRequest) validate() (map[string]OperatorPin, error) {
	bad := func(format string, a ...any) (map[string]OperatorPin, error) {
		return nil, resolutionErr(ReasonInvalidRequest, Resolution{}, format, a...)
	}

	switch {
	case r.AccountNumber == "":
		return bad("AccountNumber is required")
	case r.DistributorRef == "":
		// Not optional. An unreferenced transfer cannot be retried safely,
		// and this package refuses to invent one for the same reason the CLI
		// does: a generated ref turns a transient failure into a double
		// payment.
		return bad("DistributorRef is required; it is the idempotency key")
	case r.Amount <= 0:
		return bad("Amount must be positive")
	case r.AmountCurrency == "":
		return bad("AmountCurrency is required and is never inferred from the country")
	case r.Tolerance < 0:
		return bad("Tolerance must not be negative")
	case r.Operators != nil && len(r.Operators) == 0:
		return bad("Operators is present but empty")
	case len(r.Operators) > MaxCandidates:
		return bad("at most %d operators may be pinned, got %d", MaxCandidates, len(r.Operators))
	}

	pins := make(map[string]OperatorPin, len(r.Operators))
	for op, pin := range r.Operators {
		switch {
		case strings.TrimSpace(op) == "":
			return bad("Operators contains an entry with an empty operator code")
		case pin.SkuCode == "":
			return bad("operator %s is missing SkuCode", op)
		case pin.SendValue <= 0:
			// A pin with no SendValue is what "I expected one value to apply
			// to every operator" looks like in a config file.
			return bad("operator %s must have a positive SendValue", op)
		}
		pins[operatorKey(op)] = pin
	}

	if len(pins) > 0 {
		for _, pin := range pins {
			if _, err := candidateRef(r.DistributorRef, pin.SkuCode); err != nil {
				return nil, err
			}
		}
	}
	return pins, nil
}

// operatorKey normalises an operator code for lookup.
//
// Case-insensitive because operator codes are usually hand-written by whoever
// researched the pin, and the failure mode of exact matching is a declined
// payment for a config that is obviously right. Forgiveness costs no safety
// here: a key matching nothing still fails loudly as ReasonNoPinForOperator.
func operatorKey(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// resolveCandidates turns a validated request into the transfers to attempt.
func (c *Client) resolveCandidates(ctx context.Context, req PayRequest, pins map[string]OperatorPin, operator string, res *Resolution) ([]candidate, error) {
	if operator != "" {
		if len(pins) > 0 {
			pin, ok := pins[operatorKey(operator)]
			if !ok {
				return nil, resolutionErr(ReasonNoPinForOperator, *res,
					"operator %s was determined for this account, but only %v are pinned", operator, pinnedCodes(pins))
			}
			catalogue, err := c.catalogue(ctx, ProductFilter{SkuCodes: []string{pin.SkuCode}})
			if err != nil {
				return nil, err
			}
			cand, err := verifyPin(operator, pin, catalogue, req, *res)
			if err != nil {
				return nil, err
			}
			res.Path = PathPinned
			cand.applyTo(res)
			return []candidate{cand}, nil
		}

		catalogue, err := c.catalogue(ctx, ProductFilter{
			CountryISOs:   nonEmpty(res.CountryIso),
			ProviderCodes: []string{operator},
		})
		if err != nil {
			return nil, err
		}
		cand, err := resolveFromCatalogue(operator, catalogue, req, *res)
		if err != nil {
			return nil, err
		}
		res.Path = PathCatalogue
		cand.applyTo(res)
		return []candidate{cand}, nil
	}

	if len(pins) == 0 {
		return nil, resolutionErr(ReasonOperatorNotDetermined, *res,
			"could not determine the operator for %s and no operators are pinned to try", req.AccountNumber)
	}

	skus := make([]string, 0, len(pins))
	for _, pin := range pins {
		skus = append(skus, pin.SkuCode)
	}
	catalogue, err := c.catalogue(ctx, ProductFilter{SkuCodes: skus})
	if err != nil {
		return nil, err
	}

	// EVERY pin is verified before any of them is sent, and one stale pin fails
	// the whole payment even though it may not be this account's operator.
	//
	// The alternative -- skip the stale candidate and carry on -- pays this
	// account correctly while leaving a catalogue change undetected, which is
	// the silent-wrong-amount failure this whole API exists to remove. Failing
	// is the loud option, and a caller who would rather pay can catch
	// ReasonPinSkuMissing and retry without that pin.
	candidates := make([]candidate, 0, len(pins))
	for _, op := range pinnedCodes(pins) {
		cand, err := verifyPin(op, pins[op], catalogue, req, *res)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, cand)
	}
	res.Path = PathDiscovery
	return candidates, nil
}

// sendCandidates walks the candidates under the cascade policy.
func (c *Client) sendCandidates(ctx context.Context, req PayRequest, candidates []candidate, res Resolution) (PayResult, error) {
	discovery := len(candidates) > 1

	var lastResp SendTransferResponse
	var lastErr error

	for i, cand := range candidates {
		ref := req.DistributorRef
		if discovery {
			// Only the discovery path derives. A single send keeps the ref it
			// was given, so a caller retrying a payment it has already
			// submitted reuses the exact reference DingConnect saw.
			derived, err := candidateRef(req.DistributorRef, cand.SkuCode)
			if err != nil {
				return PayResult{Resolution: res}, err
			}
			ref = derived
		}

		resp, err := c.SendTransfer(ctx, SendTransferRequest{
			SkuCode:         cand.SkuCode,
			SendValue:       cand.SendValue,
			SendCurrencyIso: req.SendCurrencyIso,
			AccountNumber:   req.AccountNumber,
			DistributorRef:  ref,
			Settings:        req.Settings,
		})
		lastResp, lastErr = resp, err

		outcome := outcomeOf(resp, err)
		if discovery {
			res.Attempts = append(res.Attempts, Attempt{
				SkuCode:        cand.SkuCode,
				SendValue:      cand.SendValue,
				DistributorRef: ref,
				Codes:          outcome.codes,
				Completed:      outcome.completed,
			})
		}

		if outcome.completed {
			cand.applyTo(&res)
			res.Delivered = resp.TransferRecord.Price.ReceiveValue
			return PayResult{Transfer: resp.TransferRecord, Response: resp, Resolution: res}, nil
		}

		if decideCascade(outcome, i < len(candidates)-1) == actionReturn {
			return PayResult{Response: resp, Resolution: res}, err
		}
	}

	// Exhausted. Return the LAST REAL FAILURE rather than a synthesised one:
	// the final RechargeNotAllowed is the honest answer, namely that no pinned
	// product accepted this account.
	return PayResult{Response: lastResp, Resolution: res}, lastErr
}

// candidate is one product+price resolved and ready to send.
type candidate struct {
	Operator  string
	SkuCode   string
	SendValue float64
	Delivered float64
	Currency  string
}

func (c candidate) applyTo(res *Resolution) {
	res.SkuCode = c.SkuCode
	res.SendValue = c.SendValue
	res.Expected = c.Delivered
	if c.Currency != "" {
		res.Currency = c.Currency
	}
	if c.Operator != "" {
		res.Operator = c.Operator
	}
}

func pinnedCodes(pins map[string]OperatorPin) []string {
	ops := make([]string, 0, len(pins))
	for op := range pins {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return ops
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// ---------------------------------------------------------------------------
// The cascade policy
// ---------------------------------------------------------------------------

// outcome reduces one transfer to the three facts the cascade policy needs.
type outcome struct {
	completed bool
	codes     []string
	// fault: no verdict at all -- a transport failure or an undecodable body.
	fault bool
}

type cascadeAction string

const (
	actionReturn  cascadeAction = "return"
	actionAdvance cascadeAction = "advance"
)

func outcomeOf(resp SendTransferResponse, err error) outcome {
	if err == nil {
		// ResultCode 1 with a non-Completed record should not happen for the
		// instant transfers this client sends, but treating it as success
		// would credit a payment that never landed.
		if resp.TransferRecord != nil && resp.TransferRecord.Completed() {
			return outcome{completed: true}
		}
		return outcome{}
	}

	e := asError(err)
	if e == nil {
		return outcome{fault: true}
	}
	if len(e.Codes) == 0 {
		if e.ResultCode == 0 {
			return outcome{fault: true}
		}
		return outcome{}
	}
	codes := make([]string, 0, len(e.Codes))
	for _, c := range e.Codes {
		codes = append(codes, c.Code)
	}
	return outcome{codes: codes}
}

// decideCascade is the entire policy governing whether to try another product.
//
// ADVANCE IS AN ALLOW-LIST AND STOP IS THE BASE CASE. The final return is not a
// fallthrough that happens to be safe -- it is the default, which is what makes
// an error code introduced by DingConnect tomorrow unable to cause a send
// nobody designed. Adding a code to errors.go can only ever make the cascade
// stop, never make it spend money.
//
// A stop code ANYWHERE in the array wins. Never read Codes[0]: a response can
// carry RateLimited alongside RechargeNotAllowed, and reading only the first
// would advance past a rate limit.
func decideCascade(o outcome, hasNext bool) cascadeAction {
	if o.completed {
		return actionReturn
	}

	// A fault means we do not know whether money moved -- the request may have
	// been executed and the response lost. Advancing to another product risks
	// paying twice. Retrying the SAME candidate is safe (the ref is the
	// idempotency key); moving to a different one is not.
	if o.fault {
		return actionReturn
	}

	for _, c := range o.codes {
		switch c {
		case CodeRateLimited:
			// NEVER advanced past. DingConnect returns RateLimited both for
			// genuine transport throttling and for a per-account rule being
			// breached, and the response does not distinguish them. Sending
			// again on a different product is the one action that is
			// unrecoverable if it is the latter.
			//
			// Note this is deliberately stricter than Error.Retryable, which
			// answers a different question -- "could an identical request
			// succeed later" -- and is right about it. Retrying the same
			// request after a wait is fine; advancing to a new product now is
			// not.
			return actionReturn
		case CodeAccountNumberInvalid:
			// The account number itself is bad. No other product can help.
			return actionReturn
		}
	}

	for _, c := range o.codes {
		if c == CodeRechargeNotAllowed && hasNext {
			return actionAdvance
		}
	}

	return actionReturn
}

// candidateRef derives a per-candidate idempotency key.
//
// Deterministic, because that is the property everything else rests on: a retry
// of the same candidate must produce the same ref or DingConnect's duplicate
// protection is defeated and the account is paid twice.
//
// Whether a REJECTED transfer consumes its ref is UNVERIFIED against the live
// API. Deriving is correct under either answer: if refs are not consumed the
// derived ones are merely more unique than they needed to be, whereas a shared
// ref would make every discovery cascade fail at its second candidate with
// DuplicateTransactionPrevented if they are.
func candidateRef(base, sku string) (string, error) {
	ref := base + "_" + sku
	if len(ref) > MaxDistributorRefLen {
		return "", resolutionErr(ReasonInvalidRequest, Resolution{},
			"derived DistributorRef %q is %d characters, over the %d limit; shorten DistributorRef",
			ref, len(ref), MaxDistributorRefLen)
	}
	return ref, nil
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// epsilon absorbs binary floating-point error in money arithmetic. Well below
// the smallest unit of any currency DingConnect quotes.
const epsilon = 1e-9

// window is the acceptable delivered range for a request.
func (r PayRequest) upper() float64 { return r.Amount + r.Tolerance }

func (r PayRequest) contains(delivered float64) bool {
	// Boundaries are inclusive, and tolerant of float noise: a send value
	// solved to deliver exactly Amount can land a fraction under it, and
	// rejecting the value we just solved for would be absurd.
	return delivered >= r.Amount-epsilon && delivered <= r.upper()+epsilon
}

// deliveredFor reports what a product delivers for a given send value.
//
// A fixed product has one price pair, read directly. A range product is
// INTERPOLATED LINEARLY between Minimum and Maximum.
//
// The interpolation is an inference, not a documented guarantee: Product
// exposes only its two bounds, and CommissionRate is a single per-product
// number, which implies price is linear in send value across the band. Fixed
// products interpolate nothing and are unaffected. EstimatePrices prices a
// prospective transfer exactly and is the way to confirm a range product before
// trusting a tight tolerance.
func deliveredFor(p Product, send float64) (float64, bool) {
	lo, hi := p.Minimum, p.Maximum

	if p.FixedValue() {
		if diff := send - lo.SendValue; diff > epsilon || diff < -epsilon {
			return 0, false
		}
		return lo.ReceiveValue, true
	}

	if send < lo.SendValue-epsilon || send > hi.SendValue+epsilon {
		return 0, false
	}
	span := hi.SendValue - lo.SendValue
	if span <= 0 {
		return 0, false
	}
	return lo.ReceiveValue + ((send-lo.SendValue)/span)*(hi.ReceiveValue-lo.ReceiveValue), true
}

// sendValueFor is deliveredFor's inverse: the send value that delivers `want`,
// clamped into the product's accepted range.
//
// Clamping upward can overshoot the caller's window, so callers MUST re-check
// the resulting delivered value rather than trusting the clamp.
func sendValueFor(p Product, want float64) (float64, bool) {
	lo, hi := p.Minimum, p.Maximum

	if p.FixedValue() {
		return lo.SendValue, true
	}

	span := hi.ReceiveValue - lo.ReceiveValue
	if span <= 0 {
		return 0, false
	}
	send := lo.SendValue + ((want-lo.ReceiveValue)/span)*(hi.SendValue-lo.SendValue)

	switch {
	case send < lo.SendValue:
		send = lo.SendValue
	case send > hi.SendValue:
		return 0, false // the product cannot reach the requested amount
	}
	return roundUpCents(send), true
}

// roundUpCents rounds a send value up to the cent. Up, not to nearest: rounding
// down could land the delivered value a hair under the floor the caller asked
// for, and the whole point of Amount is that it is a floor.
func roundUpCents(v float64) float64 {
	r := float64(int64(v*100)) / 100
	if r < v {
		r += 0.01
	}
	return r
}

// ---------------------------------------------------------------------------
// Verification and selection
// ---------------------------------------------------------------------------

// verifyPin checks a pinned product against the caller's declared intent.
//
// This is why an intent is carried alongside a pin: the pinned SendValue is a
// number nothing could check on its own.
//
// A pin that still satisfies the window but is NO LONGER THE CHEAPEST available
// product is honoured silently. That is deliberate. A pin overridden over
// pennies is not a pin, and payments would become nondeterministic for a saving
// nobody asked for.
func verifyPin(operator string, pin OperatorPin, catalogue []Product, req PayRequest, res Resolution) (candidate, error) {
	prod, ok := findProduct(catalogue, pin.SkuCode)
	if !ok {
		return candidate{}, resolutionErr(ReasonPinSkuMissing, res,
			"pinned SkuCode %s for operator %s is no longer in the catalogue", pin.SkuCode, operator)
	}

	if !strings.EqualFold(prod.Minimum.ReceiveCurrencyIso, req.AmountCurrency) {
		return candidate{}, resolutionErr(ReasonCurrencyMismatch, res,
			"pinned SkuCode %s delivers %s but AmountCurrency is %s; refusing to send %v of the wrong currency",
			pin.SkuCode, prod.Minimum.ReceiveCurrencyIso, req.AmountCurrency, req.Amount)
	}

	delivered, ok := deliveredFor(prod, pin.SendValue)
	if !ok {
		return candidate{}, resolutionErr(ReasonPinOutOfWindow, res,
			"pinned SendValue %v is outside SkuCode %s's accepted range %v-%v",
			pin.SendValue, pin.SkuCode, prod.Minimum.SendValue, prod.Maximum.SendValue)
	}

	if !req.contains(delivered) {
		return candidate{}, resolutionErr(ReasonPinOutOfWindow, res,
			"pinned SkuCode %s at SendValue %v now delivers %v %s, outside the declared window %v-%v %s; a commission rate has moved",
			pin.SkuCode, pin.SendValue, delivered, prod.Minimum.ReceiveCurrencyIso,
			req.Amount, req.upper(), req.AmountCurrency)
	}

	return candidate{
		Operator:  operator,
		SkuCode:   pin.SkuCode,
		SendValue: pin.SendValue,
		Delivered: delivered,
		Currency:  prod.Minimum.ReceiveCurrencyIso,
	}, nil
}

func findProduct(catalogue []Product, sku string) (Product, bool) {
	for _, p := range catalogue {
		if p.SkuCode == sku {
			return p, true
		}
	}
	return Product{}, false
}

// resolveFromCatalogue picks the cheapest product satisfying the intent.
//
// "Cheapest" is lowest SendValue -- least spent for a delivery that satisfies
// the floor. Selecting on delivered value instead would agree in every
// catalogue where price rises with delivery, but "spend the least" is the rule
// a caller can defend.
func resolveFromCatalogue(operator string, catalogue []Product, req PayRequest, res Resolution) (candidate, error) {
	var best candidate
	var found bool
	var otherCurrency string

	for _, prod := range catalogue {
		if operator != "" && !strings.EqualFold(prod.ProviderCode, operator) {
			continue
		}
		if !strings.EqualFold(prod.Minimum.ReceiveCurrencyIso, req.AmountCurrency) {
			if otherCurrency == "" {
				otherCurrency = prod.Minimum.ReceiveCurrencyIso
			}
			continue
		}

		send, ok := sendValueFor(prod, req.Amount)
		if !ok {
			continue
		}
		delivered, ok := deliveredFor(prod, send)
		if !ok || !req.contains(delivered) {
			// A fixed product missing the window, or a range product clamped
			// up past the ceiling.
			continue
		}
		if found && send >= best.SendValue {
			continue
		}
		best = candidate{
			Operator:  prod.ProviderCode,
			SkuCode:   prod.SkuCode,
			SendValue: send,
			Delivered: delivered,
			Currency:  prod.Minimum.ReceiveCurrencyIso,
		}
		found = true
	}

	if !found {
		return candidate{}, resolutionErr(ReasonImpossibleAmount, res,
			"no %s product delivers between %v and %v %s. %s",
			operator, req.Amount, req.upper(), req.AmountCurrency,
			availability(catalogue, operator, otherCurrency))
	}
	return best, nil
}

// availability describes what was actually on offer, so a failure names the
// window AND the alternatives. "Could not resolve" alone is not actionable by
// whoever has to fix the configuration.
func availability(catalogue []Product, operator, otherCurrency string) string {
	var parts []string
	for _, p := range catalogue {
		if operator != "" && !strings.EqualFold(p.ProviderCode, operator) {
			continue
		}
		if p.FixedValue() {
			parts = append(parts, fmt.Sprintf("%s delivers %v %s", p.SkuCode, p.Minimum.ReceiveValue, p.Minimum.ReceiveCurrencyIso))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s delivers %v-%v %s",
			p.SkuCode, p.Minimum.ReceiveValue, p.Maximum.ReceiveValue, p.Minimum.ReceiveCurrencyIso))
	}
	if len(parts) == 0 {
		if otherCurrency != "" {
			return fmt.Sprintf("The catalogue quotes %s, not the requested currency.", otherCurrency)
		}
		return "The catalogue offered nothing for that operator."
	}
	sort.Strings(parts)
	return "Available: " + strings.Join(parts, "; ") + "."
}

// ---------------------------------------------------------------------------
// Operator detection and the catalogue cache
// ---------------------------------------------------------------------------

// detectOperator resolves an account number to its operator.
//
// Returns an empty operator when detection is INCONCLUSIVE rather than failed,
// which is not an error: the caller falls back to discovery over the pins.
// Inconclusive covers a lookup that errored, returned nothing, returned a
// NearestMatch partial, or returned several operators.
//
// Several Items is a data quirk, not a decision to make. An account number
// belongs to one operator, so there is never a choice between several correct
// answers; declining to arbitrate and letting a transfer settle it is the only
// approach that cannot pick wrong.
func (c *Client) detectOperator(ctx context.Context, accountNumber string) (operator, country string) {
	lookup, err := c.AccountLookup(ctx, accountNumber)
	if err != nil {
		// Includes the NearestMatch partial, which surfaces as an error: the
		// number normalised but no operator resolved.
		return "", lookup.CountryIso
	}
	if len(lookup.Items) != 1 {
		return "", lookup.CountryIso
	}
	return lookup.Items[0].ProviderCode, lookup.CountryIso
}

// catalogueCache is a TTL cache of product-catalogue slices, held per Client.
//
// Per Client, and therefore per API key, which is the scope that matters:
// commission rates -- and so what a SKU delivers -- are a property of the
// distributor account. A cache shared across accounts would price one caller's
// payment with another's rates.
type catalogueCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]catalogueEntry
	now     func() time.Time // injectable so tests need not sleep
}

type catalogueEntry struct {
	products []Product
	expires  time.Time
}

// WithCatalogueTTL sets how long Pay trusts a cached product catalogue.
// Zero disables caching, which is useful in tests that assert fetch counts.
func WithCatalogueTTL(d time.Duration) Option {
	return func(c *Client) { c.catalogueTTL = d }
}

// catalogue returns a filtered slice of the product catalogue, fetching only on
// a miss.
func (c *Client) catalogue(ctx context.Context, f ProductFilter) ([]Product, error) {
	c.cacheOnce.Do(func() {
		c.cache = &catalogueCache{ttl: c.catalogueTTL, entries: map[string]catalogueEntry{}, now: time.Now}
	})

	key := cacheKey(f)

	if c.cache.ttl > 0 {
		c.cache.mu.Lock()
		entry, ok := c.cache.entries[key]
		fresh := ok && c.cache.now().Before(entry.expires)
		c.cache.mu.Unlock()
		if fresh {
			return entry.products, nil
		}
	}

	products, err := c.Products(ctx, f)
	if err != nil {
		return nil, err
	}

	if c.cache.ttl > 0 {
		c.cache.mu.Lock()
		c.cache.entries[key] = catalogueEntry{products: products, expires: c.cache.now().Add(c.cache.ttl)}
		c.cache.mu.Unlock()
	}
	return products, nil
}

func cacheKey(f ProductFilter) string {
	norm := func(in []string) string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	return norm(f.SkuCodes) + "|" + norm(f.CountryISOs) + "|" + norm(f.ProviderCodes) +
		"|" + norm(f.RegionCodes) + "|" + norm(f.Benefits) + "|" + f.AccountNumber
}
