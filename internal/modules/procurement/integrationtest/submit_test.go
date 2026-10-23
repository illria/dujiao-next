package procurement_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dujiao-next/internal/constants"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	procurementcontract "github.com/dujiao-next/internal/modules/procurement/contract"
	siteconnectionapp "github.com/dujiao-next/internal/modules/siteconnection/application"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

// ── SubmitToUpstream tests ──

func TestSubmitToUpstream_Success(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-SUBMIT-001", constants.OrderStatusPaid, constants.FulfillmentTypeUpstream)
	// 创建 product mapping 和 sku mapping
	pm := &mappingdomain.Mapping{
		ConnectionID:      1,
		LocalProductID:    1,
		UpstreamProductID: 101,
		IsActive:          true,
	}
	db.Create(pm)
	sm := &mappingdomain.SKUMapping{
		ProductMappingID: pm.ID,
		LocalSKUID:       1,
		UpstreamSKUID:    201,
		UpstreamIsActive: true,
	}
	db.Create(sm)

	// mock upstream server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"order_id": 999,
			"order_no": "UP-999",
			"status":   "accepted",
			"amount":   "50.00",
			"currency": "CNY",
		})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, err := connSvc.Create(siteconnectionapp.CreateInput{
		Name:      "test-upstream",
		BaseURL:   server.URL,
		ApiKey:    "key",
		ApiSecret: "secret",
		Protocol:  constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")

	svc := newTestProcurementService(db, connSvc)

	if err := svc.SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("SubmitToUpstream: %v", err)
	}

	// 验证采购单状态 = accepted
	var updatedProc ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "accepted" {
		t.Errorf("expected procurement status 'accepted', got %q", updatedProc.Status)
	}
	if updatedProc.UpstreamOrderID != 999 {
		t.Errorf("expected upstream_order_id=999, got %d", updatedProc.UpstreamOrderID)
	}

	// 验证本地订单状态 = fulfilling
	var updatedOrder orderdomain.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusFulfilling {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusFulfilling, updatedOrder.Status)
	}
}

func TestSubmitToUpstream_NonRetryableError_Rejects(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-NONRETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	pm := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	sm := &mappingdomain.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true}
	db.Create(sm)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            false,
			"error_code":    "product_out_of_stock",
			"error_message": "product out of stock",
		})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, _ := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
	})

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")
	svc := newTestProcurementService(db, connSvc)

	// 不可重试错误应返回 error
	_ = svc.SubmitToUpstream(proc.ID)

	// 验证采购单状态 = rejected
	var updatedProc ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "rejected" {
		t.Errorf("expected procurement status 'rejected', got %q", updatedProc.Status)
	}

	// 验证本地订单状态回退到 paid
	var updatedOrder orderdomain.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q after rejection, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

func TestSubmitToUpstream_RetryableError_Retries(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-RETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	pm := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	sm := &mappingdomain.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true}
	db.Create(sm)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":            false,
			"error_code":    "server_error",
			"error_message": "temporary failure",
		})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, _ := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
		RetryMax: 3,
	})

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")
	svc := newTestProcurementService(db, connSvc)

	// 可重试错误不应返回 error（已入队重试）
	if err := svc.SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("expected no error for retryable failure, got: %v", err)
	}

	// 验证采购单状态 = failed（而非 rejected）
	var updatedProc ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "failed" {
		t.Errorf("expected procurement status 'failed', got %q", updatedProc.Status)
	}
	if updatedProc.RetryCount != 1 {
		t.Errorf("expected retry_count=1, got %d", updatedProc.RetryCount)
	}
}

func TestSubmitToUpstream_SMMFailureBecomesUnknownWithoutRollbackOrRetry(t *testing.T) {
	t.Setenv(constants.ConnectionSecretRefSMMPanel, "fixture")
	t.Setenv("SMMPANEL_ORDERING_ENABLED", "true")
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-SMM-UNKNOWN-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	var item orderdomain.OrderItem
	if err := db.Where("order_id = ?", order.ID).First(&item).Error; err != nil {
		t.Fatalf("load order item: %v", err)
	}
	item.Quantity = 1000
	item.ManualFormSubmissionJSON = jsonmap.JSON{"link": "https://example.com/post/1"}
	item.UpstreamRateSnapshot = "10.85"
	item.UpstreamPricingUnit = constants.PricingUnitPer1000
	item.UpstreamCostSnapshot = "10.85"
	item.UpstreamSellSnapshot = "21.70"
	if err := db.Save(&item).Error; err != nil {
		t.Fatalf("save SMM order snapshot: %v", err)
	}

	productMapping := &mappingdomain.Mapping{
		ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 36,
		UpstreamRateText: "10.85", PricingUnit: constants.PricingUnitPer1000,
		UpstreamServiceType: "Default", MinQuantity: 500, MaxQuantity: 200000,
		IsOrderable: true, IsActive: true,
	}
	if err := db.Create(productMapping).Error; err != nil {
		t.Fatalf("create product mapping: %v", err)
	}
	if err := db.Create(&mappingdomain.SKUMapping{
		ProductMappingID: productMapping.ID, LocalSKUID: 1, UpstreamSKUID: 36,
		UpstreamRateText: "10.85", PricingUnit: constants.PricingUnitPer1000,
		UpstreamIsActive: true, UpstreamStock: -1,
	}).Error; err != nil {
		t.Fatalf("create SMM SKU mapping: %v", err)
	}

	servicesCalls, addCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("action") {
		case "services":
			servicesCalls++
			_, _ = w.Write([]byte(`[{"service":"36","name":"Telegram","type":"Default","rate":"10.85","min":"500","max":"200000"}]`))
		case "add":
			addCalls++
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"error":"gateway timeout"}`))
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, err := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-smm", BaseURL: server.URL,
		Protocol: constants.ConnectionProtocolSMMPanel,
		SourceCurrency: "USD", SourceToUSDT: "1",
		MarkupMultiplier: "2", MinimumOrderAmountUSDT: "1",
		DefaultPricingUnit: constants.PricingUnitPer1000,
	})
	if err != nil {
		t.Fatalf("create SMM connection: %v", err)
	}
	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, constants.ProcurementStatusPending)
	if err := db.Model(proc).Updates(map[string]interface{}{
		"upstream_rate_snapshot": "10.85",
		"upstream_pricing_unit":  constants.PricingUnitPer1000,
		"upstream_cost_snapshot": "10.85",
		"upstream_sell_snapshot": "21.70",
	}).Error; err != nil {
		t.Fatalf("save procurement snapshot: %v", err)
	}

	if err := newTestProcurementService(db, connSvc).SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("SubmitToUpstream: %v", err)
	}

	var updatedProc ProcurementOrder
	if err := db.First(&updatedProc, proc.ID).Error; err != nil {
		t.Fatalf("reload procurement order: %v", err)
	}
	var updatedOrder orderdomain.Order
	if err := db.First(&updatedOrder, order.ID).Error; err != nil {
		t.Fatalf("reload local order: %v", err)
	}
	if updatedProc.Status != constants.ProcurementStatusUnknown || updatedProc.RetryCount != 0 || updatedProc.NextRetryAt != nil {
		t.Fatalf("expected unknown without retry, got status=%q retries=%d next=%v", updatedProc.Status, updatedProc.RetryCount, updatedProc.NextRetryAt)
	}
	if updatedOrder.Status != constants.OrderStatusFulfilling {
		t.Fatalf("SMM unknown outcome must not roll local order back, got %q", updatedOrder.Status)
	}
	if servicesCalls != 1 || addCalls != 1 {
		t.Fatalf("expected one preflight and one non-retried add, services=%d add=%d", servicesCalls, addCalls)
	}
}

func TestHandleSubmitFailure_MaxRetriesExhausted(t *testing.T) {
	db := setupProcurementTestDB(t)

	order := createProcTestOrder(t, db, "PROC-MAXRETRY-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	productMapping := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(productMapping)
	db.Create(&mappingdomain.SKUMapping{
		ProductMappingID: productMapping.ID,
		LocalSKUID:       1,
		UpstreamSKUID:    201,
		UpstreamIsActive: true,
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":            false,
			"error_code":    "server_error",
			"error_message": "timeout after retries",
		})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, err := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
		RetryMax: 2, RetryIntervals: "[30,60]",
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "failed")
	// 设置 retry_count 已达上限
	db.Model(proc).Update("retry_count", 2)

	svc := newTestProcurementService(db, connSvc)

	// 通过公开提交入口验证：可重试错误在次数耗尽后仍必须转为 rejected。
	_ = svc.SubmitToUpstream(proc.ID)

	// 验证采购单状态 = rejected
	var updatedProc ProcurementOrder
	db.First(&updatedProc, proc.ID)
	if updatedProc.Status != "rejected" {
		t.Errorf("expected procurement status 'rejected', got %q", updatedProc.Status)
	}

	// 验证本地订单回退到 paid
	var updatedOrder orderdomain.Order
	db.First(&updatedOrder, order.ID)
	if updatedOrder.Status != constants.OrderStatusPaid {
		t.Errorf("expected order status %q, got %q", constants.OrderStatusPaid, updatedOrder.Status)
	}
}

func TestCancelManual_AcceptedKeepsStatusWhenUpstreamRefuses(t *testing.T) {
	db := setupProcurementTestDB(t)
	order := createProcTestOrder(t, db, "PROC-CANCEL-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)

	// 上游已支付的订单不允许取消
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": "cancel_not_allowed"})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, err := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL, ApiKey: "key", ApiSecret: "secret",
		Protocol: constants.ConnectionProtocolDujiaoNext,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, constants.ProcurementStatusAccepted)
	db.Model(proc).Update("upstream_order_id", 999)

	svc := newTestProcurementService(db, connSvc)
	if err := svc.CancelManual(proc.ID); !errors.Is(err, procurementcontract.ErrUpstreamCancelRefused) {
		t.Fatalf("expected ErrUpstreamCancelRefused, got %v", err)
	}
	var reloaded ProcurementOrder
	db.First(&reloaded, proc.ID)
	if reloaded.Status != constants.ProcurementStatusAccepted {
		t.Fatalf("expected procurement to stay accepted, got %q", reloaded.Status)
	}
}

// 上游以非 200 + 错误码拒单（如余额不足）时按错误码判断，不可重试的直接拒绝，不反复打上游。
func TestSubmitToUpstream_HTTPErrorWithNonRetryableCode_Rejects(t *testing.T) {
	db := setupProcurementTestDB(t)
	order := createProcTestOrder(t, db, "PROC-HTTP402-001", constants.OrderStatusFulfilling, constants.FulfillmentTypeUpstream)
	pm := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	db.Create(&mappingdomain.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": "insufficient_balance", "error_message": "balance not enough"})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, _ := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
	})
	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending")
	_ = newTestProcurementService(db, connSvc).SubmitToUpstream(proc.ID)

	var updated ProcurementOrder
	db.First(&updated, proc.ID)
	if updated.Status != constants.ProcurementStatusRejected || updated.RetryCount != 0 {
		t.Fatalf("expected rejected without retries, got status=%q retry_count=%d", updated.Status, updated.RetryCount)
	}
}

// 本地订单已退款时（如巡检补投历史积压单），不能再去上游下单扣款。
func TestSubmitToUpstream_LocalOrderRefunded_RejectsWithoutCallingUpstream(t *testing.T) {
	db := setupProcurementTestDB(t)
	order := createProcTestOrder(t, db, "PROC-REFUNDED-001", constants.OrderStatusRefunded, constants.FulfillmentTypeUpstream)
	pm := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
	db.Create(pm)
	db.Create(&mappingdomain.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true})

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "order_id": 999, "order_no": "UP-999"})
	}))
	defer server.Close()

	connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
	conn, _ := connSvc.Create(siteconnectionapp.CreateInput{
		Name: "test-upstream", BaseURL: server.URL,
		ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
	})
	proc := createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "failed")
	if err := newTestProcurementService(db, connSvc).SubmitToUpstream(proc.ID); err != nil {
		t.Fatalf("SubmitToUpstream: %v", err)
	}

	var updated ProcurementOrder
	db.First(&updated, proc.ID)
	if called || updated.Status != constants.ProcurementStatusRejected {
		t.Fatalf("expected rejected without upstream call, got called=%v status=%q", called, updated.Status)
	}
}

// 下单响应回来前采购单已被回调推进或被管理员取消：不回退状态，但必须补记上游订单号供对账与退款同步。
func TestSubmitToUpstream_StatusChangedDuringRequest_KeepsStatusAndRecordsUpstreamOrder(t *testing.T) {
	for _, concurrent := range []string{constants.ProcurementStatusFulfilled, constants.ProcurementStatusCanceled} {
		t.Run(concurrent, func(t *testing.T) {
			db := setupProcurementTestDB(t)
			order := createProcTestOrder(t, db, "PROC-RACE-"+concurrent, constants.OrderStatusPaid, constants.FulfillmentTypeUpstream)
			pm := &mappingdomain.Mapping{ConnectionID: 1, LocalProductID: 1, UpstreamProductID: 101, IsActive: true}
			db.Create(pm)
			db.Create(&mappingdomain.SKUMapping{ProductMappingID: pm.ID, LocalSKUID: 1, UpstreamSKUID: 201, UpstreamIsActive: true})

			var procID uint
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				db.Model(&ProcurementOrder{}).Where("id = ?", procID).Update("status", concurrent)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "order_id": 999, "order_no": "UP-999", "amount": "50.00", "currency": "CNY"})
			}))
			defer server.Close()

			connSvc := newTestSiteConnectionService(db, "test-key", t.TempDir())
			conn, _ := connSvc.Create(siteconnectionapp.CreateInput{
				Name: "test-upstream", BaseURL: server.URL,
				ApiKey: "key", ApiSecret: "secret", Protocol: constants.ConnectionProtocolDujiaoNext,
			})
			procID = createTestProcurementOrder(t, db, conn.ID, order.ID, order.OrderNo, "pending").ID
			if err := newTestProcurementService(db, connSvc).SubmitToUpstream(procID); err != nil {
				t.Fatalf("SubmitToUpstream: %v", err)
			}

			var updated ProcurementOrder
			db.First(&updated, procID)
			if updated.Status != concurrent || updated.UpstreamOrderID != 999 || updated.UpstreamOrderNo != "UP-999" {
				t.Fatalf("expected status=%q with upstream order recorded, got status=%q upstream_order_id=%d no=%q",
					concurrent, updated.Status, updated.UpstreamOrderID, updated.UpstreamOrderNo)
			}
		})
	}
}
