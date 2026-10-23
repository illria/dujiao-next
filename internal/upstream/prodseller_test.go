package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/shopspring/decimal"
)

func TestProdSellerReadOnlyEndpointsPreserveExternalIDAndNeverPOST(t *testing.T) {
	t.Setenv(ProdSellerAPIKeyEnv, "unit-test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != "unit-test-key" {
			t.Fatalf("X-API-Key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/balance":
			_, _ = w.Write([]byte(`{"telegramId":"123","username":"seller","balance":12.3456,"membership":"pro"}`))
		case "/v1/products":
			_, _ = w.Write([]byte(`{"products":[{"id":"66f1a9b4c7e8d90123456789","name":"Product","description":"Description","price":0.398,"publicPrice":0.5,"imageUrl":"https://cdn.example.test/p.png","delivery":{"type":"instant"},"sold":3133,"inStock":true,"requiresEmailActivation":false}]}`))
		case "/v1/products/66f1a9b4c7e8d90123456789":
			_, _ = w.Write([]byte(`{"id":"66f1a9b4c7e8d90123456789","name":"Product","description":"Description","price":0.398,"publicPrice":0.5,"delivery":{"type":"instant"},"inStock":true,"stock":7,"requiresEmailActivation":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := NewProdSellerAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL + "/v1", ApiKey: ProdSellerAPIKeyEnv})
	products, err := adapter.ListExternalProducts(context.Background())
	if err != nil {
		t.Fatalf("ListExternalProducts: %v", err)
	}
	if len(products.Products) != 1 || products.Products[0].ID != "66f1a9b4c7e8d90123456789" {
		t.Fatalf("unexpected products: %#v", products.Products)
	}
	if products.Products[0].Price.String() != "0.398" {
		t.Fatalf("price = %s", products.Products[0].Price)
	}
	if products.Products[0].InStock == nil || !*products.Products[0].InStock {
		t.Fatalf("expected list inStock=true, got %#v", products.Products[0].InStock)
	}
	if products.Products[0].Stock != nil {
		t.Fatalf("list endpoint should not be required to expose exact stock, got %v", *products.Products[0].Stock)
	}
	balance, err := adapter.GetExternalBalance(context.Background())
	if err != nil {
		t.Fatalf("GetExternalBalance: %v", err)
	}
	if !balance.Balance.Equal(decimal.RequireFromString("12.3456")) {
		t.Fatalf("balance = %s", balance.Balance)
	}
	product, err := adapter.GetExternalProduct(context.Background(), products.Products[0].ID)
	if err != nil || product == nil {
		t.Fatalf("GetExternalProduct: %v", err)
	}
	if product.Stock == nil || *product.Stock != 7 {
		t.Fatalf("expected detail stock=7, got %#v", product.Stock)
	}
	if product.IsAvailable() != true || product.StockQuantity() != 7 {
		t.Fatalf("expected detail product to be available with stock 7, got available=%v stock=%d", product.IsAvailable(), product.StockQuantity())
	}
}

func TestProdSellerProductAvailabilityResolution(t *testing.T) {
	trueValue := true
	falseValue := false
	stock := 47
	zero := 0
	for _, test := range []struct {
		name      string
		product   ProdSellerProduct
		available bool
		quantity  int
	}{
		{name: "explicit in stock", product: ProdSellerProduct{InStock: &trueValue}, available: true, quantity: -1},
		{name: "detail quantity overrides list flag", product: ProdSellerProduct{InStock: &falseValue, Stock: &stock}, available: true, quantity: 47},
		{name: "detail quantity", product: ProdSellerProduct{Stock: &stock}, available: true, quantity: 47},
		{name: "detail zero", product: ProdSellerProduct{Stock: &zero}, available: false, quantity: 0},
	}{
		t.Run(test.name, func(t *testing.T) {
			if got := test.product.IsAvailable(); got != test.available {
				t.Fatalf("IsAvailable() = %v, want %v", got, test.available)
			}
			if got := test.product.StockQuantity(); got != test.quantity {
				t.Fatalf("StockQuantity() = %d, want %d", got, test.quantity)
			}
		})
	}
}

func TestProdSellerErrorsAreNormalizedWithoutResponseBody(t *testing.T) {
	t.Setenv(ProdSellerAPIKeyEnv, "unit-test-key")
	for _, test := range []struct {
		status    int
		wantRetry bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusPaymentRequired, false},
		{http.StatusConflict, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	} {
		t.Run(fmt.Sprintf("status_%d", test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "9")
					w.Header().Set("X-RateLimit-Limit", "300")
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("X-RateLimit-Reset", "1700000000")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{"secret":"must not be surfaced"}`))
			}))
			defer server.Close()
			adapter := NewProdSellerAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL, ApiKey: ProdSellerAPIKeyEnv})
			_, err := adapter.GetExternalBalance(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *ProdSellerAPIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error type = %T, want *ProdSellerAPIError", err)
			}
			if apiErr.StatusCode != test.status || apiErr.Retryable() != test.wantRetry {
				t.Fatalf("api error = %#v", apiErr)
			}
			if strings.Contains(err.Error(), "must not be surfaced") || strings.Contains(err.Error(), "unit-test-key") {
				t.Fatalf("error leaked sensitive data: %v", err)
			}
			if test.status == http.StatusTooManyRequests {
				if apiErr.RetryAfter != 9*time.Second || apiErr.RateLimit != 300 || apiErr.RateRemain != 0 {
					t.Fatalf("rate limit metadata = %#v", apiErr)
				}
				if !errors.Is(err, ErrProdSellerRetryable) {
					t.Fatal("429 should be retryable")
				}
			}
		})
	}
}

func TestProdSellerTimeoutIsRetryable(t *testing.T) {
	t.Setenv(ProdSellerAPIKeyEnv, "unit-test-key")
	adapter := NewProdSellerAdapter(&siteconnectiondomain.Connection{BaseURL: "http://127.0.0.1:1", ApiKey: ProdSellerAPIKeyEnv})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := adapter.GetExternalBalance(ctx)
	if err == nil || !errors.Is(err, ErrProdSellerRetryable) {
		t.Fatalf("timeout error = %v, want retryable", err)
	}
}
