package application

import (
	"errors"

	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/shopspring/decimal"
)

var (
	hundred    = decimal.NewFromInt(100)
	pointOne   = decimal.NewFromFloat(0.1)
	roundScale = int32(2)
)

var ErrInvalidPriceProtection = errors.New("invalid price protection configuration")

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

// CalculatePriceProtectionFloor 计算保证不亏损且达到最低利润时的最低售价。
// paymentFeeRate 是按售价收取的百分比，因此需要反解售价：
// 售价 × (1 - 手续费率) - 固定手续费 - 成本 >= 最低利润。
func CalculatePriceProtectionFloor(cost, paymentFeeRate, paymentFixedFee, minimumProfit decimal.Decimal) (decimal.Decimal, error) {
	if cost.IsNegative() || paymentFeeRate.IsNegative() || paymentFixedFee.IsNegative() || minimumProfit.IsNegative() {
		return decimal.Zero, ErrInvalidPriceProtection
	}
	if paymentFeeRate.GreaterThanOrEqual(hundred) {
		return decimal.Zero, ErrInvalidPriceProtection
	}
	denominator := decimal.NewFromInt(1).Sub(paymentFeeRate.Div(hundred))
	if denominator.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, ErrInvalidPriceProtection
	}
	return cost.Add(paymentFixedFee).Add(minimumProfit).Div(denominator), nil
}

// PriceProtectionFloorForUpstream 将上游成本换算成本地币种后计算最低保护价。
func PriceProtectionFloorForUpstream(conn *siteconnectiondomain.Connection, upstreamPrice decimal.Decimal) (decimal.Decimal, error) {
	if conn == nil {
		return decimal.Zero, ErrInvalidPriceProtection
	}
	cost := convertCurrency(upstreamPrice, conn.ExchangeRate)
	return CalculatePriceProtectionFloor(cost, conn.PaymentFeeRate, conn.PaymentFixedFee, conn.MinimumProfit)
}

func priceBelowProtectionFloor(conn *siteconnectiondomain.Connection, upstreamPrice, sellingPrice decimal.Decimal) (decimal.Decimal, bool, error) {
	floor, err := PriceProtectionFloorForUpstream(conn, upstreamPrice)
	if err != nil {
		return decimal.Zero, true, err
	}
	return floor, sellingPrice.LessThan(floor), nil
}
