package translation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
)

func TestClientTranslateSendsOnlyEnvironmentCredentialAndParsesStructuredResponse(t *testing.T) {
	const apiKey = "test-translation-key"
	var gotBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Fatalf("authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"<think>internal reasoning</think>\\n{\"title_zh_cn\":\"中文标题\",\"title_zh_tw\":\"繁中標題\",\"description_zh_cn\":\"简体说明\",\"description_zh_tw\":\"繁中說明\"}"}}]}`)
	}))
	defer server.Close()

	client := newClient(server.URL+"/v1", "Qwen/Qwen3-8B", apiKey, server.Client())
	got, err := client.Translate(context.Background(), mappingcontract.ProductTranslationRequest{
		Name:        "Original title",
		Description: "Original description",
	})
	if err != nil {
		t.Fatalf("Translate returned error: %v", err)
	}
	if got.TitleZhCN != "中文标题" || got.TitleZhTW != "繁中標題" || got.DescriptionZhCN != "简体说明" || got.DescriptionZhTW != "繁中說明" {
		t.Fatalf("unexpected translation result: %#v", got)
	}
	if gotBody["model"] != "Qwen/Qwen3-8B" {
		t.Fatalf("unexpected model: %#v", gotBody["model"])
	}
	messages, ok := gotBody["messages"].([]interface{})
	if !ok || len(messages) != 2 {
		t.Fatalf("unexpected messages: %#v", gotBody["messages"])
	}
	userMessage, _ := messages[1].(map[string]interface{})
	if !strings.Contains(userMessage["content"].(string), "Original title") {
		t.Fatalf("source title was not sent as translation data: %#v", userMessage)
	}
	systemMessage, _ := messages[0].(map[string]interface{})
	systemPrompt, _ := systemMessage["content"].(string)
	if !strings.Contains(systemPrompt, "完整翻译") || strings.Contains(systemPrompt, "最多三行") {
		t.Fatalf("system prompt does not require faithful full translation: %q", systemPrompt)
	}
}

func TestClientDoesNotExposeResponseBodyOnHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "response contains a secret-looking value")
	}))
	defer server.Close()

	client := newClient(server.URL, "model", "test-key", server.Client())
	_, err := client.Translate(context.Background(), mappingcontract.ProductTranslationRequest{Name: "title"})
	if err == nil || !strings.Contains(err.Error(), "status 502") {
		t.Fatalf("error = %v, want redacted status error", err)
	}
	if strings.Contains(err.Error(), "secret-looking") {
		t.Fatalf("error leaked upstream response body: %v", err)
	}
}

func TestClientRejectsMissingRequiredTranslationFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"title_zh_cn\":\"only one title\"}"}}]}`)
	}))
	defer server.Close()

	client := newClient(server.URL, "model", "test-key", server.Client())
	_, err := client.Translate(context.Background(), mappingcontract.ProductTranslationRequest{Name: "title"})
	if err != ErrEmptyTranslation {
		t.Fatalf("error = %v, want ErrEmptyTranslation", err)
	}
}

func TestValidateTranslationRejectsCompressedDescription(t *testing.T) {
	request := mappingcontract.ProductTranslationRequest{
		Name:        "Nord VPN 3 months Account",
		Description: "Nord VPN 3 months Account\nFormat: Gmail | nvpn Passwort\nIMPORTANT\nTo still be able to log in even if the password is changed, use the Social Login method.\nLog in via browser, open Account Settings, connect Google/Apple, then sign in with Google/Apple.",
	}
	result := mappingcontract.ProductTranslationResult{
		TitleZhCN:       "Nord VPN 3个月账号",
		TitleZhTW:       "Nord VPN 3個月帳號",
		DescriptionZhCN: "交付格式：自动发放账号与登录信息。有效期：3个月。使用提示：请按官方流程登录。",
		DescriptionZhTW: "交付格式：自動發放帳號與登入資訊。有效期：3個月。使用提示：請按官方流程登入。",
	}
	if err := validateTranslation(request, result); err != ErrIncompleteTranslation {
		t.Fatalf("validateTranslation error = %v, want ErrIncompleteTranslation", err)
	}
}

func TestValidateTranslationRejectsGenericDeliveryTemplate(t *testing.T) {
	request := mappingcontract.ProductTranslationRequest{
		Name:        "Autodesk all Apps 1 Year",
		Description: "1 year subscription to Autodesk Student.\nInvitation in customers emails to Autodesk.\nAlmost all apps are included (3ds Max, AutoCAD, Fusion).\nFull warranty.",
	}
	result := mappingcontract.ProductTranslationResult{
		TitleZhCN:       "Autodesk 全家桶（1年）",
		TitleZhTW:       "Autodesk 全家桶（1年）",
		DescriptionZhCN: "交付格式：自动发放账号或激活信息。\n有效期：1年。\n使用提示：请按对应平台官方流程登录、兑换或激活。",
		DescriptionZhTW: "交付格式：自動發放帳號或啟用資訊。\n有效期：1年。\n使用提示：請按對應平台官方流程登入、兌換或啟用。",
	}
	if err := validateTranslation(request, result); err != ErrIncompleteTranslation {
		t.Fatalf("validateTranslation error = %v, want ErrIncompleteTranslation", err)
	}
}

func TestValidateTranslationAllowsLiteralAngleBracketBullets(t *testing.T) {
	request := mappingcontract.ProductTranslationRequest{
		Name:        "Gemini Pro 18Months (link)",
		Description: "> 18 Months Plan\n-> Full Family Account. It's Not an Invite !",
	}
	result := mappingcontract.ProductTranslationResult{
		TitleZhCN:       "Gemini Pro 18个月（链接）",
		TitleZhTW:       "Gemini Pro 18個月（連結）",
		DescriptionZhCN: "> 18个月套餐\n-> 完整家庭账户。这不是邀请！",
		DescriptionZhTW: "> 18個月方案\n-> 完整家庭帳戶。這不是邀請！",
	}
	if err := validateTranslation(request, result); err != nil {
		t.Fatalf("validateTranslation rejected literal bullets: %v", err)
	}
}

func TestValidateTranslationRejectsHTMLMarkup(t *testing.T) {
	request := mappingcontract.ProductTranslationRequest{Name: "title", Description: "plain description"}
	result := mappingcontract.ProductTranslationResult{
		TitleZhCN:       "标题",
		TitleZhTW:       "標題",
		DescriptionZhCN: "<p>说明</p>",
		DescriptionZhTW: "说明",
	}
	if err := validateTranslation(request, result); err != ErrUnsupportedText {
		t.Fatalf("validateTranslation error = %v, want ErrUnsupportedText", err)
	}
}

func TestNewFromEnvUsesConfiguredProviderWithoutPersistingCredential(t *testing.T) {
	t.Setenv(APIKeyEnv, "environment-only-key")
	t.Setenv(BaseURLEnv, "https://example.invalid/v1")
	t.Setenv(ModelEnv, "custom-model")
	t.Setenv(TimeoutEnv, "77")

	client := NewFromEnv()
	if client == nil || client.baseURL != "https://example.invalid/v1" || client.model != "custom-model" {
		t.Fatalf("unexpected environment client: %#v", client)
	}
	if client.apiKey != "environment-only-key" {
		t.Fatalf("environment key was not loaded into the request client")
	}
	if client.httpClient.Timeout != 77*time.Second {
		t.Fatalf("translation timeout = %s, want 77s", client.httpClient.Timeout)
	}
}
