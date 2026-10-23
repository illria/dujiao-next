package upstream

import (
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
	ProdSellerDefaultBaseURL = "https://prodseller.com/v1"
	ProdSellerAPIKeyEnv      = "PRODSELLER_API_KEY"
)

var (
	ErrProdSellerAPIKeyMissing = errors.New("prodseller api key environment variable is missing")
	ErrProdSellerUnsupported   = errors.New("prodseller operation is not available in the read-only phase")
	ErrProdSellerRetryable     = errors.New("prodseller request is retryable")
)

// ProdSellerAPIError 是 ProdSeller HTTP 错误的脱敏归一化表示。
// 它只保留状态码和限流元数据，不保留响应体、请求头或 API Key。
type ProdSellerAPIError struct {
	StatusCode  int
	RetryAfter  time.Duration
	RateLimit   int
	RateRemain  int
	RateResetAt time.Time
}

func (e *ProdSellerAPIError) Error() string {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return "prodseller authentication failed"
	case http.StatusPaymentRequired:
		return "prodseller balance is insufficient"
	case http.StatusNotFound:
		return "prodseller resource was not found"
	case http.StatusConflict:
		return "prodseller resource conflict or out of stock"
	case http.StatusTooManyRequests:
		return "prodseller rate limit exceeded"
	case http.StatusBadRequest:
		return "prodseller request is invalid"
	default:
		if e.StatusCode >= 500 {
			return "prodseller server error"
		}
		return "prodseller request failed"
	}
}

func (e *ProdSellerAPIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func (e *ProdSellerAPIError) Unwrap() error {
	if e.Retryable() {
		return ErrProdSellerRetryable
	}
	return nil
}

type ProdSellerDelivery struct {
	Type string `json:"type"`
}

// ProdSellerProduct 是 ProdSeller /v1/products 的只读商品结构。
// ID 保持字符串，不能转换为 uint，也不能截断。
type ProdSellerProduct struct {
	ID                      string             `json:"id"`
	Name                    string             `json:"name"`
	Description             string             `json:"description"`
	Price                   decimal.Decimal    `json:"price"`
	PublicPrice             decimal.Decimal    `json:"publicPrice"`
	ImageURL                string             `json:"imageUrl"`
	Delivery                ProdSellerDelivery `json:"delivery"`
	Sold                    bool               `json:"sold"`
	InStock                 bool               `json:"inStock"`
	Stock                   *int               `json:"stock"`
	RequiresEmailActivation bool               `json:"requiresEmailActivation"`
}

type ProdSellerProductListResult struct {
	Products []ProdSellerProduct `json:"products"`
}

type ProdSellerBalance struct {
	TelegramID json.RawMessage `json:"telegramId"`
	Username   string          `json:"username"`
	Balance    decimal.Decimal `json:"balance"`
	Currency   string          `json:"currency"`
	Membership string          `json:"membership"`
}

// UnmarshalJSON 接受文档中出现过的直接数组、products 数组和 data 包装三种列表响应。
// 这样不会把上游的响应包装差异泄漏到业务层，也不影响外部 ID 的字符串语义。
func (r *ProdSellerProductListResult) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		var products []ProdSellerProduct
		if err := json.Unmarshal(data, &products); err != nil {
			return err
		}
		r.Products = products
		return nil
	}

	var envelope struct {
		Products []ProdSellerProduct `json:"products"`
		Items    []ProdSellerProduct `json:"items"`
		Data     json.RawMessage     `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Products != nil {
		r.Products = envelope.Products
		return nil
	}
	if envelope.Items != nil {
		r.Products = envelope.Items
		return nil
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		return json.Unmarshal(envelope.Data, r)
	}
	r.Products = nil
	return nil
}

// UnmarshalJSON 兼容 GET /products/:id 返回直接商品或 {product: ...} 包装。
func (p *ProdSellerProduct) UnmarshalJSON(data []byte) error {
	type productAlias ProdSellerProduct
	var envelope struct {
		Product *productAlias `json:"product"`
		Data    *productAlias `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Product != nil {
		*p = ProdSellerProduct(*envelope.Product)
		return nil
	}
	if envelope.Data != nil {
		*p = ProdSellerProduct(*envelope.Data)
		return nil
	}
	var direct productAlias
	if err := json.Unmarshal(data, &direct); err != nil {
		return err
	}
	*p = ProdSellerProduct(direct)
	return nil
}

// ExternalCatalogAdapter 是保留字符串外部 ID 的只读商品目录端口。
// 仅实现该接口的适配器才能被外部 ID 同步流程使用。
type ExternalCatalogAdapter interface {
	ListExternalProducts(ctx context.Context) (*ProdSellerProductListResult, error)
	GetExternalProduct(ctx context.Context, productID string) (*ProdSellerProduct, error)
	GetExternalBalance(ctx context.Context) (*ProdSellerBalance, error)
}

type ProdSellerAdapter struct {
	baseURL   string
	apiKeyEnv string
	client    *http.Client
}

var _ Adapter = (*ProdSellerAdapter)(nil)
var _ ExternalCatalogAdapter = (*ProdSellerAdapter)(nil)

func NewProdSellerAdapter(conn *siteconnectiondomain.Connection) *ProdSellerAdapter {
	baseURL := strings.TrimRight(strings.TrimSpace(conn.BaseURL), "/")
	if baseURL == "" {
		baseURL = ProdSellerDefaultBaseURL
	}
	return &ProdSellerAdapter{
		baseURL:   baseURL,
		// ProdSeller 的密钥来源固定为服务端环境变量，连接记录只保留该变量名。
		apiKeyEnv: ProdSellerAPIKeyEnv,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (a *ProdSellerAdapter) resolveAPIKey() (string, error) {
	if a == nil {
		return "", ErrProdSellerAPIKeyMissing
	}
	key, ok := os.LookupEnv(a.apiKeyEnv)
	if !ok || strings.TrimSpace(key) == "" {
		return "", ErrProdSellerAPIKeyMissing
	}
	return key, nil
}

func (a *ProdSellerAdapter) getJSON(ctx context.Context, path string, target interface{}) error {
	key, err := a.resolveAPIKey()
	if err != nil {
		return err
	}
	requestURL, err := url.Parse(a.baseURL + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return fmt.Errorf("prodseller request url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return fmt.Errorf("prodseller request: %w", err)
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		// 不携带 URL 中的凭证或响应体；调用方可按 ErrProdSellerRetryable 处理。
		return fmt.Errorf("%w: %v", ErrProdSellerRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		apiErr := &ProdSellerAPIError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			RateLimit:  parseHeaderInt(resp.Header.Get("X-RateLimit-Limit")),
			RateRemain: parseHeaderInt(resp.Header.Get("X-RateLimit-Remaining")),
		}
		if reset := parseHeaderInt64(resp.Header.Get("X-RateLimit-Reset")); reset > 0 {
			apiErr.RateResetAt = time.Unix(reset, 0)
		}
		return apiErr
	}

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("%w: invalid response", ErrProdSellerRetryable)
	}
	return nil
}

func parseRetryAfter(raw string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if retryAt, parseErr := http.ParseTime(strings.TrimSpace(raw)); parseErr == nil {
		if wait := time.Until(retryAt); wait > 0 {
			return wait
		}
	}
	return 0
}

func parseHeaderInt(raw string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func parseHeaderInt64(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func (a *ProdSellerAdapter) Ping(ctx context.Context) (*PingResult, error) {
	balance, err := a.GetExternalBalance(ctx)
	if err != nil {
		return nil, err
	}
	currency := strings.TrimSpace(balance.Currency)
	if currency == "" {
		currency = "USDT"
	}
	return &PingResult{
		SiteName:        "ProdSeller",
		ProtocolVersion: "v1",
		Balance:         balance.Balance.String(),
		Currency:        currency,
		MemberLevel:     map[string]interface{}{"membership": balance.Membership},
	}, nil
}

func (a *ProdSellerAdapter) ListCategories(context.Context) (*CategoryListResult, error) {
	return &CategoryListResult{Supported: false, Categories: nil}, nil
}

func (a *ProdSellerAdapter) ListProducts(ctx context.Context, opts ListProductsOpts) (*ProductListResult, error) {
	result, err := a.ListExternalProducts(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]UpstreamProduct, 0, len(result.Products))
	for _, product := range result.Products {
		items = append(items, product.toUpstreamProduct())
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = len(items)
	}
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start >= len(items) {
		items = nil
	} else {
		end := start + pageSize
		if end > len(items) {
			end = len(items)
		}
		items = items[start:end]
	}
	return &ProductListResult{Total: len(result.Products), Items: items}, nil
}

func (a *ProdSellerAdapter) GetProduct(ctx context.Context, productID uint) (*UpstreamProduct, error) {
	product, err := a.GetExternalProduct(ctx, strconv.FormatUint(uint64(productID), 10))
	if err != nil {
		return nil, err
	}
	converted := product.toUpstreamProduct()
	return &converted, nil
}

func (a *ProdSellerAdapter) CreateOrder(context.Context, CreateUpstreamOrderReq) (*CreateUpstreamOrderResp, error) {
	return nil, ErrProdSellerUnsupported
}

func (a *ProdSellerAdapter) GetOrder(context.Context, uint) (*UpstreamOrderDetail, error) {
	return nil, ErrProdSellerUnsupported
}

func (a *ProdSellerAdapter) CancelOrder(context.Context, uint) error {
	return ErrProdSellerUnsupported
}

func (a *ProdSellerAdapter) DownloadImage(context.Context, string) (string, error) {
	return "", ErrProdSellerUnsupported
}

func (a *ProdSellerAdapter) ListExternalProducts(ctx context.Context) (*ProdSellerProductListResult, error) {
	var result ProdSellerProductListResult
	if err := a.getJSON(ctx, "products", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *ProdSellerAdapter) GetExternalProduct(ctx context.Context, productID string) (*ProdSellerProduct, error) {
	productID = strings.TrimSpace(productID)
	if productID == "" {
		return nil, &ProdSellerAPIError{StatusCode: http.StatusBadRequest}
	}
	var result ProdSellerProduct
	if err := a.getJSON(ctx, "products/"+url.PathEscape(productID), &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *ProdSellerAdapter) GetExternalBalance(ctx context.Context) (*ProdSellerBalance, error) {
	var result ProdSellerBalance
	if err := a.getJSON(ctx, "balance", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (p ProdSellerProduct) toUpstreamProduct() UpstreamProduct {
	fulfillment := constants.FulfillmentTypeManual
	if strings.EqualFold(strings.TrimSpace(p.Delivery.Type), "instant") {
		fulfillment = constants.FulfillmentTypeAuto
	}
	active := p.InStock
	stock := -1
	if p.Stock != nil {
		stock = *p.Stock
	}
	if !p.InStock {
		stock = 0
	}
	title := jsonmap.JSON{"zh-CN": p.Name, "zh-TW": p.Name, "en": p.Name}
	description := jsonmap.JSON{"zh-CN": p.Description, "zh-TW": p.Description, "en": p.Description}
	return UpstreamProduct{
		ExternalID:      p.ID,
		Title:           title,
		Description:     description,
		Content:         description,
		Images:          nonEmptyImageList(p.ImageURL),
		PriceAmount:     p.Price.String(),
		OriginalPrice:   p.PublicPrice.String(),
		Currency:        "USDT",
		FulfillmentType: fulfillment,
		IsActive:        active,
		SKUs: []UpstreamSKU{{
			ExternalID:    p.ID,
			SKUCode:       DefaultExternalSKUCode,
			PriceAmount:   p.Price.String(),
			StockQuantity: stock,
			StockStatus:   externalStockStatus(stock),
			IsActive:      active,
		}},
	}
}

const DefaultExternalSKUCode = "DEFAULT"

func nonEmptyImageList(imageURL string) []string {
	if strings.TrimSpace(imageURL) == "" {
		return nil
	}
	return []string{strings.TrimSpace(imageURL)}
}

func externalStockStatus(stock int) string {
	if stock < 0 {
		return "unlimited"
	}
	if stock == 0 {
		return "out_of_stock"
	}
	return "in_stock"
}
