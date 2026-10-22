package application

import (
	"errors"
	"testing"

	"github.com/dujiao-next/internal/constants"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/shopspring/decimal"
)

func TestCalculateMarkedUpPrice(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		markup   string
		rounding string
		expected string
	}{
		{"zero markup returns original", "10.00", "0", "none", "10.00"},
		{"50% markup", "10.00", "50", "none", "15.00"},
		{"100% markup doubles price", "10.00", "100", "none", "20.00"},
		{"200% markup triples price", "10.00", "200", "none", "30.00"},
		{"400% markup makes cost five times", "10.00", "400", "ceil_cent", "50.00"},
		{"small price with markup", "0.01", "100", "none", "0.02"},
		{"ceil_cent rounds USDT price upward", "0.011", "100", "ceil_cent", "0.03"},
		{"fractional result rounds to 2 decimals", "10.00", "33", "none", "13.30"},
		{"negative markup floors at zero", "5.00", "-200", "none", "0"},
		{"ceil_int rounds up", "10.00", "23.4", "ceil_int", "13.00"},
		{"ceil_int exact integer stays", "10.00", "100", "ceil_int", "20"},
		{"ceil_tenth rounds up to 0.1", "10.00", "23.4", "ceil_tenth", "12.40"},
		{"ceil_tenth exact tenth stays", "10.00", "50", "ceil_tenth", "15.00"},
		{"zero price stays zero", "0.00", "100", "none", "0.00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := decimal.RequireFromString(tt.upstream)
			markup := decimal.RequireFromString(tt.markup)
			expected := decimal.RequireFromString(tt.expected)

			result := CalculateMarkedUpPrice(upstream, markup, tt.rounding)
			if !result.Equal(expected) {
				t.Errorf("CalculateMarkedUpPrice(%s, %s%%, %s) = %s, want %s",
					tt.upstream, tt.markup, tt.rounding, result.String(), tt.expected)
			}
		})
	}
}

func TestCalculateLocalPrice(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		rate     string
		markup   string
		rounding string
		expected string
	}{
		{"USD to CNY with markup", "1.00", "7.2", "50", "none", "10.80"},
		{"rate 1 same as no conversion", "10.00", "1", "50", "none", "15.00"},
		{"rate 0 treated as 1", "10.00", "0", "50", "none", "15.00"},
		{"negative rate treated as 1", "10.00", "-1", "50", "none", "15.00"},
		{"rate with no markup", "1.00", "7.2", "0", "none", "7.20"},
		{"rate with ceil_int", "1.00", "7.2", "50", "ceil_int", "11.00"},
		{"rate with ceil_tenth", "1.00", "7.35", "0", "ceil_tenth", "7.35"},
		{"rate with ceil_tenth fractional", "1.00", "7.34", "1", "ceil_tenth", "7.50"},
		{"high precision rate", "100.00", "0.138462", "0", "none", "13.85"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := decimal.RequireFromString(tt.upstream)
			rate := decimal.RequireFromString(tt.rate)
			markup := decimal.RequireFromString(tt.markup)
			expected := decimal.RequireFromString(tt.expected)

			result := CalculateLocalPrice(upstream, rate, markup, tt.rounding)
			if !result.Equal(expected) {
				t.Errorf("CalculateLocalPrice(%s, rate=%s, %s%%, %s) = %s, want %s",
					tt.upstream, tt.rate, tt.markup, tt.rounding, result.String(), tt.expected)
			}
		})
	}
}

func smmTestPriceConfig() SMMPriceConfig {
	return SMMPriceConfig{
		SourceCurrency:         "USD",
		SourceToUSDT:           decimal.RequireFromString("1"),
		MarkupMultiplier:       decimal.RequireFromString("2"),
		MinimumOrderAmountUSDT: decimal.RequireFromString("1"),
		PricingUnit:            constants.PricingUnitPer1000,
	}
}

func TestCalculateSMMPriceUsesFixedPointAndCeilsToCent(t *testing.T) {
	tests := []struct {
		name          string
		rate          string
		quantity      int
		pricingUnit   string
		expectedRaw   string
		expectedCost  string
		expectedPrice string
		belowMinimum  bool
	}{
		{
			name:          "small rate 0.398 per 1000",
			rate:          "0.398",
			quantity:      100,
			pricingUnit:   constants.PricingUnitPer1000,
			expectedRaw:   "0.0398",
			expectedCost:  "0.0398",
			expectedPrice: "0.08",
			belowMinimum:  true,
		},
		{
			name:          "exact cent does not round past the cent",
			rate:          "5",
			quantity:      1000,
			pricingUnit:   constants.PricingUnitPer1000,
			expectedRaw:   "5",
			expectedCost:  "5",
			expectedPrice: "10",
		},
		{
			name:          "per unit",
			rate:          "0.398",
			quantity:      3,
			pricingUnit:   constants.PricingUnitPerUnit,
			expectedRaw:   "1.194",
			expectedCost:  "1.194",
			expectedPrice: "2.39",
		},
		{
			name:          "per order ignores quantity for cost",
			rate:          "0.398",
			quantity:      200000,
			pricingUnit:   constants.PricingUnitPerOrder,
			expectedRaw:   "0.398",
			expectedCost:  "0.398",
			expectedPrice: "0.80",
			belowMinimum:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := smmTestPriceConfig()
			cfg.PricingUnit = tt.pricingUnit
			result, err := CalculateSMMPrice(tt.rate, tt.quantity, cfg)
			if err != nil {
				t.Fatalf("CalculateSMMPrice() error = %v", err)
			}
			for name, got := range map[string]decimal.Decimal{
				"raw cost":      result.RawCost,
				"converted cost": result.ConvertedCost,
				"sell price":    result.SellPrice,
			} {
				var expected string
				switch name {
				case "raw cost":
					expected = tt.expectedRaw
				case "converted cost":
					expected = tt.expectedCost
				default:
					expected = tt.expectedPrice
				}
				if !got.Equal(decimal.RequireFromString(expected)) {
					t.Errorf("%s = %s, want %s", name, got, expected)
				}
			}
			if result.BelowMinimumOrderAmount != tt.belowMinimum {
				t.Errorf("BelowMinimumOrderAmount = %v, want %v", result.BelowMinimumOrderAmount, tt.belowMinimum)
			}
		})
	}
}

func TestCalculateSMMPriceRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name string
		edit func(*SMMPriceConfig)
		want error
	}{
		{
			name: "missing source currency",
			edit: func(cfg *SMMPriceConfig) { cfg.SourceCurrency = "" },
			want: ErrSMMSourceCurrencyMissing,
		},
		{
			name: "missing exchange rate",
			edit: func(cfg *SMMPriceConfig) { cfg.SourceToUSDT = decimal.Zero },
			want: ErrSMMExchangeRateMissing,
		},
		{
			name: "negative exchange rate",
			edit: func(cfg *SMMPriceConfig) { cfg.SourceToUSDT = decimal.RequireFromString("-1") },
			want: ErrSMMExchangeRateInvalid,
		},
		{
			name: "zero markup multiplier",
			edit: func(cfg *SMMPriceConfig) { cfg.MarkupMultiplier = decimal.Zero },
			want: ErrSMMMarkupMultiplierInvalid,
		},
		{
			name: "invalid pricing unit",
			edit: func(cfg *SMMPriceConfig) { cfg.PricingUnit = "unknown" },
			want: ErrSMMPricingUnitInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := smmTestPriceConfig()
			tt.edit(&cfg)
			_, err := CalculateSMMPrice("1", 1, cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEffectiveSMMMinimumQuantityHonorsAmountAndBounds(t *testing.T) {
	cfg := smmTestPriceConfig()
	quantity, err := EffectiveSMMMinimumQuantity("0.398", 100, 0, cfg)
	if err != nil {
		t.Fatalf("EffectiveSMMMinimumQuantity() error = %v", err)
	}
	if quantity != 1244 {
		t.Fatalf("effective minimum quantity = %d, want 1244", quantity)
	}

	priceAtMinimum, err := CalculateSMMPrice("0.398", quantity, cfg)
	if err != nil {
		t.Fatalf("price at effective minimum error = %v", err)
	}
	if priceAtMinimum.BelowMinimumOrderAmount {
		t.Fatalf("effective minimum price = %s, still below %s", priceAtMinimum.SellPrice, cfg.MinimumOrderAmountUSDT)
	}
	priceBefore, err := CalculateSMMPrice("0.398", quantity-1, cfg)
	if err != nil {
		t.Fatalf("price before effective minimum error = %v", err)
	}
	if !priceBefore.BelowMinimumOrderAmount {
		t.Fatalf("quantity before effective minimum unexpectedly reaches %s", priceBefore.SellPrice)
	}

	bounded := cfg
	if _, err := EffectiveSMMMinimumQuantity("0.398", 100, 1243, bounded); !errors.Is(err, ErrSMMMinimumExceedsMaximum) {
		t.Fatalf("max-bound error = %v, want %v", err, ErrSMMMinimumExceedsMaximum)
	}

	upstreamMinimum := cfg
	quantity, err = EffectiveSMMMinimumQuantity("0.398", 1300, 2000, upstreamMinimum)
	if err != nil {
		t.Fatalf("upstream minimum error = %v", err)
	}
	if quantity != 1300 {
		t.Fatalf("effective minimum quantity = %d, want upstream minimum 1300", quantity)
	}
}

func TestEffectiveSMMMinimumQuantityRejectsUnreachableAndInvalidRanges(t *testing.T) {
	cfg := smmTestPriceConfig()
	if _, err := EffectiveSMMMinimumQuantity("0", 1, 0, cfg); !errors.Is(err, ErrSMMMinimumOrderUnreachable) {
		t.Fatalf("zero rate error = %v, want %v", err, ErrSMMMinimumOrderUnreachable)
	}
	if _, err := EffectiveSMMMinimumQuantity("1", 100, 99, cfg); !errors.Is(err, ErrSMMUpstreamQuantityRangeInvalid) {
		t.Fatalf("invalid range error = %v, want %v", err, ErrSMMUpstreamQuantityRangeInvalid)
	}

	perOrder := cfg
	perOrder.PricingUnit = constants.PricingUnitPerOrder
	perOrder.MinimumOrderAmountUSDT = decimal.RequireFromString("1")
	if _, err := EffectiveSMMMinimumQuantity("0.398", 1, 0, perOrder); !errors.Is(err, ErrSMMMinimumOrderUnreachable) {
		t.Fatalf("per-order unreachable error = %v, want %v", err, ErrSMMMinimumOrderUnreachable)
	}
}

func TestSMMPricingHandlesLargeQuantityAndCurrencyMismatch(t *testing.T) {
	cfg := smmTestPriceConfig()
	result, err := CalculateSMMPrice("0.398", 2_000_000_000, cfg)
	if err != nil {
		t.Fatalf("large quantity error = %v", err)
	}
	if !result.SellPrice.Equal(decimal.RequireFromString("1592000")) {
		t.Fatalf("large quantity sell price = %s, want 1592000", result.SellPrice)
	}
	if err := ValidateSMMCurrency("USD", "EUR"); !errors.Is(err, ErrSMMCurrencyMismatch) {
		t.Fatalf("currency mismatch error = %v, want %v", err, ErrSMMCurrencyMismatch)
	}
	if err := ValidateSMMCurrency("USD", "usd"); err != nil {
		t.Fatalf("case-insensitive currency validation error = %v", err)
	}
}

func TestSMMCatalogPricingFailsClosedForMissingConfigAndUnsupportedType(t *testing.T) {
	missingRate := &siteconnectiondomain.Connection{SourceCurrency: "USD"}
	_, _, orderable, reason := smmCatalogPricing("10.85", 500, 200000, true, "", missingRate)
	if orderable || reason != smmReasonPricingMissing {
		t.Fatalf("missing pricing config result = orderable %v reason %q", orderable, reason)
	}

	configured := &siteconnectiondomain.Connection{
		SourceCurrency:         "USD",
		SourceToUSDT:           "1",
		MarkupMultiplier:       "2",
		MinimumOrderAmountUSDT: "1",
		DefaultPricingUnit:     constants.PricingUnitPer1000,
	}
	_, _, orderable, reason = smmCatalogPricing("16.81", 1000, 100000, false, "", configured)
	if orderable || reason != smmReasonServiceTypeUnsupported {
		t.Fatalf("unsupported service result = orderable %v reason %q", orderable, reason)
	}
}
