package application

import (
	"context"
	"testing"
	"time"

	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	settingsintegration "github.com/dujiao-next/internal/modules/settings/schema/integration"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/dujiao-next/internal/upstream"
	"github.com/shopspring/decimal"
)

// fakeSettingsProvider 以固定同步间隔实现 SettingsProvider。
type fakeSettingsProvider struct {
	interval time.Duration
}

type fakeProductTranslator struct {
	result mappingcontract.ProductTranslationResult
	calls  int
}

func (f *fakeProductTranslator) Translate(context.Context, mappingcontract.ProductTranslationRequest) (mappingcontract.ProductTranslationResult, error) {
	f.calls++
	return f.result, nil
}

type prodSellerDetailAdapter struct {
	details map[string]*upstream.ProdSellerProduct
}

func (f prodSellerDetailAdapter) ListExternalProducts(context.Context) (*upstream.ProdSellerProductListResult, error) {
	return nil, nil
}

func (f prodSellerDetailAdapter) GetExternalProduct(_ context.Context, productID string) (*upstream.ProdSellerProduct, error) {
	return f.details[productID], nil
}

func (f prodSellerDetailAdapter) GetExternalBalance(context.Context) (*upstream.ProdSellerBalance, error) {
	return nil, nil
}

func TestEnrichProdSellerProductsUsesExactDetailStock(t *testing.T) {
	listInStock := false
	stock := 47
	products, err := enrichProdSellerProducts(prodSellerDetailAdapter{
		details: map[string]*upstream.ProdSellerProduct{
			"product-1": {
				ID:    "product-1",
				Stock: &stock,
			},
		},
	}, []upstream.ProdSellerProduct{{ID: "product-1", InStock: &listInStock}})
	if err != nil {
		t.Fatalf("enrichProdSellerProducts returned error: %v", err)
	}
	if len(products) != 1 || products[0].Stock == nil || *products[0].Stock != 47 {
		t.Fatalf("expected exact detail stock=47, got %#v", products)
	}
	if !products[0].IsAvailable() {
		t.Fatal("expected positive detail stock to restore availability")
	}
}

func (f fakeSettingsProvider) GetUpstreamSyncConfig(fallbackInterval string) (settingsintegration.UpstreamSyncConfig, error) {
	cfg := settingsintegration.DefaultUpstreamSyncConfig()
	cfg.IntervalMinutes = int(f.interval / time.Minute)
	return cfg, nil
}

func (f fakeSettingsProvider) GetUpstreamSyncInterval(fallbackInterval string) (time.Duration, error) {
	return f.interval, nil
}

func TestComputeFullSyncIntervalFloorsAt24h(t *testing.T) {
	svc := &Service{settings: fakeSettingsProvider{interval: 5 * time.Minute}}

	// 5m 同步间隔 × 3 = 15m < 24h，期望落到 24h floor
	got := svc.computeFullSyncInterval()
	if got != fullSyncIntervalFloor {
		t.Fatalf("expected floor=24h, got %v", got)
	}
}

func TestComputeFullSyncIntervalScalesWithLongInterval(t *testing.T) {
	svc := &Service{settings: fakeSettingsProvider{interval: 12 * time.Hour}}

	// 12h * 3 = 36h，应使用 scaled 值
	got := svc.computeFullSyncInterval()
	want := 36 * time.Hour
	if got != want {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestComputeFullSyncIntervalWithoutSettings(t *testing.T) {
	svc := &Service{}
	got := svc.computeFullSyncInterval()
	if got != fullSyncIntervalFloor {
		t.Fatalf("expected floor when settings=nil, got %v", got)
	}
}

type captureImportProductRepository struct {
	product *productdomain.Product
}

func (r *captureImportProductRepository) Create(product *productdomain.Product) error {
	product.ID = 41
	r.product = product
	return nil
}

func (r *captureImportProductRepository) QuickUpdate(string, map[string]interface{}) error {
	return nil
}

type captureImportSKURepository struct{}

func (captureImportSKURepository) Create(sku *productdomain.ProductSKU) error {
	sku.ID = 51
	return nil
}

type captureImportMappingRepository struct {
	mapping *mappingdomain.Mapping
}

func (r *captureImportMappingRepository) Create(mapping *mappingdomain.Mapping) error {
	r.mapping = mapping
	return nil
}

type captureImportSKUMappingRepository struct{}

func (captureImportSKUMappingRepository) Create(*mappingdomain.SKUMapping) error { return nil }

type captureImportUnitOfWork struct {
	repositories mappingcontract.ImportRepositories
}

func (u captureImportUnitOfWork) WithinTransaction(fn func(mappingcontract.ImportRepositories) error) error {
	return fn(u.repositories)
}

func TestCreateProdSellerProductBindsMappingToCreatedProduct(t *testing.T) {
	products := &captureImportProductRepository{}
	mappings := &captureImportMappingRepository{}
	translator := &fakeProductTranslator{result: mappingcontract.ProductTranslationResult{
		TitleZhCN:       "中文标题",
		TitleZhTW:       "繁中標題",
		DescriptionZhCN: "中文描述",
		DescriptionZhTW: "繁中描述",
	}}
	svc := &Service{
		translator: translator,
		transactions: captureImportUnitOfWork{repositories: mappingcontract.ImportRepositories{
			Products:    products,
			SKUs:        captureImportSKURepository{},
			Mappings:    mappings,
			SKUMappings: captureImportSKUMappingRepository{},
		}},
	}
	conn := &siteconnectiondomain.Connection{
		ID:           7,
		ExchangeRate: decimal.NewFromInt(1),
	}
	inStock := true
	source := &upstream.ProdSellerProduct{
		ID:          "external-41",
		Name:        "Example",
		Description: "Description",
		Price:       decimal.NewFromFloat(1.25),
		PublicPrice: decimal.NewFromFloat(1.5),
		Delivery:    upstream.ProdSellerDelivery{Type: "instant"},
		InStock:     &inStock,
	}

	created, err := svc.createProdSellerProduct(7, 11, conn, source, time.Now())
	if err != nil {
		t.Fatalf("createProdSellerProduct returned error: %v", err)
	}
	if products.product == nil || products.product.ID != 41 {
		t.Fatalf("expected created product ID 41, got %#v", products.product)
	}
	if !products.product.IsActive {
		t.Fatal("expected an available instant product to be active after import")
	}
	if mappings.mapping == nil || mappings.mapping.LocalProductID != 41 {
		t.Fatalf("expected mapping to reference created product ID 41, got %#v", mappings.mapping)
	}
	if created.LocalProductID != 41 {
		t.Fatalf("expected returned mapping to reference created product ID 41, got %d", created.LocalProductID)
	}
	if got := created.Product; got != nil {
		t.Fatalf("expected no preloaded product relation in returned mapping, got %#v", got)
	}
	if translator.calls != 1 {
		t.Fatalf("expected one translation call, got %d", translator.calls)
	}
	if products.product.TitleJSON["zh-CN"] != "中文标题" || products.product.DescriptionJSON["zh-CN"] != "中文描述" {
		t.Fatalf("translated content was not stored: %#v", products.product)
	}
	if created.UpstreamNameHash == "" || created.UpstreamDescriptionHash == "" {
		t.Fatalf("successful translation must persist source hashes: %#v", created)
	}
}

type repairMappingRepository struct {
	updated *mappingdomain.Mapping
}

func (r *repairMappingRepository) GetByID(uint) (*mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) GetByLocalProductID(uint) (*mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) GetByConnectionAndUpstreamID(uint, uint) (*mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) GetByConnectionAndUpstreamExternalID(uint, string) (*mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) Create(*mappingdomain.Mapping) error { return nil }

func (r *repairMappingRepository) Update(mapping *mappingdomain.Mapping) error {
	r.updated = mapping
	return nil
}

func (r *repairMappingRepository) Delete(uint) error { return nil }

func (r *repairMappingRepository) DeleteByLocalProduct(uint) error { return nil }

func (r *repairMappingRepository) List(mappingcontract.ListFilter) ([]mappingdomain.Mapping, int64, error) {
	return nil, 0, nil
}

func (r *repairMappingRepository) ListByLocalProductIDs([]uint) ([]mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) ListActiveByConnection(uint) ([]mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) ListByConnection(uint) ([]mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) ListAllActive() ([]mappingdomain.Mapping, error) {
	return nil, nil
}

func (r *repairMappingRepository) ListUpstreamIDsByConnection(uint) ([]uint, error) {
	return nil, nil
}

type repairSKUMappingRepository struct {
	mappings []mappingdomain.SKUMapping
}

func (r *repairSKUMappingRepository) GetByLocalSKUID(uint) (*mappingdomain.SKUMapping, error) {
	return nil, nil
}

func (r *repairSKUMappingRepository) ListByProductMapping(uint) ([]mappingdomain.SKUMapping, error) {
	return r.mappings, nil
}

func (r *repairSKUMappingRepository) ListByProductMappingIDs([]uint) ([]mappingdomain.SKUMapping, error) {
	return nil, nil
}

func (r *repairSKUMappingRepository) Create(*mappingdomain.SKUMapping) error { return nil }

func (r *repairSKUMappingRepository) Update(*mappingdomain.SKUMapping) error { return nil }

func (r *repairSKUMappingRepository) DeleteByProductMapping(uint) error { return nil }

type repairProductRepository struct {
	product *productdomain.Product
}

func (r *repairProductRepository) GetByID(id string) (*productdomain.Product, error) {
	if r.product != nil && id == "23" {
		return r.product, nil
	}
	return nil, nil
}

func (r *repairProductRepository) Update(*productdomain.Product) error { return nil }

func (r *repairProductRepository) QuickUpdate(string, map[string]interface{}) error { return nil }

func (r *repairProductRepository) CountBySlug(string, *string) (int64, error) { return 0, nil }

type repairSKURepository struct {
	sku *productdomain.ProductSKU
}

func (r *repairSKURepository) GetByID(id uint) (*productdomain.ProductSKU, error) {
	if r.sku != nil && id == r.sku.ID {
		return r.sku, nil
	}
	return nil, nil
}

func (r *repairSKURepository) ListByProduct(uint, bool) ([]productdomain.ProductSKU, error) {
	return nil, nil
}

func (r *repairSKURepository) Create(*productdomain.ProductSKU) error { return nil }

func (r *repairSKURepository) Update(*productdomain.ProductSKU) error { return nil }

func TestRepairProdSellerLocalProductIDFromExistingSKUMapping(t *testing.T) {
	mappingRepo := &repairMappingRepository{}
	svc := &Service{
		mappings:    mappingRepo,
		skuMappings: &repairSKUMappingRepository{mappings: []mappingdomain.SKUMapping{{LocalSKUID: 9}}},
		skus:        &repairSKURepository{sku: &productdomain.ProductSKU{ID: 9, ProductID: 23}},
		products:    &repairProductRepository{product: &productdomain.Product{ID: 23}},
	}
	mapping := &mappingdomain.Mapping{ID: 5, ConnectionID: 7}

	if err := svc.repairProdSellerLocalProductID(mapping); err != nil {
		t.Fatalf("repairProdSellerLocalProductID returned error: %v", err)
	}
	if mapping.LocalProductID != 23 {
		t.Fatalf("expected recovered local product ID 23, got %d", mapping.LocalProductID)
	}
	if mappingRepo.updated != mapping {
		t.Fatalf("expected repaired mapping to be persisted")
	}
}

func TestRepairProdSellerLocalProductIDFailsClosedWithoutRecoverableSKU(t *testing.T) {
	svc := &Service{
		mappings:    &repairMappingRepository{},
		skuMappings: &repairSKUMappingRepository{},
		skus:        &repairSKURepository{},
		products:    &repairProductRepository{},
	}
	err := svc.repairProdSellerLocalProductID(&mappingdomain.Mapping{ID: 5})
	if err == nil {
		t.Fatalf("expected repair to fail closed, got %v", err)
	}
}
