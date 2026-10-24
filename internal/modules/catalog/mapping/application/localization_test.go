package application

import (
	"testing"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/shared/jsonmap"
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
		"en":               "English title",
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
