package application

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dujiao-next/internal/constants"
	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	siteconnectioncontract "github.com/dujiao-next/internal/modules/siteconnection/contract"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

// ReapplyMarkup 对指定连接的所有映射商品重新应用加价规则
func (s *Service) ReapplyMarkup(connectionID uint) (int, error) {
	conn, err := s.connections.GetByID(connectionID)
	if err != nil {
		return 0, err
	}
	if conn == nil {
		return 0, siteconnectioncontract.ErrNotFound
	}

	mappings, err := s.mappings.ListActiveByConnection(connectionID)
	if err != nil {
		return 0, err
	}

	updated := 0
	for _, mapping := range mappings {
		if mapping.PriceMode == mappingdomain.PriceModeManual {
			continue
		}
		skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
		if err != nil {
			continue
		}

		for _, sm := range skuMappings {
			newLocalPrice := priceForMapping(conn, &mapping, sm.UpstreamPrice.Decimal, decimal.Zero)
			localSKU, err := s.skus.GetByID(sm.LocalSKUID)
			if err != nil || localSKU == nil {
				continue
			}
			localSKU.PriceAmount = money.FromDecimal(newLocalPrice.Round(2))
			localSKU.CostPriceAmount = money.FromDecimal(convertCurrency(sm.UpstreamPrice.Decimal, conn.ExchangeRate).Round(2)) // 成本价 = 上游价格 × 汇率（本地币种）
			_ = s.skus.Update(localSKU)
		}

		// 更新 Product.PriceAmount
		localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
		if err == nil && localProduct != nil {
			s.recalcProductPrice(localProduct)
			updated++
		}
	}

	return updated, nil
}

// recalcProductPrice 重新计算商品基准价格和成本价为最低活跃 SKU 价格
func (s *Service) recalcProductPrice(product *productdomain.Product) {
	allSKUs, err := s.skus.ListByProduct(product.ID, true)
	if err != nil || len(allSKUs) == 0 {
		return
	}
	minPrice := allSKUs[0].PriceAmount.Decimal
	minCostPrice := allSKUs[0].CostPriceAmount.Decimal
	for _, sku := range allSKUs[1:] {
		if sku.PriceAmount.Decimal.LessThan(minPrice) {
			minPrice = sku.PriceAmount.Decimal
		}
		if sku.CostPriceAmount.Decimal.LessThan(minCostPrice) {
			minCostPrice = sku.CostPriceAmount.Decimal
		}
	}
	product.PriceAmount = money.FromDecimal(minPrice.Round(2))
	product.CostPriceAmount = money.FromDecimal(minCostPrice.Round(2))
	_ = s.products.Update(product)
}

var ErrInvalidPricePolicy = errors.New("invalid product price policy")

// SetPricePolicy 为单个映射商品设置手动售价或自动加价倍数。
// 所有计算使用 decimal；手动售价写入本地商品，自动模式在下一次同步时重算。
func (s *Service) SetPricePolicy(mappingID uint, mode string, manualPrice, multiplier decimal.Decimal) error {
	mapping, err := s.mappings.GetByID(mappingID)
	if err != nil {
		return err
	}
	if mapping == nil {
		return mappingcontract.ErrMappingNotFound
	}
	conn, err := s.connections.GetByID(mapping.ConnectionID)
	if err != nil {
		return err
	}
	if conn == nil {
		return siteconnectioncontract.ErrNotFound
	}

	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case mappingdomain.PriceModeManual:
		if manualPrice.LessThanOrEqual(decimal.Zero) {
			return ErrInvalidPricePolicy
		}
	case mappingdomain.PriceModeAuto:
		if multiplier.IsNegative() {
			return ErrInvalidPricePolicy
		}
	default:
		return ErrInvalidPricePolicy
	}

	localSKUs, err := s.skus.ListByProduct(mapping.LocalProductID, false)
	if err != nil {
		return err
	}
	skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
	if err != nil {
		return err
	}
	upstreamByLocalSKU := make(map[uint]mappingdomain.SKUMapping, len(skuMappings))
	for _, skuMapping := range skuMappings {
		upstreamByLocalSKU[skuMapping.LocalSKUID] = skuMapping
	}

	for i := range localSKUs {
		localSKU := &localSKUs[i]
		if mode == mappingdomain.PriceModeManual {
			localSKU.PriceAmount = money.FromDecimal(manualPrice.Round(2))
		} else if upstream, ok := upstreamByLocalSKU[localSKU.ID]; ok {
			localSKU.PriceAmount = money.FromDecimal(priceForMapping(conn, mapping, upstream.UpstreamPrice.Decimal, multiplier))
			localSKU.CostPriceAmount = money.FromDecimal(convertCurrency(upstream.UpstreamPrice.Decimal, conn.ExchangeRate).Round(2))
		}
		if err := s.skus.Update(localSKU); err != nil {
			return err
		}
	}

	mapping.PriceMode = mode
	mapping.PriceMultiplier = decimal.Zero
	if mode == mappingdomain.PriceModeAuto && multiplier.IsPositive() {
		mapping.PriceMultiplier = multiplier
	}
	if err := s.mappings.Update(mapping); err != nil {
		return err
	}

	localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil {
		return err
	}
	if localProduct != nil {
		if mode == mappingdomain.PriceModeManual {
			localProduct.PriceAmount = money.FromDecimal(manualPrice.Round(2))
			localProduct.CostPriceAmount = lowestMappedCost(localSKUs)
			if err := s.products.Update(localProduct); err != nil {
				return err
			}
		} else {
			s.recalcProductPrice(localProduct)
		}
	}
	return nil
}

// BatchSetPricePolicy 对多个映射商品应用同一套价格策略。
func (s *Service) BatchSetPricePolicy(mappingIDs []uint, mode string, manualPrice, multiplier decimal.Decimal) (int, error) {
	updated := 0
	for _, mappingID := range mappingIDs {
		if err := s.SetPricePolicy(mappingID, mode, manualPrice, multiplier); err != nil {
			if errors.Is(err, mappingcontract.ErrMappingNotFound) {
				continue
			}
			return updated, fmt.Errorf("update mapping %d price policy: %w", mappingID, err)
		}
		updated++
	}
	return updated, nil
}

func shouldAutoSyncPrice(mapping *mappingdomain.Mapping, conn *siteconnectiondomain.Connection) bool {
	return mapping != nil && conn != nil && mapping.PriceMode != mappingdomain.PriceModeManual &&
		(conn.AutoSyncPrice || conn.Protocol == constants.ConnectionProtocolProdSeller)
}

func priceForMapping(conn *siteconnectiondomain.Connection, mapping *mappingdomain.Mapping, upstreamPrice, multiplier decimal.Decimal) decimal.Decimal {
	if conn == nil {
		return decimal.Zero
	}
	if multiplier.IsZero() && mapping != nil && mapping.PriceMultiplier.IsPositive() {
		multiplier = mapping.PriceMultiplier
	}
	if multiplier.IsPositive() {
		converted := convertCurrency(upstreamPrice, conn.ExchangeRate)
		markupPercent := multiplier.Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(100))
		return CalculateMarkedUpPrice(converted, markupPercent, conn.PriceRoundingMode).Round(2)
	}
	return CalculateLocalPrice(upstreamPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode).Round(2)
}

func lowestMappedCost(skus []productdomain.ProductSKU) money.Amount {
	if len(skus) == 0 {
		return money.FromDecimal(decimal.Zero)
	}
	lowest := skus[0].CostPriceAmount.Decimal
	for _, sku := range skus[1:] {
		if sku.CostPriceAmount.Decimal.LessThan(lowest) {
			lowest = sku.CostPriceAmount.Decimal
		}
	}
	return money.FromDecimal(lowest)
}
