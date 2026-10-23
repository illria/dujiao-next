package upay

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"

	"github.com/shopspring/decimal"
)

var (
	ErrConfigInvalid    = errors.New("upay config invalid")
	ErrRequestFailed    = errors.New("upay request failed")
	ErrResponseInvalid  = errors.New("upay response invalid")
	ErrSignatureInvalid = errors.New("upay signature invalid")
	ErrUnsupportedChain = errors.New("upay chain type not supported")
)

const (
	StatusPending = "0"
	StatusSuccess = "1"
	StatusExpired = "2"
	StatusFailed  = "3"

	ChainTypeTRC20 = "1"

	applyPath = "/v1/api/open/order/apply"
	queryPath = "/v1/api/open/order/queryall"

	defaultGatewayURL = "https://api.upay.ink"
	queryPageSize     = 100
	maxQueryPages     = 10
)

// Config contains only UPay connection metadata. AppID/AppSecret may be
// supplied by the admin channel configuration or by UPAY_APP_ID/
// UPAY_APP_SECRET on the server; neither value is ever logged by this package.
type Config struct {
	GatewayURL   string `json:"gateway_url"`
	AppID        string `json:"app_id"`
	AppSecret    string `json:"app_secret"`
	ChainType    string `json:"chain_type"`
	FiatCurrency string `json:"fiat_currency"`
	NotifyURL    string `json:"notify_url"`
	RedirectURL  string `json:"redirect_url"`
}

func ParseConfig(raw map[string]interface{}) (*Config, error) {
	// A channel may intentionally keep only non-secret settings in the database
	// while credentials are supplied through UPAY_APP_ID/UPAY_APP_SECRET.
	// Treat a nil JSON object as an empty config so Normalize can fill those
	// environment-backed fields instead of rejecting the channel prematurely.
	if raw == nil {
		raw = map[string]interface{}{}
	}
	return common.ParseConfig[Config](raw, ErrConfigInvalid)
}

func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.GatewayURL = strings.TrimRight(strings.TrimSpace(c.GatewayURL), "/")
	if c.GatewayURL == "" {
		c.GatewayURL = defaultGatewayURL
	}
	c.AppID = strings.TrimSpace(c.AppID)
	if c.AppID == "" {
		c.AppID = strings.TrimSpace(os.Getenv("UPAY_APP_ID"))
	}
	c.AppSecret = strings.TrimSpace(c.AppSecret)
	if c.AppSecret == "" {
		c.AppSecret = strings.TrimSpace(os.Getenv("UPAY_APP_SECRET"))
	}
	c.ChainType = strings.TrimSpace(c.ChainType)
	if c.ChainType == "" {
		c.ChainType = ChainTypeTRC20
	}
	c.FiatCurrency = strings.ToUpper(strings.TrimSpace(c.FiatCurrency))
	if c.FiatCurrency == "" {
		c.FiatCurrency = "USD"
	}
	c.NotifyURL = strings.TrimSpace(c.NotifyURL)
	c.RedirectURL = strings.TrimSpace(c.RedirectURL)
}

func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("%w: config is nil", ErrConfigInvalid)
	}
	cfg.Normalize()
	if cfg.AppID == "" {
		return fmt.Errorf("%w: app_id is required", ErrConfigInvalid)
	}
	if cfg.AppSecret == "" {
		return fmt.Errorf("%w: app_secret is required", ErrConfigInvalid)
	}
	if cfg.NotifyURL == "" {
		return fmt.Errorf("%w: notify_url is required", ErrConfigInvalid)
	}
	if !strings.EqualFold(cfg.FiatCurrency, "USD") {
		return fmt.Errorf("%w: fiat_currency must be USD", ErrConfigInvalid)
	}
	if cfg.ChainType != ChainTypeTRC20 {
		return fmt.Errorf("%w: %s", ErrUnsupportedChain, cfg.ChainType)
	}
	return nil
}

// Sign implements UPay's documented MD5 signature: ASCII-sort the signed
// field names, join key=value with &, append &appSecret=..., and uppercase MD5.
func Sign(params map[string]string, appSecret string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if isSignatureField(key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+params[key])
	}
	canonical := strings.Join(pairs, "&") + "&appSecret=" + appSecret
	sum := md5.Sum([]byte(canonical))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func isSignatureField(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return key == "sign" || key == "signature"
}

type CreateInput struct {
	MerchantOrderNo string
	FiatAmount      string
	FiatCurrency    string
	ChainType       string
	NotifyURL       string
	RedirectURL     string
	ProductName     string
	Attach          string
}

type CreateResult struct {
	AppID           string
	OrderNo         string
	MerchantOrderNo string
	ExchangeRate    string
	Crypto          string
	Status          string
	PayURL          string
}

type Order struct {
	AppID           string
	OrderNo         string
	MerchantOrderNo string
	ChainType       string
	Crypto          string
	ActualCrypto    string
	Poundage        string
	ActualPoundage  string
	Status          string
	Attach          string
	CreatedAt       string
	CompletedAt     string
}

type CallbackData struct {
	AppID           string
	OrderNo         string
	MerchantOrderNo string
	ChainType       string
	Crypto          string
	ActualCrypto    string
	Poundage        string
	ActualPoundage  string
	Status          string
	Attach          string
	CreatedAt       string
	CompletedAt     string
	Signature       string
	signedParams    map[string]string
}

func CreatePayment(ctx context.Context, cfg *Config, input CreateInput) (*CreateResult, error) {
	if cfg == nil {
		return nil, ErrConfigInvalid
	}
	cfg.Normalize()
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	merchantOrderNo := strings.TrimSpace(input.MerchantOrderNo)
	if len(merchantOrderNo) < 14 || len(merchantOrderNo) > 32 {
		return nil, fmt.Errorf("%w: merchant_order_no length must be 14-32", ErrConfigInvalid)
	}
	fiatCurrency := strings.ToUpper(strings.TrimSpace(input.FiatCurrency))
	if fiatCurrency == "" {
		fiatCurrency = cfg.FiatCurrency
	}
	if fiatCurrency != "USD" {
		return nil, fmt.Errorf("%w: fiat_currency must be USD", ErrConfigInvalid)
	}
	chainType := strings.TrimSpace(input.ChainType)
	if chainType == "" {
		chainType = cfg.ChainType
	}
	if chainType != ChainTypeTRC20 {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedChain, chainType)
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(input.FiatAmount))
	if err != nil || !amount.IsPositive() {
		return nil, fmt.Errorf("%w: fiat_amount is invalid", ErrConfigInvalid)
	}
	if len(strings.TrimSpace(input.ProductName)) > 32 || len(strings.TrimSpace(input.Attach)) > 64 {
		return nil, fmt.Errorf("%w: optional field is too long", ErrConfigInvalid)
	}
	notifyURL := strings.TrimSpace(input.NotifyURL)
	if notifyURL == "" {
		notifyURL = cfg.NotifyURL
	}
	if notifyURL == "" {
		return nil, fmt.Errorf("%w: notify_url is required", ErrConfigInvalid)
	}
	redirectURL := strings.TrimSpace(input.RedirectURL)
	if redirectURL == "" {
		redirectURL = cfg.RedirectURL
	}

	params := map[string]string{
		"appId":           cfg.AppID,
		"merchantOrderNo": merchantOrderNo,
		"chainType":       chainType,
		"fiatAmount":      amount.StringFixed(4),
		"fiatCurrency":    fiatCurrency,
		"notifyUrl":       notifyURL,
	}
	signedParams := cloneStringMap(params)
	if productName := strings.TrimSpace(input.ProductName); productName != "" {
		params["productName"] = productName
	}
	if attach := strings.TrimSpace(input.Attach); attach != "" {
		params["attach"] = attach
	}
	if redirectURL != "" {
		params["redirectUrl"] = redirectURL
	}
	// UPay marks productName, attach, and redirectUrl as unsigned fields.
	// Keep them in the JSON request, but sign only the required signed fields.
	params["signature"] = Sign(signedParams, cfg.AppSecret)

	raw, err := postJSON(ctx, cfg.GatewayURL+applyPath, params)
	if err != nil {
		return nil, err
	}
	data, err := responseObject(raw)
	if err != nil {
		return nil, err
	}
	result := &CreateResult{
		AppID:           stringValue(data["appId"]),
		OrderNo:         stringValue(data["orderNo"]),
		MerchantOrderNo: stringValue(data["merchantOrderNo"]),
		ExchangeRate:    stringValue(data["exchangeRate"]),
		Crypto:          stringValue(data["crypto"]),
		Status:          normalizeStatus(stringValue(data["status"])),
		PayURL:          stringValue(data["payUrl"]),
	}
	if result.OrderNo == "" || result.MerchantOrderNo == "" || result.ExchangeRate == "" || result.Crypto == "" || result.PayURL == "" {
		return nil, fmt.Errorf("%w: create response missing required fields", ErrResponseInvalid)
	}
	if rate, rateErr := decimal.NewFromString(result.ExchangeRate); rateErr != nil || !rate.IsPositive() {
		return nil, fmt.Errorf("%w: exchange_rate is invalid", ErrResponseInvalid)
	}
	if crypto, cryptoErr := decimal.NewFromString(result.Crypto); cryptoErr != nil || !crypto.IsPositive() {
		return nil, fmt.Errorf("%w: crypto is invalid", ErrResponseInvalid)
	}
	if ToPaymentStatus(result.Status) == "" {
		return nil, fmt.Errorf("%w: status is invalid", ErrResponseInvalid)
	}
	return result, nil
}

// QueryOrder scans the documented paging endpoint and returns nil,nil when
// the provider has not made an order visible yet. That is a pending result,
// not a reason to re-submit the order.
func QueryOrder(ctx context.Context, cfg *Config, providerRef string) (*Order, error) {
	if cfg == nil {
		return nil, ErrConfigInvalid
	}
	cfg.Normalize()
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	providerRef = strings.TrimSpace(providerRef)
	if providerRef == "" {
		return nil, fmt.Errorf("%w: provider_ref is required", ErrConfigInvalid)
	}
	for page := 1; page <= maxQueryPages; page++ {
		params := map[string]string{
			"appId":    cfg.AppID,
			"pageNo":   strconv.Itoa(page),
			"pageSize": strconv.Itoa(queryPageSize),
		}
		params["signature"] = Sign(params, cfg.AppSecret)
		raw, err := postJSON(ctx, cfg.GatewayURL+queryPath, params)
		if err != nil {
			return nil, err
		}
		data, err := responseObject(raw)
		if err != nil {
			return nil, err
		}
		list, _ := data["list"].([]interface{})
		for _, item := range list {
			object, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			order := parseOrder(object)
			if order.AppID != "" && order.AppID != cfg.AppID {
				continue
			}
			if order.OrderNo == providerRef || order.MerchantOrderNo == providerRef {
				return &order, nil
			}
		}
		total := intValue(data["total"])
		if len(list) < queryPageSize || (total > 0 && page*queryPageSize >= total) {
			break
		}
	}
	return nil, nil
}

func ParseCallback(body []byte) (*CallbackData, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ErrResponseInvalid
	}
	var raw map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: invalid callback body", ErrResponseInvalid)
	}
	data := &CallbackData{
		AppID:           stringValue(raw["appId"]),
		OrderNo:         stringValue(raw["orderNo"]),
		MerchantOrderNo: stringValue(raw["merchantOrderNo"]),
		ChainType:       stringValue(raw["chainType"]),
		Crypto:          stringValue(raw["crypto"]),
		ActualCrypto:    stringValue(raw["actualCrypto"]),
		Poundage:        stringValue(raw["poundage"]),
		ActualPoundage:  stringValue(raw["actualPoundage"]),
		Status:          normalizeStatus(stringValue(raw["status"])),
		Attach:          stringValue(raw["attach"]),
		CreatedAt:       stringValue(raw["createdAt"]),
		CompletedAt:     stringValue(raw["completedAt"]),
		Signature:       stringValue(raw["signature"]),
	}
	data.signedParams = make(map[string]string, len(raw))
	for key, value := range raw {
		if isSignatureField(key) || key == "attach" {
			continue
		}
		data.signedParams[key] = stringValue(value)
	}
	return data, nil
}

func VerifyCallback(cfg *Config, data *CallbackData) error {
	if cfg == nil || data == nil {
		return ErrConfigInvalid
	}
	cfg.Normalize()
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	requiredFields := map[string]string{
		"appId":           data.AppID,
		"orderNo":         data.OrderNo,
		"merchantOrderNo": data.MerchantOrderNo,
		"chainType":       data.ChainType,
		"crypto":          data.Crypto,
		"actualCrypto":    data.ActualCrypto,
		"poundage":        data.Poundage,
		"actualPoundage":  data.ActualPoundage,
		"status":          data.Status,
		"createdAt":       data.CreatedAt,
		"completedAt":     data.CompletedAt,
		"signature":       data.Signature,
	}
	for field, value := range requiredFields {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: callback field %s is required", ErrResponseInvalid, field)
		}
	}
	if data.AppID == "" || data.AppID != cfg.AppID || data.OrderNo == "" || data.MerchantOrderNo == "" {
		return fmt.Errorf("%w: callback identity mismatch", ErrResponseInvalid)
	}
	if data.ChainType != ChainTypeTRC20 {
		return fmt.Errorf("%w: %s", ErrUnsupportedChain, data.ChainType)
	}
	if data.Status != StatusPending && data.Status != StatusSuccess && data.Status != StatusExpired && data.Status != StatusFailed {
		return fmt.Errorf("%w: status is invalid", ErrResponseInvalid)
	}
	params := data.signedParams
	if len(params) == 0 {
		params = map[string]string{
			"appId":           data.AppID,
			"orderNo":         data.OrderNo,
			"merchantOrderNo": data.MerchantOrderNo,
			"chainType":       data.ChainType,
			"crypto":          data.Crypto,
			"actualCrypto":    data.ActualCrypto,
			"poundage":        data.Poundage,
			"actualPoundage":  data.ActualPoundage,
			"status":          data.Status,
			"createdAt":       data.CreatedAt,
			"completedAt":     data.CompletedAt,
		}
	}
	want := Sign(params, cfg.AppSecret)
	got := strings.ToUpper(strings.TrimSpace(data.Signature))
	if got == "" || len(got) != len(want) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return ErrSignatureInvalid
	}
	return nil
}

func ToPaymentStatus(status string) string {
	switch normalizeStatus(status) {
	case StatusSuccess:
		return constants.PaymentStatusSuccess
	case StatusExpired:
		return constants.PaymentStatusExpired
	case StatusFailed:
		return constants.PaymentStatusFailed
	case StatusPending:
		return constants.PaymentStatusPending
	default:
		return ""
	}
}

func ParseUnixSeconds(value string) *time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	t := time.Unix(seconds, 0)
	return &t
}

func postJSON(ctx context.Context, endpoint string, params map[string]string) (map[string]interface{}, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("%w: encode request failed", ErrRequestFailed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: create request failed", ErrRequestFailed)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: upstream request failed", ErrRequestFailed)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read response failed", ErrRequestFailed)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%w: http status %d", ErrRequestFailed, resp.StatusCode)
	}
	var raw map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: invalid response body", ErrResponseInvalid)
	}
	return raw, nil
}

func responseObject(raw map[string]interface{}) (map[string]interface{}, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: empty response", ErrResponseInvalid)
	}
	if success, ok := raw["success"].(bool); ok && !success {
		return nil, fmt.Errorf("%w: provider rejected request", ErrResponseInvalid)
	}
	if data, ok := raw["data"].(map[string]interface{}); ok {
		return data, nil
	}
	if _, ok := raw["orderNo"]; ok {
		return raw, nil
	}
	if code := strings.TrimSpace(stringValue(raw["code"])); code != "" && code != "0" && code != "200" && !strings.EqualFold(code, "ok") && !strings.EqualFold(code, "success") {
		return nil, fmt.Errorf("%w: provider rejected request", ErrResponseInvalid)
	}
	return nil, fmt.Errorf("%w: data is missing", ErrResponseInvalid)
}

func parseOrder(raw map[string]interface{}) Order {
	return Order{
		AppID:           stringValue(raw["appId"]),
		OrderNo:         stringValue(raw["orderNo"]),
		MerchantOrderNo: stringValue(raw["merchantOrderNo"]),
		ChainType:       stringValue(raw["chainType"]),
		Crypto:          stringValue(raw["crypto"]),
		ActualCrypto:    stringValue(raw["actualCrypto"]),
		Poundage:        stringValue(raw["poundage"]),
		ActualPoundage:  stringValue(raw["actualPoundage"]),
		Status:          normalizeStatus(stringValue(raw["status"])),
		Attach:          stringValue(raw["attach"]),
		CreatedAt:       stringValue(raw["createdAt"]),
		CompletedAt:     stringValue(raw["completedAt"]),
	}
}

func stringValue(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func intValue(value interface{}) int {
	parsed, err := strconv.Atoi(stringValue(value))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func normalizeStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}
	return status
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
