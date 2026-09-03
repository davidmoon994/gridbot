// Package server 提供 REST API 和内嵌 Web 控制台。
//
// 所有路由（除了登录/初始化相关的少数几个）都必须携带有效会话 Cookie 才能访问，
// 由 authGate 中间件统一把关：没有管理账户时强制跳到 /setup.html，
// 有账户但未登录时强制跳到 /login.html，API 请求则直接返回 401。
package server

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gridbot/auth"
	"gridbot/exchange"
	"gridbot/exfactory"
	"gridbot/manager"
	"gridbot/store"
	"gridbot/strategy"
)

//go:embed webroot
var webrootEmbed embed.FS

const sessionCookieName = "gridbot_session"

// Server 封装 HTTP 服务
type Server struct {
	mgr     *manager.Manager
	authMgr *auth.Manager
	mux     *http.ServeMux
}

// New 创建 Server 并注册所有路由
func New(mgr *manager.Manager, authMgr *auth.Manager) *Server {
	s := &Server{mgr: mgr, authMgr: authMgr, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回可直接用于 http.ListenAndServe 的 handler，外层套一层认证网关
func (s *Server) Handler() http.Handler { return s.authGate(s.mux) }

// isPublicPath 判断某个请求路径是否无需登录即可访问：
// 登录页/初始化页本身，以及登录/初始化/登录状态查询这几个认证相关的API。
func isPublicPath(path string) bool {
	if path == "/login.html" || path == "/setup.html" {
		return true
	}
	return strings.HasPrefix(path, "/api/auth/")
}

func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}

func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// authGate 是认证网关中间件：见文件头注释
func (s *Server) authGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if isPublicPath(path) {
			next.ServeHTTP(w, r)
			return
		}

		hasAccount, err := s.authMgr.HasAccount()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "读取账户状态失败: "+err.Error())
			return
		}
		if !hasAccount {
			if isAPIPath(path) {
				writeError(w, http.StatusUnauthorized, "尚未创建管理账户，请先访问 /setup.html 完成初始化")
				return
			}
			http.Redirect(w, r, "/setup.html", http.StatusFound)
			return
		}

		if !s.authMgr.ValidSession(s.sessionToken(r)) {
			if isAPIPath(path) {
				writeError(w, http.StatusUnauthorized, "未登录或会话已过期，请重新登录")
				return
			}
			http.Redirect(w, r, "/login.html", http.StatusFound)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	sub, err := fs.Sub(webrootEmbed, "webroot")
	if err == nil {
		s.mux.Handle("/", http.FileServer(http.FS(sub)))
	}

	// ---- 认证相关（无需登录即可访问） ----
	s.mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("POST /api/auth/setup", s.handleAuthSetup)
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("POST /api/auth/change-password", s.handleChangePassword)

	// ---- 交易所凭证绑定/解绑（需要登录） ----
	s.mux.HandleFunc("GET /api/exchanges", s.handleListExchanges)
	s.mux.HandleFunc("POST /api/exchanges", s.handleBindExchange)
	s.mux.HandleFunc("DELETE /api/exchanges/{type}", s.handleUnbindExchange)
	s.mux.HandleFunc("POST /api/exchanges/{type}/activate", s.handleActivateExchange)

	// ---- 网格与行情（需要登录） ----
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/grids", s.handleListGrids)
	s.mux.HandleFunc("POST /api/grids", s.handleCreateGrid)
	s.mux.HandleFunc("POST /api/grids/{symbol}/stop", s.handleStopGrid)
	s.mux.HandleFunc("GET /api/grids/{symbol}/snapshot", s.handleSnapshot)
	s.mux.HandleFunc("GET /api/grids/{symbol}/events", s.handleEvents)
	s.mux.HandleFunc("GET /api/grids/{symbol}/pnl-history", s.handlePnLHistory)
	s.mux.HandleFunc("GET /api/ticker/{symbol}", s.handleTicker)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- 认证相关 handler ----

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	hasAccount, err := s.authMgr.HasAccount()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	loggedIn := hasAccount && s.authMgr.ValidSession(s.sessionToken(r))
	writeJSON(w, http.StatusOK, map[string]bool{"has_account": hasAccount, "logged_in": loggedIn})
}

type authSetupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req authSetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.authMgr.CreateAccount(req.Username, req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, err := s.authMgr.NewSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req authSetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	ok, err := s.authMgr.VerifyLogin(req.Username, req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token, err := s.authMgr.NewSession()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.authMgr.Logout(s.sessionToken(r))
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if err := s.authMgr.ChangePassword(req.OldPassword, req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
}

// ---- 交易所凭证绑定/解绑 handler ----

// maskedCredential 是返回给前端的脱敏视图，绝不回传完整的 api_secret/passphrase
type maskedCredential struct {
	ExchangeType string `json:"exchange_type"`
	APIKeyMasked string `json:"api_key_masked"`
	QuoteAsset   string `json:"quote_asset"`
	Testnet      bool   `json:"testnet"`
	HedgeMode    bool   `json:"hedge_mode"`
	Active       bool   `json:"active"`
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func (s *Server) handleListExchanges(w http.ResponseWriter, r *http.Request) {
	creds, err := s.mgr.Store().ListCredentials()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activeType := s.mgr.ExchangeID()
	activeTestnet := s.mgr.IsTestnet()
	out := make([]maskedCredential, 0, len(creds))
	for _, c := range creds {
		out = append(out, maskedCredential{
			ExchangeType: c.ExchangeType,
			APIKeyMasked: maskAPIKey(c.APIKey),
			QuoteAsset:   c.QuoteAsset,
			Testnet:      c.Testnet,
			HedgeMode:    c.HedgeMode,
			Active:       c.ExchangeType == activeType && c.Testnet == activeTestnet,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current_exchange": activeType,
		"current_testnet":  activeTestnet,
		"supported_types":  exfactory.SupportedExchangeTypes,
		"credentials":      out,
	})
}

type bindExchangeRequest struct {
	ExchangeType string `json:"exchange_type"`
	APIKey       string `json:"api_key"`
	APISecret    string `json:"api_secret"`
	Passphrase   string `json:"passphrase"` // OKX 需要，Binance 忽略
	QuoteAsset   string `json:"quote_asset"`
	Testnet      bool   `json:"testnet"`
	HedgeMode    bool   `json:"hedge_mode"`
}

// handleBindExchange 绑定一个交易所的 API Key：保存凭证到数据库，
// 并立即尝试连接、切换为当前生效交易所（相当于"绑定即启用"）。
// 如果连接测试失败（比如 Key 填错了），凭证仍会保存，但不会切换过去，
// 便于用户排查问题后重新激活，而不用重新输入一遍。
//
// 测试网和实盘各自独立保存一份（唯一键是 exchange_type + testnet 的组合），
// 绑定测试网凭证不会覆盖已经绑定好的实盘凭证，之后可以用 handleActivateExchange
// 在两者之间一键切换，不需要重新输入 Key。
func (s *Server) handleBindExchange(w http.ResponseWriter, r *http.Request) {
	var req bindExchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !exfactory.IsSupported(req.ExchangeType) {
		writeError(w, http.StatusBadRequest, "不支持的交易所类型: "+req.ExchangeType)
		return
	}
	if req.APIKey == "" || req.APISecret == "" {
		writeError(w, http.StatusBadRequest, "API Key / Secret 不能为空")
		return
	}
	if req.QuoteAsset == "" {
		req.QuoteAsset = "USDT"
	}

	cred := store.ExchangeCredential{
		ExchangeType: req.ExchangeType,
		APIKey:       req.APIKey,
		APISecret:    req.APISecret,
		Passphrase:   req.Passphrase,
		QuoteAsset:   req.QuoteAsset,
		Testnet:      req.Testnet,
		HedgeMode:    req.HedgeMode,
	}
	if err := s.mgr.Store().SaveCredential(cred); err != nil {
		writeError(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}

	ex, err := exfactory.Build(cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 连接测试：拉一次余额，确认 Key/Secret/Passphrase 有效且权限足够
	if _, testErr := ex.GetBalances(r.Context()); testErr != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":      false,
			"saved":   true,
			"warning": "凭证已保存，但连接测试失败，尚未切换为当前交易所: " + testErr.Error(),
		})
		return
	}

	s.mgr.SetExchange(ex, req.ExchangeType, req.Testnet, req.QuoteAsset)
	_ = s.mgr.Store().SetActiveExchange(req.ExchangeType, req.Testnet)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUnbindExchange 解绑某个交易所（测试网/实盘分别解绑）：删除保存的凭证；
// 如果当前正在使用它，自动退回模拟盘（不会自动撤单/平仓，请解绑前自行确认没有未处理的仓位）。
func (s *Server) handleUnbindExchange(w http.ResponseWriter, r *http.Request) {
	exType := r.PathValue("type")
	testnet := r.URL.Query().Get("testnet") == "true"
	if err := s.mgr.Store().DeleteCredential(exType, testnet); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.mgr.ExchangeID() == exType && s.mgr.IsTestnet() == testnet {
		fallbackToPaper(s.mgr)
		_ = s.mgr.Store().SetActiveExchange("paper", false)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleActivateExchange 切换到某个此前已经绑定过的交易所（不需要重新输入 Key）。
// testnet 查询参数决定激活的是测试网那份凭证还是实盘那份——这就是"控制台一键切换
// 测试网/实盘"的核心：两份凭证一直都保存着，这里只是换一下 Manager 当前指向哪一份。
func (s *Server) handleActivateExchange(w http.ResponseWriter, r *http.Request) {
	exType := r.PathValue("type")
	testnet := r.URL.Query().Get("testnet") == "true"
	cred, err := s.mgr.Store().GetCredential(exType, testnet)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cred == nil {
		mode := "实盘"
		if testnet {
			mode = "测试网"
		}
		writeError(w, http.StatusNotFound, "尚未绑定该交易所的"+mode+"凭证，请先绑定")
		return
	}
	ex, err := exfactory.Build(*cred)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.SetExchange(ex, exType, testnet, cred.QuoteAsset)
	_ = s.mgr.Store().SetActiveExchange(exType, testnet)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// fallbackToPaper 解绑当前生效交易所后的兜底方案：切回内置模拟盘，
// 用一组常见交易对的默认初始价格，避免解绑后交易所字段变成不可用的空值。
func fallbackToPaper(mgr *manager.Manager) {
	defaultPrices := map[string]float64{
		"BTCUSDT": 96000,
		"ETHUSDT": 3400,
		"SOLUSDT": 210,
	}
	paperEx := exchange.NewPaperExchange(defaultPrices, "USDT", 10000)
	paperEx.StartAutoTick(2 * time.Second)
	mgr.SetExchange(paperEx, "paper", false, "USDT")
}

// ---- 网格与行情 handler ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawEx := s.mgr.RawExchange()
	balances, err := rawEx.GetBalances(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"exchange": s.mgr.ExchangeID(),
		"testnet":  s.mgr.IsTestnet(),
		"balances": balances,
		"symbols":  s.mgr.ListSymbols(),
	})
}

func (s *Server) handleListGrids(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	symbols := s.mgr.ListSymbols()
	type item struct {
		Symbol   string            `json:"symbol"`
		Running  bool              `json:"running"`
		Snapshot strategy.Snapshot `json:"snapshot"`
	}
	out := make([]item, 0, len(symbols))
	for _, sym := range symbols {
		snap, _ := s.mgr.Snapshot(ctx, sym)
		out = append(out, item{Symbol: sym, Running: s.mgr.IsRunning(sym), Snapshot: snap})
	}
	writeJSON(w, http.StatusOK, out)
}

// createGridRequest 是 POST /api/grids 的请求体，字段与 strategy.Config 对应，
// 附带合理默认值，前端只需填写关键参数。
type createGridRequest struct {
	Symbol                 string  `json:"symbol"`
	GridCount              int     `json:"grid_count"`
	EMAPeriod              int     `json:"ema_period"`
	ATRPeriod              int     `json:"atr_period"`
	ATRSpacingMultiplier   float64 `json:"atr_spacing_multiplier"`
	MinSpacingPercent      float64 `json:"min_spacing_percent"`
	MaxSpacingPercent      float64 `json:"max_spacing_percent"`
	RecenterThresholdGrids float64 `json:"recenter_threshold_grids"`
	MinRecenterIntervalSec int     `json:"min_recenter_interval_sec"`
	PerGridQuoteAmount     float64 `json:"per_grid_quote_amount"`
	Leverage               float64 `json:"leverage"`
	Mode                   string  `json:"mode"` // "long_only" | "neutral"
	MaxTotalPositionQuote  float64 `json:"max_total_position_quote"`
	TickIntervalSec        int     `json:"tick_interval_sec"`
}

func (req createGridRequest) toConfig() strategy.Config {
	mode := strategy.ModeLongOnly
	if req.Mode == string(strategy.ModeNeutral) {
		mode = strategy.ModeNeutral
	}
	cfg := strategy.Config{
		Symbol:                 req.Symbol,
		GridCount:              orDefaultInt(req.GridCount, 8),
		EMAPeriod:              orDefaultInt(req.EMAPeriod, 20),
		ATRPeriod:              orDefaultInt(req.ATRPeriod, 14),
		ATRSpacingMultiplier:   orDefaultFloat(req.ATRSpacingMultiplier, 0.6),
		MinSpacingPercent:      orDefaultFloat(req.MinSpacingPercent, 0.15),
		MaxSpacingPercent:      orDefaultFloat(req.MaxSpacingPercent, 3.0),
		RecenterThresholdGrids: orDefaultFloat(req.RecenterThresholdGrids, 6),
		MinRecenterIntervalSec: orDefaultInt(req.MinRecenterIntervalSec, 900),
		PerGridQuoteAmount:     orDefaultFloat(req.PerGridQuoteAmount, 50),
		Leverage:               orDefaultFloat(req.Leverage, 3),
		Mode:                   mode,
		MaxTotalPositionQuote:  orDefaultFloat(req.MaxTotalPositionQuote, 2000),
	}
	return cfg
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
func orDefaultFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func (s *Server) handleCreateGrid(w http.ResponseWriter, r *http.Request) {
	var req createGridRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Symbol == "" {
		writeError(w, http.StatusBadRequest, "symbol 不能为空")
		return
	}
	cfg := req.toConfig()
	interval := time.Duration(orDefaultInt(req.TickIntervalSec, 5)) * time.Second

	finalCfg, err := s.mgr.StartGrid(cfg, interval)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "config": finalCfg})
}

func (s *Server) handleStopGrid(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	s.mgr.StopGrid(symbol)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	snap, ok := s.mgr.Snapshot(r.Context(), symbol)
	if !ok {
		writeError(w, http.StatusNotFound, "未找到该交易对的网格")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := s.mgr.Store().RecentEvents(symbol, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handlePnLHistory(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	points, err := s.mgr.Store().PnLHistory(symbol, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, points)
}

func (s *Server) handleTicker(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	ticker, err := s.mgr.RawExchange().GetTicker(r.Context(), symbol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ticker)
}
