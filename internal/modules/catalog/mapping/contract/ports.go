package contract

import (
	"context"
	"time"

	categoryapp "github.com/dujiao-next/internal/modules/catalog/category/application"
	categorydomain "github.com/dujiao-next/internal/modules/catalog/category/domain"
	settingsintegration "github.com/dujiao-next/internal/modules/settings/schema/integration"
	siteconnectiondomain "github.com/dujiao-next/internal/modules/siteconnection/domain"
	"github.com/dujiao-next/internal/upstream"
)

// ConnectionProvider 隔离连接读取与上游协议适配器构造。
type ConnectionProvider interface {
	GetByID(id uint) (*siteconnectiondomain.Connection, error)
	GetAdapter(conn *siteconnectiondomain.Connection) (upstream.Adapter, error)
}

// MediaRecorder 记录导入后落盘的上游媒体。
type MediaRecorder interface {
	RecordLocalFile(ctx context.Context, localPath, scene string)
}

// CategoryCreator 自动创建导入商品所需分类。
type CategoryCreator interface {
	Create(input categoryapp.UpsertInput) (*categorydomain.Category, error)
}

// SettingsProvider 提供上游同步动态配置。
type SettingsProvider interface {
	GetUpstreamSyncConfig(fallbackInterval string) (settingsintegration.UpstreamSyncConfig, error)
	GetUpstreamSyncInterval(fallbackInterval string) (time.Duration, error)
}

// ProductTranslationRequest 是上游商品文案翻译端口的输入。
// 上游文本被视为不可信数据，适配器必须将其作为待翻译内容传递，不能将其
// 当作系统指令执行。
type ProductTranslationRequest struct {
	Name        string
	Description string
}

// ProductTranslationResult 保存翻译模型返回的中文文案。
// 英文原文由同步用例直接写入 en-US，不接受模型返回的英文作为事实来源。
type ProductTranslationResult struct {
	TitleZhCN       string
	TitleZhTW       string
	DescriptionZhCN string
	DescriptionZhTW string
}

// ProductTranslator 是商品同步使用的翻译端口。
// 真实密钥由基础设施适配器从进程环境变量读取，不进入连接记录或数据库。
type ProductTranslator interface {
	Translate(ctx context.Context, request ProductTranslationRequest) (ProductTranslationResult, error)
}
