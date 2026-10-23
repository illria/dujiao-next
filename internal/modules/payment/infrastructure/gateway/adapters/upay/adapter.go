package upayadapter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gatewaycommon "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"

	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"

	"github.com/dujiao-next/internal/constants"
	upay "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/upay"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/dujiao-next/internal/shared/money"

	"github.com/shopspring/decimal"
)

// upayAdapter adapts the UPay.ink JSON API to Dujiao-Next's payment ports.
// UPay's merchant amount remains the site's USD amount. The crypto amount and
// exchange rate returned by UPay are retained in ProviderPayload as an audit
// snapshot and are used for strict callback validation.
type upayAdapter struct{}

func NewUpayAdapter() paymentcontract.GatewayProvider { return &upayAdapter{} }

var (
	_ paymentcontract.GatewayProvider         = (*upayAdapter)(nil)
	_ paymentcontract.GatewayCapturer         = (*upayAdapter)(nil)
	_ paymentcontract.GatewayCallbackVerifier = (*upayAdapter)(nil)
)

func (a *upayAdapter) Type() string {
	return constants.PaymentProviderUpay + ":"
}

func (a *upayAdapter) parseConfig(raw jsonmap.JSON) (*upay.Config, error) {
	cfg, err := upay.ParseConfig(raw)
	if err != nil {
		return nil, mapUpayError(err)
	}
	if err := upay.ValidateConfig(cfg); err != nil {
		return nil, mapUpayError(err)
	}
	return cfg, nil
}

// ValidateConfig is called with channel_type when an admin channel is saved,
// and with interaction_mode by the capture path. Accept both values while
// keeping the only supported network and interaction mode explicit.
func (a *upayAdapter) ValidateConfig(raw jsonmap.JSON, value string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", constants.PaymentChannelTypeUsdtTrc20, constants.PaymentInteractionRedirect:
	default:
		return fmt.Errorf("%w: upay value %s", paymentcontract.ErrGatewayUnsupportedChannel, value)
	}
	return func() error {
		_, err := a.parseConfig(raw)
		return err
	}()
}

func (a *upayAdapter) CreatePayment(ctx context.Context, raw jsonmap.JSON, input paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	cfg, err := a.parseConfig(raw)
	if err != nil {
		return nil, err
	}
	if strings.ToLower(strings.TrimSpace(input.ChannelType)) != constants.PaymentChannelTypeUsdtTrc20 {
		return nil, fmt.Errorf("%w: upay only supports %s", paymentcontract.ErrGatewayUnsupportedChannel, constants.PaymentChannelTypeUsdtTrc20)
	}
	if currency := strings.ToUpper(strings.TrimSpace(input.Currency)); currency != "" && currency != "USD" {
		return nil, fmt.Errorf("%w: upay requires site currency USD", paymentcontract.ErrGatewayConfigInvalid)
	}
	mode, _ := input.Extra["interaction_mode"].(string)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "" && mode != constants.PaymentInteractionRedirect {
		return nil, fmt.Errorf("%w: upay only supports redirect interaction_mode", paymentcontract.ErrGatewayConfigInvalid)
	}

	returnURL := strings.TrimSpace(input.ReturnURL)
	if returnURL == "" {
		returnURL = cfg.RedirectURL
	}
	returnURL = gatewaycommon.AppendQueryParams(returnURL, input.ReturnURLQuery)

	native, err := upay.CreatePayment(ctx, cfg, upay.CreateInput{
		MerchantOrderNo: input.OrderNo,
		FiatAmount:      input.Amount.Decimal.String(),
		FiatCurrency:    "USD",
		ChainType:       upay.ChainTypeTRC20,
		NotifyURL:       cfg.NotifyURL,
		RedirectURL:     returnURL,
		ProductName:     input.Subject,
	})
	if err != nil {
		return nil, mapUpayError(err)
	}

	payload := buildCreatePayload(native, input.Amount.Decimal.String())
	return &paymentcontract.GatewayCreateResult{
		ProviderRef:        native.OrderNo,
		RedirectURL:        native.PayURL,
		Payload:            payload,
		DisplayChannelType: constants.PaymentChannelTypeUsdtTrc20,
		AmountSent:         input.Amount.Decimal.String(),
		CurrencySent:       "USD",
	}, nil
}

func (a *upayAdapter) QueryPayment(ctx context.Context, raw jsonmap.JSON, providerRef string) (*paymentcontract.GatewayQueryResult, error) {
	cfg, err := a.parseConfig(raw)
	if err != nil {
		return nil, err
	}
	order, err := upay.QueryOrder(ctx, cfg, providerRef)
	if err != nil {
		return nil, mapUpayError(err)
	}
	if order == nil {
		return &paymentcontract.GatewayQueryResult{
			ProviderRef: strings.TrimSpace(providerRef),
			Status:      constants.PaymentStatusPending,
			Currency:    "USDT",
			Payload:     jsonmap.JSON{paymentcontract.GatewayPayloadUpayStatus: upay.StatusPending},
		}, nil
	}
	if order.ChainType != "" && order.ChainType != upay.ChainTypeTRC20 {
		return nil, fmt.Errorf("%w: response chain type %s", paymentcontract.ErrGatewayUnsupportedChannel, order.ChainType)
	}
	status := upay.ToPaymentStatus(order.Status)
	if status == "" {
		return nil, fmt.Errorf("%w: response status is invalid", paymentcontract.ErrGatewayResponseInvalid)
	}
	amount := parseMoneyAmount(order.ActualCrypto)
	payload := buildOrderPayload(order)
	return &paymentcontract.GatewayQueryResult{
		ProviderRef: gatewaycommon.PickFirstNonEmpty(order.OrderNo, providerRef),
		Status:      status,
		Amount:      amount,
		Currency:    "USDT",
		PaidAt:      upay.ParseUnixSeconds(order.CompletedAt),
		Payload:     payload,
	}, nil
}

func (a *upayAdapter) VerifyCallback(raw jsonmap.JSON, _ map[string][]string, body []byte) (*paymentcontract.GatewayCallbackResult, error) {
	cfg, err := upay.ParseConfig(raw)
	if err != nil {
		return nil, mapUpayError(err)
	}
	data, err := upay.ParseCallback(body)
	if err != nil {
		return nil, mapUpayError(err)
	}
	if err := upay.VerifyCallback(cfg, data); err != nil {
		return nil, mapUpayError(err)
	}
	if data.ChainType != upay.ChainTypeTRC20 {
		return nil, fmt.Errorf("%w: response chain type %s", paymentcontract.ErrGatewayUnsupportedChannel, data.ChainType)
	}
	return &paymentcontract.GatewayCallbackResult{
		OrderNo:     data.MerchantOrderNo,
		ProviderRef: data.OrderNo,
		Status:      upay.ToPaymentStatus(data.Status),
		Amount:      parseMoneyAmount(data.ActualCrypto),
		Currency:    "USDT",
		PaidAt:      upay.ParseUnixSeconds(data.CompletedAt),
		Payload:     buildCallbackPayload(data),
	}, nil
}

func buildCreatePayload(result *upay.CreateResult, fiatAmount string) jsonmap.JSON {
	payload := jsonmap.JSON{
		paymentcontract.GatewayPayloadUpayFiatAmount:     strings.TrimSpace(fiatAmount),
		paymentcontract.GatewayPayloadUpayFiatCurrency:   "USD",
		paymentcontract.GatewayPayloadUpayCryptoCurrency: "USDT",
		paymentcontract.GatewayPayloadUpayChainType:      upay.ChainTypeTRC20,
	}
	if result == nil {
		return payload
	}
	payload[paymentcontract.GatewayPayloadUpayOrderNo] = result.OrderNo
	payload[paymentcontract.GatewayPayloadUpayMerchantOrder] = result.MerchantOrderNo
	payload[paymentcontract.GatewayPayloadUpayCryptoAmount] = result.Crypto
	payload[paymentcontract.GatewayPayloadUpayExchangeRate] = result.ExchangeRate
	payload[paymentcontract.GatewayPayloadUpayStatus] = result.Status
	return payload
}

func buildOrderPayload(order *upay.Order) jsonmap.JSON {
	payload := jsonmap.JSON{}
	if order == nil {
		return payload
	}
	setUpayOrderPayload(payload, order.AppID, order.OrderNo, order.MerchantOrderNo, order.ChainType, order.ActualCrypto, order.Status, order.CreatedAt, order.CompletedAt)
	payload[paymentcontract.GatewayPayloadUpayProviderCrypto] = strings.TrimSpace(order.Crypto)
	return payload
}

func buildCallbackPayload(data *upay.CallbackData) jsonmap.JSON {
	payload := jsonmap.JSON{}
	if data == nil {
		return payload
	}
	setUpayOrderPayload(payload, data.AppID, data.OrderNo, data.MerchantOrderNo, data.ChainType, data.ActualCrypto, data.Status, data.CreatedAt, data.CompletedAt)
	// Keep provider response fields under names that cannot overwrite the
	// creation snapshot used for strict amount comparison.
	payload[paymentcontract.GatewayPayloadUpayProviderCrypto] = data.Crypto
	payload["upay_poundage"] = data.Poundage
	payload["upay_actual_poundage"] = data.ActualPoundage
	return payload
}

func setUpayOrderPayload(payload jsonmap.JSON, appID, orderNo, merchantOrderNo, chainType, actualCrypto, status, createdAt, completedAt string) {
	payload[paymentcontract.GatewayPayloadUpayOrderNo] = strings.TrimSpace(orderNo)
	payload[paymentcontract.GatewayPayloadUpayMerchantOrder] = strings.TrimSpace(merchantOrderNo)
	payload[paymentcontract.GatewayPayloadUpayChainType] = strings.TrimSpace(chainType)
	payload[paymentcontract.GatewayPayloadUpayCryptoCurrency] = "USDT"
	payload[paymentcontract.GatewayPayloadUpayActualCrypto] = strings.TrimSpace(actualCrypto)
	payload[paymentcontract.GatewayPayloadUpayStatus] = strings.TrimSpace(status)
	payload[paymentcontract.GatewayPayloadUpayCreatedAt] = strings.TrimSpace(createdAt)
	payload[paymentcontract.GatewayPayloadUpayCompletedAt] = strings.TrimSpace(completedAt)
	if strings.TrimSpace(appID) != "" {
		payload["upay_app_id"] = strings.TrimSpace(appID)
	}
}

func parseMoneyAmount(value string) money.Amount {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil || !parsed.IsPositive() {
		return money.Amount{}
	}
	return money.FromDecimal(parsed)
}

func mapUpayError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, upay.ErrConfigInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayConfigInvalid, err)
	case errors.Is(err, upay.ErrUnsupportedChain):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayUnsupportedChannel, err)
	case errors.Is(err, upay.ErrRequestFailed):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayRequestFailed, err)
	case errors.Is(err, upay.ErrSignatureInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewaySignatureInvalid, err)
	case errors.Is(err, upay.ErrResponseInvalid):
		return fmt.Errorf("%w: %v", paymentcontract.ErrGatewayResponseInvalid, err)
	default:
		return err
	}
}
