package application

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/provider"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	notificationcontract "github.com/dujiao-next/internal/modules/notification/contract"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"
	"github.com/shopspring/decimal"
)

type upayCallbackGatewayStub struct {
	mu            sync.Mutex
	queryCalls    int
	callback      *paymentcontract.GatewayCallbackResult
	query         *paymentcontract.GatewayQueryResult
}

func (g *upayCallbackGatewayStub) Type() string { return constants.PaymentProviderUpay + ":test" }

func (g *upayCallbackGatewayStub) ValidateConfig(jsonmap.JSON, string) error { return nil }

func (g *upayCallbackGatewayStub) CreatePayment(context.Context, jsonmap.JSON, paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	return nil, errors.New("not implemented in callback test")
}

func (g *upayCallbackGatewayStub) QueryPayment(context.Context, jsonmap.JSON, string) (*paymentcontract.GatewayQueryResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queryCalls++
	if g.query == nil {
		return nil, nil
	}
	copy := *g.query
	return &copy, nil
}

func (g *upayCallbackGatewayStub) VerifyCallback(jsonmap.JSON, map[string][]string, []byte) (*paymentcontract.GatewayCallbackResult, error) {
	if g.callback == nil {
		return nil, errors.New("missing callback fixture")
	}
	copy := *g.callback
	return &copy, nil
}

type countingNotificationEnqueuer struct {
	mu    sync.Mutex
	count int
}

func (n *countingNotificationEnqueuer) Enqueue(notificationcontract.EnqueueInput) error {
	n.mu.Lock()
	n.count++
	n.mu.Unlock()
	return nil
}

func (n *countingNotificationEnqueuer) Count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.count
}

type countingProcurementCreator struct {
	mu    sync.Mutex
	count int
}

func (p *countingProcurementCreator) CreateForOrder(uint) error {
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	return nil
}

func (p *countingProcurementCreator) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

type upayCallbackQueueStub struct {
	paymentcontract.Queue
}

func (q *upayCallbackQueueStub) Enabled() bool { return true }

func (q *upayCallbackQueueStub) EnqueueStatusEmail(uint, string) error { return nil }

func (q *upayCallbackQueueStub) EnqueueOrderAutoFulfill(uint) error { return nil }

func upayFactsPayment() *paymentdomain.Payment {
	return &paymentdomain.Payment{
		ProviderType:   constants.PaymentProviderUpay,
		Currency:       "USD",
		Status:         constants.PaymentStatusPending,
		GatewayOrderNo: "MERCHANT-20261024001",
		ProviderPayload: jsonmap.JSON{
			paymentcontract.GatewayPayloadUpayFiatAmount:     "10.50",
			paymentcontract.GatewayPayloadUpayFiatCurrency:   "USD",
			paymentcontract.GatewayPayloadUpayCryptoAmount:    "8.5065",
			paymentcontract.GatewayPayloadUpayProviderCrypto: "8.5065",
			paymentcontract.GatewayPayloadUpayExchangeRate:    "1.2345",
			paymentcontract.GatewayPayloadUpayCryptoCurrency: "USDT",
		},
	}
}

func TestPaymentCoveredOrderAmountUsesUPayFiatSnapshot(t *testing.T) {
	payment := upayFactsPayment()
	covered := paymentCoveredOrderAmount(payment, decimal.RequireFromString("8.5065"))
	if !covered.Equal(decimal.RequireFromString("10.50")) {
		t.Fatalf("UPay covered order amount = %s, want 10.50", covered)
	}
}

func TestValidatePaymentCurrencyForChannelRequiresUSDForUPay(t *testing.T) {
	channel := &paymentdomain.PaymentChannel{ProviderType: constants.PaymentProviderUpay}
	if err := validatePaymentCurrencyForChannel("CNY", channel); !errors.Is(err, ErrPaymentCurrencyMismatch) {
		t.Fatalf("CNY UPay currency error = %v, want currency mismatch", err)
	}
	if err := validatePaymentCurrencyForChannel("USD", channel); err != nil {
		t.Fatalf("USD UPay currency rejected: %v", err)
	}
}

func upayCallbackInput(status, actualCrypto, currency string) PaymentCallbackInput {
	return PaymentCallbackInput{
		OrderNo:  "MERCHANT-20261024001",
		Status:   status,
		Currency: currency,
		Amount:   money.FromDecimal(decimal.RequireFromString("10.50")),
		Payload: jsonmap.JSON{
			paymentcontract.GatewayPayloadUpayActualCrypto: actualCrypto,
			paymentcontract.GatewayPayloadUpayProviderCrypto: actualCrypto,
		},
	}
}

func TestValidateUpayCallbackFactsRequiresExactCryptoAmount(t *testing.T) {
	payment := upayFactsPayment()
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusSuccess, upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USDT")); err != nil {
		t.Fatalf("exact UPay callback rejected: %v", err)
	}

	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusSuccess, upayCallbackInput(constants.PaymentStatusSuccess, "8.5064", "USDT")); !errors.Is(err, ErrPaymentAmountMismatch) {
		t.Fatalf("underpaid callback error = %v, want amount mismatch", err)
	}
	providerMismatch := upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USDT")
	providerMismatch.Payload[paymentcontract.GatewayPayloadUpayProviderCrypto] = "8.5064"
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusSuccess, providerMismatch); !errors.Is(err, ErrPaymentAmountMismatch) {
		t.Fatalf("provider crypto mismatch error = %v, want amount mismatch", err)
	}
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusSuccess, upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USD")); !errors.Is(err, ErrPaymentCurrencyMismatch) {
		t.Fatalf("currency mismatch error = %v, want currency mismatch", err)
	}
}

func TestValidateUpayCallbackFactsKeepsExpiredAndPendingOutOfPaidPath(t *testing.T) {
	payment := upayFactsPayment()
	pending := upayCallbackInput(constants.PaymentStatusPending, "", "USDT")
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusPending, pending); err != nil {
		t.Fatalf("pending callback rejected: %v", err)
	}

	expired := upayCallbackInput(constants.PaymentStatusExpired, "", "USDT")
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusExpired, expired); !errors.Is(err, ErrPaymentManualReview) {
		t.Fatalf("expired callback error = %v, want manual review", err)
	}

	payment.Status = constants.PaymentStatusExpired
	if err := validateCallbackPaymentFacts(payment, payment.GatewayOrderNo, constants.PaymentStatusSuccess, upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USDT")); !errors.Is(err, ErrPaymentManualReview) {
		t.Fatalf("success after local expiry error = %v, want manual review", err)
	}
}

func TestUpayPaymentCallbackReplayIsIdempotent(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	channel := &paymentdomain.PaymentChannel{
		Name: "UPay test",
		ProviderType: constants.PaymentProviderUpay,
		ChannelType: constants.PaymentChannelTypeUsdtTrc20,
		InteractionMode: constants.PaymentInteractionRedirect,
		ConfigJSON: jsonmap.JSON{},
		IsActive: true,
		FeeRate: money.FromDecimal(decimal.Zero),
		FixedFee: money.FromDecimal(decimal.Zero),
		CreatedAt: testNow(),
		UpdatedAt: testNow(),
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	order := &orderdomain.Order{
		OrderNo: "DJUPAYTEST001",
		Status: constants.OrderStatusPendingPayment,
		Currency: "USD",
		OriginalAmount: money.FromDecimal(decimal.RequireFromString("10.50")),
		TotalAmount: money.FromDecimal(decimal.RequireFromString("10.50")),
		OnlinePaidAmount: money.FromDecimal(decimal.RequireFromString("10.50")),
		CreatedAt: testNow(),
		UpdatedAt: testNow(),
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	payment := upayFactsPayment()
	payment.OrderID = order.ID
	payment.ChannelID = channel.ID
	payment.ChannelType = channel.ChannelType
	payment.InteractionMode = channel.InteractionMode
	payment.Amount = money.FromDecimal(decimal.RequireFromString("10.50"))
	payment.Status = constants.PaymentStatusPending
	payment.CreatedAt = testNow()
	payment.UpdatedAt = testNow()
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}

	input := upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USDT")
	input.PaymentID = payment.ID
	input.ChannelID = channel.ID
	input.ProviderRef = "UP-ORDER-1"
	expiredInput := upayCallbackInput(constants.PaymentStatusExpired, "", "USDT")
	expiredInput.PaymentID = payment.ID
	expiredInput.ChannelID = channel.ID
	expiredInput.ProviderRef = "UP-ORDER-1"
	if err := svc.HandleCallback(expiredInput); !errors.Is(err, ErrPaymentManualReview) {
		t.Fatalf("expired UPay callback error = %v, want manual review", err)
	}
	var pendingPayment paymentdomain.Payment
	if err := db.First(&pendingPayment, payment.ID).Error; err != nil {
		t.Fatalf("reload payment after expired callback: %v", err)
	}
	if pendingPayment.Status != constants.PaymentStatusPending {
		t.Fatalf("expired callback changed payment status to %s", pendingPayment.Status)
	}
	first, err := svc.HandleCallback(input)
	if err != nil {
		t.Fatalf("first UPay callback: %v", err)
	}
	if first.Status != constants.PaymentStatusSuccess {
		t.Fatalf("first callback status = %s", first.Status)
	}
	if _, err := svc.HandleCallback(input); err != nil {
		t.Fatalf("replayed UPay callback: %v", err)
	}
	var count int64
	if err := db.Model(&paymentdomain.Payment{}).Where("id = ? AND status = ?", payment.ID, constants.PaymentStatusSuccess).Count(&count).Error; err != nil {
		t.Fatalf("count payment: %v", err)
	}
	if count != 1 {
		t.Fatalf("successful payment rows = %d, want 1", count)
	}
}

func TestHandleSyncCallbackQueriesUPayBeforeMarkingPaid(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	registry, ok := svc.paymentProviderRegistry.(*provider.Registry)
	if !ok {
		t.Fatal("payment provider registry is not the test registry")
	}
	stub := &upayCallbackGatewayStub{
		callback: &paymentcontract.GatewayCallbackResult{
			OrderNo:     "MERCHANT-20261024002",
			ProviderRef: "UP-ORDER-2",
			Status:      constants.PaymentStatusSuccess,
			Amount:      money.FromDecimal(decimal.RequireFromString("999.99")),
			Currency:    "USDT",
			Payload: jsonmap.JSON{
				paymentcontract.GatewayPayloadUpayActualCrypto:    "0.0001",
				paymentcontract.GatewayPayloadUpayProviderCrypto: "0.0001",
			},
		},
		query: &paymentcontract.GatewayQueryResult{
			ProviderRef: "UP-ORDER-2",
			Status:      constants.PaymentStatusSuccess,
			Amount:      money.FromDecimal(decimal.RequireFromString("10.50")),
			Currency:    "USDT",
			Payload: jsonmap.JSON{
				paymentcontract.GatewayPayloadUpayActualCrypto:    "8.5065",
				paymentcontract.GatewayPayloadUpayProviderCrypto: "8.5065",
			},
		},
	}
	registry.Register(constants.PaymentProviderUpay, constants.PaymentChannelTypeUsdtTrc20, stub)

	channel := &paymentdomain.PaymentChannel{
		Name:            "UPay callback query test",
		ProviderType:    constants.PaymentProviderUpay,
		ChannelType:     constants.PaymentChannelTypeUsdtTrc20,
		InteractionMode: constants.PaymentInteractionRedirect,
		ConfigJSON:      jsonmap.JSON{},
		IsActive:        true,
		CreatedAt:       testNow(),
		UpdatedAt:       testNow(),
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	order := &orderdomain.Order{
		OrderNo:          "DJUPAYTEST002",
		Status:           constants.OrderStatusPendingPayment,
		Currency:         "USD",
		OriginalAmount:   money.FromDecimal(decimal.RequireFromString("10.50")),
		TotalAmount:      money.FromDecimal(decimal.RequireFromString("10.50")),
		OnlinePaidAmount: money.FromDecimal(decimal.RequireFromString("10.50")),
		CreatedAt:        testNow(),
		UpdatedAt:        testNow(),
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	payment := upayFactsPayment()
	payment.OrderID = order.ID
	payment.ChannelID = channel.ID
	payment.ChannelType = channel.ChannelType
	payment.InteractionMode = channel.InteractionMode
	payment.ProviderRef = "UP-ORDER-2"
	payment.Amount = money.FromDecimal(decimal.RequireFromString("10.50"))
	payment.Status = constants.PaymentStatusPending
	payment.CreatedAt = testNow()
	payment.UpdatedAt = testNow()
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}

	updated, err := svc.HandleSyncCallback(channel, nil, []byte(`{"ignored":"by-test-verifier"}`))
	if err != nil {
		t.Fatalf("HandleSyncCallback() error = %v", err)
	}
	if stub.queryCalls != 1 {
		t.Fatalf("UPay query calls = %d, want 1", stub.queryCalls)
	}
	if updated == nil || updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status = %+v, want success", updated)
	}
	if updated.ProviderPayload[paymentcontract.GatewayPayloadUpayActualCrypto] != "8.5065" {
		t.Fatalf("callback facts were not replaced by query facts: %#v", updated.ProviderPayload)
	}
}

func TestUpayConcurrentCallbacksTriggerPostCommitSideEffectsOnce(t *testing.T) {
	svc, db := setupPaymentServiceWalletTest(t)
	notifications := &countingNotificationEnqueuer{}
	procurements := &countingProcurementCreator{}
	svc.notificationSvc = notifications
	svc.procurementSvc = procurements
	svc.queue = &upayCallbackQueueStub{}

	channel := &paymentdomain.PaymentChannel{
		Name:            "UPay concurrency test",
		ProviderType:    constants.PaymentProviderUpay,
		ChannelType:     constants.PaymentChannelTypeUsdtTrc20,
		InteractionMode: constants.PaymentInteractionRedirect,
		ConfigJSON:      jsonmap.JSON{},
		IsActive:        true,
		CreatedAt:       testNow(),
		UpdatedAt:       testNow(),
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	order := &orderdomain.Order{
		OrderNo:          "DJUPAYTEST003",
		Status:           constants.OrderStatusPendingPayment,
		Currency:         "USD",
		OriginalAmount:   money.FromDecimal(decimal.RequireFromString("10.50")),
		TotalAmount:      money.FromDecimal(decimal.RequireFromString("10.50")),
		OnlinePaidAmount: money.FromDecimal(decimal.RequireFromString("10.50")),
		CreatedAt:        testNow(),
		UpdatedAt:        testNow(),
	}
	if err := db.Create(order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	payment := upayFactsPayment()
	payment.OrderID = order.ID
	payment.ChannelID = channel.ID
	payment.ChannelType = channel.ChannelType
	payment.InteractionMode = channel.InteractionMode
	payment.ProviderRef = "UP-ORDER-3"
	payment.Amount = money.FromDecimal(decimal.RequireFromString("10.50"))
	payment.Status = constants.PaymentStatusPending
	payment.CreatedAt = testNow()
	payment.UpdatedAt = testNow()
	if err := db.Create(payment).Error; err != nil {
		t.Fatalf("create payment: %v", err)
	}

	input := upayCallbackInput(constants.PaymentStatusSuccess, "8.5065", "USDT")
	input.PaymentID = payment.ID
	input.ChannelID = channel.ID
	input.ProviderRef = "UP-ORDER-3"
	const concurrentCallbacks = 10
	errorsCh := make(chan error, concurrentCallbacks)
	var wg sync.WaitGroup
	for i := 0; i < concurrentCallbacks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.HandleCallback(input)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent callback error: %v", err)
		}
	}
	if got := notifications.Count(); got != 1 {
		t.Fatalf("paid notifications = %d, want 1", got)
	}
	if got := procurements.Count(); got != 1 {
		t.Fatalf("procurement triggers = %d, want 1", got)
	}
	var updated paymentdomain.Payment
	if err := db.First(&updated, payment.ID).Error; err != nil {
		t.Fatalf("reload payment: %v", err)
	}
	if updated.Status != constants.PaymentStatusSuccess {
		t.Fatalf("payment status = %s, want success", updated.Status)
	}
}

func testNow() time.Time { return time.Now() }
