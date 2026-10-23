package paymentcallbackhttp

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	"github.com/gin-gonic/gin"
)

type callbackTestService struct {
	called bool
}

func (s *callbackTestService) HandleSyncCallback(*paymentdomain.PaymentChannel, map[string][]string, []byte) (*paymentdomain.Payment, error) {
	s.called = true
	return nil, errors.New("signature invalid")
}

func (s *callbackTestService) HandleWechatWebhook(WechatWebhookInput) (*paymentdomain.Payment, string, error) {
	return nil, "", errors.New("not implemented")
}

type callbackTestPaymentLookup struct {
	payment *paymentdomain.Payment
}

func (l callbackTestPaymentLookup) GetByGatewayOrderNo(string) (*paymentdomain.Payment, error) {
	return l.payment, nil
}

func (l callbackTestPaymentLookup) GetLatestByProviderRef(string) (*paymentdomain.Payment, error) {
	return l.payment, nil
}

type callbackTestChannelLookup struct {
	channel *paymentdomain.PaymentChannel
}

func (l callbackTestChannelLookup) GetByID(uint) (*paymentdomain.PaymentChannel, error) {
	return l.channel, nil
}

func TestPaymentCallbackRejectsUnknownPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", strings.NewReader(`{"payment_id":1,"status":"success"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	(&Handler{}).PaymentCallback(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}
}

func TestUPayCallbackWithoutSignatureReturnsFailureInsteadOfSkippingVerification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &callbackTestService{}
	handler := NewHandler(
		service,
		callbackTestPaymentLookup{payment: &paymentdomain.Payment{ID: 1, ChannelID: 2, GatewayOrderNo: "MERCHANT-20261024001", ProviderRef: "UP-ORDER-1"}},
		callbackTestChannelLookup{channel: &paymentdomain.PaymentChannel{ID: 2, ProviderType: constants.PaymentProviderUpay}},
		nil,
	)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", strings.NewReader(`{"appId":"app-test","orderNo":"UP-ORDER-1","merchantOrderNo":"MERCHANT-20261024001"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.PaymentCallback(c)

	if !service.called {
		t.Fatal("UPay callback without signature did not reach verification service")
	}
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "FAIL" {
		t.Fatalf("missing signature response = %d %q, want 200 FAIL", w.Code, w.Body.String())
	}
}

func TestCallbackRateLimiterBlocksBurstAndResetsWindow(t *testing.T) {
	limiter := callbackRateLimiter{}
	now := time.Unix(1792838400, 0)
	for i := 0; i < callbackRateLimitCount; i++ {
		if !limiter.allow("198.51.100.10", now) {
			t.Fatalf("request %d was unexpectedly rate limited", i+1)
		}
	}
	if limiter.allow("198.51.100.10", now) {
		t.Fatal("request after the configured burst should be rate limited")
	}
	if !limiter.allow("198.51.100.10", now.Add(callbackRateLimitWindow)) {
		t.Fatal("rate limiter did not reset after the window")
	}
}

func TestPaymentCallbackRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", bytes.NewReader(bytes.Repeat([]byte("x"), maxCallbackBodyBytes+1)))

	(&Handler{}).PaymentCallback(c)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized callback status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestParseCallbackFormNormalizesNonStandardQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, target := range map[string]string{
		"semicolon separator":    "/api/v1/payments/callback?pid=2026;out_trade_no=ORDER-1;trade_status=TRADE_SUCCESS;sign=abc",
		"html escaped ampersand": "/api/v1/payments/callback?pid=2026&amp;out_trade_no=ORDER-1&amp;trade_status=TRADE_SUCCESS&amp;sign=abc",
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, target, nil)
			form, err := parseCallbackForm(c)
			if err != nil {
				t.Fatalf("parse callback form failed: %v", err)
			}
			for key, want := range map[string]string{"out_trade_no": "ORDER-1", "trade_status": "TRADE_SUCCESS", "sign": "abc"} {
				if got := getFirstValue(form, key); got != want {
					t.Fatalf("unexpected %s: %q", key, got)
				}
			}
		})
	}
}

func TestParseCallbackFormPrefersSignedPostForm(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback?channel_id=999",
		strings.NewReader("out_trade_no=ORDER-POST&trade_status=TRADE_SUCCESS&sign=abc&notify_id=n1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.Request = req

	form, err := parseCallbackForm(c)
	if err != nil {
		t.Fatalf("parse callback form failed: %v", err)
	}
	if got := getFirstValue(form, "out_trade_no"); got != "ORDER-POST" {
		t.Fatalf("unexpected out_trade_no: %s", got)
	}
	if got := getFirstValue(form, "channel_id"); got != "" {
		t.Fatalf("expected query param excluded from signed form, got %s", got)
	}
}

func TestWechatCallbackFeatureGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"id":"EV-1","resource":{"algorithm":"AEAD_AES_256_GCM"}}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", strings.NewReader(body))
	for key, value := range map[string]string{
		"Wechatpay-Signature": "mock-sign", "Wechatpay-Timestamp": "1760000000",
		"Wechatpay-Nonce": "mock-nonce", "Wechatpay-Serial": "mock-serial",
	} {
		c.Request.Header.Set(key, value)
	}
	if !isWechatCallbackRequest(c, []byte(body)) {
		t.Fatal("expected wechat callback request")
	}
	c.Request.Header.Del("Wechatpay-Signature")
	if isWechatCallbackRequest(c, []byte(body)) {
		t.Fatal("expected missing header to reject wechat callback")
	}
}

func TestEpusdtFeatureGuardFallsThroughWithoutPID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, body := range map[string]string{
		"bepusdt payload": `{"trade_id":"t1","order_id":"o1","status":2,"signature":"x"}`,
		"empty payload":   "",
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payments/callback", bytes.NewBufferString(body))
			if handled := (&Handler{}).handleEpusdtCallback(c); handled {
				t.Fatal("expected epusdt feature guard to fall through")
			}
		})
	}
}
