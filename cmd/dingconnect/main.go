// Command dingconnect is a CLI for the DingConnect mobile top-up API.
//
// Every subcommand prints a human-readable table by default and raw JSON with
// --json, so it composes with jq.
//
// The API key is read from DINGCONNECT_API_KEY or from --api-key. Nothing is
// loaded from disk: like every other service in this org, configuration comes
// from the environment, and putting a file there is the caller's job --
// `denv .env dingconnect balance`. An implicit .env lookup would be
// cwd-dependent, so which key a transfer used would depend on where the
// command was run from.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	dingconnect "github.com/vlab-research/go-dingconnect"
)

const usage = `dingconnect - CLI for the DingConnect top-up API

Usage:
  dingconnect <command> [flags]

Catalogue:
  countries               List supported countries
  currencies              List supported currencies
  regions                 List billing regions
  providers               List carriers and merchants
  provider-status         Report which providers are processing transfers
  products                List sellable SKUs with their prices
  descriptions            Show human-readable copy for SKUs

Account:
  balance                 Show the distributor balance
  lookup                  Resolve a phone number to its providers and SKUs
  transfers               List transfer history

Money:
  estimate                Price prospective transfers without sending
  send                    Send a top-up (validates only unless --confirm)
  cancel                  Request cancellation of a transfer

Reference:
  error-codes             List DingConnect error codes and their meanings

Global flags:
  --api-key string        API key (default $DINGCONNECT_API_KEY)
  --json                  Emit raw JSON instead of a table
  --timeout duration      Request timeout (default 90s)

The API key comes from the environment. To load it from a file for a single
command, use denv:

  denv .env dingconnect balance

Run "dingconnect <command> --help" for the flags of a single command.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// globals holds the flags every subcommand shares.
type globals struct {
	apiKey  string
	asJSON  bool
	timeout time.Duration
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.apiKey, "api-key", "", "DingConnect API key (default $DINGCONNECT_API_KEY)")
	fs.BoolVar(&g.asJSON, "json", false, "emit raw JSON")
	fs.DurationVar(&g.timeout, "timeout", dingconnect.DefaultTimeout, "request timeout")
}

func (g *globals) client() (*dingconnect.Client, error) {
	key := g.apiKey
	if key == "" {
		key = os.Getenv("DINGCONNECT_API_KEY")
	}
	if key == "" {
		return nil, errors.New("no API key: export DINGCONNECT_API_KEY, pass --api-key, or run via `denv .env dingconnect ...`")
	}
	timeout := g.timeout
	if timeout <= 0 {
		timeout = dingconnect.DefaultTimeout
	}
	return dingconnect.New(key, dingconnect.WithHTTPClient(&http.Client{Timeout: timeout})), nil
}

type command struct {
	name string
	run  func(ctx context.Context, g *globals, args []string) error
}

func run() error {
	cmds := []command{
		{"balance", cmdBalance},
		{"countries", cmdCountries},
		{"currencies", cmdCurrencies},
		{"regions", cmdRegions},
		{"providers", cmdProviders},
		{"provider-status", cmdProviderStatus},
		{"products", cmdProducts},
		{"descriptions", cmdDescriptions},
		{"lookup", cmdLookup},
		{"estimate", cmdEstimate},
		{"send", cmdSend},
		{"transfers", cmdTransfers},
		{"cancel", cmdCancel},
		{"error-codes", cmdErrorCodes},
	}

	if len(os.Args) < 2 {
		fmt.Print(usage)
		return errors.New("no command given")
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		fmt.Print(usage)
		return nil
	}

	// Signal handling means a hung request is interruptible with Ctrl-C
	// rather than needing the timeout to expire.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, c := range cmds {
		if c.name == name {
			g := &globals{}
			return c.run(ctx, g, os.Args[2:])
		}
	}

	fmt.Print(usage)
	return fmt.Errorf("unknown command %q", name)
}

// parse wires up a subcommand's FlagSet with the global flags and parses argv.
func parse(g *globals, name string, args []string, extra func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	g.register(fs)
	if extra != nil {
		extra(fs)
	}
	err := fs.Parse(args)
	return fs, err
}

// repeated collects a flag that may be given more than once, and also accepts
// a single comma-separated value, so both --country NG --country KE and
// --country NG,KE work.
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ",") }

func (r *repeated) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*r = append(*r, p)
		}
	}
	return nil
}

// emit writes v as JSON. Used for --json on every command.
func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table renders rows under the given headers, aligned.
func table(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

// report prints a non-fatal note about a partial result. DingConnect's
// NearestMatch is common on lookups and does not mean the call was useless.
func report(err error) error {
	if err == nil {
		return nil
	}
	if dingconnect.IsNearestMatch(err) {
		fmt.Fprintf(os.Stderr, "note: %v\n", err)
		return nil
	}
	return err
}

func cmdBalance(ctx context.Context, g *globals, args []string) error {
	if _, err := parse(g, "balance", args, nil); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	b, err := c.Balance(ctx)
	if err != nil {
		return err
	}
	if g.asJSON {
		return emit(b)
	}
	fmt.Printf("%.2f %s\n", b.Balance, b.CurrencyIso)
	return nil
}

func cmdCountries(ctx context.Context, g *globals, args []string) error {
	var iso repeated
	fs, err := parse(g, "countries", args, func(fs *flag.FlagSet) {
		fs.Var(&iso, "country", "filter by country ISO (repeatable)")
	})
	if err != nil {
		return err
	}
	_ = fs
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.Countries(ctx)
	if err != nil {
		return err
	}
	// GetCountries takes no filter, so narrow client-side.
	if len(iso) > 0 {
		want := set(iso)
		var keep []dingconnect.Country
		for _, x := range items {
			if want[strings.ToUpper(x.CountryIso)] {
				keep = append(keep, x)
			}
		}
		items = keep
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		prefixes := make([]string, 0, len(x.InternationalDialingInformation))
		for _, d := range x.InternationalDialingInformation {
			prefixes = append(prefixes, "+"+d.Prefix)
		}
		rows = append(rows, []string{x.CountryIso, x.CountryName, strings.Join(prefixes, " ")})
	}
	table([]string{"ISO", "NAME", "PREFIXES"}, rows)
	fmt.Printf("\n%d countries\n", len(items))
	return nil
}

func cmdCurrencies(ctx context.Context, g *globals, args []string) error {
	if _, err := parse(g, "currencies", args, nil); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.Currencies(ctx)
	if err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		rows = append(rows, []string{x.CurrencyIso, x.CurrencyName})
	}
	table([]string{"ISO", "NAME"}, rows)
	fmt.Printf("\n%d currencies\n", len(items))
	return nil
}

func cmdRegions(ctx context.Context, g *globals, args []string) error {
	var iso repeated
	if _, err := parse(g, "regions", args, func(fs *flag.FlagSet) {
		fs.Var(&iso, "country", "country ISO (repeatable)")
	}); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.Regions(ctx, iso...)
	if err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		rows = append(rows, []string{x.RegionCode, x.RegionName, x.CountryIso})
	}
	table([]string{"CODE", "NAME", "COUNTRY"}, rows)
	fmt.Printf("\n%d regions\n", len(items))
	return nil
}

func cmdProviders(ctx context.Context, g *globals, args []string) error {
	var country, provider, region repeated
	var account string
	if _, err := parse(g, "providers", args, func(fs *flag.FlagSet) {
		fs.Var(&country, "country", "country ISO (repeatable)")
		fs.Var(&provider, "provider", "provider code (repeatable)")
		fs.Var(&region, "region", "region code (repeatable)")
		fs.StringVar(&account, "account", "", "account number the provider must serve")
	}); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.Providers(ctx, dingconnect.ProviderFilter{
		CountryISOs:   country,
		ProviderCodes: provider,
		RegionCodes:   region,
		AccountNumber: account,
	})
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		rows = append(rows, []string{x.ProviderCode, x.CountryIso, x.Name, strings.Join(x.PaymentTypes, ",")})
	}
	table([]string{"CODE", "COUNTRY", "NAME", "PAYMENT"}, rows)
	fmt.Printf("\n%d providers\n", len(items))
	return nil
}

func cmdProviderStatus(ctx context.Context, g *globals, args []string) error {
	var provider repeated
	if _, err := parse(g, "provider-status", args, func(fs *flag.FlagSet) {
		fs.Var(&provider, "provider", "provider code (repeatable)")
	}); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.ProviderStatuses(ctx, provider...)
	if err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		state := "up"
		if !x.IsProcessingTransfers {
			state = "DOWN"
		}
		rows = append(rows, []string{x.ProviderCode, state, x.Message})
	}
	table([]string{"CODE", "STATUS", "MESSAGE"}, rows)
	return nil
}

func cmdProducts(ctx context.Context, g *globals, args []string) error {
	var country, provider, sku, region, benefit repeated
	var account string
	if _, err := parse(g, "products", args, func(fs *flag.FlagSet) {
		fs.Var(&country, "country", "country ISO (repeatable)")
		fs.Var(&provider, "provider", "provider code (repeatable)")
		fs.Var(&sku, "sku", "SKU code (repeatable)")
		fs.Var(&region, "region", "region code (repeatable)")
		fs.Var(&benefit, "benefit", "benefit type (repeatable)")
		fs.StringVar(&account, "account", "", "account number the product must serve")
	}); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.Products(ctx, dingconnect.ProductFilter{
		CountryISOs:   country,
		ProviderCodes: provider,
		SkuCodes:      sku,
		RegionCodes:   region,
		Benefits:      benefit,
		AccountNumber: account,
	})
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ProviderCode != items[j].ProviderCode {
			return items[i].ProviderCode < items[j].ProviderCode
		}
		return items[i].Minimum.SendValue < items[j].Minimum.SendValue
	})
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		send := fmt.Sprintf("%.2f %s", x.Minimum.SendValue, x.Minimum.SendCurrencyIso)
		if !x.FixedValue() {
			send = fmt.Sprintf("%.2f-%.2f %s", x.Minimum.SendValue, x.Maximum.SendValue, x.Minimum.SendCurrencyIso)
		}
		recv := fmt.Sprintf("%.2f %s", x.Minimum.ReceiveValue, x.Minimum.ReceiveCurrencyIso)
		if !x.FixedValue() {
			recv = fmt.Sprintf("%.2f-%.2f %s", x.Minimum.ReceiveValue, x.Maximum.ReceiveValue, x.Minimum.ReceiveCurrencyIso)
		}
		// Flag SKUs needing extra settings -- sending without them fails.
		var needs []string
		for _, s := range x.SettingDefinitions {
			if s.IsMandatory {
				needs = append(needs, s.Name)
			}
		}
		rows = append(rows, []string{
			x.SkuCode, x.ProviderCode, send, recv,
			fmt.Sprintf("%.1f%%", x.CommissionRate*100),
			x.ProcessingMode, strings.Join(needs, ","),
		})
	}
	table([]string{"SKU", "PROVIDER", "SEND", "RECEIVE", "COMM", "MODE", "REQUIRES"}, rows)
	fmt.Printf("\n%d products\n", len(items))
	return nil
}

func cmdDescriptions(ctx context.Context, g *globals, args []string) error {
	var sku, lang repeated
	if _, err := parse(g, "descriptions", args, func(fs *flag.FlagSet) {
		fs.Var(&sku, "sku", "SKU code (repeatable)")
		fs.Var(&lang, "lang", "language code (repeatable, default en)")
	}); err != nil {
		return err
	}
	if len(lang) == 0 {
		lang = repeated{"en"}
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.ProductDescriptions(ctx, lang, sku)
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	for _, x := range items {
		fmt.Printf("%s [%s]\n  %s\n", x.LocalizationKey, x.LanguageCode, x.DisplayText)
		if x.DescriptionMarkdown != "" {
			fmt.Printf("  %s\n", x.DescriptionMarkdown)
		}
		fmt.Println()
	}
	return nil
}

func cmdLookup(ctx context.Context, g *globals, args []string) error {
	var account string
	fs, err := parse(g, "lookup", args, func(fs *flag.FlagSet) {
		fs.StringVar(&account, "account", "", "account number to resolve")
	})
	if err != nil {
		return err
	}
	// Accept the number as a bare argument too: `dingconnect lookup +2348012345678`.
	if account == "" && fs.NArg() > 0 {
		account = fs.Arg(0)
	}
	if account == "" {
		return errors.New("account number required: dingconnect lookup <number>")
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	res, err := c.AccountLookup(ctx, account)
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(res)
	}
	fmt.Printf("Country:    %s\n", res.CountryIso)
	fmt.Printf("Normalized: %s\n", res.AccountNumberNormalized)
	if len(res.Items) == 0 {
		fmt.Println("\nNo exact provider match.")
		return nil
	}
	fmt.Println()
	rows := make([][]string, 0, len(res.Items))
	for _, x := range res.Items {
		rows = append(rows, []string{x.ProviderCode, fmt.Sprint(len(x.SkuCodes)), strings.Join(x.SkuCodes, " ")})
	}
	table([]string{"PROVIDER", "SKUS", "CODES"}, rows)
	return nil
}

func cmdEstimate(ctx context.Context, g *globals, args []string) error {
	var sku, currency, account string
	var value float64
	if _, err := parse(g, "estimate", args, func(fs *flag.FlagSet) {
		fs.StringVar(&sku, "sku", "", "SKU code (required)")
		fs.Float64Var(&value, "value", 0, "send value (required)")
		fs.StringVar(&currency, "currency", "", "send currency ISO")
		fs.StringVar(&account, "account", "", "destination account number")
	}); err != nil {
		return err
	}
	if sku == "" || value <= 0 {
		return errors.New("--sku and a positive --value are required")
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.EstimatePrices(ctx, []dingconnect.EstimateRequest{{
		BatchItemRef:    "cli-1",
		SkuCode:         sku,
		SendValue:       value,
		SendCurrencyIso: currency,
		AccountNumber:   account,
	}})
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		if x.ResultCode != dingconnect.ResultCodeSuccess {
			rows = append(rows, []string{x.SkuCode, "-", "-", "-", codes(x.ErrorCodes)})
			continue
		}
		rows = append(rows, []string{
			x.SkuCode,
			fmt.Sprintf("%.2f %s", x.Price.SendValue, x.Price.SendCurrencyIso),
			fmt.Sprintf("%.2f %s", x.Price.ReceiveValue, x.Price.ReceiveCurrencyIso),
			fmt.Sprintf("%.2f", x.Price.DistributorFee),
			"",
		})
	}
	table([]string{"SKU", "SEND", "RECEIVE", "FEE", "ERROR"}, rows)
	return nil
}

func cmdSend(ctx context.Context, g *globals, args []string) error {
	var sku, account, ref, currency string
	var value float64
	var confirm bool
	var settings repeated
	if _, err := parse(g, "send", args, func(fs *flag.FlagSet) {
		fs.StringVar(&sku, "sku", "", "SKU code (required)")
		fs.Float64Var(&value, "value", 0, "send value (required)")
		fs.StringVar(&account, "account", "", "destination account number (required)")
		fs.StringVar(&ref, "ref", "", "distributor reference, the idempotency key (required)")
		fs.StringVar(&currency, "currency", "", "send currency ISO")
		fs.Var(&settings, "setting", "product setting as name=value (repeatable)")
		fs.BoolVar(&confirm, "confirm", false, "actually send; without this the transfer is only validated")
	}); err != nil {
		return err
	}
	switch {
	case sku == "":
		return errors.New("--sku is required")
	case account == "":
		return errors.New("--account is required")
	case value <= 0:
		return errors.New("--value must be positive")
	case ref == "":
		// Refusing to invent a reference is deliberate: the ref is the
		// idempotency key, and a generated one would make a retry send twice.
		return errors.New("--ref is required (it is the idempotency key; reuse the same value when retrying)")
	}

	parsed := make([]dingconnect.Setting, 0, len(settings))
	for _, s := range settings {
		name, val, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("bad --setting %q: want name=value", s)
		}
		parsed = append(parsed, dingconnect.Setting{Name: name, Value: val})
	}

	c, err := g.client()
	if err != nil {
		return err
	}

	req := dingconnect.SendTransferRequest{
		SkuCode:         sku,
		SendValue:       value,
		SendCurrencyIso: currency,
		AccountNumber:   account,
		DistributorRef:  ref,
		ValidateOnly:    !confirm,
		Settings:        parsed,
	}

	if !confirm {
		fmt.Fprintln(os.Stderr, "validate-only: no money will move. Re-run with --confirm to send.")
	}

	res, err := c.SendTransfer(ctx, req)

	if g.asJSON {
		// Emit before returning the error so a failed transfer is still
		// machine-readable.
		if emitErr := emit(res); emitErr != nil {
			return emitErr
		}
		return err
	}

	if res.TransferRecord != nil {
		r := res.TransferRecord
		fmt.Printf("State:      %s\n", r.ProcessingState)
		fmt.Printf("SKU:        %s\n", r.SkuCode)
		fmt.Printf("Account:    %s\n", r.AccountNumber)
		if r.TransferId.TransferRef != "" {
			fmt.Printf("TransferRef:    %s\n", r.TransferId.TransferRef)
			fmt.Printf("DistributorRef: %s\n", r.TransferId.DistributorRef)
		}
		if r.Price.SendValue != 0 {
			fmt.Printf("Send:       %.2f %s\n", r.Price.SendValue, r.Price.SendCurrencyIso)
			fmt.Printf("Receive:    %.2f %s\n", r.Price.ReceiveValue, r.Price.ReceiveCurrencyIso)
		}
		if r.ReceiptText != "" {
			fmt.Printf("Receipt:    %s\n", r.ReceiptText)
		}
	}
	if err != nil {
		if dingconnect.IsRetryable(err) {
			fmt.Fprintf(os.Stderr, "\nthis failure is retryable -- retry with the SAME --ref %q\n", ref)
		}
		return err
	}
	if !confirm {
		fmt.Println("\nValidation passed. Re-run with --confirm to send for real.")
	}
	return nil
}

func cmdTransfers(ctx context.Context, g *globals, args []string) error {
	var skip, take int
	var account string
	var refs, skus repeated
	if _, err := parse(g, "transfers", args, func(fs *flag.FlagSet) {
		fs.IntVar(&take, "take", 25, "page size")
		fs.IntVar(&skip, "skip", 0, "records to skip")
		fs.StringVar(&account, "account", "", "filter by account number")
		fs.Var(&refs, "ref", "filter by distributor reference (repeatable)")
		fs.Var(&skus, "sku", "filter by SKU code (repeatable)")
	}); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	res, err := c.TransferRecords(ctx, dingconnect.TransferFilter{
		Skip:            skip,
		Take:            take,
		AccountNumber:   account,
		DistributorRefs: refs,
		SkuCodes:        skus,
	})
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(res)
	}
	rows := make([][]string, 0, len(res.Items))
	for _, x := range res.Items {
		rows = append(rows, []string{
			x.TransferId.TransferRef, x.TransferId.DistributorRef, x.SkuCode,
			x.AccountNumber, x.ProcessingState,
			fmt.Sprintf("%.2f %s", x.Price.SendValue, x.Price.SendCurrencyIso),
			x.StartedUtc,
		})
	}
	table([]string{"TRANSFER", "REF", "SKU", "ACCOUNT", "STATE", "SEND", "STARTED"}, rows)
	fmt.Printf("\n%d records", len(res.Items))
	if res.ThereAreMoreItems {
		fmt.Printf(" (more available: --skip %d)", skip+take)
	}
	fmt.Println()
	return nil
}

func cmdCancel(ctx context.Context, g *globals, args []string) error {
	var ref, transferRef string
	if _, err := parse(g, "cancel", args, func(fs *flag.FlagSet) {
		fs.StringVar(&ref, "ref", "", "distributor reference of the transfer to cancel")
		fs.StringVar(&transferRef, "transfer-ref", "", "DingConnect transfer reference")
	}); err != nil {
		return err
	}
	if ref == "" && transferRef == "" {
		return errors.New("one of --ref or --transfer-ref is required")
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.CancelTransfers(ctx, []dingconnect.CancelRequest{{
		BatchItemRef:   "cli-1",
		DistributorRef: ref,
		TransferRef:    transferRef,
	}})
	if err := report(err); err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		state := "cancelled"
		if x.ResultCode != dingconnect.ResultCodeSuccess {
			state = codes(x.ErrorCodes)
		}
		rows = append(rows, []string{x.TransferId.TransferRef, x.TransferId.DistributorRef, state})
	}
	table([]string{"TRANSFER", "REF", "RESULT"}, rows)
	return nil
}

func cmdErrorCodes(ctx context.Context, g *globals, args []string) error {
	if _, err := parse(g, "error-codes", args, nil); err != nil {
		return err
	}
	c, err := g.client()
	if err != nil {
		return err
	}
	items, err := c.ErrorCodeDescriptions(ctx)
	if err != nil {
		return err
	}
	if g.asJSON {
		return emit(items)
	}
	rows := make([][]string, 0, len(items))
	for _, x := range items {
		rows = append(rows, []string{x.Code, x.Message})
	}
	table([]string{"CODE", "MEANING"}, rows)
	return nil
}

func codes(cs []dingconnect.ErrorCode) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.String()
	}
	return strings.Join(out, ", ")
}

func set(vs []string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[strings.ToUpper(v)] = true
	}
	return m
}
