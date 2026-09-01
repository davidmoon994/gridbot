package config

import (
	"encoding/json"
	"os"
)

// ExchangeConfig 仅描述"没有绑定任何真实交易所时"的模拟盘默认参数。
//
// 币安/OKX 的 API Key 不再写在这里——请启动服务后打开 Web 控制台，
// 在"交易所账号"面板里绑定，会保存到 SQLite 数据库并在下次启动时自动恢复，
// 不需要改动这个配置文件。
type ExchangeConfig struct {
	QuoteAsset string `json:"quote_asset"` // 模拟盘的计价货币，如 USDT
	// PaperInitialBalance 模拟盘初始资金
	PaperInitialBalance float64            `json:"paper_initial_balance"`
	PaperInitialPrices  map[string]float64 `json:"paper_initial_prices"`
}

// AppConfig 应用整体配置
type AppConfig struct {
	ListenAddr string         `json:"listen_addr"`
	DBPath     string         `json:"db_path"`
	Exchange   ExchangeConfig `json:"exchange"`
	// TickIntervalSec 网格引擎默认轮询间隔（秒）
	TickIntervalSec int `json:"tick_interval_sec"`

	// TLSCertFile / TLSKeyFile 都非空时，Web 控制台用 HTTPS 提供服务。
	// 只在 127.0.0.1/localhost 本机访问时，明文 HTTP 是安全的（流量不出本机）；
	// 如果打算让局域网内其他设备（比如手机）访问，必须配置证书启用 HTTPS，
	// 否则 API Key、登录密码这些敏感信息会在网络上明文传输。
	// 自签名证书可以用 `openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365 -nodes` 生成。
	TLSCertFile string `json:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file"`
}

// Default 返回一份开箱即用的默认配置：模拟盘 + 常见币种初始价
//
// ListenAddr 默认绑定 127.0.0.1（仅本机可访问），而不是 ":3000"（监听所有网卡，
// 局域网内其他设备也能连上）。如果你确实需要让局域网/其他设备访问，
// 请显式改成 "0.0.0.0:3000" 或具体网卡IP，并同时配置 tls_cert_file/tls_key_file
// 启用 HTTPS，否则登录密码和交易所 API Key 会在网络上明文传输。
func Default() AppConfig {
	return AppConfig{
		ListenAddr: "127.0.0.1:3000",
		DBPath:     "gridbot.db",
		Exchange: ExchangeConfig{
			QuoteAsset:          "USDT",
			PaperInitialBalance: 10000,
			PaperInitialPrices: map[string]float64{
				"BTCUSDT": 96000,
				"ETHUSDT": 3400,
				"SOLUSDT": 210,
			},
		},
		TickIntervalSec: 5,
	}
}

// Load 从文件加载配置；文件不存在时返回默认配置并写出一份示例文件
func Load(path string) (AppConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := Default()
		_ = Save(path, cfg)
		return cfg, nil
	}
	if err != nil {
		return AppConfig{}, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return AppConfig{}, err
	}
	return cfg, nil
}

// Save 写出配置文件
func Save(path string, cfg AppConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}
