package application

import (
	"testing"

	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
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
		{"small price with markup", "0.01", "100", "none", "0.02"},
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

func TestPriceForMappingUsesExactDecimalMultiplier(t *testing.T) {
	conn := &siteconnectiondomain.Connection{
		ExchangeRate:       decimal.NewFromInt(1),
		PriceMarkupPercent: decimal.Zero,
		PriceRoundingMode:  "none",
	}
	mapping := &mappingdomain.Mapping{}
	got := priceForMapping(conn, mapping, decimal.RequireFromString("0.398"), decimal.NewFromInt(5))
	if !got.Equal(decimal.RequireFromString("1.99")) {
		t.Fatalf("priceForMapping = %s, want 1.99", got)
	}
}

func TestManualPriceModeDisablesAutomaticSync(t *testing.T) {
	conn := &siteconnectiondomain.Connection{Protocol: "prodseller", AutoSyncPrice: true}
	if shouldAutoSyncPrice(&mappingdomain.Mapping{PriceMode: mappingdomain.PriceModeManual}, conn) {
		t.Fatal("manual price policy must not be overwritten by sync")
	}
	if !shouldAutoSyncPrice(&mappingdomain.Mapping{PriceMode: mappingdomain.PriceModeAuto}, conn) {
		t.Fatal("auto price policy should sync")
	}
}

func TestCalculatePriceProtectionFloorUsesExactDecimalMath(t *testing.T) {
	cost := decimal.RequireFromString("10.00")
	feeRate := decimal.RequireFromString("2.5")
	fixedFee := decimal.RequireFromString("0.10")
	minimumProfit := decimal.RequireFromString("0.50")

	floor, err := CalculatePriceProtectionFloor(cost, feeRate, fixedFee, minimumProfit)
	if err != nil {
		t.Fatalf("CalculatePriceProtectionFloor returned error: %v", err)
	}
	want := decimal.RequireFromString("10.8717948717948718")
	if !floor.Equal(want) {
		t.Fatalf("protection floor = %s, want %s", floor, want)
	}
}

func TestCalculatePriceProtectionFloorRejectsInvalidFeeRate(t *testing.T) {
	if _, err := CalculatePriceProtectionFloor(decimal.NewFromInt(1), decimal.NewFromInt(100), decimal.Zero, decimal.Zero); err == nil {
		t.Fatal("expected a 100%% fee rate to be rejected")
	}
	if _, err := CalculatePriceProtectionFloor(decimal.NewFromInt(1), decimal.NewFromInt(-1), decimal.Zero, decimal.Zero); err == nil {
		t.Fatal("expected a negative fee rate to be rejected")
	}
}

func TestPriceBelowProtectionFloorHandlesRoundingBoundary(t *testing.T) {
	conn := &siteconnectiondomain.Connection{
		ExchangeRate:   decimal.NewFromInt(1),
		PaymentFeeRate: decimal.Zero,
		MinimumProfit:  decimal.RequireFromString("0.01"),
	}

	_, below, err := priceBelowProtectionFloor(conn, decimal.RequireFromString("0.398"), decimal.RequireFromString("0.41"))
	if err != nil {
		t.Fatalf("priceBelowProtectionFloor returned error: %v", err)
	}
	if below {
		t.Fatal("0.41 should satisfy the exact 0.408 protection floor")
	}

	_, below, err = priceBelowProtectionFloor(conn, decimal.RequireFromString("0.398"), decimal.RequireFromString("0.40"))
	if err != nil {
		t.Fatalf("priceBelowProtectionFloor returned error: %v", err)
	}
	if !below {
		t.Fatal("0.40 should be below the exact 0.408 protection floor")
	}
}
