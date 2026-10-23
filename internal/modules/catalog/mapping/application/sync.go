package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	categoryapp "github.com/dujiao-next/internal/modules/catalog/category/application"
	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	siteconnectioncontract "github.com/dujiao-next/internal/modules/siteconnection/contract"

	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"

	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"

	"github.com/dujiao-next/internal/cache"
	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/logger"
	settingsintegration "github.com/dujiao-next/internal/modules/settings/schema/integration"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/jsonslice"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/dujiao-next/internal/upstream"

	"github.com/shopspring/decimal"
)

// SyncProduct 同步单个映射商品的上游数据（全量同步）
func (s *Service) SyncProduct(mappingID uint) error {
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

	// ProdSeller 使用字符串外部 ID，不能走旧的 uint 上游商品同步路径。
	// 单品手动同步同样只调用 GET /products/:id，保持与全量目录同步一致。
	if conn.Protocol == constants.ConnectionProtocolProdSeller {
		adapter, err := s.connections.GetAdapter(conn)
		if err != nil {
			return err
		}
		external, ok := adapter.(upstream.ExternalCatalogAdapter)
		if !ok {
			return fmt.Errorf("connection %d does not support external catalog", conn.ID)
		}
		externalID := ""
		if mapping.UpstreamProductExternalID != nil {
			externalID = strings.TrimSpace(*mapping.UpstreamProductExternalID)
		}
		if externalID == "" {
			externalID = strconv.FormatUint(uint64(mapping.UpstreamProductID), 10)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		product, err := external.GetExternalProduct(ctx, externalID)
		cancel()
		if err != nil {
			return fmt.Errorf("fetch prodseller product: %w", err)
		}
		if product == nil {
			return errors.New("prodseller returned an empty product")
		}
		return s.syncExistingProdSellerProduct(mapping, conn, product, time.Now())
	}

	adapter, err := s.connections.GetAdapter(conn)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	upProduct, err := adapter.GetProduct(ctx, mapping.UpstreamProductID)
	if err != nil {
		// 上游软删除 → 标记本地为 deleted，自动停用映射
		if errors.Is(err, upstream.ErrUpstreamProductDeleted) {
			now := time.Now()
			return s.markUpstreamUnavailable(mapping, mappingdomain.UpstreamStatusDeleted, now)
		}
		// 旧版上游下架兜底（新版上游下架返回 200 + is_active=false，走下方分支）
		if errors.Is(err, upstream.ErrUpstreamProductUnavailable) {
			now := time.Now()
			return s.markUpstreamUnavailable(mapping, mappingdomain.UpstreamStatusInactive, now)
		}
		return fmt.Errorf("fetch upstream product: %w", err)
	}

	now := time.Now()

	// 上游 200 但 is_active=false → 视为下架
	if !upProduct.IsActive {
		return s.markUpstreamUnavailable(mapping, mappingdomain.UpstreamStatusInactive, now)
	}

	// ── 1. 同步本地商品字段（表单配置、上下架状态） ──
	localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil {
		return fmt.Errorf("get local product: %w", err)
	}
	if localProduct != nil {
		needsProductUpdate := false
		// 同步人工交付表单配置
		if upProduct.ManualFormSchema != nil {
			localProduct.ManualFormSchemaJSON = upProduct.ManualFormSchema
			needsProductUpdate = true
		}
		if needsProductUpdate {
			_ = s.products.Update(localProduct)
		}
	}

	// ── 2. 同步 SKU：新增 / 更新 / 停用 ──
	skuMappings, err := s.skuMappings.ListByProductMapping(mappingID)
	if err != nil {
		return err
	}

	// 构建上游 SKU 查找表
	upstreamSKUMap := make(map[uint]upstream.UpstreamSKU, len(upProduct.SKUs))
	for _, us := range upProduct.SKUs {
		upstreamSKUMap[us.ID] = us
	}

	// 构建已有映射查找表（按上游 SKU ID）
	existingByUpstreamID := make(map[uint]*mappingdomain.SKUMapping, len(skuMappings))
	for i := range skuMappings {
		existingByUpstreamID[skuMappings[i].UpstreamSKUID] = &skuMappings[i]
	}

	// 2a. 更新已有映射 + 同步本地 SKU
	for i := range skuMappings {
		upSKU, ok := upstreamSKUMap[skuMappings[i].UpstreamSKUID]
		if !ok {
			// 上游 SKU 已删除 → 停用本地 SKU 和映射
			skuMappings[i].UpstreamIsActive = false
			skuMappings[i].UpstreamStock = 0
			skuMappings[i].StockSyncedAt = &now
			_ = s.skuMappings.Update(&skuMappings[i])

			// 停用本地 SKU
			localSKU, _ := s.skus.GetByID(skuMappings[i].LocalSKUID)
			if localSKU != nil && localSKU.IsActive {
				localSKU.IsActive = false
				_ = s.skus.Update(localSKU)
			}
			continue
		}

		upPrice, _ := decimal.NewFromString(upSKU.PriceAmount)

		// 更新 SKU 映射记录
		skuMappings[i].UpstreamPrice = money.FromDecimal(upPrice.Round(2))
		skuMappings[i].UpstreamIsActive = upSKU.IsActive
		skuMappings[i].StockSyncedAt = &now
		skuMappings[i].UpstreamStock = upSKU.StockQuantity
		_ = s.skuMappings.Update(&skuMappings[i])

		// 同步本地 SKU 字段
		localSKU, _ := s.skus.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil {
			localSKU.SpecValuesJSON = upSKU.SpecValues
			localSKU.IsActive = upSKU.IsActive
			// 如果启用了自动同步价格，按加价比例更新本地售价和成本价
			if shouldAutoSyncPrice(mapping, conn) {
				newLocalPrice := priceForMapping(conn, mapping, upPrice, decimal.Zero)
				localSKU.PriceAmount = money.FromDecimal(newLocalPrice.Round(2))
				localSKU.CostPriceAmount = money.FromDecimal(convertCurrency(upPrice, conn.ExchangeRate).Round(2))
			}
			_ = s.skus.Update(localSKU)
		}
	}

	// 2b. 上游新增的 SKU → 创建本地 SKU + 映射
	for _, upSKU := range upProduct.SKUs {
		if _, exists := existingByUpstreamID[upSKU.ID]; exists {
			continue
		}

		skuPrice, _ := decimal.NewFromString(upSKU.PriceAmount)
		localPrice := CalculateLocalPrice(skuPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
		newLocalSKU := productdomain.ProductSKU{
			ProductID:       mapping.LocalProductID,
			SKUCode:         upSKU.SKUCode,
			SpecValuesJSON:  upSKU.SpecValues,
			PriceAmount:     money.FromDecimal(localPrice.Round(2)),
			CostPriceAmount: money.FromDecimal(convertCurrency(skuPrice, conn.ExchangeRate).Round(2)), // 成本价 = 上游价格 × 汇率（本地币种）
			IsActive:        upSKU.IsActive,
			SortOrder:       0,
		}
		if err := s.skus.Create(&newLocalSKU); err != nil {
			continue
		}

		newMapping := &mappingdomain.SKUMapping{
			ProductMappingID: mappingID,
			LocalSKUID:       newLocalSKU.ID,
			UpstreamSKUID:    upSKU.ID,
			UpstreamPrice:    money.FromDecimal(skuPrice.Round(2)),
			UpstreamIsActive: upSKU.IsActive,
			UpstreamStock:    upSKU.StockQuantity,
			StockSyncedAt:    &now,
		}
		_ = s.skuMappings.Create(newMapping)
	}

	// ── 2c. 如果启用了自动同步价格，更新 Product.PriceAmount 为最低 SKU 价格 ──
	if shouldAutoSyncPrice(mapping, conn) && localProduct != nil {
		s.recalcProductPrice(localProduct)
	}

	// 上游未返回批发价时不覆盖本地配置，避免运营手动配置被同步任务清空。
	if len(upProduct.WholesalePrices) > 0 && localProduct != nil {
		if err := s.syncUpstreamWholesalePrices(mapping, localProduct.ID, conn, upProduct); err != nil {
			logger.Warnw("sync_upstream_wholesale_prices_failed",
				"mapping_id", mapping.ID,
				"connection_id", mapping.ConnectionID,
				"upstream_product_id", mapping.UpstreamProductID,
				"local_product_id", mapping.LocalProductID,
				"error", err,
			)
		}
	}

	// ── 3. 更新同步时间 + 上游交付类型 + 状态恢复 ──
	upFulfillment := upProduct.FulfillmentType
	if upFulfillment != constants.FulfillmentTypeAuto {
		upFulfillment = constants.FulfillmentTypeManual
	}
	mapping.UpstreamFulfillmentType = upFulfillment
	mapping.UpstreamStatus = mappingdomain.UpstreamStatusActive
	mapping.LastSyncedAt = &now
	return s.mappings.Update(mapping)
}

func (s *Service) syncUpstreamWholesalePrices(mapping *mappingdomain.Mapping, localProductID uint, conn *siteconnectiondomain.Connection, upProduct *upstream.UpstreamProduct) error {
	if s == nil || mapping == nil || conn == nil || upProduct == nil || localProductID == 0 || len(upProduct.WholesalePrices) == 0 {
		return nil
	}
	localSKUs, err := s.skus.ListByProduct(localProductID, false)
	if err != nil {
		return err
	}
	skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
	if err != nil {
		return err
	}
	wholesalePrices := convertUpstreamWholesalePrices(
		upProduct.WholesalePrices,
		conn.ExchangeRate,
		conn.PriceMarkupPercent,
		conn.PriceRoundingMode,
		buildUpstreamWholesaleSKUIndex(localSKUs, upProduct.SKUs, skuMappings),
	)
	if len(wholesalePrices) == 0 {
		logger.Warnw("sync_upstream_wholesale_prices_empty_after_convert",
			"mapping_id", mapping.ID,
			"connection_id", mapping.ConnectionID,
			"upstream_product_id", mapping.UpstreamProductID,
			"local_product_id", localProductID,
			"upstream_tier_count", len(upProduct.WholesalePrices),
		)
		return nil
	}
	return s.products.QuickUpdate(
		strconv.FormatUint(uint64(localProductID), 10),
		map[string]interface{}{"wholesale_prices": wholesalePrices},
	)
}

// markUpstreamUnavailable 上游下架/删除时的统一处理
// status: mappingdomain.UpstreamStatusInactive(下架) / mappingdomain.UpstreamStatusDeleted(已删除)
//   - 本地 Product 下架（IsActive=false），不删除
//   - 所有 SKUMapping 标记为 UpstreamIsActive=false, UpstreamStock=0
//   - 所有本地 SKU 下架
//   - mapping.UpstreamStatus 写入对应状态
//   - status==deleted 时同时停用映射（IsActive=false），避免后续白白调上游
func (s *Service) markUpstreamUnavailable(mapping *mappingdomain.Mapping, status string, now time.Time) error {
	// 本地商品下架
	localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err == nil && localProduct != nil && localProduct.IsActive {
		localProduct.IsActive = false
		_ = s.products.Update(localProduct)
	}

	// SKU 映射 + 本地 SKU 下架
	skuMappings, _ := s.skuMappings.ListByProductMapping(mapping.ID)
	for i := range skuMappings {
		skuMappings[i].UpstreamIsActive = false
		skuMappings[i].UpstreamStock = 0
		skuMappings[i].StockSyncedAt = &now
		_ = s.skuMappings.Update(&skuMappings[i])

		localSKU, _ := s.skus.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil && localSKU.IsActive {
			localSKU.IsActive = false
			_ = s.skus.Update(localSKU)
		}
	}

	mapping.UpstreamStatus = status
	mapping.LastSyncedAt = &now
	if status == mappingdomain.UpstreamStatusDeleted {
		mapping.IsActive = false
	}
	if err := s.mappings.Update(mapping); err != nil {
		return err
	}

	logger.Infow("upstream_product_unavailable",
		"mapping_id", mapping.ID,
		"connection_id", mapping.ConnectionID,
		"upstream_product_id", mapping.UpstreamProductID,
		"local_product_id", mapping.LocalProductID,
		"status", status,
	)
	return nil
}

// SyncAllStock 同步所有活跃映射的库存（供定时任务调用）
// 使用 Redis 锁防止任务重叠执行，并发调用上游 API 提升吞吐量
func (s *Service) SyncAllStock(cfg settingsintegration.UpstreamSyncConfig) error {
	ctx := context.Background()
	const lockKey = "upstream:sync_stock_running"

	locked, err := cache.SetNX(ctx, lockKey, "1", 30*time.Minute)
	if err != nil {
		logger.Warnw("sync_stock_lock_error", "error", err)
		// Redis 不可用时降级为直接执行
	} else if !locked {
		logger.Debugw("sync_stock_skip_already_running")
		return nil
	}
	defer cache.Del(ctx, lockKey)

	// ProdSeller 的 /products 是完整只读目录，没有 Dujiao-Next 的分页映射前提；
	// 先按启用连接发现并幂等导入新商品，再处理旧协议的库存同步。
	if activeProvider, ok := s.connections.(interface {
		ListActive() ([]siteconnectiondomain.Connection, error)
	}); ok {
		activeConnections, listErr := activeProvider.ListActive()
		if listErr != nil {
			return listErr
		}
		for _, conn := range activeConnections {
			if conn.Protocol != constants.ConnectionProtocolProdSeller {
				continue
			}
			if err := s.SyncProdSellerCatalog(conn.ID); err != nil {
				logger.Warnw("sync_prodseller_catalog_failed", "connection_id", conn.ID, "error", err)
			}
		}
	}

	mappings, err := s.mappings.ListAllActive()
	if err != nil {
		return err
	}
	if len(mappings) == 0 {
		return nil
	}

	// ── 按连接分组 ──
	byConn := make(map[uint][]mappingdomain.Mapping)
	for _, m := range mappings {
		byConn[m.ConnectionID] = append(byConn[m.ConnectionID], m)
	}

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup

	// 每个连接并发处理，并发数由配置控制
	sem := make(chan struct{}, cfg.SyncConnConcurrency)

	for connID, connMappings := range byConn {
		wg.Add(1)
		sem <- struct{}{}
		go func(connID uint, connMappings []mappingdomain.Mapping) {
			defer wg.Done()
			defer func() { <-sem }()
			if len(connMappings) > 0 && connMappings[0].Connection != nil && connMappings[0].Connection.Protocol == constants.ConnectionProtocolProdSeller {
				return
			}
			if err := s.SyncConnectionStock(connID, connMappings, cfg.SyncPageSize, cfg.SyncMaxPages); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				logger.Warnw("sync_connection_stock_failed", "connection_id", connID, "error", err)
			}
		}(connID, connMappings)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// EnsureUpstreamStockForOrder 下单前对上游履约 SKU 进行库存兜底校验。
//
// 语义（失败优先安全开放，避免上游抖动导致全站不能下单）：
//   - 该 SKU 没有上游映射 → 视为非上游商品，返回 nil（不做任何事）
//   - 本地缓存 upstream_stock == -1 → 上游无限库存，返回 nil
//   - 本地缓存 upstream_stock >= requiredQty → 缓存充足，返回 nil
//   - 否则触发实时同步上游单品，再读缓存：
//     · 同步失败 → 返回 nil（容忍上游抖动，让缓存继续兜底）
//     · 同步后仍 < requiredQty → 返回 mappingcontract.ErrUpstreamStockInsufficient
//
// 后台 "pre_order_stock_check_enabled" 关闭时整个方法直接返回 nil。
func (s *Service) EnsureUpstreamStockForOrder(localSKUID uint, requiredQty int) error {
	if s == nil || s.skuMappings == nil || localSKUID == 0 || requiredQty <= 0 {
		return nil
	}

	// 后台总开关
	if s.settings != nil {
		cfg, err := s.settings.GetUpstreamSyncConfig("")
		if err == nil && !cfg.PreOrderStockCheckEnabled {
			return nil
		}
	}

	skuMapping, err := s.skuMappings.GetByLocalSKUID(localSKUID)
	if err != nil {
		// 查库失败不阻断下单
		logger.Warnw("preorder_stock_check_sku_mapping_lookup_failed", "local_sku_id", localSKUID, "error", err)
		return nil
	}
	if skuMapping == nil {
		// 没有上游映射 = 非上游商品
		return nil
	}
	if skuMapping.UpstreamStock < 0 {
		// 无限库存
		return nil
	}
	if skuMapping.UpstreamStock >= requiredQty {
		return nil
	}

	// 缓存库存不足 → 实时同步上游商品
	if syncErr := s.SyncProduct(skuMapping.ProductMappingID); syncErr != nil {
		// 上游抖动：fail-open，记录但不阻断下单
		logger.Warnw("preorder_stock_check_realtime_sync_failed",
			"local_sku_id", localSKUID,
			"product_mapping_id", skuMapping.ProductMappingID,
			"required_qty", requiredQty,
			"cached_stock", skuMapping.UpstreamStock,
			"error", syncErr,
		)
		return nil
	}

	// 重新读取最新缓存
	refreshed, err := s.skuMappings.GetByLocalSKUID(localSKUID)
	if err != nil || refreshed == nil {
		logger.Warnw("preorder_stock_check_refresh_failed", "local_sku_id", localSKUID, "error", err)
		return nil
	}
	if refreshed.UpstreamStock < 0 || refreshed.UpstreamStock >= requiredQty {
		return nil
	}
	return mappingcontract.ErrUpstreamStockInsufficient
}

// fullSyncIntervalFloor 强制全量同步的下限：任何情况下两次全量间隔至少 24h，
// 用于发现上游下架/删除（增量模式下这些商品不会再次出现在 updated_after 之后的列表里）。
const fullSyncIntervalFloor = 24 * time.Hour

// fullSyncIntervalSyncMultiplier 全量间隔相对增量同步间隔的倍数。
// 例如增量间隔=6h 时，全量间隔=max(24h, 6h*3)=24h；增量=12h 时，全量=max(24h, 36h)=36h。
const fullSyncIntervalSyncMultiplier = 3

// computeFullSyncInterval 计算当前强制全量同步间隔，跟随后台配置的同步间隔联动。
// 保证 ≥ 24h，避免用户把同步间隔调到 30h 时全量阈值（24h）反而每次都触发。
func (s *Service) computeFullSyncInterval() time.Duration {
	floor := fullSyncIntervalFloor
	if s == nil || s.settings == nil {
		return floor
	}
	syncInterval, err := s.settings.GetUpstreamSyncInterval("")
	if err != nil || syncInterval <= 0 {
		return floor
	}
	scaled := syncInterval * time.Duration(fullSyncIntervalSyncMultiplier)
	if scaled > floor {
		return scaled
	}
	return floor
}

// SyncConnectionStock 按连接批量同步：一次 ListProducts 拉取所有商品，内存匹配映射
func (s *Service) SyncConnectionStock(connectionID uint, connMappings []mappingdomain.Mapping, pageSize int, maxPages int) error {
	conn, err := s.connections.GetByID(connectionID)
	if err != nil || conn == nil {
		return fmt.Errorf("get connection %d: %w", connectionID, err)
	}

	adapter, err := s.connections.GetAdapter(conn)
	if err != nil {
		return fmt.Errorf("get adapter for connection %d: %w", connectionID, err)
	}

	// 读取上次同步时间用于增量同步
	syncCtx := context.Background()
	lastSyncKey := fmt.Sprintf("upstream:last_sync:%d", connectionID)
	lastFullSyncKey := fmt.Sprintf("upstream:last_full_sync:%d", connectionID)
	var updatedAfter *time.Time
	if lastSyncStr, err := cache.GetString(syncCtx, lastSyncKey); err == nil && lastSyncStr != "" {
		if t, err := time.Parse(time.RFC3339, lastSyncStr); err == nil {
			// 往前推 1 分钟作为安全窗口
			safeTime := t.Add(-1 * time.Minute)
			updatedAfter = &safeTime
		}
	}

	// 距离上次全量超过阈值则强制走全量，用于发现上游下架/删除。
	// 全量间隔跟随增量同步间隔联动（≥24h），避免用户把间隔调长后全量反而每次都触发。
	fullSyncInterval := s.computeFullSyncInterval()
	if updatedAfter != nil {
		if lastFullStr, err := cache.GetString(syncCtx, lastFullSyncKey); err == nil && lastFullStr != "" {
			if t, parseErr := time.Parse(time.RFC3339, lastFullStr); parseErr == nil {
				if time.Since(t) >= fullSyncInterval {
					logger.Infow("sync_force_full", "connection_id", connectionID, "last_full_sync", t, "full_sync_interval", fullSyncInterval)
					updatedAfter = nil
				}
			}
		} else {
			// 从未记录过全量时间 → 本次走全量
			updatedAfter = nil
		}
	}

	syncStartTime := time.Now()

	// 批量拉取上游商品（分页）。include_inactive=true 让上游连同已下架商品一起返回，
	// 下游凭此识别"上游已下架"和"上游已删除"两种状态。
	// fetchComplete 表示是否拉满了 result.Total —— 只有完整拉取才允许根据 missing 推断"上游已删除"，
	// 否则上游分页限流/截断/缓存抖动等会导致大量 mapping 被误标为 deleted。
	upstreamProducts := make(map[uint]upstream.UpstreamProduct)
	includesInactive := false
	fetchComplete := false
	expectedTotal := 0
	page := 1
	for {
		ctx, cancel := context.WithTimeout(syncCtx, 30*time.Second)
		result, err := adapter.ListProducts(ctx, upstream.ListProductsOpts{
			Page:            page,
			PageSize:        pageSize,
			UpdatedAfter:    updatedAfter,
			IncludeInactive: true,
		})
		cancel()
		if err != nil {
			// 增量拉取失败时回退到全量
			if updatedAfter != nil {
				logger.Warnw("sync_incremental_failed_fallback_full", "connection_id", connectionID, "error", err)
				updatedAfter = nil
				page = 1
				upstreamProducts = make(map[uint]upstream.UpstreamProduct)
				expectedTotal = 0
				continue
			}
			return fmt.Errorf("list upstream products page %d: %w", page, err)
		}

		// 上游回声字段：旧版上游不识别 include_inactive，会返回 false
		if page == 1 {
			includesInactive = result.IncludesInactive
			expectedTotal = result.Total
		}

		for _, p := range result.Items {
			upstreamProducts[p.ID] = p
		}

		if len(upstreamProducts) >= result.Total {
			fetchComplete = true
			break
		}
		if len(result.Items) == 0 {
			// 上游声称 total 还有但本页返回空 —— 视为分页异常截断，不允许进入删除判定
			logger.Warnw("sync_pagination_truncated",
				"connection_id", connectionID,
				"page", page,
				"fetched", len(upstreamProducts),
				"expected_total", result.Total,
			)
			break
		}
		page++
		if page > maxPages {
			logger.Warnw("sync_pagination_max_pages_reached",
				"connection_id", connectionID,
				"max_pages", maxPages,
				"fetched", len(upstreamProducts),
				"expected_total", expectedTotal,
			)
			break
		}
	}

	// 如果是增量同步且无更新，跳过
	if updatedAfter != nil && len(upstreamProducts) == 0 {
		logger.Debugw("sync_skip_no_updates", "connection_id", connectionID)
		// 仍然更新时间戳
		_ = cache.SetString(syncCtx, lastSyncKey, syncStartTime.Format(time.RFC3339), 48*time.Hour)
		return nil
	}

	// 对每个映射执行同步
	now := time.Now()
	isFullSync := updatedAfter == nil
	for i := range connMappings {
		mapping := &connMappings[i]
		upProduct, ok := upstreamProducts[mapping.UpstreamProductID]
		if !ok {
			if !isFullSync {
				// 增量模式下未返回说明没有变化，跳过
				continue
			}
			// 全量模式 + 上游真实支持 include_inactive + 本次拉取完整：
			// 下架商品也应在列表中，仍然 missing 即说明上游已软删除。
			if includesInactive && fetchComplete {
				_ = s.markUpstreamUnavailable(mapping, mappingdomain.UpstreamStatusDeleted, now)
				continue
			}
			// 拉取不完整（分页异常 / 翻页超限）或旧上游不支持 include_inactive：
			// 无法可靠判定"上游已删除"，仅打日志告警避免误下架。
			logger.Warnw("sync_upstream_product_missing_skipped",
				"connection_id", connectionID,
				"upstream_product_id", mapping.UpstreamProductID,
				"local_product_id", mapping.LocalProductID,
				"includes_inactive", includesInactive,
				"fetch_complete", fetchComplete,
			)
			continue
		}
		// 上游 is_active=false → 标记为 inactive
		if !upProduct.IsActive {
			_ = s.markUpstreamUnavailable(mapping, mappingdomain.UpstreamStatusInactive, now)
			continue
		}
		s.syncProductFromData(mapping, conn, &upProduct, &now)
	}

	// 记录本次同步时间
	_ = cache.SetString(syncCtx, lastSyncKey, syncStartTime.Format(time.RFC3339), 48*time.Hour)
	if isFullSync {
		_ = cache.SetString(syncCtx, lastFullSyncKey, syncStartTime.Format(time.RFC3339), 7*24*time.Hour)
	}

	logger.Infow("sync_connection_stock_done",
		"connection_id", connectionID,
		"mappings", len(connMappings),
		"upstream_fetched", len(upstreamProducts),
		"incremental", !isFullSync,
		"includes_inactive", includesInactive,
		"fetch_complete", fetchComplete,
	)
	return nil
}

// syncProductFromData 使用已拉取的上游数据同步单个映射（不再发 HTTP 请求）
// 调用方应保证 upProduct.IsActive == true（下架/删除分支由 caller 处理）
func (s *Service) syncProductFromData(mapping *mappingdomain.Mapping, conn *siteconnectiondomain.Connection, upProduct *upstream.UpstreamProduct, now *time.Time) {
	// ── 1. 同步本地商品字段 ──
	localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil || localProduct == nil {
		return
	}

	if upProduct.ManualFormSchema != nil {
		localProduct.ManualFormSchemaJSON = upProduct.ManualFormSchema
		_ = s.products.Update(localProduct)
	}

	// ── 2. 同步 SKU ──
	skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
	if err != nil {
		return
	}

	upstreamSKUMap := make(map[uint]upstream.UpstreamSKU, len(upProduct.SKUs))
	for _, us := range upProduct.SKUs {
		upstreamSKUMap[us.ID] = us
	}

	existingByUpstreamID := make(map[uint]*mappingdomain.SKUMapping, len(skuMappings))
	for i := range skuMappings {
		existingByUpstreamID[skuMappings[i].UpstreamSKUID] = &skuMappings[i]
	}

	// 更新已有映射
	for i := range skuMappings {
		upSKU, ok := upstreamSKUMap[skuMappings[i].UpstreamSKUID]
		if !ok {
			skuMappings[i].UpstreamIsActive = false
			skuMappings[i].UpstreamStock = 0
			skuMappings[i].StockSyncedAt = now
			_ = s.skuMappings.Update(&skuMappings[i])
			localSKU, _ := s.skus.GetByID(skuMappings[i].LocalSKUID)
			if localSKU != nil && localSKU.IsActive {
				localSKU.IsActive = false
				_ = s.skus.Update(localSKU)
			}
			continue
		}

		upPrice, priceErr := decimal.NewFromString(upSKU.PriceAmount)
		if priceErr != nil {
			logger.Warnw("sync_sku_price_parse_error",
				"upstream_sku_id", upSKU.ID,
				"price_amount", upSKU.PriceAmount,
				"error", priceErr,
			)
			// 仅同步库存状态，跳过价格更新
			skuMappings[i].UpstreamIsActive = upSKU.IsActive
			skuMappings[i].StockSyncedAt = now
			skuMappings[i].UpstreamStock = upSKU.StockQuantity
			_ = s.skuMappings.Update(&skuMappings[i])
			continue
		}
		skuMappings[i].UpstreamPrice = money.FromDecimal(upPrice.Round(2))
		skuMappings[i].UpstreamIsActive = upSKU.IsActive
		skuMappings[i].StockSyncedAt = now
		skuMappings[i].UpstreamStock = upSKU.StockQuantity
		_ = s.skuMappings.Update(&skuMappings[i])

		localSKU, _ := s.skus.GetByID(skuMappings[i].LocalSKUID)
		if localSKU != nil {
			localSKU.SpecValuesJSON = upSKU.SpecValues
			localSKU.IsActive = upSKU.IsActive
			if shouldAutoSyncPrice(mapping, conn) {
				newLocalPrice := priceForMapping(conn, mapping, upPrice, decimal.Zero)
				localSKU.PriceAmount = money.FromDecimal(newLocalPrice.Round(2))
				localSKU.CostPriceAmount = money.FromDecimal(convertCurrency(upPrice, conn.ExchangeRate).Round(2))
			}
			_ = s.skus.Update(localSKU)
		}
	}

	// 上游新增 SKU
	for _, upSKU := range upProduct.SKUs {
		if _, exists := existingByUpstreamID[upSKU.ID]; exists {
			continue
		}
		skuPrice, priceErr := decimal.NewFromString(upSKU.PriceAmount)
		if priceErr != nil {
			logger.Warnw("sync_new_sku_price_parse_error",
				"upstream_sku_id", upSKU.ID,
				"price_amount", upSKU.PriceAmount,
				"error", priceErr,
			)
			continue
		}
		localPrice := CalculateLocalPrice(skuPrice, conn.ExchangeRate, conn.PriceMarkupPercent, conn.PriceRoundingMode)
		newLocalSKU := productdomain.ProductSKU{
			ProductID:       mapping.LocalProductID,
			SKUCode:         upSKU.SKUCode,
			SpecValuesJSON:  upSKU.SpecValues,
			PriceAmount:     money.FromDecimal(localPrice.Round(2)),
			CostPriceAmount: money.FromDecimal(convertCurrency(skuPrice, conn.ExchangeRate).Round(2)),
			IsActive:        upSKU.IsActive,
			SortOrder:       0,
		}
		if err := s.skus.Create(&newLocalSKU); err != nil {
			continue
		}
		newSKUMapping := &mappingdomain.SKUMapping{
			ProductMappingID: mapping.ID,
			LocalSKUID:       newLocalSKU.ID,
			UpstreamSKUID:    upSKU.ID,
			UpstreamPrice:    money.FromDecimal(skuPrice.Round(2)),
			UpstreamIsActive: upSKU.IsActive,
			UpstreamStock:    upSKU.StockQuantity,
			StockSyncedAt:    now,
		}
		_ = s.skuMappings.Create(newSKUMapping)
	}

	// 同步价格
	if shouldAutoSyncPrice(mapping, conn) && localProduct != nil {
		s.recalcProductPrice(localProduct)
	}

	if len(upProduct.WholesalePrices) > 0 {
		if err := s.syncUpstreamWholesalePrices(mapping, localProduct.ID, conn, upProduct); err != nil {
			logger.Warnw("sync_upstream_wholesale_prices_failed",
				"mapping_id", mapping.ID,
				"connection_id", mapping.ConnectionID,
				"upstream_product_id", mapping.UpstreamProductID,
				"local_product_id", mapping.LocalProductID,
				"error", err,
			)
		}
	}

	// ── 3. 更新映射记录（同时把状态从 inactive/deleted 恢复为 active）──
	upFulfillment := upProduct.FulfillmentType
	if upFulfillment != constants.FulfillmentTypeAuto {
		upFulfillment = constants.FulfillmentTypeManual
	}
	mapping.UpstreamFulfillmentType = upFulfillment
	mapping.UpstreamStatus = mappingdomain.UpstreamStatusActive
	mapping.LastSyncedAt = now
	_ = s.mappings.Update(mapping)
}

var prodSellerSlugSafePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// ListProdSellerProducts 只读拉取 ProdSeller 商品，不写入本地数据库。
func (s *Service) ListProdSellerProducts(connectionID uint) (*upstream.ProdSellerProductListResult, error) {
	conn, err := s.connections.GetByID(connectionID)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, siteconnectioncontract.ErrNotFound
	}
	adapter, err := s.connections.GetAdapter(conn)
	if err != nil {
		return nil, err
	}
	external, ok := adapter.(upstream.ExternalCatalogAdapter)
	if !ok {
		return nil, fmt.Errorf("connection %d does not support external catalog", connectionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return external.ListExternalProducts(ctx)
}

// SyncProdSellerCatalog 拉取完整 ProdSeller 商品列表并幂等更新本地商品。
// ProdSeller API 没有 webhook，本方法由上游同步 scheduler 周期调用，也可由后台手动触发。
func (s *Service) SyncProdSellerCatalog(connectionID uint) error {
	conn, err := s.connections.GetByID(connectionID)
	if err != nil {
		return err
	}
	if conn == nil {
		return siteconnectioncontract.ErrNotFound
	}
	if conn.Protocol != constants.ConnectionProtocolProdSeller {
		return fmt.Errorf("connection %d is not a prodseller connection", connectionID)
	}
	adapter, err := s.connections.GetAdapter(conn)
	if err != nil {
		return err
	}
	external, ok := adapter.(upstream.ExternalCatalogAdapter)
	if !ok {
		return fmt.Errorf("connection %d does not support external catalog", connectionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result, err := external.ListExternalProducts(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("list prodseller products: %w", err)
	}
	if result == nil {
		return errors.New("prodseller returned an empty product list")
	}

	existing, err := s.mappings.ListByConnection(connectionID)
	if err != nil {
		return err
	}
	byExternalID := make(map[string]*mappingdomain.Mapping, len(existing))
	for i := range existing {
		if existing[i].UpstreamProductExternalID != nil && strings.TrimSpace(*existing[i].UpstreamProductExternalID) != "" {
			byExternalID[strings.TrimSpace(*existing[i].UpstreamProductExternalID)] = &existing[i]
		}
	}

	categoryID, err := s.ensureProdSellerCategory()
	if err != nil {
		return err
	}
	now := time.Now()
	seen := make(map[string]struct{}, len(result.Products))
	for _, product := range result.Products {
		externalID := strings.TrimSpace(product.ID)
		if externalID == "" {
			return errors.New("prodseller returned a product without an id")
		}
		seen[externalID] = struct{}{}
		if mapping := byExternalID[externalID]; mapping != nil {
			if err := s.syncExistingProdSellerProduct(mapping, conn, &product, now); err != nil {
				return fmt.Errorf("sync prodseller product %s: %w", externalID, err)
			}
			continue
		}
		created, err := s.createProdSellerProduct(connectionID, categoryID, conn, &product, now)
		if err != nil {
			return fmt.Errorf("create prodseller product %s: %w", externalID, err)
		}
		// 让同一批响应中的重复外部 ID 复用刚创建的映射，避免重复创建本地商品。
		byExternalID[externalID] = created
	}

	// 只有完整 GET /products 成功后，才把列表中消失的旧映射视为删除。
	for _, mapping := range existing {
		if mapping.UpstreamProductExternalID == nil {
			continue
		}
		externalID := strings.TrimSpace(*mapping.UpstreamProductExternalID)
		if externalID == "" {
			continue
		}
		if _, ok := seen[externalID]; ok {
			continue
		}
		if err := s.markUpstreamUnavailable(&mapping, mappingdomain.UpstreamStatusDeleted, now); err != nil {
			return fmt.Errorf("mark missing prodseller product %s: %w", externalID, err)
		}
	}
	return nil
}

func (s *Service) ensureProdSellerCategory() (uint, error) {
	if s.categories == nil {
		return 0, errors.New("category repository is unavailable")
	}
	category, err := s.categories.GetBySlugUnscoped("prodseller")
	if err != nil {
		return 0, err
	}
	if category != nil {
		if category.DeletedAt != nil {
			category.NameJSON = jsonmap.JSON{
				"zh-CN": "ProdSeller",
				"zh-TW": "ProdSeller",
				"en":    "ProdSeller",
			}
			category.IsActive = true
			if err := s.categories.Restore(category); err != nil {
				return 0, fmt.Errorf("restore prodseller category: %w", err)
			}
		}
		return category.ID, nil
	}
	if s.categoryCreator == nil {
		return 0, errors.New("category creator is unavailable")
	}
	category, err = s.categoryCreator.Create(categoryapp.UpsertInput{
		Slug: "prodseller",
		NameJSON: map[string]interface{}{
			"zh-CN": "ProdSeller",
			"zh-TW": "ProdSeller",
			"en":    "ProdSeller",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("create prodseller category: %w", err)
	}
	return category.ID, nil
}

func (s *Service) createProdSellerProduct(connectionID, categoryID uint, conn *siteconnectiondomain.Connection, source *upstream.ProdSellerProduct, now time.Time) (*mappingdomain.Mapping, error) {
	externalID := strings.TrimSpace(source.ID)
	externalIDRef := externalID
	sourcePrice := source.Price
	mapping := &mappingdomain.Mapping{
		ConnectionID:              connectionID,
		UpstreamProductID:         0,
		UpstreamProductExternalID: &externalIDRef,
		UpstreamFulfillmentType:   prodSellerFulfillmentType(source.Delivery.Type),
		UpstreamStatus:            mappingdomain.UpstreamStatusActive,
		UpstreamCurrency:          "USDT",
		UpstreamPublicPrice:       money.FromDecimal(source.PublicPrice.Round(2)),
		PriceMode:                 mappingdomain.PriceModeAuto,
		IsActive:                  true,
		LastSyncedAt:              &now,
	}
	localPrice := priceForMapping(conn, mapping, sourcePrice, decimal.Zero)
	slug := prodSellerSlug(connectionID, externalID)
	supported := prodSellerProductSupported(source)
	product := &productdomain.Product{
		CategoryID:       categoryID,
		Slug:             slug,
		TitleJSON:        prodSellerLocalizedText(source.Name),
		DescriptionJSON:  prodSellerLocalizedText(source.Description),
		ContentJSON:      prodSellerLocalizedText(source.Description),
		PriceAmount:      money.FromDecimal(localPrice),
		CostPriceAmount:  money.FromDecimal(convertCurrency(sourcePrice, conn.ExchangeRate).Round(2)),
		Images:           jsonslice.Strings(nonEmptySourceImage(source.ImageURL)),
		Tags:             jsonslice.Strings([]string{"prodseller"}),
		PurchaseType:     constants.ProductPurchaseMember,
		FulfillmentType:  constants.FulfillmentTypeUpstream,
		ManualStockTotal: 0,
		IsMapped:         true,
		IsActive:         false,
		SortOrder:        0,
	}
	if !supported {
		product.IsActive = false
	}

	if s.transactions == nil {
		return nil, errors.New("mapping transaction is unavailable")
	}
	if err := s.transactions.WithinTransaction(func(repos mappingcontract.ImportRepositories) error {
		if err := repos.Products.Create(product); err != nil {
			return fmt.Errorf("create local product: %w", err)
		}
		mapping.LocalProductID = product.ID
		localSKU := &productdomain.ProductSKU{
			ProductID:       product.ID,
			SKUCode:         upstream.DefaultExternalSKUCode,
			SpecValuesJSON:  jsonmap.JSON{},
			PriceAmount:     money.FromDecimal(localPrice),
			CostPriceAmount: money.FromDecimal(convertCurrency(sourcePrice, conn.ExchangeRate).Round(2)),
			IsActive:        supported && source.InStock,
			SortOrder:       0,
		}
		if err := repos.SKUs.Create(localSKU); err != nil {
			return fmt.Errorf("create local sku: %w", err)
		}
		if err := repos.Mappings.Create(mapping); err != nil {
			return fmt.Errorf("create product mapping: %w", err)
		}
		skuExternalID := externalID
		if err := repos.SKUMappings.Create(&mappingdomain.SKUMapping{
			ProductMappingID:      mapping.ID,
			LocalSKUID:            localSKU.ID,
			UpstreamSKUID:         0,
			UpstreamSKUExternalID: &skuExternalID,
			UpstreamPrice:         money.FromDecimal(sourcePrice.Round(2)),
			UpstreamStock:         prodSellerStock(source),
			UpstreamIsActive:      source.InStock,
			StockSyncedAt:         &now,
		}); err != nil {
			return fmt.Errorf("create sku mapping: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return mapping, nil
}

func (s *Service) syncExistingProdSellerProduct(mapping *mappingdomain.Mapping, conn *siteconnectiondomain.Connection, source *upstream.ProdSellerProduct, now time.Time) error {
	// v1.5.2 曾在创建本地商品后遗漏回填 LocalProductID，导致少量已落库映射指向 0。
	// 这些映射通常仍保留着 SKU 映射，因此先通过 SKU -> Product 关系做非破坏性修复，
	// 再继续正常同步；不删除历史映射，也不创建重复的上游商品映射。
	if mapping.LocalProductID == 0 {
		if err := s.repairProdSellerLocalProductID(mapping); err != nil {
			return err
		}
	}

	localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(mapping.LocalProductID), 10))
	if err != nil {
		return err
	}
	if localProduct == nil {
		return errors.New("local product not found")
	}

	skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
	if err != nil {
		return err
	}
	localSKUs, err := s.skus.ListByProduct(mapping.LocalProductID, false)
	if err != nil {
		return err
	}
	if len(localSKUs) == 0 {
		newSKU := &productdomain.ProductSKU{
			ProductID:      mapping.LocalProductID,
			SKUCode:        upstream.DefaultExternalSKUCode,
			SpecValuesJSON: jsonmap.JSON{},
			IsActive:       prodSellerProductSupported(source) && source.InStock,
			SortOrder:      0,
		}
		if err := s.skus.Create(newSKU); err != nil {
			return err
		}
		localSKUs = []productdomain.ProductSKU{*newSKU}
	}

	localSKU := &localSKUs[0]
	var skuMapping *mappingdomain.SKUMapping
	for i := range skuMappings {
		if skuMappings[i].UpstreamSKUExternalID != nil && *skuMappings[i].UpstreamSKUExternalID == source.ID {
			skuMapping = &skuMappings[i]
			break
		}
	}
	if skuMapping == nil && len(skuMappings) > 0 {
		skuMapping = &skuMappings[0]
	}

	if mapping.PriceMode == "" {
		mapping.PriceMode = mappingdomain.PriceModeAuto
	}
	if shouldAutoSyncPrice(mapping, conn) {
		localSKU.PriceAmount = money.FromDecimal(priceForMapping(conn, mapping, source.Price, decimal.Zero))
	}
	localSKU.CostPriceAmount = money.FromDecimal(convertCurrency(source.Price, conn.ExchangeRate).Round(2))
	localSKU.IsActive = prodSellerProductSupported(source) && source.InStock
	if err := s.skus.Update(localSKU); err != nil {
		return err
	}
	if skuMapping == nil {
		skuExternalID := strings.TrimSpace(source.ID)
		skuMapping = &mappingdomain.SKUMapping{ProductMappingID: mapping.ID, LocalSKUID: localSKU.ID, UpstreamSKUExternalID: &skuExternalID}
	}
	skuMapping.UpstreamSKUID = 0
	skuExternalID := strings.TrimSpace(source.ID)
	skuMapping.UpstreamSKUExternalID = &skuExternalID
	skuMapping.UpstreamPrice = money.FromDecimal(source.Price.Round(2))
	skuMapping.UpstreamStock = prodSellerStock(source)
	skuMapping.UpstreamIsActive = source.InStock
	skuMapping.StockSyncedAt = &now
	if skuMapping.ID == 0 {
		if err := s.skuMappings.Create(skuMapping); err != nil {
			return err
		}
	} else if err := s.skuMappings.Update(skuMapping); err != nil {
		return err
	}

	localProduct.TitleJSON = prodSellerLocalizedText(source.Name)
	localProduct.DescriptionJSON = prodSellerLocalizedText(source.Description)
	localProduct.ContentJSON = prodSellerLocalizedText(source.Description)
	localProduct.Images = jsonslice.Strings(nonEmptySourceImage(source.ImageURL))
	localProduct.IsMapped = true
	localProduct.FulfillmentType = constants.FulfillmentTypeUpstream
	if !prodSellerProductSupported(source) || !source.InStock {
		localProduct.IsActive = false
	}
	if shouldAutoSyncPrice(mapping, conn) {
		s.recalcProductPrice(localProduct)
	} else if err := s.products.Update(localProduct); err != nil {
		return err
	}

	mapping.UpstreamProductID = 0
	externalID := strings.TrimSpace(source.ID)
	mapping.UpstreamProductExternalID = &externalID
	mapping.UpstreamFulfillmentType = prodSellerFulfillmentType(source.Delivery.Type)
	mapping.UpstreamStatus = mappingdomain.UpstreamStatusActive
	mapping.UpstreamCurrency = "USDT"
	mapping.UpstreamPublicPrice = money.FromDecimal(source.PublicPrice.Round(2))
	mapping.IsActive = true
	mapping.LastSyncedAt = &now
	return s.mappings.Update(mapping)
}

// repairProdSellerLocalProductID 修复历史 ProdSeller 映射遗漏的本地商品 ID。
// 只沿已有的 SKU 映射回溯，不删除数据、不创建重复商品；无法安全回溯时明确报错，
// 让后台同步保持失败可见，避免静默生成无法追踪的商品。
func (s *Service) repairProdSellerLocalProductID(mapping *mappingdomain.Mapping) error {
	if mapping == nil || mapping.ID == 0 {
		return errors.New("invalid prodseller mapping")
	}
	if s.skuMappings == nil || s.skus == nil || s.products == nil || s.mappings == nil {
		return errors.New("prodseller mapping repair repositories are unavailable")
	}

	skuMappings, err := s.skuMappings.ListByProductMapping(mapping.ID)
	if err != nil {
		return fmt.Errorf("list prodseller SKU mappings for repair: %w", err)
	}
	for _, skuMapping := range skuMappings {
		if skuMapping.LocalSKUID == 0 {
			continue
		}
		localSKU, err := s.skus.GetByID(skuMapping.LocalSKUID)
		if err != nil {
			return fmt.Errorf("get prodseller SKU for repair: %w", err)
		}
		if localSKU == nil || localSKU.ProductID == 0 {
			continue
		}
		localProduct, err := s.products.GetByID(strconv.FormatUint(uint64(localSKU.ProductID), 10))
		if err != nil {
			return fmt.Errorf("get prodseller product for repair: %w", err)
		}
		if localProduct == nil {
			continue
		}

		mapping.LocalProductID = localProduct.ID
		if err := s.mappings.Update(mapping); err != nil {
			return fmt.Errorf("repair prodseller mapping local product: %w", err)
		}
		logger.Warnw("prodseller_mapping_local_product_repaired",
			"mapping_id", mapping.ID,
			"connection_id", mapping.ConnectionID,
			"local_product_id", mapping.LocalProductID,
		)
		return nil
	}

	return errors.New("prodseller mapping has no recoverable local product")
}

func prodSellerLocalizedText(text string) jsonmap.JSON {
	return jsonmap.JSON{"zh-CN": text, "zh-TW": text, "en": text}
}

func prodSellerSlug(connectionID uint, externalID string) string {
	normalized := strings.ToLower(strings.TrimSpace(externalID))
	normalized = strings.NewReplacer("_", "-", "/", "-", " ", "-").Replace(normalized)
	if len(normalized) <= 70 && prodSellerSlugSafePattern.MatchString(normalized) {
		return fmt.Sprintf("prodseller-%d-%s", connectionID, normalized)
	}
	hash := sha256.Sum256([]byte(externalID))
	return fmt.Sprintf("prodseller-%d-%s", connectionID, hex.EncodeToString(hash[:8]))
}

func prodSellerFulfillmentType(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "instant") {
		return constants.FulfillmentTypeAuto
	}
	return constants.FulfillmentTypeManual
}

func prodSellerProductSupported(product *upstream.ProdSellerProduct) bool {
	return product != nil && strings.EqualFold(strings.TrimSpace(product.Delivery.Type), "instant") && !product.RequiresEmailActivation
}

func prodSellerStock(product *upstream.ProdSellerProduct) int {
	if product == nil || !product.InStock {
		return 0
	}
	if product.Stock == nil {
		return -1
	}
	return *product.Stock
}

func nonEmptySourceImage(imageURL string) []string {
	if strings.TrimSpace(imageURL) == "" {
		return nil
	}
	return []string{strings.TrimSpace(imageURL)}
}
