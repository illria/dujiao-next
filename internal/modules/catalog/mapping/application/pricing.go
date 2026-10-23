package application

import (
	"errors"
	"strings"

	"github.com/dujiao-next/internal/constants"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/shopspring/decimal"
)

var (
	hundred    = decimal.NewFromInt(100)
	pointOne   = decimal.RequireFromString("0.1")
	pointCents = decimal.RequireFromString("0.01")
	roundScale = int32(2)
)

var (
	ErrSMMSourceCurrencyMissing        = errors.New("smm source currency is required")
	ErrSMMExchangeRateMissing          = errors.New("smm source to usdt rate is required")
	ErrSMMExchangeRateInvalid          = errors.New("smm source to usdt rate must be positive")
	ErrSMMMarkupMultiplierInvalid      = errors.New("smm markup multiplier must be at least 1")
	ErrSMMMinimumAmountInvalid         = errors.New("smm minimum order amount must be positive")
	ErrSMMPricingUnitInvalid           = errors.New("smm pricing unit is invalid")
	ErrSMMRateInvalid                  = errors.New("smm rate must be a non-negative decimal")
	ErrSMMQuantityInvalid              = errors.New("smm quantity must be positive")
	ErrSMMCurrencyMismatch             = errors.New("smm balance currency does not match source currency")
	ErrSMMMinimumOrderUnreachable      = errors.New("smm minimum order amount cannot be reached")
	ErrSMMMinimumExceedsMaximum        = errors.New("smm minimum order quantity exceeds upstream maximum")
	ErrSMMUpstreamQuantityRangeInvalid = errors.New("smm upstream quantity range is invalid")
	ErrSMMServiceNotOrderable          = errors.New("smm service is not orderable")
	ErrSMMQuantityBelowMinimum         = errors.New("smm quantity is below the effective minimum")
	ErrSMMQuantityAboveMaximum         = errors.New("smm quantity is above the upstream maximum")
)

const (
	smmPricingMode                  = "smm_rate"
	smmReasonServiceTypeUnsupported = "service_type_not_supported"
	smmReasonPricingMissing         = "pricing_configuration_missing"
	smmReasonPricingInvalid         = "pricing_configuration_invalid"
	smmReasonRateInvalid            = "rate_invalid"
	smmReasonMinimumUnreachable     = "minimum_order_unreachable"
)

// SMMPriceConfig contains the fixed-point pricing inputs for an SMM service.
// SourceToUSDT and MarkupMultiplier are decimal values; no floating-point
// arithmetic is used in SMM pricing.
type SMMPriceConfig struct {
	SourceCurrency         string
	SourceToUSDT           decimal.Decimal
	ExchangeRateBufferBPS  int
	MarkupMultiplier       decimal.Decimal
	MinimumOrderAmountUSDT decimal.Decimal
	PricingUnit            string
}

type SMMPriceResult struct {
	Rate                    decimal.Decimal
	Quantity                int
	RawCost                 decimal.Decimal
	ConvertedCost           decimal.Decimal
	SellPrice               decimal.Decimal
	MinimumOrderAmountUSDT  decimal.Decimal
	BelowMinimumOrderAmount bool
}

func ValidateSMMCurrency(configuredCurrency, observedCurrency string) error {
	configuredCurrency = strings.TrimSpace(configuredCurrency)
	observedCurrency = strings.TrimSpace(observedCurrency)
	if configuredCurrency == "" {
		return ErrSMMSourceCurrencyMissing
	}
	if observedCurrency == "" || !strings.EqualFold(configuredCurrency, observedCurrency) {
		return ErrSMMCurrencyMismatch
	}
	return nil
}

func smmPriceConfigFromConnection(conn *siteconnectiondomain.Connection) (SMMPriceConfig, error) {
	if conn == nil {
		return SMMPriceConfig{}, ErrSMMSourceCurrencyMissing
	}
	cfg := SMMPriceConfig{
		SourceCurrency:        conn.SourceCurrency,
		ExchangeRateBufferBPS: conn.ExchangeRateBufferBPS,
		PricingUnit:           conn.DefaultPricingUnit,
	}
	if strings.TrimSpace(cfg.PricingUnit) == "" {
		cfg.PricingUnit = constants.PricingUnitPer1000
	}
	if strings.TrimSpace(conn.SourceToUSDT) != "" {
		parsed, err := decimal.NewFromString(strings.TrimSpace(conn.SourceToUSDT))
		if err != nil {
			return SMMPriceConfig{}, ErrSMMExchangeRateInvalid
		}
		cfg.SourceToUSDT = parsed
	}
	if strings.TrimSpace(conn.MarkupMultiplier) == "" {
		cfg.MarkupMultiplier = decimal.NewFromInt(2)
	} else {
		parsed, err := decimal.NewFromString(strings.TrimSpace(conn.MarkupMultiplier))
		if err != nil {
			return SMMPriceConfig{}, ErrSMMMarkupMultiplierInvalid
		}
		cfg.MarkupMultiplier = parsed
	}
	if strings.TrimSpace(conn.MinimumOrderAmountUSDT) == "" {
		cfg.MinimumOrderAmountUSDT = decimal.NewFromInt(1)
	} else {
		parsed, err := decimal.NewFromString(strings.TrimSpace(conn.MinimumOrderAmountUSDT))
		if err != nil {
			return SMMPriceConfig{}, ErrSMMMinimumAmountInvalid
		}
		cfg.MinimumOrderAmountUSDT = parsed
	}
	return cfg, validateSMMPriceConfig(cfg)
}

func smmCatalogPricing(rateText string, upstreamMin, upstreamMax int, upstreamOrderable bool, unsupportedReason string, conn *siteconnectiondomain.Connection) (SMMPriceResult, int, bool, string) {
	if !upstreamOrderable {
		if strings.TrimSpace(unsupportedReason) == "" {
			unsupportedReason = smmReasonServiceTypeUnsupported
		}
		return SMMPriceResult{}, upstreamMin, false, unsupportedReason
	}

	cfg, err := smmPriceConfigFromConnection(conn)
	if err != nil {
		if errors.Is(err, ErrSMMSourceCurrencyMissing) || errors.Is(err, ErrSMMExchangeRateMissing) {
			return SMMPriceResult{}, upstreamMin, false, smmReasonPricingMissing
		}
		return SMMPriceResult{}, upstreamMin, false, smmReasonPricingInvalid
	}
	result, err := CalculateSMMPrice(rateText, 1000, cfg)
	if err != nil {
		return SMMPriceResult{}, upstreamMin, false, smmReasonRateInvalid
	}
	effectiveMin, err := EffectiveSMMMinimumQuantity(rateText, upstreamMin, upstreamMax, cfg)
	if err != nil {
		if errors.Is(err, ErrSMMMinimumExceedsMaximum) || errors.Is(err, ErrSMMMinimumOrderUnreachable) {
			return result, upstreamMin, false, smmReasonMinimumUnreachable
		}
		return result, upstreamMin, false, smmReasonPricingInvalid
	}
	return result, effectiveMin, true, ""
}

func CalculateSMMPrice(rateText string, quantity int, cfg SMMPriceConfig) (SMMPriceResult, error) {
	if err := validateSMMPriceConfig(cfg); err != nil {
		return SMMPriceResult{}, err
	}
	if quantity <= 0 {
		return SMMPriceResult{}, ErrSMMQuantityInvalid
	}

	rate, err := decimal.NewFromString(strings.TrimSpace(rateText))
	if err != nil || rate.IsNegative() {
		return SMMPriceResult{}, ErrSMMRateInvalid
	}

	rawCost := rate
	switch cfg.PricingUnit {
	case constants.PricingUnitPer1000:
		rawCost = rate.Mul(decimal.NewFromInt(int64(quantity))).Div(decimal.NewFromInt(1000))
	case constants.PricingUnitPerUnit:
		rawCost = rate.Mul(decimal.NewFromInt(int64(quantity)))
	case constants.PricingUnitPerOrder:
		// Per-order rates do not change with quantity, but quantity is still
		// validated above so the later order flow has one consistent contract.
	default:
		return SMMPriceResult{}, ErrSMMPricingUnitInvalid
	}

	bufferMultiplier := decimal.NewFromInt(10000).Add(decimal.NewFromInt(int64(cfg.ExchangeRateBufferBPS))).Div(decimal.NewFromInt(10000))
	convertedCost := rawCost.Mul(cfg.SourceToUSDT).Mul(bufferMultiplier)
	sellPrice := ceilSMMCent(convertedCost.Mul(cfg.MarkupMultiplier))
	if sellPrice.LessThan(convertedCost) {
		// This is defensive: valid multipliers are >= 1, but the invariant is
		// important enough to keep explicit at the final money boundary.
		sellPrice = ceilSMMCent(convertedCost)
	}

	return SMMPriceResult{
		Rate:                    rate,
		Quantity:                quantity,
		RawCost:                 rawCost,
		ConvertedCost:           convertedCost,
		SellPrice:               sellPrice,
		MinimumOrderAmountUSDT:  cfg.MinimumOrderAmountUSDT,
		BelowMinimumOrderAmount: sellPrice.LessThan(cfg.MinimumOrderAmountUSDT),
	}, nil
}

// EffectiveSMMMinimumQuantity returns the smallest quantity that reaches the
// configured minimum amount, while respecting the upstream min/max bounds.
// The result is intended for the later checkout flow; Step 1 only exposes the
// calculation and does not change the order path.
func EffectiveSMMMinimumQuantity(rateText string, upstreamMin, upstreamMax int, cfg SMMPriceConfig) (int, error) {
	if err := validateSMMPriceConfig(cfg); err != nil {
		return 0, err
	}
	if upstreamMin < 0 || upstreamMax < 0 || (upstreamMax > 0 && upstreamMin > upstreamMax) {
		return 0, ErrSMMUpstreamQuantityRangeInvalid
	}

	minimumQuantity := upstreamMin
	if minimumQuantity < 1 {
		minimumQuantity = 1
	}

	if cfg.PricingUnit == constants.PricingUnitPerOrder {
		price, err := CalculateSMMPrice(rateText, minimumQuantity, cfg)
		if err != nil {
			return 0, err
		}
		if price.BelowMinimumOrderAmount {
			return 0, ErrSMMMinimumOrderUnreachable
		}
		return minimumQuantity, nil
	}

	rate, err := decimal.NewFromString(strings.TrimSpace(rateText))
	if err != nil || rate.IsNegative() {
		return 0, ErrSMMRateInvalid
	}
	if rate.IsZero() {
		return 0, ErrSMMMinimumOrderUnreachable
	}

	bufferMultiplier := decimal.NewFromInt(10000).Add(decimal.NewFromInt(int64(cfg.ExchangeRateBufferBPS))).Div(decimal.NewFromInt(10000))
	perQuantitySellBeforeRounding := rate.Mul(cfg.SourceToUSDT).Mul(bufferMultiplier).Mul(cfg.MarkupMultiplier)
	if cfg.PricingUnit == constants.PricingUnitPer1000 {
		perQuantitySellBeforeRounding = perQuantitySellBeforeRounding.Div(decimal.NewFromInt(1000))
	}
	if perQuantitySellBeforeRounding.IsZero() {
		return 0, ErrSMMMinimumOrderUnreachable
	}

	upperDecimal := cfg.MinimumOrderAmountUSDT.Div(perQuantitySellBeforeRounding).Ceil()
	maxInt := int64(^uint(0) >> 1)
	if upperDecimal.GreaterThan(decimal.NewFromInt(maxInt)) {
		return 0, ErrSMMMinimumOrderUnreachable
	}
	upper := int(upperDecimal.IntPart())
	if upper < minimumQuantity {
		upper = minimumQuantity
	}
	if upstreamMax > 0 && upper > upstreamMax {
		upper = upstreamMax
	}
	if upper < minimumQuantity {
		return 0, ErrSMMMinimumExceedsMaximum
	}

	upperPrice, err := CalculateSMMPrice(rateText, upper, cfg)
	if err != nil {
		return 0, err
	}
	if upperPrice.BelowMinimumOrderAmount {
		if upstreamMax > 0 {
			return 0, ErrSMMMinimumExceedsMaximum
		}
		return 0, ErrSMMMinimumOrderUnreachable
	}

	// The rounded selling price is monotonic. Binary search avoids a large
	// linear loop for low rates or very large service quantities.
	low := minimumQuantity
	if low > upper {
		return 0, ErrSMMMinimumExceedsMaximum
	}
	lowPrice, err := CalculateSMMPrice(rateText, low, cfg)
	if err != nil {
		return 0, err
	}
	if !lowPrice.BelowMinimumOrderAmount {
		return low, nil
	}
	for low < upper {
		mid := low + (upper-low)/2
		midPrice, err := CalculateSMMPrice(rateText, mid, cfg)
		if err != nil {
			return 0, err
		}
		if midPrice.BelowMinimumOrderAmount {
			low = mid + 1
		} else {
			upper = mid
		}
	}
	return low, nil
}

func validateSMMPriceConfig(cfg SMMPriceConfig) error {
	if strings.TrimSpace(cfg.SourceCurrency) == "" {
		return ErrSMMSourceCurrencyMissing
	}
	if cfg.SourceToUSDT.IsZero() {
		return ErrSMMExchangeRateMissing
	}
	if cfg.SourceToUSDT.IsNegative() {
		return ErrSMMExchangeRateInvalid
	}
	if cfg.ExchangeRateBufferBPS < 0 {
		return ErrSMMExchangeRateInvalid
	}
	if cfg.MarkupMultiplier.LessThan(decimal.NewFromInt(1)) {
		return ErrSMMMarkupMultiplierInvalid
	}
	if cfg.MinimumOrderAmountUSDT.LessThanOrEqual(decimal.Zero) {
		return ErrSMMMinimumAmountInvalid
	}
	switch cfg.PricingUnit {
	case constants.PricingUnitPer1000, constants.PricingUnitPerUnit, constants.PricingUnitPerOrder:
		return nil
	default:
		return ErrSMMPricingUnitInvalid
	}
}

func ceilSMMCent(value decimal.Decimal) decimal.Decimal {
	return value.Div(pointCents).Ceil().Mul(pointCents)
}

// convertCurrency 仅按汇率转换，不应用加价。
// 用于计算成本价：上游价格 × 汇率 = 本地币种成本（不含加价利润）。
// exchangeRate 为 0 或负数时视为 1（同币种）。
func convertCurrency(upstreamPrice, exchangeRate decimal.Decimal) decimal.Decimal {
	if exchangeRate.LessThanOrEqual(decimal.Zero) {
		exchangeRate = decimal.NewFromInt(1)
	}
	return upstreamPrice.Mul(exchangeRate)
}

// CalculateLocalPrice 先按汇率转换，再应用加价和取整。
// 计算链路：上游价格 × 汇率 → 加价 → 取整 → 本地售价。
// exchangeRate 为 0 或负数时视为 1（同币种）。
func CalculateLocalPrice(upstreamPrice, exchangeRate, markupPercent decimal.Decimal, roundingMode string) decimal.Decimal {
	if exchangeRate.LessThanOrEqual(decimal.Zero) {
		exchangeRate = decimal.NewFromInt(1)
	}
	converted := upstreamPrice.Mul(exchangeRate)
	return CalculateMarkedUpPrice(converted, markupPercent, roundingMode)
}

// CalculateMarkedUpPrice 根据加价百分比计算本地售价。
// markupPercent=100 表示上浮 100%，即 upstream × 2。
// markupPercent=0 时直接返回原价（向后兼容）。
func CalculateMarkedUpPrice(upstreamPrice, markupPercent decimal.Decimal, roundingMode string) decimal.Decimal {
	if markupPercent.IsZero() {
		return upstreamPrice.Round(roundScale)
	}

	// result = upstreamPrice * (1 + markupPercent / 100)
	multiplier := decimal.NewFromInt(1).Add(markupPercent.Div(hundred))
	result := upstreamPrice.Mul(multiplier)

	if result.IsNegative() {
		return decimal.Zero
	}

	switch roundingMode {
	case "ceil_cent":
		// 以 0.01 为最小单位向上取整，适用于 USDT 两位小数售价。
		scaled := result.Div(pointCents)
		return scaled.Ceil().Mul(pointCents)
	case "ceil_int":
		// 向上取整到整数：12.01 → 13
		if result.Equal(result.Floor()) {
			return result
		}
		return result.Ceil()
	case "ceil_tenth":
		// 向上取整到 0.1：12.34 → 12.40
		scaled := result.Div(pointOne)
		if scaled.Equal(scaled.Floor()) {
			return result.Round(1)
		}
		return scaled.Ceil().Mul(pointOne)
	default:
		return result.Round(roundScale)
	}
}

// GetSMMOrderPricing calculates the exact price for one SMM service order.
// The calculation uses the latest locally synced fixed-point rate and is kept
// outside any order/database transaction. The later procurement preflight
// performs a second live services read immediately before action=add.
//
// The bool result is false for non-SMM mappings so callers can keep the
// existing price path for all other upstream protocols.
func (s *Service) GetSMMOrderPricing(localSKUID uint, quantity int) (sellPrice, cost decimal.Decimal, rate, pricingUnit string, isSMM bool, err error) {
	if s == nil || s.skuMappings == nil || localSKUID == 0 || quantity <= 0 {
		return decimal.Zero, decimal.Zero, "", "", false, nil
	}

	skuMapping, err := s.skuMappings.GetByLocalSKUID(localSKUID)
	if err != nil {
		return decimal.Zero, decimal.Zero, "", "", false, err
	}
	if skuMapping == nil || s.mappings == nil || s.connections == nil {
		return decimal.Zero, decimal.Zero, "", "", false, nil
	}

	mapping, err := s.mappings.GetByID(skuMapping.ProductMappingID)
	if err != nil {
		return decimal.Zero, decimal.Zero, "", "", false, err
	}
	if mapping == nil {
		return decimal.Zero, decimal.Zero, "", "", false, nil
	}
	connection, err := s.connections.GetByID(mapping.ConnectionID)
	if err != nil {
		return decimal.Zero, decimal.Zero, "", "", false, err
	}
	if connection == nil || connection.Protocol != constants.ConnectionProtocolSMMPanel {
		return decimal.Zero, decimal.Zero, "", "", false, nil
	}

	if !mapping.IsActive || !mapping.IsOrderable || (strings.TrimSpace(mapping.UpstreamServiceType) != "" && !strings.EqualFold(strings.TrimSpace(mapping.UpstreamServiceType), "default")) {
		return decimal.Zero, decimal.Zero, "", "", true, ErrSMMServiceNotOrderable
	}
	if !skuMapping.UpstreamIsActive {
		return decimal.Zero, decimal.Zero, "", "", true, ErrSMMServiceNotOrderable
	}

	rate = strings.TrimSpace(skuMapping.UpstreamRateText)
	if rate == "" {
		rate = strings.TrimSpace(mapping.UpstreamRateText)
	}
	pricingUnit = strings.TrimSpace(skuMapping.PricingUnit)
	if pricingUnit == "" {
		pricingUnit = strings.TrimSpace(mapping.PricingUnit)
	}
	if pricingUnit == "" {
		pricingUnit = constants.PricingUnitPer1000
	}

	cfg, err := smmPriceConfigFromConnection(connection)
	if err != nil {
		return decimal.Zero, decimal.Zero, rate, pricingUnit, true, err
	}
	// The service mapping is authoritative for the unit. The connection field
	// is only the default used when an upstream service does not provide one.
	cfg.PricingUnit = pricingUnit
	result, err := CalculateSMMPrice(rate, quantity, cfg)
	if err != nil {
		return decimal.Zero, decimal.Zero, rate, pricingUnit, true, err
	}
	effectiveMin, err := EffectiveSMMMinimumQuantity(rate, mapping.MinQuantity, mapping.MaxQuantity, cfg)
	if err != nil {
		return decimal.Zero, decimal.Zero, rate, pricingUnit, true, err
	}
	if quantity < effectiveMin {
		return decimal.Zero, decimal.Zero, rate, pricingUnit, true, errors.Join(ErrSMMQuantityBelowMinimum, ErrSMMMinimumOrderUnreachable)
	}
	if mapping.MaxQuantity > 0 && quantity > mapping.MaxQuantity {
		return decimal.Zero, decimal.Zero, rate, pricingUnit, true, ErrSMMQuantityAboveMaximum
	}
	return result.SellPrice, result.ConvertedCost, rate, pricingUnit, true, nil
}
