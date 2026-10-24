package application

import (
	"testing"

	"github.com/dujiao-next/internal/constants"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/upstream"
)

func TestProdSellerLocalizedTextUsesSupportedLocaleKeys(t *testing.T) {
	got := prodSellerLocalizedText("Upstream name")
	if got[constants.LocaleZhCN] != "Upstream name" || got[constants.LocaleZhTW] != "Upstream name" || got[constants.LocaleEnUS] != "Upstream name" {
		t.Fatalf("unexpected localized product text: %#v", got)
	}
	if _, exists := got["en"]; exists {
		t.Fatalf("legacy en key must not be emitted: %#v", got)
	}
}
func TestPreserveProdSellerLocalizedTextKeepsManualContentAndMigratesLegacyEnglish(t *testing.T) {
	existing := jsonmap.JSON{
		constants.LocaleZhCN: "中文标题",
		"en":                 "English title",
	}

	got := preserveProdSellerLocalizedText(existing, "New upstream title")
	if got[constants.LocaleZhCN] != "中文标题" {
		t.Fatalf("manual zh-CN content was overwritten: %#v", got)
	}
	if got[constants.LocaleZhTW] != "New upstream title" {
		t.Fatalf("missing zh-TW content was not filled: %#v", got)
	}
	if got[constants.LocaleEnUS] != "English title" {
		t.Fatalf("legacy English content was not migrated: %#v", got)
	}
	if _, exists := got["en"]; exists {
		t.Fatalf("legacy en key should be removed: %#v", got)
	}
}

func TestPreserveProdSellerLocalizedTextDoesNotMutateExistingMap(t *testing.T) {
	existing := jsonmap.JSON{constants.LocaleZhCN: "中文标题"}
	_ = preserveProdSellerLocalizedText(existing, "Upstream title")
	if _, exists := existing[constants.LocaleZhTW]; exists {
		t.Fatalf("existing map was mutated with zh-TW")
	}
	if _, exists := existing[constants.LocaleEnUS]; exists {
		t.Fatalf("existing map was mutated with en-US")
	}
}

func TestProdSellerTextHashIsStableForWhitespaceAndChangesForContent(t *testing.T) {
	if prodSellerTextHash("  upstream title  ") != prodSellerTextHash("upstream title") {
		t.Fatal("source hash should normalize surrounding whitespace")
	}
	if prodSellerTextHash("upstream title") == prodSellerTextHash("changed title") {
		t.Fatal("different source text must have different hashes")
	}
}

func TestSyncProdSellerLocalizedTextUpdatesChineseOnlyAfterTranslation(t *testing.T) {
	existing := jsonmap.JSON{
		constants.LocaleZhCN: "旧中文",
		constants.LocaleZhTW: "舊中文",
		constants.LocaleEnUS: "旧英文",
	}

	translated := syncProdSellerLocalizedText(existing, "New upstream title", "新的中文", "新的繁中", true)
	if translated[constants.LocaleZhCN] != "新的中文" || translated[constants.LocaleZhTW] != "新的繁中" {
		t.Fatalf("translated Chinese content was not updated: %#v", translated)
	}
	if translated[constants.LocaleEnUS] != "New upstream title" {
		t.Fatalf("English source was not updated: %#v", translated)
	}

	failed := syncProdSellerLocalizedText(existing, "Another upstream title", "", "", false)
	if failed[constants.LocaleZhCN] != "旧中文" || failed[constants.LocaleZhTW] != "舊中文" {
		t.Fatalf("failed translation must keep prior Chinese content: %#v", failed)
	}
	if failed[constants.LocaleEnUS] != "Another upstream title" {
		t.Fatalf("failed translation must still update English source: %#v", failed)
	}
}

func TestNeedsProdSellerTranslationDetectsSourceChangeAndMissingLocale(t *testing.T) {
	stock := true
	source := &upstream.ProdSellerProduct{Name: "Title", Description: "Description", InStock: &stock}
	product := &productdomain.Product{TitleJSON: jsonmap.JSON{constants.LocaleZhCN: "中文", constants.LocaleZhTW: "繁中"}, DescriptionJSON: jsonmap.JSON{constants.LocaleZhCN: "说明", constants.LocaleZhTW: "說明"}}
	mapping := &mappingdomain.Mapping{UpstreamNameHash: prodSellerTextHash(source.Name), UpstreamDescriptionHash: prodSellerTextHash(source.Description)}
	if needsProdSellerTranslation(mapping, product, source) {
		t.Fatal("unchanged source with complete Chinese content should not translate again")
	}
	source.Description = "Changed description"
	if !needsProdSellerTranslation(mapping, product, source) {
		t.Fatal("changed upstream description must trigger translation")
	}

	source.Description = "Description"
	product.TitleJSON[constants.LocaleZhTW] = ""
	if !needsProdSellerTranslation(mapping, product, source) {
		t.Fatal("missing Chinese locale must trigger translation")
	}
}
