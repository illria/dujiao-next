package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/shopspring/decimal"
)

const (
	smmPanelDefaultBaseURL = "https://116212.vip/api/v2"
	smmPanelHTTPTimeout    = 10 * time.Second
	smmPanelMaxBodyBytes   = 4 << 20
)

var (
	// ErrSMMPanelAPIKeyMissing 表示 SecretRef 指向的环境变量不存在或为空。
	ErrSMMPanelAPIKeyMissing = errors.New("smm panel api key environment variable is missing")
	// ErrSMMPanelAPIKeyRefInvalid 表示秘密引用不是合法的环境变量名。
	ErrSMMPanelAPIKeyRefInvalid = errors.New("smm panel api key environment variable name is invalid")
	// ErrSMMPanelAuthentication 表示 API Key 无效或已失效。
	ErrSMMPanelAuthentication      = errors.New("smm panel authentication failed")
	ErrSMMPanelCurrencyMismatch    = errors.New("smm panel balance currency does not match source currency")
	ErrSMMPanelInsufficientBalance = errors.New("smm panel balance is insufficient")
	ErrSMMPanelConflict            = errors.New("smm panel request conflict or service unavailable")
	ErrSMMPanelRateLimited         = errors.New("smm panel request is rate limited")
	// ErrSMMPanelRetryable 表示只读请求遇到网络、超时、限流或 5xx。
	ErrSMMPanelRetryable = errors.New("smm panel request is retryable")
	// ErrSMMPanelInvalidRequest 表示请求或响应格式不合法。
	ErrSMMPanelInvalidRequest = errors.New("smm panel request is invalid")
	// ErrSMMPanelReadOnly 表示未显式开启真实 action=add。
	ErrSMMPanelReadOnly           = errors.New("smm panel write operation is disabled")
	ErrSMMPanelServiceNotFound    = errors.New("smm panel service not found")
	ErrSMMPanelLinkInvalid        = errors.New("smm panel target link is invalid")
	ErrSMMPanelSnapshotMissing    = errors.New("smm panel price snapshot is missing")
	ErrSMMPanelPriceChanged       = errors.New("smm panel price changed after snapshot")
	ErrSMMPanelUnprofitable       = errors.New("smm panel order is no longer profitable")
	ErrSMMPanelOrderUnknown       = errors.New("smm panel order submission outcome is unknown")
	ErrSMMPanelPricingUnavailable = errors.New("smm panel pricing configuration is unavailable")
)

const smmPanelOrderingEnabledEnv = "SMMPANEL_ORDERING_ENABLED"

// SMMPanelAPIError 是 SMM Panel HTTP 错误的脱敏结构。
// 不保存或回显响应正文，避免请求体、账户信息或其他敏感字段进入日志。
type SMMPanelAPIError struct {
	StatusCode int
	Action     string
	Kind       error
}

func (e *SMMPanelAPIError) Error() string {
	if e == nil {
		return "smm panel api error"
	}
	return fmt.Sprintf("smm panel api error: action %s, status %d", e.Action, e.StatusCode)
}

func (e *SMMPanelAPIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

// SMMPanelBalance 是 balance action 的脱敏结果。
type SMMPanelBalance struct {
	Balance  string `json:"balance"`
	Currency string `json:"currency"`
}

// SMMPanelAdapter 读取 balance/services，并在显式开启 SMMPANEL_ORDERING_ENABLED
// 后支持 add/status。API Key 只存在内存中，来源是 SecretRef 指向的环境变量；
// 连接模型和数据库不保存真实 Key。
type SMMPanelAdapter struct {
	baseURL                string
	secretRef              string
	apiKey                 string
	sourceCurrency         string
	sourceToUSDT           decimal.Decimal
	exchangeRateBufferBPS  int
	markupMultiplier       decimal.Decimal
	minimumOrderAmountUSDT decimal.Decimal
	orderingEnabled        bool
	uploadsDir             string
	client                 *http.Client
}

var _ Adapter = (*SMMPanelAdapter)(nil)

// NewSMMPanelAdapter 从连接引用的环境变量读取 API Key。
func NewSMMPanelAdapter(conn *siteconnectiondomain.Connection, uploadsDir string) (*SMMPanelAdapter, error) {
	if conn == nil {
		return nil, ErrSMMPanelAPIKeyRefInvalid
	}
	secretRef := strings.TrimSpace(conn.SecretRef)
	if secretRef == "" {
		secretRef = constants.ConnectionSecretRefSMMPanel
	}
	if secretRef != constants.ConnectionSecretRefSMMPanel || !isEnvironmentVariableName(secretRef) {
		return nil, fmt.Errorf("%w: %q", ErrSMMPanelAPIKeyRefInvalid, secretRef)
	}
	apiKey, ok := os.LookupEnv(secretRef)
	if !ok || strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("%w: %s", ErrSMMPanelAPIKeyMissing, secretRef)
	}

	baseURL := strings.TrimRight(strings.TrimSpace(conn.BaseURL), "/")
	if baseURL == "" {
		baseURL = smmPanelDefaultBaseURL
	}

	sourceToUSDT := decimal.Zero
	if parsed, parseErr := decimal.NewFromString(strings.TrimSpace(conn.SourceToUSDT)); parseErr == nil {
		sourceToUSDT = parsed
	}
	markupMultiplier := decimal.NewFromInt(2)
	if parsed, parseErr := decimal.NewFromString(strings.TrimSpace(conn.MarkupMultiplier)); parseErr == nil && parsed.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		markupMultiplier = parsed
	} else if strings.TrimSpace(conn.MarkupMultiplier) != "" {
		markupMultiplier = decimal.Zero
	}
	minimumOrderAmountUSDT := decimal.NewFromInt(1)
	if parsed, parseErr := decimal.NewFromString(strings.TrimSpace(conn.MinimumOrderAmountUSDT)); parseErr == nil && parsed.GreaterThan(decimal.Zero) {
		minimumOrderAmountUSDT = parsed
	} else if strings.TrimSpace(conn.MinimumOrderAmountUSDT) != "" {
		minimumOrderAmountUSDT = decimal.Zero
	}
	return &SMMPanelAdapter{
		baseURL:                baseURL,
		secretRef:              secretRef,
		apiKey:                 apiKey,
		sourceCurrency:         strings.TrimSpace(conn.SourceCurrency),
		sourceToUSDT:           sourceToUSDT,
		exchangeRateBufferBPS:  conn.ExchangeRateBufferBPS,
		markupMultiplier:       markupMultiplier,
		minimumOrderAmountUSDT: minimumOrderAmountUSDT,
		orderingEnabled:        smmPanelOrderingEnabled(),
		uploadsDir:             uploadsDir,
		client:                 &http.Client{Timeout: smmPanelHTTPTimeout},
	}, nil
}

// SecretRef 返回环境变量名，不返回 API Key。
func (a *SMMPanelAdapter) SecretRef() string {
	if a == nil {
		return ""
	}
	return a.secretRef
}

// Ping 只调用 balance，不调用 services 以外的任何 action。
func (a *SMMPanelAdapter) Ping(ctx context.Context) (*PingResult, error) {
	balance, err := a.GetBalance(ctx)
	if err != nil {
		return nil, err
	}
	if a.sourceCurrency != "" && !strings.EqualFold(a.sourceCurrency, balance.Currency) {
		return nil, fmt.Errorf("%w: configured currency %s, observed currency %s", ErrSMMPanelCurrencyMismatch, a.sourceCurrency, balance.Currency)
	}
	return &PingResult{
		SiteName:        "SMM Panel",
		ProtocolVersion: "smm-panel-v2",
		Balance:         balance.Balance,
		Currency:        balance.Currency,
	}, nil
}

// GetBalance 读取 SMM Panel 账户余额与 currency。HTTP 方法是 POST，action 本身只读。
func (a *SMMPanelAdapter) GetBalance(ctx context.Context) (*SMMPanelBalance, error) {
	var raw json.RawMessage
	if err := a.postAction(ctx, "balance", nil, &raw); err != nil {
		return nil, err
	}
	return decodeSMMPanelBalance(raw)
}

// ListProducts 将 services action 归一化为通用商品列表，支持本地分页。
// SMM Panel 的 services action 本身没有分页参数，因此只向上游请求一次。
func (a *SMMPanelAdapter) ListProducts(ctx context.Context, opts ListProductsOpts) (*ProductListResult, error) {
	var raw json.RawMessage
	if err := a.postAction(ctx, "services", nil, &raw); err != nil {
		return nil, err
	}
	items, err := decodeSMMPanelServices(raw)
	if err != nil {
		return nil, err
	}

	page := opts.Page
	if page <= 0 {
		page = 1
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = len(items)
	}
	start := (page - 1) * pageSize
	if start >= len(items) {
		return &ProductListResult{Total: len(items), Items: []UpstreamProduct{}, IncludesInactive: false}, nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return &ProductListResult{
		Total:            len(items),
		Items:            items[start:end],
		IncludesInactive: false,
	}, nil
}

// GetProduct 通过只读 services 列表查找一个数字 service ID。
func (a *SMMPanelAdapter) GetProduct(ctx context.Context, productID uint) (*UpstreamProduct, error) {
	if productID == 0 {
		return nil, ErrSMMPanelServiceNotFound
	}
	result, err := a.ListProducts(ctx, ListProductsOpts{Page: 1, PageSize: 0})
	if err != nil {
		return nil, err
	}
	for i := range result.Items {
		if result.Items[i].ID == productID {
			return &result.Items[i], nil
		}
	}
	return nil, ErrSMMPanelServiceNotFound
}

// ListCategories：SMM Panel 的 services 响应不提供可安全映射的数字分类树。
func (a *SMMPanelAdapter) ListCategories(context.Context) (*CategoryListResult, error) {
	return &CategoryListResult{Supported: false, Categories: []UpstreamCategory{}}, nil
}

// CreateOrder 在 action=add 前重新读取 services，核对服务、数量、费率和成本快照。
// 真实写入默认关闭；开启后，add 的任何网络/超时/5xx 结果都返回
// ErrSMMPanelOrderUnknown，调用方不得自动重试。
func (a *SMMPanelAdapter) CreateOrder(ctx context.Context, req CreateUpstreamOrderReq) (*CreateUpstreamOrderResp, error) {
	if !a.orderingEnabled {
		return nil, ErrSMMPanelReadOnly
	}
	if err := validateSMMPanelOrderRequest(req); err != nil {
		return nil, err
	}
	service, err := a.GetProduct(ctx, req.SKUID)
	if err != nil {
		return nil, err
	}
	if !service.Orderable || !strings.EqualFold(strings.TrimSpace(service.ServiceType), "default") {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "service_not_orderable", ErrorMessage: "service is not orderable"}, nil
	}
	if service.MinQuantity > 0 && req.Quantity < service.MinQuantity {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "quantity_below_upstream_min", ErrorMessage: "quantity is below the upstream minimum"}, nil
	}
	if service.MaxQuantity > 0 && req.Quantity > service.MaxQuantity {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "quantity_above_upstream_max", ErrorMessage: "quantity is above the upstream maximum"}, nil
	}

	currentCost, err := a.calculateSMMCost(service.UpstreamRate, req.Quantity, service.PricingUnit)
	if err != nil {
		return nil, err
	}
	currentSell := ceilSMMPanelCent(currentCost.Mul(a.markupMultiplier))
	if currentSell.LessThan(a.minimumOrderAmountUSDT) {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "minimum_order_amount_not_met", ErrorMessage: "quantity does not reach the minimum order amount"}, nil
	}
	currentPricingUnit := strings.TrimSpace(service.PricingUnit)
	if currentPricingUnit == "" {
		currentPricingUnit = constants.PricingUnitPer1000
	}
	snapshotRate, snapshotRateErr := decimal.NewFromString(strings.TrimSpace(req.SnapshotRate))
	snapshotPricingUnit := strings.TrimSpace(req.SnapshotPricingUnit)
	if snapshotRateErr != nil || snapshotRate.LessThanOrEqual(decimal.Zero) || snapshotPricingUnit == "" {
		return nil, ErrSMMPanelSnapshotMissing
	}
	if snapshotPricingUnit != currentPricingUnit {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "smm_pricing_unit_changed", ErrorMessage: "latest pricing unit differs from the order snapshot"}, nil
	}
	snapshotCost, snapshotErr := decimal.NewFromString(strings.TrimSpace(req.SnapshotCost))
	if snapshotErr != nil || snapshotCost.IsNegative() {
		return nil, ErrSMMPanelSnapshotMissing
	}
	snapshotSell, snapshotErr := decimal.NewFromString(strings.TrimSpace(req.SnapshotSell))
	if snapshotErr != nil || snapshotSell.LessThanOrEqual(decimal.Zero) {
		return nil, ErrSMMPanelSnapshotMissing
	}
	if currentCost.GreaterThan(snapshotCost) {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "smm_cost_changed", ErrorMessage: "latest cost exceeds the order snapshot"}, nil
	}
	if snapshotSell.LessThanOrEqual(currentCost) {
		return &CreateUpstreamOrderResp{OK: false, ErrorCode: "smm_unprofitable", ErrorMessage: "latest cost is not profitable"}, nil
	}

	link, _ := req.ManualFormData["link"].(string)
	link = strings.TrimSpace(link)
	var raw json.RawMessage
	if err := a.postAction(ctx, "add", url.Values{
		"service":  []string{strconv.FormatUint(uint64(req.SKUID), 10)},
		"link":     []string{link},
		"quantity": []string{strconv.Itoa(req.Quantity)},
	}, &raw); err != nil {
		return nil, errors.Join(ErrSMMPanelOrderUnknown, err)
	}
	result, err := decodeSMMPanelCreateOrder(raw, a.sourceCurrency)
	if err != nil {
		// A successful HTTP response can still be truncated or malformed after
		// the provider has accepted the order. Without an idempotency key it is
		// safer to preserve the unknown outcome than to submit again.
		return nil, errors.Join(ErrSMMPanelOrderUnknown, err)
	}
	return result, nil
}

// GetOrder 读取 SMM action=status，不包含任何写操作。
func (a *SMMPanelAdapter) GetOrder(ctx context.Context, orderID uint) (*UpstreamOrderDetail, error) {
	if orderID == 0 {
		return nil, ErrSMMPanelInvalidRequest
	}
	var raw json.RawMessage
	if err := a.postAction(ctx, "status", url.Values{
		"order": []string{strconv.FormatUint(uint64(orderID), 10)},
	}, &raw); err != nil {
		return nil, err
	}
	return decodeSMMPanelOrderStatus(raw, orderID, a.sourceCurrency)
}

// CancelOrder 在步骤一禁用，后续履约阶段另行实现。
func (a *SMMPanelAdapter) CancelOrder(context.Context, uint) error {
	return ErrSMMPanelReadOnly
}

// DownloadImage 不主动下载 SMM Panel 的第三方图片。
func (a *SMMPanelAdapter) DownloadImage(context.Context, string) (string, error) {
	return "", ErrSMMPanelReadOnly
}

func smmPanelOrderingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(smmPanelOrderingEnabledEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func validateSMMPanelOrderRequest(req CreateUpstreamOrderReq) error {
	if req.SKUID == 0 || req.Quantity <= 0 {
		return ErrSMMPanelInvalidRequest
	}
	rawLink, ok := req.ManualFormData["link"]
	link, linkOK := rawLink.(string)
	if !ok || !linkOK {
		return ErrSMMPanelLinkInvalid
	}
	link = strings.TrimSpace(link)
	if link == "" || len(link) > 2048 || strings.ContainsAny(link, "\r\n\t ") {
		return ErrSMMPanelLinkInvalid
	}
	parsed, err := url.ParseRequestURI(link)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ErrSMMPanelLinkInvalid
	}
	return nil
}

func (a *SMMPanelAdapter) calculateSMMCost(rateText string, quantity int, pricingUnit string) (decimal.Decimal, error) {
	if strings.TrimSpace(a.sourceCurrency) == "" || a.sourceToUSDT.LessThanOrEqual(decimal.Zero) || a.exchangeRateBufferBPS < 0 || a.markupMultiplier.LessThan(decimal.NewFromInt(1)) || a.minimumOrderAmountUSDT.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, ErrSMMPanelPricingUnavailable
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(rateText))
	if err != nil || rate.LessThanOrEqual(decimal.Zero) || quantity <= 0 {
		return decimal.Zero, ErrSMMPanelInvalidRequest
	}
	rawCost := rate
	switch strings.TrimSpace(pricingUnit) {
	case "", constants.PricingUnitPer1000:
		rawCost = rate.Mul(decimal.NewFromInt(int64(quantity))).Div(decimal.NewFromInt(1000))
	case constants.PricingUnitPerUnit:
		rawCost = rate.Mul(decimal.NewFromInt(int64(quantity)))
	case constants.PricingUnitPerOrder:
	default:
		return decimal.Zero, ErrSMMPanelInvalidRequest
	}
	buffer := decimal.NewFromInt(10000).Add(decimal.NewFromInt(int64(a.exchangeRateBufferBPS))).Div(decimal.NewFromInt(10000))
	return rawCost.Mul(a.sourceToUSDT).Mul(buffer), nil
}

func ceilSMMPanelCent(value decimal.Decimal) decimal.Decimal {
	return value.Div(decimal.RequireFromString("0.01")).Ceil().Mul(decimal.RequireFromString("0.01"))
}

type smmPanelOrderEnvelope struct {
	Order    flexibleString  `json:"order"`
	ID       flexibleString  `json:"id"`
	OrderID  flexibleString  `json:"order_id"`
	OrderNo  flexibleString  `json:"order_no"`
	Status   string          `json:"status"`
	Charge   flexibleString  `json:"charge"`
	Amount   flexibleString  `json:"amount"`
	Currency string          `json:"currency"`
	Data     json.RawMessage `json:"data"`
}

func decodeSMMPanelCreateOrder(raw json.RawMessage, currency string) (*CreateUpstreamOrderResp, error) {
	var envelope smmPanelOrderEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: add response", ErrSMMPanelInvalidRequest)
	}
	if len(envelope.Data) > 0 && string(envelope.Order) == "" && string(envelope.ID) == "" && string(envelope.OrderID) == "" {
		var nested smmPanelOrderEnvelope
		if json.Unmarshal(envelope.Data, &nested) == nil {
			envelope = nested
		}
	}
	orderText := firstSMMPanelString(envelope.OrderID, envelope.Order, envelope.ID)
	if orderText == "" {
		return nil, fmt.Errorf("%w: add order id missing", ErrSMMPanelInvalidRequest)
	}
	orderID, err := strconv.ParseUint(orderText, 10, 64)
	if err != nil || uint64(uint(orderID)) != orderID || orderID == 0 {
		return nil, fmt.Errorf("%w: add order id invalid", ErrSMMPanelInvalidRequest)
	}
	amount := firstSMMPanelString(envelope.Charge, envelope.Amount)
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(envelope.Currency))
	}
	return &CreateUpstreamOrderResp{
		OK: true, OrderID: uint(orderID), OrderNo: firstSMMPanelString(envelope.OrderNo, envelope.Order),
		Status: strings.TrimSpace(envelope.Status), Amount: amount, Currency: strings.ToUpper(strings.TrimSpace(currency)),
	}, nil
}

func decodeSMMPanelOrderStatus(raw json.RawMessage, requestedID uint, currency string) (*UpstreamOrderDetail, error) {
	var envelope smmPanelOrderEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: status response", ErrSMMPanelInvalidRequest)
	}
	if len(envelope.Data) > 0 && strings.TrimSpace(envelope.Status) == "" {
		var nested smmPanelOrderEnvelope
		if json.Unmarshal(envelope.Data, &nested) == nil {
			envelope = nested
		}
	}
	if strings.TrimSpace(envelope.Status) == "" {
		return nil, fmt.Errorf("%w: status missing", ErrSMMPanelInvalidRequest)
	}
	orderID := requestedID
	if text := firstSMMPanelString(envelope.OrderID, envelope.Order, envelope.ID); text != "" {
		if parsed, parseErr := strconv.ParseUint(text, 10, 64); parseErr == nil && uint64(uint(parsed)) == parsed && parsed > 0 {
			orderID = uint(parsed)
		}
	}
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(envelope.Currency))
	}
	status := strings.TrimSpace(envelope.Status)
	var fulfillment *UpstreamFulfillment
	switch strings.ToLower(status) {
	case "completed", "complete", "delivered", "fulfilled":
		fulfillment = &UpstreamFulfillment{Type: "smm", Status: "delivered"}
	}
	return &UpstreamOrderDetail{
		OrderID: orderID, OrderNo: firstSMMPanelString(envelope.OrderNo, envelope.Order), Status: status,
		Amount: firstSMMPanelString(envelope.Charge, envelope.Amount), Currency: strings.ToUpper(strings.TrimSpace(currency)),
		Fulfillment: fulfillment,
	}, nil
}

func firstSMMPanelString(values ...flexibleString) string {
	for _, value := range values {
		if text := strings.TrimSpace(string(value)); text != "" {
			return text
		}
	}
	return ""
}

func (a *SMMPanelAdapter) postAction(ctx context.Context, action string, extra url.Values, result interface{}) error {
	if a == nil || a.client == nil {
		return fmt.Errorf("%w: adapter is nil", ErrSMMPanelInvalidRequest)
	}
	form := url.Values{}
	form.Set("key", a.apiKey)
	form.Set("action", action)
	for key, values := range extra {
		for _, value := range values {
			form.Add(key, value)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: create request", ErrSMMPanelInvalidRequest)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// form 中的 key 只存在于内存请求，不进入日志、错误或持久化记录。

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSMMPanelRetryable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, smmPanelMaxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("%w: read response", ErrSMMPanelRetryable)
	}
	if len(body) > smmPanelMaxBodyBytes {
		return fmt.Errorf("%w: response too large", ErrSMMPanelRetryable)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return smmPanelHTTPError(resp.StatusCode, action)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("%w: empty response", ErrSMMPanelInvalidRequest)
	}

	if hasSMMPanelError(body) {
		return &SMMPanelAPIError{StatusCode: resp.StatusCode, Action: action, Kind: ErrSMMPanelAuthentication}
	}
	if result == nil {
		return nil
	}
	if raw, ok := result.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], body...)
		return nil
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("%w: decode response", ErrSMMPanelInvalidRequest)
	}
	return nil
}

func smmPanelHTTPError(statusCode int, action string) error {
	kind := ErrSMMPanelInvalidRequest
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = ErrSMMPanelAuthentication
	case http.StatusPaymentRequired:
		kind = ErrSMMPanelInsufficientBalance
	case http.StatusConflict:
		kind = ErrSMMPanelConflict
	case http.StatusTooManyRequests:
		kind = errors.Join(ErrSMMPanelRetryable, ErrSMMPanelRateLimited)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = ErrSMMPanelRetryable
	}
	return &SMMPanelAPIError{StatusCode: statusCode, Action: action, Kind: kind}
}

func hasSMMPanelError(body []byte) bool {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	raw, ok := envelope["error"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	if stringValue := strings.TrimSpace(string(raw)); stringValue == "" || stringValue == `""` || stringValue == "false" {
		return false
	}
	return true
}

type smmPanelBalanceEnvelope struct {
	Balance  flexibleString           `json:"balance"`
	Currency string                   `json:"currency"`
	Data     *smmPanelBalanceEnvelope `json:"data"`
}

func decodeSMMPanelBalance(raw json.RawMessage) (*SMMPanelBalance, error) {
	var envelope smmPanelBalanceEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: balance response", ErrSMMPanelInvalidRequest)
	}
	if envelope.Data != nil && string(envelope.Balance) == "" {
		envelope = *envelope.Data
	}
	return &SMMPanelBalance{
		Balance:  string(envelope.Balance),
		Currency: strings.ToUpper(strings.TrimSpace(envelope.Currency)),
	}, nil
}

type smmPanelService struct {
	Service        flexibleString `json:"service"`
	ID             flexibleString `json:"id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	DescriptionAlt string         `json:"desc"`
	Category       string         `json:"category"`
	Type           string         `json:"type"`
	Rate           flexibleString `json:"rate"`
	Min            flexibleString `json:"min"`
	Max            flexibleString `json:"max"`
	Raw            jsonmap.JSON   `json:"-"`
}

func decodeSMMPanelServices(raw json.RawMessage) ([]UpstreamProduct, error) {
	items, err := extractSMMPanelServiceItems(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: services response", ErrSMMPanelInvalidRequest)
	}
	result := make([]UpstreamProduct, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		var service smmPanelService
		if err := json.Unmarshal(item, &service); err != nil {
			return nil, fmt.Errorf("%w: service item", ErrSMMPanelInvalidRequest)
		}
		var metadata jsonmap.JSON
		if err := json.Unmarshal(item, &metadata); err != nil {
			return nil, fmt.Errorf("%w: service metadata", ErrSMMPanelInvalidRequest)
		}
		service.Raw = metadata
		product := service.toUpstreamProduct()
		identity := serviceIDForProduct(product)
		if identity != "" {
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
		}
		result = append(result, product)
	}
	return result, nil
}

func serviceIDForProduct(product UpstreamProduct) string {
	if product.ExternalID != "" {
		return product.ExternalID
	}
	if product.ID > 0 {
		return strconv.FormatUint(uint64(product.ID), 10)
	}
	return ""
}

func extractSMMPanelServiceItems(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty response")
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, err
	}
	for _, key := range []string{"services", "items", "data"} {
		if nested, ok := object[key]; ok {
			return extractSMMPanelServiceItems(nested)
		}
	}
	if _, ok := object["service"]; ok {
		return []json.RawMessage{trimmed}, nil
	}
	if _, ok := object["id"]; ok {
		return []json.RawMessage{trimmed}, nil
	}
	return nil, errors.New("services array not found")
}

func (s smmPanelService) toUpstreamProduct() UpstreamProduct {
	serviceID := strings.TrimSpace(string(s.Service))
	if serviceID == "" {
		serviceID = strings.TrimSpace(string(s.ID))
	}
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = serviceID
	}
	description := strings.TrimSpace(s.Description)
	if description == "" {
		description = strings.TrimSpace(s.DescriptionAlt)
	}
	serviceType := strings.TrimSpace(s.Type)
	orderable := strings.EqualFold(serviceType, "default")
	unsupportedReason := ""
	if !orderable {
		unsupportedReason = "service_type_not_supported"
	}
	minQuantity := parseSMMPanelQuantity(string(s.Min))
	maxQuantity := parseSMMPanelQuantity(string(s.Max))
	metadata := s.Raw
	if metadata == nil {
		metadata = jsonmap.JSON{}
	}
	metadata["pricing_unit"] = constants.PricingUnitPer1000
	metadata["service_type"] = serviceType
	metadata["orderable"] = orderable
	metadata["min_quantity"] = minQuantity
	metadata["max_quantity"] = maxQuantity
	if !orderable {
		metadata["unsupported_reason"] = unsupportedReason
	}

	product := UpstreamProduct{
		Title:                   localizedJSON(name),
		Description:             localizedJSON(description),
		PriceAmount:             string(s.Rate),
		UpstreamRate:            string(s.Rate),
		PricingUnit:             constants.PricingUnitPer1000,
		ServiceType:             serviceType,
		SourceCategory:          strings.TrimSpace(s.Category),
		MinQuantity:             minQuantity,
		MaxQuantity:             maxQuantity,
		Orderable:               orderable,
		UnsupportedReason:       unsupportedReason,
		ServiceParameters:       metadata,
		FulfillmentType:         constants.FulfillmentTypeAuto,
		RequiresEmailActivation: false,
		InStock:                 true,
		Stock:                   -1,
		IsActive:                serviceID != "",
	}
	if serviceID == "" {
		product.IsActive = false
	}
	if id, err := strconv.ParseUint(serviceID, 10, 64); err == nil && uint64(uint(id)) == id {
		product.ID = uint(id)
	} else {
		product.ExternalID = serviceID
	}
	product.SKUs = []UpstreamSKU{{
		ID:            product.ID,
		ExternalID:    product.ExternalID,
		SKUCode:       "DEFAULT",
		PriceAmount:   string(s.Rate),
		UpstreamRate:  string(s.Rate),
		PricingUnit:   constants.PricingUnitPer1000,
		StockStatus:   "unlimited",
		StockQuantity: -1,
		IsActive:      product.IsActive,
	}}
	if orderable {
		product.ManualFormSchema = jsonmap.JSON{
			// Dujiao-Next 的人工表单解析器使用 fields schema；不要把
			// 上游常见的 JSON-Schema 直接写入本地商品，否则结算时
			// 不会渲染 link 输入框，也不会执行校验。
			"fields": []interface{}{
				map[string]interface{}{
					"key":      "link",
					"type":     "text",
					"required": true,
					"label": map[string]interface{}{
						"zh-CN": "目标链接",
						"zh-TW": "目標連結",
						"en-US": "Target link",
					},
					"placeholder": map[string]interface{}{
						"zh-CN": "请输入 http:// 或 https:// 链接",
						"zh-TW": "請輸入 http:// 或 https:// 連結",
						"en-US": "Enter an http:// or https:// URL",
					},
					"regex":   `^https?://[^\s]+$`,
					"max_len": 2048,
				},
			},
		}
	}
	return product
}

func parseSMMPanelQuantity(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := decimal.NewFromString(value)
	if err != nil || !parsed.Equal(parsed.Truncate(0)) || parsed.IsNegative() {
		return 0
	}
	maxInt := int64(^uint(0) >> 1)
	if parsed.GreaterThan(decimal.NewFromInt(maxInt)) {
		return 0
	}
	return int(parsed.IntPart())
}
