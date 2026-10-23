package upayadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	upay "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/upay"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

const adapterTestSecret = "adapter-unit-secret"

func adapterConfig(gatewayURL string) jsonmap.JSON {
	return jsonmap.JSON{
		"gateway_url":   gatewayURL,
		"app_id":        "app-test",
		"app_secret":    adapterTestSecret,
		"chain_type":    upay.ChainTypeTRC20,
		"fiat_currency": "USD",
		"notify_url":    "https://merchant.example/upay/callback",
		"redirect_url":  "https://merchant.example/pay",
	}
}

func TestUpayAdapterTypeAndConfig(t *testing.T) {
	a := NewUpayAdapter()
	if got, want := a.Type(), constants.PaymentProviderUpay+":"; got != want {
		t.Fatalf("Type() = %q, want %q", got, want)
	}
	if err := a.ValidateConfig(adapterConfig("https://api.example"), constants.PaymentChannelTypeUsdtTrc20); err != nil {
		t.Fatalf("ValidateConfig() error = %v", err)
	}
	if err := a.ValidateConfig(jsonmap.JSON{"chain_type": "2"}, constants.PaymentChannelTypeUsdtTrc20); !errors.Is(err, paymentcontract.ErrGatewayUnsupportedChannel) {
		t.Fatalf("ERC20 config error = %v, want unsupported channel", err)
	}
}

func TestUpayAdapterCreatePaymentKeepsFiatAndCryptoFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"appId":"app-test","orderNo":"UP-ORDER-1","merchantOrderNo":"MERCHANT-20261024001","exchangeRate":"1.2345","crypto":"8.5065","status":"0","payUrl":"https://pay.example/up/UP-ORDER-1"}}`))
	}))
	defer server.Close()

	result, err := NewUpayAdapter().CreatePayment(context.Background(), adapterConfig(server.URL), paymentcontract.GatewayCreateInput{
		OrderNo:     "MERCHANT-20261024001",
		Subject:     "Test product",
		Amount:      money.FromDecimal(decimal.RequireFromString("10.50")),
		Currency:    "USD",
		ChannelType: constants.PaymentChannelTypeUsdtTrc20,
		Extra:       jsonmap.JSON{"interaction_mode": constants.PaymentInteractionRedirect},
	})
	if err != nil {
		t.Fatalf("CreatePayment() error = %v", err)
	}
	if result.RedirectURL != "https://pay.example/up/UP-ORDER-1" || result.ProviderRef != "UP-ORDER-1" {
		t.Fatalf("unexpected redirect result: %+v", result)
	}
	if result.AmountSent != "10.5" || result.CurrencySent != "USD" {
		t.Fatalf("sent fiat facts = %s %s", result.AmountSent, result.CurrencySent)
	}
	if result.Payload[paymentcontract.GatewayPayloadUpayCryptoAmount] != "8.5065" || result.Payload[paymentcontract.GatewayPayloadUpayExchangeRate] != "1.2345" {
		t.Fatalf("UPay snapshot missing: %#v", result.Payload)
	}
}

func TestUpayAdapterVerifyCallbackRejectsBadSignatureAndMapsSuccess(t *testing.T) {
	callback := map[string]string{
		"appId":           "app-test",
		"orderNo":         "UP-ORDER-1",
		"merchantOrderNo": "MERCHANT-20261024001",
		"chainType":       upay.ChainTypeTRC20,
		"crypto":          "8.5065",
		"actualCrypto":    "8.5065",
		"poundage":        "0",
		"actualPoundage":  "0",
		"status":          upay.StatusSuccess,
		"createdAt":       "1792838400",
		"completedAt":     "1792838460",
	}
	callback["signature"] = upay.Sign(callback, adapterTestSecret)
	body, err := json.Marshal(callback)
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}

	verifier, ok := NewUpayAdapter().(paymentcontract.GatewayCallbackVerifier)
	if !ok {
		t.Fatal("UPay adapter does not expose callback verification")
	}
	result, err := verifier.VerifyCallback(adapterConfig("https://api.example"), nil, body)
	if err != nil {
		t.Fatalf("VerifyCallback() error = %v", err)
	}
	if result.OrderNo != "MERCHANT-20261024001" || result.ProviderRef != "UP-ORDER-1" || result.Status != constants.PaymentStatusSuccess {
		t.Fatalf("unexpected callback result: %+v", result)
	}
	if result.Payload[paymentcontract.GatewayPayloadUpayActualCrypto] != "8.5065" {
		t.Fatalf("actual crypto snapshot = %#v", result.Payload[paymentcontract.GatewayPayloadUpayActualCrypto])
	}

	callback["signature"] = strings.Repeat("0", 32)
	badBody, _ := json.Marshal(callback)
	if err := func() error {
		_, err := verifier.VerifyCallback(adapterConfig("https://api.example"), nil, badBody)
		return err
	}(); !errors.Is(err, paymentcontract.ErrGatewaySignatureInvalid) {
		t.Fatalf("bad callback error = %v, want signature error", err)
	}
}

func TestUpayAdapterQueryPaymentUsesActualCryptoAndNeverCreatesOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/open/order/queryall" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"total":1,"list":[{"appId":"app-test","orderNo":"UP-ORDER-1","merchantOrderNo":"MERCHANT-20261024001","chainType":"1","crypto":"8.5065","actualCrypto":"8.5065","status":"1","completedAt":"1792838460"}]}}`))
	}))
	defer server.Close()

	result, err := NewUpayAdapter().QueryPayment(context.Background(), adapterConfig(server.URL), "UP-ORDER-1")
	if err != nil {
		t.Fatalf("QueryPayment() error = %v", err)
	}
	if result.Status != constants.PaymentStatusSuccess || result.Currency != "USDT" {
		t.Fatalf("query result = %+v", result)
	}
	if result.Payload[paymentcontract.GatewayPayloadUpayActualCrypto] != "8.5065" {
		t.Fatalf("query actual crypto = %#v", result.Payload[paymentcontract.GatewayPayloadUpayActualCrypto])
	}
}
