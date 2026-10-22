package migrations

import (
	"testing"

	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
)

func TestAutoMigrateCreatesSMMCatalogColumnsIdempotently(t *testing.T) {
	db := setupRegistryMigrationTestDB(t)

	models := []interface{}{
		&siteconnectiondomain.Connection{},
		&mappingdomain.Mapping{},
		&mappingdomain.SKUMapping{},
	}
	for pass := 0; pass < 2; pass++ {
		if err := db.AutoMigrate(models...); err != nil {
			t.Fatalf("auto migrate pass %d failed: %v", pass+1, err)
		}
	}

	columns := []struct {
		model  interface{}
		column string
	}{
		{&siteconnectiondomain.Connection{}, "source_currency"},
		{&siteconnectiondomain.Connection{}, "source_to_usdt"},
		{&siteconnectiondomain.Connection{}, "exchange_rate_buffer_bps"},
		{&siteconnectiondomain.Connection{}, "markup_multiplier"},
		{&siteconnectiondomain.Connection{}, "minimum_order_amount_usdt"},
		{&siteconnectiondomain.Connection{}, "default_pricing_unit"},
		{&mappingdomain.Mapping{}, "pricing_mode"},
		{&mappingdomain.Mapping{}, "upstream_rate_text"},
		{&mappingdomain.Mapping{}, "pricing_unit"},
		{&mappingdomain.Mapping{}, "upstream_currency"},
		{&mappingdomain.Mapping{}, "upstream_service_type"},
		{&mappingdomain.Mapping{}, "upstream_service_category"},
		{&mappingdomain.Mapping{}, "min_quantity"},
		{&mappingdomain.Mapping{}, "max_quantity"},
		{&mappingdomain.Mapping{}, "effective_min_quantity"},
		{&mappingdomain.Mapping{}, "is_orderable"},
		{&mappingdomain.Mapping{}, "unsupported_reason"},
		{&mappingdomain.Mapping{}, "service_parameters_json"},
		{&mappingdomain.SKUMapping{}, "upstream_rate_text"},
		{&mappingdomain.SKUMapping{}, "pricing_unit"},
	}
	for _, item := range columns {
		if !db.Migrator().HasColumn(item.model, item.column) {
			t.Errorf("model %T is missing SMM column %s", item.model, item.column)
		}
	}
}
