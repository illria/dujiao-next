package upay

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/constants"
)

const testAppSecret = "unit-test-secret"

func testConfig(gatewayURL string) *Config {
	return &Config{
		GatewayURL:   gatewayURL,
		AppID:        "app-test",
		AppSecret:    testAppSecret,
		ChainType:    ChainTypeTRC20,
		FiatCurrency: "USD",
		NotifyURL:    "https://merchant.example/upay/callback",
		RedirectURL:  "https://merchant.example/pay",
	}
}

func TestSignSortsFieldsAndUsesUppercaseMD5(t *testing.T) {
	params := map[string]string{
		"notifyUrl":       "https://merchant.example/upay/callback",
		"fiatCurrency":    "USD",
		"merchantOrderNo": "MERCHANT-20261024001",
		"appId":           "app-test",
		"chainType":       ChainTypeTRC20,
		"fiatAmount":      "10.0000",
		"signature":       "ignored",
	}

	wantBytes := md5.Sum([]byte("appId=app-test&chainType=1&fiatAmount=10.0000&fiatCurrency=USD&merchantOrderNo=MERCHANT-20261024001&notifyUrl=https://merchant.example/upay/callback&appSecret=" + testAppSecret))
	want := strings.ToUpper(hex.EncodeToString(wantBytes[:]))
	if got := Sign(params, testAppSecret); got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
}

func TestSignKeepsEmptyValuesAndExcludesSignAliases(t *testing.T) {
	params := map[string]string{
		"empty":     "",
		"filled":    "value",
		"sign":      "ignored",
		"SIGNATURE": "ignored",
	}
	wantBytes := md5.Sum([]byte("empty=&filled=value&appSecret=" + testAppSecret))
	want := strings.ToUpper(hex.EncodeToString(wantBytes[:]))
	if got := Sign(params, testAppSecret); got != want {
		t.Fatalf("Sign() with empty value = %q, want %q", got, want)
	}
}

func TestParseConfigUsesEnvironmentCredentialsForEmptyConfig(t *testing.T) {
	t.Setenv("UPAY_APP_ID", "env-app")
	t.Setenv("UPAY_APP_SECRET", "env-secret")

	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig() error = %v", err)
	}
	if cfg.AppID != "env-app" || cfg.AppSecret != "env-secret" {
		t.Fatalf("environment credentials were not loaded: app_id=%q app_secret_set=%t", cfg.AppID, cfg.AppSecret != "")
	}
}

func TestCreatePaymentPostsUSDTRC20AndParsesCryptoSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != applyPath {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		var params map[string]string
		if err := json.Unmarshal(body, &params); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if params["fiatCurrency"] != "USD" || params["chainType"] != ChainTypeTRC20 || params["fiatAmount"] != "10.5000" {
			t.Fatalf("unexpected order params: %#v", params)
		}
		signedParams := map[string]string{
			"appId":           params["appId"],
			"merchantOrderNo": params["merchantOrderNo"],
			"chainType":       params["chainType"],
			"fiatAmount":      params["fiatAmount"],
			"fiatCurrency":    params["fiatCurrency"],
			"notifyUrl":       params["notifyUrl"],
		}
		if got := params["signature"]; got != Sign(signedParams, testAppSecret) {
			t.Fatalf("request signature = %q, want %q", got, Sign(signedParams, testAppSecret))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"appId":"app-test","orderNo":"UP-ORDER-1","merchantOrderNo":"MERCHANT-20261024001","exchangeRate":"1.2345","crypto":"8.5065","status":"0","payUrl":"https://pay.example/up/UP-ORDER-1"}}`))
	}))
	defer server.Close()

	result, err := CreatePayment(context.Background(), testConfig(server.URL), CreateInput{
		MerchantOrderNo: "MERCHANT-20261024001",
		FiatAmount:      "10.5",
		FiatCurrency:    "USD",
		ChainType:       ChainTypeTRC20,
		ProductName:     "Test product",
		Attach:          "test-attach",
		RedirectURL:     "https://merchant.example/return?payment=1",
	})
	if err != nil {
		t.Fatalf("CreatePayment() error = %v", err)
	}
	if result.OrderNo != "UP-ORDER-1" || result.Crypto != "8.5065" || result.ExchangeRate != "1.2345" {
		t.Fatalf("unexpected create result: %+v", result)
	}
	if result.PayURL != "https://pay.example/up/UP-ORDER-1" {
		t.Fatalf("PayURL = %q", result.PayURL)
	}
}

func TestCreatePaymentReturnsRequestErrorForUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"temporary"}`))
	}))
	defer server.Close()

	_, err := CreatePayment(context.Background(), testConfig(server.URL), CreateInput{
		MerchantOrderNo: "MERCHANT-20261024001",
		FiatAmount:      "1",
		FiatCurrency:    "USD",
		ChainType:       ChainTypeTRC20,
	})
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("CreatePayment() error = %v, want ErrRequestFailed", err)
	}
}

func TestVerifyCallbackAcceptsValidSignatureAndRejectsTampering(t *testing.T) {
	callback := map[string]string{
		"appId":           "app-test",
		"orderNo":         "UP-ORDER-1",
		"merchantOrderNo": "MERCHANT-20261024001",
		"chainType":       ChainTypeTRC20,
		"crypto":          "8.5065",
		"actualCrypto":    "8.5065",
		"poundage":        "0.0000",
		"actualPoundage":  "0.0000",
		"status":          StatusSuccess,
		"createdAt":       "1792838400",
		"completedAt":     "1792838460",
	}
	callback["signature"] = Sign(callback, testAppSecret)
	body, err := json.Marshal(callback)
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}

	parsed, err := ParseCallback(body)
	if err != nil {
		t.Fatalf("ParseCallback() error = %v", err)
	}
	if err := VerifyCallback(testConfig("https://api.example"), parsed); err != nil {
		t.Fatalf("VerifyCallback() error = %v", err)
	}

	parsed.Signature = strings.Repeat("0", 32)
	if !errors.Is(VerifyCallback(testConfig("https://api.example"), parsed), ErrSignatureInvalid) {
		t.Fatal("tampered callback should fail signature verification")
	}

	parsed.Signature = ""
	if !errors.Is(VerifyCallback(testConfig("https://api.example"), parsed), ErrSignatureInvalid) {
		t.Fatal("callback without signature should fail verification")
	}

	parsed.Signature = Sign(callback, testAppSecret)
	parsed.AppID = ""
	if !errors.Is(VerifyCallback(testConfig("https://api.example"), parsed), ErrResponseInvalid) {
		t.Fatal("callback without app id should fail identity verification")
	}
}

func TestVerifyCallbackRejectsMissingRequiredFields(t *testing.T) {
	fields := []string{
		"appId",
		"orderNo",
		"merchantOrderNo",
		"chainType",
		"crypto",
		"actualCrypto",
		"poundage",
		"actualPoundage",
		"status",
		"createdAt",
		"completedAt",
	}
	for _, missing := range fields {
		t.Run(missing, func(t *testing.T) {
			callback := map[string]string{
				"appId":           "app-test",
				"orderNo":         "UP-ORDER-1",
				"merchantOrderNo": "MERCHANT-20261024001",
				"chainType":       ChainTypeTRC20,
				"crypto":          "8.5065",
				"actualCrypto":    "8.5065",
				"poundage":        "0.0000",
				"actualPoundage":  "0.0000",
				"status":          StatusSuccess,
				"createdAt":       "1792838400",
				"completedAt":     "1792838460",
			}
			delete(callback, missing)
			callback["signature"] = Sign(callback, testAppSecret)
			body, err := json.Marshal(callback)
			if err != nil {
				t.Fatalf("marshal callback: %v", err)
			}
			parsed, err := ParseCallback(body)
			if err != nil {
				t.Fatalf("ParseCallback() error = %v", err)
			}
			if !errors.Is(VerifyCallback(testConfig("https://api.example"), parsed), ErrResponseInvalid) {
				t.Fatalf("callback missing %s should fail closed", missing)
			}
		})
	}
}

func TestQueryOrderFindsProviderOrderAndReturnsNilWhenNotVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != queryPath {
			t.Fatalf("unexpected query path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var params map[string]string
		if err := json.Unmarshal(body, &params); err != nil {
			t.Fatalf("decode query body: %v", err)
		}
		if params["signature"] != Sign(params, testAppSecret) {
			t.Fatalf("query signature mismatch")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total":1,"list":[{"appId":"app-test","orderNo":"UP-ORDER-1","merchantOrderNo":"MERCHANT-20261024001","chainType":"1","crypto":"8.5065","actualCrypto":"8.5065","status":"1","completedAt":"1792838460"}]}}`))
	}))
	defer server.Close()

	order, err := QueryOrder(context.Background(), testConfig(server.URL), "UP-ORDER-1")
	if err != nil {
		t.Fatalf("QueryOrder() error = %v", err)
	}
	if order == nil || order.ActualCrypto != "8.5065" || order.Status != StatusSuccess {
		t.Fatalf("unexpected queried order: %+v", order)
	}

	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"total":0,"list":[]}}`))
	}))
	defer emptyServer.Close()
	missing, err := QueryOrder(context.Background(), testConfig(emptyServer.URL), "UP-NOT-VISIBLE")
	if err != nil {
		t.Fatalf("QueryOrder() missing order error = %v", err)
	}
	if missing != nil {
		t.Fatalf("missing order = %+v, want nil", missing)
	}
}

func TestToPaymentStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   string
	}{
		{name: "pending", status: StatusPending, want: constants.PaymentStatusPending},
		{name: "success", status: StatusSuccess, want: constants.PaymentStatusSuccess},
		{name: "expired", status: StatusExpired, want: constants.PaymentStatusExpired},
		{name: "failed", status: StatusFailed, want: constants.PaymentStatusFailed},
		{name: "unknown", status: "9", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToPaymentStatus(tt.status); got != tt.want {
				t.Fatalf("ToPaymentStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}
