// GridBot：移动网格自动交易机器人
//
// 架构参考 NOFX（https://github.com/NoFxAiOS/nofx）的"策略提议 + 代码层风控强制执行"
// 模式，但决策层不是大模型，而是经典的移动网格算法：
//   - 网格中心跟随 EMA 移动
//   - 网格间距按 ATR 动态调整
//   - 价格偏离网格过远时自动重新居中
//   - 所有下单都必须经过独立的风控引擎（杠杆、仓位、保证金、回撤、熔断）
//
// 交易所 API Key 不写在配置文件/代码里，而是通过 Web 控制台绑定/解绑，
// 保存在本地 SQLite 数据库中，重启后自动按上次绑定的交易所恢复连接。
// Web 控制台本身需要账号密码登录才能访问。
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"gridbot/auth"
	"gridbot/config"
	"gridbot/exchange"
	"gridbot/exfactory"
	"gridbot/manager"
	"gridbot/risk"
	"gridbot/server"
	"gridbot/store"
	"gridbot/strategy"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	authMgr := auth.New(st)
	riskEngine := risk.NewEngine(risk.DefaultLimits())
	mgr := manager.New(riskEngine, st)

	startupExchange(mgr, st, cfg)

	// 从数据库恢复此前保存的网格配置（重启后自动续跑）
	restoreGrids(mgr, st, time.Duration(cfg.TickIntervalSec)*time.Second)

	srv := server.New(mgr, authMgr)
	if hasAccount, _ := authMgr.HasAccount(); !hasAccount {
		log.Printf("尚未创建管理账户，首次访问会自动跳转到初始化页面 /setup.html")
	}
	log.Printf("当前交易所: %s", mgr.ExchangeID())

	useTLS := cfg.TLSCertFile != "" && cfg.TLSKeyFile != ""
	warnIfExposedWithoutTLS(cfg.ListenAddr, useTLS)

	if useTLS {
		log.Printf("GridBot 启动，Web 控制台: %s", displayAddr("https", cfg.ListenAddr))
		if err := http.ListenAndServeTLS(cfg.ListenAddr, cfg.TLSCertFile, cfg.TLSKeyFile, srv.Handler()); err != nil {
			log.Fatalf("HTTPS 服务退出: %v", err)
		}
		return
	}

	log.Printf("GridBot 启动，Web 控制台: %s", displayAddr("http", cfg.ListenAddr))
	if err := http.ListenAndServe(cfg.ListenAddr, srv.Handler()); err != nil {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}

// displayAddr 把监听地址拼成一个可以直接在浏览器打开的URL。
// ListenAddr 可能是 ":3000"（仅端口）或 "127.0.0.1:3000"（含主机名）两种写法，
// 统一处理成正确的展示形式，避免出现 "127.0.0.1127.0.0.1:3000" 这种重复拼接。
func displayAddr(scheme, listenAddr string) string {
	if strings.HasPrefix(listenAddr, ":") {
		return scheme + "://127.0.0.1" + listenAddr
	}
	return scheme + "://" + listenAddr
}

// warnIfExposedWithoutTLS 在监听地址明显不是"仅本机可访问"、又没有配置 TLS 证书时，
// 打印一条醒目的警告。API Key、登录密码这些敏感信息在没有 TLS 的情况下会明文
// 走网络，只在 127.0.0.1/localhost（流量根本不出本机）时才是安全的。
func warnIfExposedWithoutTLS(listenAddr string, useTLS bool) {
	if useTLS {
		return
	}
	isLocalOnly := strings.HasPrefix(listenAddr, "127.0.0.1:") ||
		strings.HasPrefix(listenAddr, "localhost:") ||
		strings.HasPrefix(listenAddr, "[::1]:")
	if isLocalOnly {
		return
	}
	log.Printf("=====================================================================")
	log.Printf("⚠️  警告：监听地址 %s 不是仅本机可访问，但没有配置 TLS 证书。", listenAddr)
	log.Printf("⚠️  这意味着登录密码、交易所 API Key 等敏感信息会以明文形式在网络中传输，")
	log.Printf("⚠️  任何能监听到这段网络流量的人都可能截获它们。")
	log.Printf("⚠️  如果只在本机使用，请把 listen_addr 改回 \"127.0.0.1:端口\"；")
	log.Printf("⚠️  如果确实需要让局域网/公网访问，请在配置文件中设置 tls_cert_file/tls_key_file 启用 HTTPS。")
	log.Printf("=====================================================================")
}

// startupExchange 决定进程启动时用哪个交易所：
//  1. 如果数据库里记录了"当前生效交易所"且对应凭证还在，就用它重新连接；
//  2. 否则退化为配置文件里的模拟盘（paper）默认设置。
//
// 之后运行期间可以随时通过 Web 控制台绑定/解绑/切换，不需要重启进程。
func startupExchange(mgr *manager.Manager, st *store.Store, cfg config.AppConfig) {
	activeType, err := st.GetActiveExchange()
	if err != nil {
		log.Printf("读取当前生效交易所失败: %v", err)
	}
	if activeType != "" && activeType != "paper" {
		cred, err := st.GetCredential(activeType)
		if err != nil {
			log.Printf("读取交易所凭证失败: %v", err)
		}
		if cred != nil {
			ex, err := exfactory.Build(*cred)
			if err != nil {
				log.Printf("恢复交易所 %s 连接失败，退回模拟盘: %v", activeType, err)
			} else {
				mgr.SetExchange(ex, activeType, cred.QuoteAsset)
				return
			}
		}
	}

	// 退化为模拟盘
	paperEx := exchange.NewPaperExchange(cfg.Exchange.PaperInitialPrices, cfg.Exchange.QuoteAsset, cfg.Exchange.PaperInitialBalance)
	paperEx.StartAutoTick(2 * time.Second)
	mgr.SetExchange(paperEx, "paper", cfg.Exchange.QuoteAsset)
}

func restoreGrids(mgr *manager.Manager, st *store.Store, defaultInterval time.Duration) {
	// 简化实现：真实项目建议在 store 中记录每个 symbol 的 tick_interval 一并恢复。
	// 这里默认按配置的全局 TickIntervalSec 恢复所有先前启用的网格。
	configs, enabled, err := st.LoadGridConfigs()
	if err != nil {
		log.Printf("恢复网格配置失败: %v", err)
		return
	}
	for symbol, raw := range configs {
		if !enabled[symbol] {
			continue
		}
		var cfg strategy.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			log.Printf("解析网格配置失败 symbol=%s: %v", symbol, err)
			continue
		}
		if _, err := mgr.StartGrid(cfg, defaultInterval); err != nil {
			log.Printf("恢复网格失败 symbol=%s: %v", symbol, err)
			continue
		}
		log.Printf("已恢复网格: %s", symbol)
	}
}
