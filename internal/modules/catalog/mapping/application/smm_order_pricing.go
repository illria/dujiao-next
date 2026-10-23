package application

import (
	"errors"
	"strings"

	"github.com/dujiao-next/internal/constants"
	"github.com/shopspring/decimal"
)

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
