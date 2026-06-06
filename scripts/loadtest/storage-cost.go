// storage-cost models KMail's object-storage unit economics and
// checks them against the "~$0.12 / user / month" projection in
// docs/PROPOSAL.md (the "Cost model" block, derived from Wasabi at
// ~$6.99 / TB-mo with a 10 GB mailbox stored twice — primary +
// retention copy).
//
// It is a deterministic *model*, not a measurement: there is no
// Wasabi/S3 bill to read in compose-local, so this computes the
// blended $/user/mo from a plan-tier mailbox-size distribution and
// the storage price, and reports how the result compares to the
// projection. Every input is a flag so a real run (with the actual
// negotiated $/TB-mo and the seeded fleet's measured mailbox sizes)
// is a one-command follow-up that overwrites the assumptions.
//
// Usage:
//
//	go run ./scripts/loadtest/storage-cost.go                  # defaults -> stdout
//	go run ./scripts/loadtest/storage-cost.go --md-out cost.md --json-out cost.json
//	go run ./scripts/loadtest/storage-cost.go \
//	  --tiers 'core:0.70:5,pro:0.25:25,privacy:0.05:50' \
//	  --price-per-tb-mo 6.99 --copies 2 --tb-bytes 1000000000000
//
// --tiers is a comma-separated list of `id:fraction:mailbox_gb`
// entries. Fractions are normalised, so they need not sum to 1.
//
// The process exits non-zero when the blended cost exceeds the
// target by more than --tolerance-pct (default off: --check=false),
// so it can gate a pricing-regression check without failing the
// default informational run.
//
//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type tier struct {
	ID        string  `json:"id"`
	Fraction  float64 `json:"fraction"`    // share of users (normalised)
	MailboxGB float64 `json:"mailbox_gb"`  // logical mailbox size
	StoredGB  float64 `json:"stored_gb"`   // after copies + compression
	CostPerMo float64 `json:"cost_per_mo"` // $/user/mo for this tier
}

type result struct {
	PricePerTBMo     float64 `json:"price_per_tb_mo"`
	Copies           float64 `json:"copies"`
	CompressionRatio float64 `json:"compression_ratio"`
	TBBytes          float64 `json:"tb_bytes"`
	Tiers            []tier  `json:"tiers"`
	BlendedMailboxGB float64 `json:"blended_mailbox_gb"`
	BlendedStoredGB  float64 `json:"blended_stored_gb"`
	BlendedCostPerMo float64 `json:"blended_cost_per_mo"`
	TargetPerMo      float64 `json:"target_per_mo"`
	DeltaPct         float64 `json:"delta_pct"`
}

func main() {
	var (
		tiersFlag   string
		pricePerTB  float64
		copies      float64
		compression float64
		tbBytes     float64
		target      float64
		tolerance   float64
		check       bool
		mdOut       string
		jsonOut     string
	)
	// Defaults align with docs/PROPOSAL.md: Wasabi ~$6.99/TB-mo,
	// stored twice (primary + retention). Tier mailbox sizes span
	// the 5–50 GB range the brief calls "realistic per plan tier",
	// mapped onto the real plan catalog (core/pro/privacy). They are
	// ASSUMPTIONS — override with measured sizes for a real run.
	flag.StringVar(&tiersFlag, "tiers", "core:0.70:5,pro:0.25:25,privacy:0.05:50",
		"comma-separated id:fraction:mailbox_gb entries")
	flag.Float64Var(&pricePerTB, "price-per-tb-mo", 6.99, "object-storage price in $/TB-mo")
	flag.Float64Var(&copies, "copies", 2, "stored copies per mailbox (primary + retention)")
	flag.Float64Var(&compression, "compression-ratio", 1.0,
		"stored = logical / ratio (1.0 = no compression/dedup credit)")
	flag.Float64Var(&tbBytes, "tb-bytes", 1e12, "bytes per TB for billing (Wasabi bills decimal 1e12)")
	flag.Float64Var(&target, "target-per-mo", 0.12, "projected $/user/mo to compare against")
	flag.Float64Var(&tolerance, "tolerance-pct", 0, "allowed overshoot of target before --check fails")
	flag.BoolVar(&check, "check", false, "exit non-zero when blended cost exceeds target+tolerance")
	flag.StringVar(&mdOut, "md-out", "", "write a Markdown report here (default stdout)")
	flag.StringVar(&jsonOut, "json-out", "", "write the JSON summary here")
	flag.Parse()

	if pricePerTB <= 0 || tbBytes <= 0 || compression <= 0 {
		fmt.Fprintln(os.Stderr, "storage-cost: price-per-tb-mo, tb-bytes and compression-ratio must be > 0")
		os.Exit(2)
	}

	tiers, err := parseTiers(tiersFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "storage-cost: %v\n", err)
		os.Exit(2)
	}

	// Normalise fractions so callers can pass raw user counts or
	// percentages interchangeably.
	var fracSum float64
	for _, t := range tiers {
		fracSum += t.Fraction
	}
	if fracSum <= 0 {
		fmt.Fprintln(os.Stderr, "storage-cost: tier fractions sum to zero")
		os.Exit(2)
	}

	// $/GB-mo from the headline $/TB-mo. gbPerTB is the number of GB in
	// one billing-TB (tbBytes/1e9 = 1000 for Wasabi's decimal TB), used
	// as the divisor so the per-GB price honours the price's own
	// decimal/binary convention: $6.99/TB ÷ 1000 GB/TB = $0.00699/GB.
	gbPerTB := tbBytes / 1e9
	pricePerGB := pricePerTB / gbPerTB

	res := result{
		PricePerTBMo:     pricePerTB,
		Copies:           copies,
		CompressionRatio: compression,
		TBBytes:          tbBytes,
		TargetPerMo:      target,
	}
	for i := range tiers {
		tiers[i].Fraction /= fracSum
		tiers[i].StoredGB = tiers[i].MailboxGB * copies / compression
		tiers[i].CostPerMo = tiers[i].StoredGB * pricePerGB
		res.BlendedMailboxGB += tiers[i].Fraction * tiers[i].MailboxGB
		res.BlendedStoredGB += tiers[i].Fraction * tiers[i].StoredGB
		res.BlendedCostPerMo += tiers[i].Fraction * tiers[i].CostPerMo
	}
	res.Tiers = tiers
	if target > 0 {
		res.DeltaPct = 100.0 * (res.BlendedCostPerMo - target) / target
	}

	report := renderMarkdown(res)
	if mdOut != "" {
		if err := os.WriteFile(mdOut, []byte(report), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "storage-cost: write md: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("storage-cost: wrote %s\n", mdOut)
	} else {
		fmt.Print(report)
	}
	if jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(jsonOut, b, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "storage-cost: write json: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("storage-cost: wrote %s\n", jsonOut)
	}

	if check && res.BlendedCostPerMo > target*(1+tolerance/100.0) {
		fmt.Fprintf(os.Stderr,
			"storage-cost: blended $%.4f/user/mo exceeds target $%.4f by %.1f%% (> %.1f%% tolerance)\n",
			res.BlendedCostPerMo, target, res.DeltaPct, tolerance)
		os.Exit(1)
	}
}

func parseTiers(s string) ([]tier, error) {
	var out []tier
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("tier %q: want id:fraction:mailbox_gb", raw)
		}
		frac, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return nil, fmt.Errorf("tier %q: bad fraction: %w", raw, err)
		}
		gb, err := strconv.ParseFloat(parts[2], 64)
		if err != nil {
			return nil, fmt.Errorf("tier %q: bad mailbox_gb: %w", raw, err)
		}
		if frac < 0 || gb < 0 {
			return nil, fmt.Errorf("tier %q: fraction and mailbox_gb must be >= 0", raw)
		}
		out = append(out, tier{ID: parts[0], Fraction: frac, MailboxGB: gb})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no tiers parsed")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MailboxGB < out[j].MailboxGB })
	return out, nil
}

func renderMarkdown(r result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Storage unit-economics model\n\n")
	fmt.Fprintf(&b, "> Deterministic model, **not** a measured cloud bill. ")
	fmt.Fprintf(&b, "Inputs are assumptions (see flags); override with the negotiated price and the seeded fleet's measured mailbox sizes for a real-infra number.\n\n")
	fmt.Fprintf(&b, "## Inputs\n\n")
	fmt.Fprintf(&b, "| Parameter | Value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| Storage price | $%.2f / TB-mo |\n", r.PricePerTBMo)
	fmt.Fprintf(&b, "| Copies stored | %.0f (primary + retention) |\n", r.Copies)
	fmt.Fprintf(&b, "| Compression/dedup credit | %.2f× |\n", r.CompressionRatio)
	fmt.Fprintf(&b, "| TB convention | %.0f bytes (%s) |\n", r.TBBytes, tbConvention(r.TBBytes))
	fmt.Fprintf(&b, "| Projection target | $%.2f / user / mo |\n\n", r.TargetPerMo)
	fmt.Fprintf(&b, "## Per-tier cost\n\n")
	fmt.Fprintf(&b, "| Tier | Users | Mailbox (GB) | Stored (GB) | $/user/mo |\n")
	fmt.Fprintf(&b, "| --- | --- | --- | --- | --- |\n")
	for _, t := range r.Tiers {
		fmt.Fprintf(&b, "| %s | %.0f%% | %.1f | %.1f | $%.4f |\n",
			t.ID, 100*t.Fraction, t.MailboxGB, t.StoredGB, t.CostPerMo)
	}
	fmt.Fprintf(&b, "\n## Blended\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| Blended logical mailbox | %.2f GB |\n", r.BlendedMailboxGB)
	fmt.Fprintf(&b, "| Blended stored | %.2f GB |\n", r.BlendedStoredGB)
	fmt.Fprintf(&b, "| **Blended cost** | **$%.4f / user / mo** |\n", r.BlendedCostPerMo)
	verdict := "within"
	if r.BlendedCostPerMo > r.TargetPerMo {
		verdict = "above"
	} else if r.BlendedCostPerMo < r.TargetPerMo {
		verdict = "below"
	}
	fmt.Fprintf(&b, "| vs target ($%.2f) | %+.1f%% (%s projection) |\n\n", r.TargetPerMo, r.DeltaPct, verdict)
	return b.String()
}

func tbConvention(b float64) string {
	if b == 1<<40 {
		return "binary TiB"
	}
	if b == 1e12 {
		return "decimal TB"
	}
	return "custom"
}
