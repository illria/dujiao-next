package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

func TestSMMPanelAdapterBalanceUsesReadOnlyAction(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	var gotAction string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotAction = r.Form.Get("action")
		if r.Form.Get("key") != "test-only-key" {
			t.Fatalf("expected test key in request form")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":"12.34","currency":"USD"}`))
	}))
	defer server.Close()

	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{
		BaseURL:   server.URL,
		SecretRef: constants.ConnectionSecretRefSMMPanel,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	balance, err := adapter.GetBalance(context.Background())
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if gotAction != "balance" {
		t.Fatalf("expected balance action, got %q", gotAction)
	}
	if balance.Balance != "12.34" || balance.Currency != "USD" {
		t.Fatalf("unexpected balance: %#v", balance)
	}
}

func TestSMMPanelAdapterRejectsNonCanonicalSecretReference(t *testing.T) {
	t.Setenv("OTHER_API_KEY", "test-only-key")
	_, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{SecretRef: "OTHER_API_KEY"}, t.TempDir())
	if !errors.Is(err, ErrSMMPanelAPIKeyRefInvalid) {
		t.Fatalf("expected canonical secret reference error, got %v", err)
	}
}

func TestSMMPanelAdapterPingRejectsConfiguredCurrencyMismatch(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("action") != "balance" {
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":"12.34","currency":"USD"}`))
	}))
	defer server.Close()

	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{
		BaseURL:        server.URL,
		SourceCurrency: "CNY",
	}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.Ping(context.Background())
	if !errors.Is(err, ErrSMMPanelCurrencyMismatch) {
		t.Fatalf("expected currency mismatch, got %v", err)
	}
}

func TestSMMPanelAdapterServicesPreserveRateAndTypeMetadata(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("action") != "services" {
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
{"service":"36","name":"Telegram member","category":"Telegram","type":"Default","rate":"10.85","min":"500","max":"200000"},
{"service":"605","name":"Telegram member 90d","category":"Telegram","type":"Package","rate":"16.81","min":"1000","max":"100000"}
]`))
	}))
	defer server.Close()

	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.ListProducts(context.Background(), ListProductsOpts{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	if result.Total != 2 || len(result.Items) != 2 {
		t.Fatalf("unexpected result size: %#v", result)
	}
	first := result.Items[0]
	if first.ID != 36 || first.UpstreamRate != "10.85" || first.PricingUnit != constants.PricingUnitPer1000 {
		t.Fatalf("unexpected first service: %#v", first)
	}
	if !first.Orderable || first.MinQuantity != 500 || first.MaxQuantity != 200000 {
		t.Fatalf("default service metadata was not parsed: %#v", first)
	}
	fields, ok := first.ManualFormSchema["fields"].([]interface{})
	if !ok || len(fields) != 1 {
		t.Fatalf("expected Dujiao-Next manual form fields schema, got %#v", first.ManualFormSchema)
	}
	second := result.Items[1]
	if second.Orderable || second.UnsupportedReason == "" || second.ServiceType != "Package" {
		t.Fatalf("unsupported service metadata was not parsed: %#v", second)
	}
}

func TestSMMPanelAdapterServicesDeduplicateRepeatedIDs(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
{"service":"36","name":"first","type":"Default","rate":"10.85"},
{"service":"36","name":"duplicate","type":"Default","rate":"99.99"}
]`))
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.ListProducts(context.Background(), ListProductsOpts{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	if result.Total != 1 || len(result.Items) != 1 || result.Items[0].Title["zh-CN"] != "first" {
		t.Fatalf("duplicate service was not collapsed idempotently: %#v", result)
	}
}

func TestSMMPanelAdapterServicesPaginationIsLocal(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
{"service":"1","name":"one","type":"Default","rate":"0.398","min":"1","max":"1000000"},
{"service":"2","name":"two","type":"Default","rate":"1.00","min":"1","max":"1000000"},
{"service":"3","name":"three","type":"Default","rate":"2.00","min":"1","max":"1000000"}
]`))
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.ListProducts(context.Background(), ListProductsOpts{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	if calls != 1 || result.Total != 3 || len(result.Items) != 1 || result.Items[0].ID != 3 {
		t.Fatalf("unexpected local pagination: calls=%d result=%#v", calls, result)
	}
	if result.Items[0].UpstreamRate != "2.00" {
		t.Fatalf("rate was not preserved as text: %q", result.Items[0].UpstreamRate)
	}
}

func TestSMMPanelAdapterWriteIsDisabledWithoutCallingAdd(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("write operation unexpectedly reached upstream: %s", r.URL.Path)
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.CreateOrder(context.Background(), CreateUpstreamOrderReq{Quantity: 1})
	if !errors.Is(err, ErrSMMPanelReadOnly) {
		t.Fatalf("expected read-only error, got %v", err)
	}
}

func TestSMMPanelAdapterAddIsGatedAndUsesLatestPricePreflight(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	t.Setenv(smmPanelOrderingEnabledEnv, "true")
	addCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("action") {
		case "services":
			_, _ = w.Write([]byte(`[{"service":"36","name":"Telegram","type":"Default","rate":"10.85","min":"500","max":"200000"}]`))
		case "add":
			addCalled = true
			if r.Form.Get("link") != "https://example.com/post/1" || r.Form.Get("quantity") != "1000" {
				t.Fatalf("unexpected add fields: %v", r.Form)
			}
			_, _ = w.Write([]byte(`{"order":123,"status":"Pending","charge":"10.85"}`))
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	defer server.Close()

	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{
		BaseURL: server.URL, SourceCurrency: "USD", SourceToUSDT: "1",
	}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	result, err := adapter.CreateOrder(context.Background(), CreateUpstreamOrderReq{
		SKUID: 36, Quantity: 1000,
		ManualFormData: jsonmap.JSON{"link": "https://example.com/post/1"},
		SnapshotRate:   "10.85", SnapshotPricingUnit: constants.PricingUnitPer1000,
		SnapshotCost: "10.85", SnapshotSell: "21.70",
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if !result.OK || result.OrderID != 123 || !addCalled {
		t.Fatalf("unexpected add result: %#v, called=%v", result, addCalled)
	}
}

func TestSMMPanelAdapterRejectsInvalidLinkBeforeAdd(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	t.Setenv(smmPanelOrderingEnabledEnv, "true")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("invalid link should not reach upstream: %s", r.URL.Path)
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.CreateOrder(context.Background(), CreateUpstreamOrderReq{
		SKUID: 36, Quantity: 1000,
		ManualFormData: jsonmap.JSON{"link": "javascript:alert(1)"},
	})
	if !errors.Is(err, ErrSMMPanelLinkInvalid) || called {
		t.Fatalf("expected invalid link without request, err=%v called=%v", err, called)
	}
}

func TestSMMPanelAdapterStatusIsReadOnly(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("action") != "status" || r.Form.Get("order") != "123" {
			t.Fatalf("unexpected status request: %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"order":123,"status":"Completed","charge":"10.85","currency":"USD"}`))
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL, SourceCurrency: "USD"}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	detail, err := adapter.GetOrder(context.Background(), 123)
	if err != nil || detail.OrderID != 123 || detail.Status != "Completed" || detail.Fulfillment == nil {
		t.Fatalf("unexpected status result: detail=%#v err=%v", detail, err)
	}
}

func TestSMMPanelAdapterPreflightBlocksCostChangeAndUnprofitableOrder(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	t.Setenv(smmPanelOrderingEnabledEnv, "true")
	for _, test := range []struct {
		name         string
		snapshotCost string
		snapshotSell string
		wantCode     string
	}{
		{name: "cost changed", snapshotCost: "10.84", snapshotSell: "21.70", wantCode: "smm_cost_changed"},
		{name: "unprofitable", snapshotCost: "10.85", snapshotSell: "10.85", wantCode: "smm_unprofitable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			addCalled := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Fatalf("parse form: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.Form.Get("action") {
				case "services":
					_, _ = w.Write([]byte(`[{"service":"36","name":"Telegram","type":"Default","rate":"10.85","min":"500","max":"200000"}]`))
				case "add":
					addCalled = true
					_, _ = w.Write([]byte(`{"order":123}`))
				default:
					t.Fatalf("unexpected action %q", r.Form.Get("action"))
				}
			}))
			defer server.Close()

			adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{
				BaseURL: server.URL, SourceCurrency: "USD", SourceToUSDT: "1",
			}, t.TempDir())
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			result, err := adapter.CreateOrder(context.Background(), CreateUpstreamOrderReq{
				SKUID: 36, Quantity: 1000,
				ManualFormData: jsonmap.JSON{"link": "https://example.com/post/1"},
				SnapshotRate:   "10.85", SnapshotPricingUnit: constants.PricingUnitPer1000,
				SnapshotCost: test.snapshotCost, SnapshotSell: test.snapshotSell,
			})
			if err != nil || result == nil || result.OK || result.ErrorCode != test.wantCode || addCalled {
				t.Fatalf("preflight result=%#v err=%v add_called=%v", result, err, addCalled)
			}
		})
	}
}

func TestSMMPanelAdapterMalformedAddResponseIsUnknown(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	t.Setenv(smmPanelOrderingEnabledEnv, "true")
	addCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("action") {
		case "services":
			_, _ = w.Write([]byte(`[{"service":"36","name":"Telegram","type":"Default","rate":"10.85","min":"500","max":"200000"}]`))
		case "add":
			addCalls++
			_, _ = w.Write([]byte(`{"unexpected":true}`))
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{
		BaseURL: server.URL, SourceCurrency: "USD", SourceToUSDT: "1",
	}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	_, err = adapter.CreateOrder(context.Background(), CreateUpstreamOrderReq{
		SKUID: 36, Quantity: 1000,
		ManualFormData: jsonmap.JSON{"link": "https://example.com/post/1"},
		SnapshotRate:   "10.85", SnapshotPricingUnit: constants.PricingUnitPer1000,
		SnapshotCost: "10.85", SnapshotSell: "21.70",
	})
	if !errors.Is(err, ErrSMMPanelOrderUnknown) || addCalls != 1 {
		t.Fatalf("expected one unknown add outcome, err=%v add_calls=%d", err, addCalls)
	}
}

func TestSMMPanelAdapterNormalizesHTTPAndAPIErrors(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	for _, test := range []struct {
		name     string
		status   int
		body     string
		wantKind error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"bad key"}`, wantKind: ErrSMMPanelAuthentication},
		{name: "insufficient balance", status: http.StatusPaymentRequired, body: `{"error":"balance"}`, wantKind: ErrSMMPanelInsufficientBalance},
		{name: "conflict", status: http.StatusConflict, body: `{"error":"conflict"}`, wantKind: ErrSMMPanelConflict},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, wantKind: ErrSMMPanelRateLimited},
		{name: "server", status: http.StatusBadGateway, body: `{"error":"temporary"}`, wantKind: ErrSMMPanelRetryable},
		{name: "api error body", status: http.StatusOK, body: `{"error":"bad key"}`, wantKind: ErrSMMPanelAuthentication},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
			if err != nil {
				t.Fatalf("new adapter: %v", err)
			}
			_, err = adapter.GetBalance(context.Background())
			if !errors.Is(err, test.wantKind) {
				t.Fatalf("expected %v, got %v", test.wantKind, err)
			}
		})
	}
}

func TestSMMPanelAdapterTimeoutIsNormalized(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = adapter.GetBalance(ctx)
	if !errors.Is(err, ErrSMMPanelRetryable) {
		t.Fatalf("expected retryable timeout, got %v", err)
	}
}

func TestSMMPanelAdapterRequestUsesFormEncoding(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "test-only-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected content type %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse request body: %v", err)
		}
		if values.Get("action") != "balance" || values.Get("key") != "test-only-key" {
			t.Fatalf("unexpected form fields: %v", values)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance":"1","currency":"USD"}`))
	}))
	defer server.Close()
	adapter, err := NewSMMPanelAdapter(&siteconnectiondomain.Connection{BaseURL: server.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	if _, err := adapter.GetBalance(context.Background()); err != nil {
		t.Fatalf("get balance: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
