package exchange

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BinanceFuturesExchange 是 Binance USDⓈ-M 合约（U本位/USDC本位共用同一套接口）的
// 完整适配器实现。BTCUSDC / ETHUSDC 这类 USDC 本位合约与 BTCUSDT 等 USDT 本位合约
// 走的是同一个 REST 域名和签名方式，只是 symbol 字符串和保证金资产不同，
// 因此这里不需要区分"USDC版"和"USDT版"，同一份代码即可支持两者。
//
// 使用前请确认：
//  1. 币安账户已开通合约交易，API Key 具备"合约交易"权限；
//  2. 如果要用 Mode=neutral（同时持有多空），必须在币安合约账户设置里打开
//     "双向持仓模式"(Hedge Mode)，并把这里的 HedgeMode 设为 true；
//  3. 建议先在测试网（testnet: true）验证下单/撤单/查询全流程无误后再切主网；
//  4. 本机系统时间需要基本准确，否则会遇到 -1021 时间戳错误
//     （代码里已经通过 /fapi/v1/time 自动计算并校正本地时间偏移，
//     但如果本机时钟偏差极大，仍建议开启系统时间自动同步）。
type BinanceFuturesExchange struct {
	apiKey     string
	apiSecret  string
	baseURL    string
	httpClient *http.Client

	// HedgeMode 是否为双向持仓模式。true 时下单会带上 positionSide=LONG/SHORT，
	// 需要与币安账户的"持仓模式"设置保持一致，否则下单会被交易所拒绝。
	HedgeMode bool

	mu           sync.Mutex
	timeOffsetMs int64 // 本地时间与币安服务器时间的差值（毫秒），下单时用于校正时间戳
	offsetSynced bool
}

// NewBinanceFuturesExchange 创建 Binance 合约适配器。
//
// 注意：币安已经把合约测试网整体迁移到了新域名/新登录方式（2026年内发生的变更）：
//   - 旧：testnet.binancefuture.com，单独注册测试网账号
//   - 新：demo-fapi.binance.com（REST）/ demo-fstream.binance.com（WebSocket），
//     登录入口是 https://demo.binance.com，用 GitHub 账号登录后在
//     Profile → API Management 里创建测试网 API Key。
//
// 旧域名已经不再可用（会被重定向或直接连不上），这里用的是新域名。
// 如果币安未来再次调整，请以 https://developers.binance.com 上的最新文档为准。
func NewBinanceFuturesExchange(apiKey, apiSecret string, testnet bool) *BinanceFuturesExchange {
	base := "https://fapi.binance.com"
	if testnet {
		base = "https://demo-fapi.binance.com"
	}
	return &BinanceFuturesExchange{
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		baseURL:    base,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *BinanceFuturesExchange) Name() string { return "binance_futures" }

// ---- 内部：签名与请求 ----

// binanceAPIError 是币安返回的标准错误结构 {"code":-1121,"msg":"Invalid symbol."}
type binanceAPIError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (b *BinanceFuturesExchange) syncTimeOffset(ctx context.Context) error {
	b.mu.Lock()
	if b.offsetSynced {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	body, err := b.rawRequest(ctx, http.MethodGet, "/fapi/v1/time", nil, false)
	if err != nil {
		return err
	}
	var resp struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析服务器时间失败: %w", err)
	}
	localMs := time.Now().UnixMilli()
	b.mu.Lock()
	b.timeOffsetMs = resp.ServerTime - localMs
	b.offsetSynced = true
	b.mu.Unlock()
	return nil
}

func (b *BinanceFuturesExchange) nowMs() int64 {
	b.mu.Lock()
	offset := b.timeOffsetMs
	b.mu.Unlock()
	return time.Now().UnixMilli() + offset
}

// rawRequest 发送一个不带签名的请求（公共接口用）
func (b *BinanceFuturesExchange) rawRequest(ctx context.Context, method, path string, params url.Values, needAuth bool) ([]byte, error) {
	if params == nil {
		params = url.Values{}
	}
	fullURL := b.baseURL + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, err
	}
	if needAuth || b.apiKey != "" {
		req.Header.Set("X-MBX-APIKEY", b.apiKey)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr binanceAPIError
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Code != 0 {
			return nil, fmt.Errorf("binance API 错误 [%d]: %s", apiErr.Code, apiErr.Msg)
		}
		return nil, fmt.Errorf("binance HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// signedRequest 发送一个需要 HMAC-SHA256 签名的请求（私有接口用：下单/撤单/查持仓/查余额等）
func (b *BinanceFuturesExchange) signedRequest(ctx context.Context, method, path string, params url.Values) ([]byte, error) {
	if err := b.syncTimeOffset(ctx); err != nil {
		// 时间同步失败不阻断请求，仅退化为使用本地时间（可能导致 -1021 错误，
		// 但直接报错会让整个策略无法运行；这里选择容错继续尝试）
		fmt.Printf("[binance_futures] 时间同步失败，使用本地时间: %v\n", err)
	}
	if params == nil {
		params = url.Values{}
	}
	params.Set("timestamp", strconv.FormatInt(b.nowMs(), 10))
	if params.Get("recvWindow") == "" {
		params.Set("recvWindow", "5000")
	}

	query := params.Encode()
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(query))
	signature := hex.EncodeToString(mac.Sum(nil))
	query += "&signature=" + signature

	fullURL := b.baseURL + path + "?" + query
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr binanceAPIError
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Code != 0 {
			return nil, fmt.Errorf("binance API 错误 [%d]: %s", apiErr.Code, apiErr.Msg)
		}
		return nil, fmt.Errorf("binance HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ---- Exchange 接口实现 ----

func (b *BinanceFuturesExchange) GetTicker(ctx context.Context, symbol string) (*Ticker, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.rawRequest(ctx, http.MethodGet, "/fapi/v1/ticker/bookTicker", params, false)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
		Time     int64  `json:"time"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 ticker 失败: %w", err)
	}
	bid := parseFloat(raw.BidPrice)
	ask := parseFloat(raw.AskPrice)
	return &Ticker{
		Symbol:    symbol,
		Price:     (bid + ask) / 2, // 用买一卖一中间价近似最新价，避免额外多发一次请求
		Bid:       bid,
		Ask:       ask,
		Volume24h: 0, // 如需24小时成交量，可另外调用 /fapi/v1/ticker/24hr（这里为降低请求权重默认不获取）
		Timestamp: time.UnixMilli(raw.Time),
	}, nil
}

func (b *BinanceFuturesExchange) GetKlines(ctx context.Context, symbol string, interval string, limit int) ([]Kline, error) {
	params := url.Values{
		"symbol":   {symbol},
		"interval": {interval},
		"limit":    {strconv.Itoa(limit)},
	}
	body, err := b.rawRequest(ctx, http.MethodGet, "/fapi/v1/klines", params, false)
	if err != nil {
		return nil, err
	}
	var raw [][]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析K线失败: %w", err)
	}
	out := make([]Kline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		openTimeMs, _ := row[0].(float64)
		out = append(out, Kline{
			OpenTime: time.UnixMilli(int64(openTimeMs)),
			Open:     parseFloat(fmt.Sprint(row[1])),
			High:     parseFloat(fmt.Sprint(row[2])),
			Low:      parseFloat(fmt.Sprint(row[3])),
			Close:    parseFloat(fmt.Sprint(row[4])),
			Volume:   parseFloat(fmt.Sprint(row[5])),
		})
	}
	return out, nil
}

func (b *BinanceFuturesExchange) GetSymbolInfo(ctx context.Context, symbol string) (*SymbolInfo, error) {
	body, err := b.rawRequest(ctx, http.MethodGet, "/fapi/v1/exchangeInfo", nil, false)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Symbols []struct {
			Symbol            string `json:"symbol"`
			PricePrecision    int    `json:"pricePrecision"`
			QuantityPrecision int    `json:"quantityPrecision"`
			Filters           []struct {
				FilterType  string `json:"filterType"`
				TickSize    string `json:"tickSize"`
				StepSize    string `json:"stepSize"`
				MinQty      string `json:"minQty"`
				Notional    string `json:"notional"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 exchangeInfo 失败: %w", err)
	}
	for _, s := range raw.Symbols {
		if s.Symbol != symbol {
			continue
		}
		info := &SymbolInfo{
			Symbol:            s.Symbol,
			PricePrecision:    s.PricePrecision,
			QuantityPrecision: s.QuantityPrecision,
			MaxLeverage:       20, // 精确值需调用 /fapi/v1/leverageBracket（签名接口），此处给出保守默认值
		}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				info.TickSize = parseFloat(f.TickSize)
			case "LOT_SIZE":
				info.StepSize = parseFloat(f.StepSize)
				info.MinQuantity = parseFloat(f.MinQty)
			case "MIN_NOTIONAL":
				if f.Notional != "" {
					info.MinNotional = parseFloat(f.Notional)
				} else {
					info.MinNotional = parseFloat(f.MinNotional)
				}
			}
		}
		return info, nil
	}
	return nil, fmt.Errorf("未找到交易对 %s，请检查符号是否正确（例如 BTCUSDC）", symbol)
}

func (b *BinanceFuturesExchange) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	symbolInfo, err := b.GetSymbolInfo(ctx, req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("下单前获取精度信息失败: %w", err)
	}
	price := roundToStep(req.Price, symbolInfo.TickSize, symbolInfo.PricePrecision)
	qty := roundToStep(req.Quantity, symbolInfo.StepSize, symbolInfo.QuantityPrecision)

	params := url.Values{
		"symbol":   {req.Symbol},
		"side":     {strings.ToUpper(string(req.Side))},
		"type":     {strings.ToUpper(string(req.Type))},
		"quantity": {trimFloat(qty, symbolInfo.QuantityPrecision)},
	}
	if req.ClientOrderID != "" {
		params.Set("newClientOrderId", req.ClientOrderID)
	}
	if req.Type == OrderTypeLimit {
		params.Set("price", trimFloat(price, symbolInfo.PricePrecision))
		params.Set("timeInForce", "GTC")
	}
	if req.ReduceOnly && !b.HedgeMode {
		// 双向持仓模式下 Binance 不接受 reduceOnly 参数（用 positionSide 区分方向即可）
		params.Set("reduceOnly", "true")
	}
	if b.HedgeMode {
		params.Set("positionSide", strings.ToUpper(string(req.PositionSide)))
	}

	body, err := b.signedRequest(ctx, http.MethodPost, "/fapi/v1/order", params)
	if err != nil {
		return nil, err
	}
	return parseBinanceOrder(body)
}

func (b *BinanceFuturesExchange) CancelOrder(ctx context.Context, symbol string, exchangeOrderID string) error {
	params := url.Values{"symbol": {symbol}, "orderId": {exchangeOrderID}}
	_, err := b.signedRequest(ctx, http.MethodDelete, "/fapi/v1/order", params)
	return err
}

func (b *BinanceFuturesExchange) GetOpenOrders(ctx context.Context, symbol string) ([]Order, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.signedRequest(ctx, http.MethodGet, "/fapi/v1/openOrders", params)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析挂单列表失败: %w", err)
	}
	out := make([]Order, 0, len(raw))
	for _, r := range raw {
		o, err := parseBinanceOrder(r)
		if err != nil {
			continue
		}
		out = append(out, *o)
	}
	return out, nil
}

func (b *BinanceFuturesExchange) GetOrder(ctx context.Context, symbol string, exchangeOrderID string) (*Order, error) {
	params := url.Values{"symbol": {symbol}, "orderId": {exchangeOrderID}}
	body, err := b.signedRequest(ctx, http.MethodGet, "/fapi/v1/order", params)
	if err != nil {
		return nil, err
	}
	return parseBinanceOrder(body)
}

func (b *BinanceFuturesExchange) GetPositions(ctx context.Context, symbol string) ([]Position, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.signedRequest(ctx, http.MethodGet, "/fapi/v2/positionRisk", params)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		UnRealizedProfit string `json:"unRealizedProfit"`
		LiquidationPrice string `json:"liquidationPrice"`
		Leverage         string `json:"leverage"`
		PositionSide     string `json:"positionSide"`
		IsolatedMargin   string `json:"isolatedMargin"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析持仓失败: %w", err)
	}
	out := make([]Position, 0, len(raw))
	for _, p := range raw {
		amt := parseFloat(p.PositionAmt)
		if amt == 0 {
			continue
		}
		side := PositionLong
		switch p.PositionSide {
		case "SHORT":
			side = PositionShort
		case "LONG":
			side = PositionLong
		default: // "BOTH"（单向持仓模式）：按数量正负推断方向
			if amt < 0 {
				side = PositionShort
			}
		}
		out = append(out, Position{
			Symbol:           p.Symbol,
			PositionSide:     side,
			Quantity:         math.Abs(amt),
			EntryPrice:       parseFloat(p.EntryPrice),
			MarkPrice:        parseFloat(p.MarkPrice),
			Leverage:         parseFloat(p.Leverage),
			MarginUsed:       parseFloat(p.IsolatedMargin),
			UnrealizedPnL:    parseFloat(p.UnRealizedProfit),
			LiquidationPrice: parseFloat(p.LiquidationPrice),
		})
	}
	return out, nil
}

func (b *BinanceFuturesExchange) GetBalances(ctx context.Context) ([]Balance, error) {
	body, err := b.signedRequest(ctx, http.MethodGet, "/fapi/v2/balance", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析余额失败: %w", err)
	}
	out := make([]Balance, 0, len(raw))
	for _, a := range raw {
		total := parseFloat(a.Balance)
		if total == 0 {
			continue
		}
		out = append(out, Balance{
			Asset:     a.Asset,
			Available: parseFloat(a.AvailableBalance),
			Total:     total,
		})
	}
	return out, nil
}

func (b *BinanceFuturesExchange) SetLeverage(ctx context.Context, symbol string, leverage float64) error {
	params := url.Values{"symbol": {symbol}, "leverage": {strconv.Itoa(int(leverage))}}
	_, err := b.signedRequest(ctx, http.MethodPost, "/fapi/v1/leverage", params)
	return err
}

// ---- 辅助函数 ----

func parseBinanceOrder(body []byte) (*Order, error) {
	var raw struct {
		OrderId       int64  `json:"orderId"`
		ClientOrderId string `json:"clientOrderId"`
		Symbol        string `json:"symbol"`
		Side          string `json:"side"`
		PositionSide  string `json:"positionSide"`
		Type          string `json:"type"`
		Price         string `json:"price"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		AvgPrice      string `json:"avgPrice"`
		Status        string `json:"status"`
		UpdateTime    int64  `json:"updateTime"`
		Time          int64  `json:"time"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析订单失败: %w", err)
	}
	status := OrderStatusNew
	switch raw.Status {
	case "FILLED":
		status = OrderStatusFilled
	case "PARTIALLY_FILLED":
		status = OrderStatusPartial
	case "CANCELED", "EXPIRED":
		status = OrderStatusCanceled
	case "REJECTED":
		status = OrderStatusRejected
	}
	posSide := PositionLong
	if raw.PositionSide == "SHORT" {
		posSide = PositionShort
	}
	createdAt := time.UnixMilli(raw.Time)
	if raw.Time == 0 {
		createdAt = time.UnixMilli(raw.UpdateTime)
	}
	return &Order{
		ExchangeOrderID: strconv.FormatInt(raw.OrderId, 10),
		ClientOrderID:   raw.ClientOrderId,
		Symbol:          raw.Symbol,
		Side:            Side(strings.ToLower(raw.Side)),
		PositionSide:    posSide,
		Type:            OrderType(strings.ToLower(raw.Type)),
		Price:           parseFloat(raw.Price),
		Quantity:        parseFloat(raw.OrigQty),
		FilledQuantity:  parseFloat(raw.ExecutedQty),
		AvgFillPrice:    parseFloat(raw.AvgPrice),
		Status:          status,
		CreatedAt:       createdAt,
		UpdatedAt:       time.UnixMilli(raw.UpdateTime),
	}, nil
}

// roundToStep 把价格/数量对齐到交易所要求的最小变动单位（tickSize/stepSize）
func roundToStep(value, step float64, precision int) float64 {
	if step <= 0 {
		return roundToPrecision(value, precision)
	}
	steps := math.Round(value / step)
	return roundToPrecision(steps*step, precision)
}

func roundToPrecision(value float64, precision int) float64 {
	mul := math.Pow(10, float64(precision))
	return math.Round(value*mul) / mul
}

// trimFloat 按精度格式化为字符串，避免浮点数二进制表示误差导致多余小数位
// （Binance 对参数格式较严格，多余的尾随小数位或科学计数法都可能被拒）
func trimFloat(v float64, precision int) string {
	return strconv.FormatFloat(v, 'f', precision, 64)
}
