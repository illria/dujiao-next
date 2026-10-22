package application

// CreateInput 创建对接连接输入。
type CreateInput struct {
	Name               string  `json:"name"`
	BaseURL            string  `json:"base_url"`
	ApiKey             string  `json:"api_key"`
	ApiSecret          string  `json:"api_secret"`
	SecretRef          string  `json:"secret_ref"`
	Protocol           string  `json:"protocol"`
	CallbackURL        string  `json:"callback_url"`
	RetryMax           int     `json:"retry_max"`
	RetryIntervals     string  `json:"retry_intervals"`
	ExchangeRate       float64 `json:"exchange_rate"`
	PriceMarkupPercent float64 `json:"price_markup_percent"`
	PriceRoundingMode  string  `json:"price_rounding_mode"`
	AutoSyncPrice      bool    `json:"auto_sync_price"`
	SourceCurrency         string `json:"source_currency"`
	SourceToUSDT           string `json:"source_to_usdt_rate"`
	ExchangeRateBufferBPS  int    `json:"exchange_rate_buffer_bps"`
	MarkupMultiplier       string `json:"markup_multiplier"`
	MinimumOrderAmountUSDT string `json:"minimum_order_amount_usdt"`
	DefaultPricingUnit     string `json:"default_pricing_unit"`
}

// UpdateInput 更新对接连接输入。
type UpdateInput struct {
	Name               string   `json:"name"`
	BaseURL            string   `json:"base_url"`
	ApiKey             string   `json:"api_key"`
	ApiSecret          string   `json:"api_secret"` // 为空则不更新
	SecretRef          string   `json:"secret_ref"`
	Protocol           string   `json:"protocol"`
	CallbackURL        string   `json:"callback_url"`
	RetryMax           int      `json:"retry_max"`
	RetryIntervals     string   `json:"retry_intervals"`
	ExchangeRate       *float64 `json:"exchange_rate"`
	PriceMarkupPercent *float64 `json:"price_markup_percent"` // 指针类型，区分 0 和未传
	PriceRoundingMode  *string  `json:"price_rounding_mode"`
	AutoSyncPrice      *bool    `json:"auto_sync_price"`
	SourceCurrency         *string `json:"source_currency"`
	SourceToUSDT           *string `json:"source_to_usdt_rate"`
	ExchangeRateBufferBPS  *int    `json:"exchange_rate_buffer_bps"`
	MarkupMultiplier       *string `json:"markup_multiplier"`
	MinimumOrderAmountUSDT *string `json:"minimum_order_amount_usdt"`
	DefaultPricingUnit     *string `json:"default_pricing_unit"`
}

// PingResult 连接测试结果。
type PingResult struct {
	SiteName        string                 `json:"site_name"`
	ProtocolVersion string                 `json:"protocol_version"`
	UserID          uint                   `json:"user_id"`
	Balance         string                 `json:"balance"`
	Currency        string                 `json:"currency"`
	MemberLevel     map[string]interface{} `json:"member_level,omitempty"`
}
