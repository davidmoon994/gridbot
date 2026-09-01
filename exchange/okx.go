package exchange

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// OKXExchange 是 OKX V5 交易的完整适配器实现，同时支持永续合约(SWAP)和现货(SPOT)，
// 通过 MarketType 字段区分（"SWAP" 默认 / "SPOT"）。
//
// 与 Binance 最大的不同：
//  1. 签名方式：base64(HMAC-SHA256(secret, timestamp+method+requestPath+body))，
//     而不是 query string 的 hex HMAC；
//  2. 交易对命名：OKX 用带连字符的 instId，合约是 "BTC-USDC-SWAP"，现货是
//     "BTC-USDC"（没有 -SWAP 后缀），而配置/网格引擎里统一用 Binance 风格的
//     "BTCUSDC"，这里做了双向转换（见 toOKXInstId）；
//  3. 合约下单数量单位是"张"，需要用 ctVal 换算；现货下单直接用标的币数量，
//     没有张数概念（见 PlaceOrder 里的 MarketType 分支）；
//  4. 没有独立的测试网域名，用同一个域名 + 请求头 x-simulated-trading:1
//     进入模拟交易环境（对应 Simulated=true）；
//  5. 现货没有持仓/杠杆/双向持仓这些概念：GetPositions 对现货固定返回空列表
//     （持仓统计改由 manager 层调用 strategy.Engine.PositionSummary() 合成，
//     原因见 exchange/binance_spot.go 顶部注释），SetLeverage 对现货是空操作。
//
// 使用前请确认：
//  1. OKX 账户已开通对应交易权限，API Key 具备"交易"权限，并正确设置了 Passphrase；
//  2. 合约模式下如果要用 Mode=neutral（同时持有多空），账户的"持仓模式"需要设置为
//     "双向持仓"（长/空模式），并把这里的 PositionMode 设为 "long_short"
//     （默认值）；如果账户是"单向持仓"（net），设为 "net"；现货模式下 PositionMode
//     无意义，会被忽略；
//  3. 建议先用 Simulated=true（模拟交易）验证下单/撤单/查询全流程无误后再转真实资金。
type OKXExchange struct {
	apiKey     string
	apiSecret  string
	passphrase string
	baseURL    string
	simulated  bool
	httpClient *http.Client

	// MarginMode 保证金模式："cross"（全仓，默认）或 "isolated"（逐仓），仅合约生效
	MarginMode string
	// PositionMode "long_short"（双向持仓，默认，neutral模式必需）或 "net"（单向持仓），仅合约生效
	PositionMode string
	// MarketType "SWAP"（永续合约，默认）或 "SPOT"（现货）
	MarketType string

	mu         sync.Mutex
	ctValCache map[string]float64 // instId -> 每张合约对应的标的币数量（仅合约使用），避免每次下单都重新请求
}

// NewOKXExchange 创建 OKX 合约适配器（MarketType 默认 "SWAP"）。simulated=true 时
// 请求头带上 x-simulated-trading:1，进入 OKX 的模拟交易环境（需要用模拟交易专用的 API Key）。
func NewOKXExchange(apiKey, apiSecret, passphrase string, simulated bool) *OKXExchange {
	return &OKXExchange{
		apiKey:       apiKey,
		apiSecret:    apiSecret,
		passphrase:   passphrase,
		baseURL:      "https://www.okx.com",
		simulated:    simulated,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		MarginMode:   "cross",
		PositionMode: "long_short",
		MarketType:   "SWAP",
		ctValCache:   map[string]float64{},
	}
}

// NewOKXSpotExchange 创建 OKX 现货适配器
func NewOKXSpotExchange(apiKey, apiSecret, passphrase string, simulated bool) *OKXExchange {
	ex := NewOKXExchange(apiKey, apiSecret, passphrase, simulated)
	ex.MarketType = "SPOT"
	return ex
}

func (o *OKXExchange) isSpot() bool { return o.MarketType == "SPOT" }

func (o *OKXExchange) Name() string {
	if o.isSpot() {
		return "okx_spot"
	}
	return "okx"
}

// ---- symbol <-> instId 转换 ----

var okxKnownQuoteAssets = []string{"USDT", "USDC", "USD", "BUSD"}

// toOKXInstId 把通用符号（如 "BTCUSDC"）转换为 OKX 的 instId：
// 合约是 "BTC-USDC-SWAP"，现货是 "BTC-USDC"（无后缀）。
func toOKXInstId(symbol string, marketType string) string {
	up := strings.ToUpper(symbol)
	for _, q := range okxKnownQuoteAssets {
		if strings.HasSuffix(up, q) && len(up) > len(q) {
			base := strings.TrimSuffix(up, q)
			if marketType == "SPOT" {
				return base + "-" + q
			}
			return base + "-" + q + "-SWAP"
		}
	}
	return up // 无法识别报价货币后缀时原样返回，调用会在交易所侧报错，便于定位问题
}

func (o *OKXExchange) instId(symbol string) string {
	return toOKXInstId(symbol, o.MarketType)
}

// ---- 内部：签名与请求 ----

type okxEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (o *OKXExchange) timestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func (o *OKXExchange) sign(timestamp, method, requestPath, body string) string {
	mac := hmac.New(sha256.New, []byte(o.apiSecret))
	mac.Write([]byte(timestamp + method + requestPath + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// request 发送一个签名请求。GET 请求 body 传空字符串，requestPath 需包含查询串。
func (o *OKXExchange) request(ctx context.Context, method, requestPath string, bodyObj interface{}) (json.RawMessage, error) {
	var bodyStr string
	var bodyReader io.Reader
	if bodyObj != nil {
		b, err := json.Marshal(bodyObj)
		if err != nil {
			return nil, err
		}
		bodyStr = string(b)
		bodyReader = bytes.NewReader(b)
	}

	ts := o.timestamp()
	sig := o.sign(ts, method, requestPath, bodyStr)

	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+requestPath, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("OK-ACCESS-KEY", o.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", sig)
	req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
	req.Header.Set("OK-ACCESS-PASSPHRASE", o.passphrase)
	req.Header.Set("Content-Type", "application/json")
	if o.simulated {
		req.Header.Set("x-simulated-trading", "1")
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", requestPath, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env okxEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析 OKX 响应失败: %w, body=%s", err, string(raw))
	}
	if env.Code != "0" && env.Code != "" {
		return nil, fmt.Errorf("OKX API 错误 [%s]: %s", env.Code, env.Msg)
	}
	return env.Data, nil
}

func (o *OKXExchange) publicGet(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	full := path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	return o.request(ctx, http.MethodGet, full, nil)
}

func (o *OKXExchange) signedGet(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	full := path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	return o.request(ctx, http.MethodGet, full, nil)
}

func (o *OKXExchange) signedPost(ctx context.Context, path string, body interface{}) (json.RawMessage, error) {
	return o.request(ctx, http.MethodPost, path, body)
}

func parseF(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ---- 合约乘数（ctVal）缓存 ----

// instrumentMeta 是下单/精度换算需要的合约元信息
type instrumentMeta struct {
	CtVal  float64 // 每张合约对应的标的币数量
	TickSz float64 // 最小价格变动单位
	LotSz  float64 // 最小下单张数变动单位
	MinSz  float64 // 最小下单张数
}

func (o *OKXExchange) getInstrumentMeta(ctx context.Context, instId string) (*instrumentMeta, error) {
	params := url.Values{"instType": {o.MarketType}, "instId": {instId}}
	data, err := o.publicGet(ctx, "/api/v5/public/instruments", params)
	if err != nil {
		return nil, err
	}
	var arr []struct {
		CtVal  string `json:"ctVal"` // 现货没有这个字段，为空字符串
		TickSz string `json:"tickSz"`
		LotSz  string `json:"lotSz"`
		MinSz  string `json:"minSz"`
	}
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("解析合约信息失败: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("未找到交易对 %s，请检查符号是否正确（例如 BTCUSDC -> %s）", instId, instId)
	}
	ctVal := 1.0 // 现货没有"张"的概念，直接把 ctVal 当作 1（数量单位=标的币本身），统一后续换算逻辑
	if o.MarketType != "SPOT" {
		ctVal = parseF(arr[0].CtVal)
	}
	return &instrumentMeta{
		CtVal:  ctVal,
		TickSz: parseF(arr[0].TickSz),
		LotSz:  parseF(arr[0].LotSz),
		MinSz:  parseF(arr[0].MinSz),
	}, nil
}

// ---- Exchange 接口实现 ----

func (o *OKXExchange) GetTicker(ctx context.Context, symbol string) (*Ticker, error) {
	instId := o.instId(symbol)
	data, err := o.publicGet(ctx, "/api/v5/market/ticker", url.Values{"instId": {instId}})
	if err != nil {
		return nil, err
	}
	var arr []struct {
		Last   string `json:"last"`
		AskPx  string `json:"askPx"`
		BidPx  string `json:"bidPx"`
		Vol24h string `json:"volCcy24h"`
		Ts     string `json:"ts"`
	}
	if err := json.Unmarshal(data, &arr); err != nil || len(arr) == 0 {
		return nil, fmt.Errorf("解析 ticker 失败或无数据: %v", err)
	}
	t := arr[0]
	tsMs, _ := strconv.ParseInt(t.Ts, 10, 64)
	return &Ticker{
		Symbol:    symbol,
		Price:     parseF(t.Last),
		Bid:       parseF(t.BidPx),
		Ask:       parseF(t.AskPx),
		Volume24h: parseF(t.Vol24h),
		Timestamp: time.UnixMilli(tsMs),
	}, nil
}

// okxBarInterval 把通用K线周期（本项目内固定使用"3m"）映射为 OKX 的 bar 参数格式。
// OKX 分钟级用小写 "3m"，小时/天级用大写 "1H"/"1D"，这里只覆盖项目实际用到的粒度，
// 如果以后策略层改用其他周期，请在这里补充映射。
func okxBarInterval(interval string) string {
	switch strings.ToLower(interval) {
	case "1h", "2h", "4h", "6h", "12h":
		return strings.ToUpper(interval)
	case "1d":
		return "1D"
	default:
		return interval // 分钟级（如 "3m"、"5m"）OKX 与通用写法一致，直接透传
	}
}

func (o *OKXExchange) GetKlines(ctx context.Context, symbol string, interval string, limit int) ([]Kline, error) {
	instId := o.instId(symbol)
	params := url.Values{
		"instId": {instId},
		"bar":    {okxBarInterval(interval)},
		"limit":  {strconv.Itoa(limit)},
	}
	data, err := o.publicGet(ctx, "/api/v5/market/candles", params)
	if err != nil {
		return nil, err
	}
	var raw [][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析K线失败: %w", err)
	}
	out := make([]Kline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		tsMs, _ := strconv.ParseInt(row[0], 10, 64)
		out = append(out, Kline{
			OpenTime: time.UnixMilli(tsMs),
			Open:     parseF(row[1]),
			High:     parseF(row[2]),
			Low:      parseF(row[3]),
			Close:    parseF(row[4]),
			Volume:   parseF(row[5]),
		})
	}
	// OKX 返回的是从新到旧排序，这里翻转为按时间正序，与 Binance 保持一致，
	// 网格引擎的 EMA/ATR 计算依赖正序输入。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (o *OKXExchange) GetSymbolInfo(ctx context.Context, symbol string) (*SymbolInfo, error) {
	instId := o.instId(symbol)
	meta, err := o.getInstrumentMeta(ctx, instId)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.ctValCache[instId] = meta.CtVal
	o.mu.Unlock()

	pricePrecision := precisionFromStep(meta.TickSz)
	// 注意：这里的 QuantityPrecision/StepSize/MinQuantity 合约模式下描述的是"张数"的
	// 精度（PlaceOrder 里会用 ctVal 把币数量换算成张数）；现货模式下 ctVal 固定为1，
	// 这些字段直接就是标的币数量本身的精度，两种模式复用同一套字段不需要额外分支。
	maxLeverage := 20.0 // 合约精确值需调用 /api/v5/account/max-avail-size 等接口，此处给出保守默认值
	if o.isSpot() {
		maxLeverage = 1 // 现货没有杠杆
	}
	return &SymbolInfo{
		Symbol:            symbol,
		PricePrecision:    pricePrecision,
		QuantityPrecision: precisionFromStep(meta.LotSz),
		MinNotional:       0, // OKX 不直接提供最小名义价值字段，用最小下单量(MinSz)约束
		MinQuantity:       meta.MinSz * meta.CtVal,
		TickSize:          meta.TickSz,
		StepSize:          meta.CtVal, // 复用字段：合约模式下是"每张对应的币数量"，现货固定为1
		MaxLeverage:       maxLeverage,
	}, nil
}

func precisionFromStep(step float64) int {
	if step <= 0 {
		return 4
	}
	precision := 0
	for step < 1 && precision < 12 {
		step *= 10
		precision++
	}
	return precision
}

func (o *OKXExchange) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	instId := o.instId(req.Symbol)
	meta, err := o.getInstrumentMeta(ctx, instId)
	if err != nil {
		return nil, fmt.Errorf("下单前获取交易对信息失败: %w", err)
	}
	if meta.CtVal <= 0 {
		return nil, fmt.Errorf("交易对 %s 的合约乘数无效，无法换算下单数量", instId)
	}

	// 网格引擎传入的 Quantity 统一是"标的币数量"。合约模式下 OKX 下单单位是"张"，
	// 需要 币数量/ctVal 换算；现货模式下 ctVal 固定为1（见 getInstrumentMeta），
	// 换算后 contracts 就等于原始币数量，两种模式共用同一套取整逻辑。
	contracts := req.Quantity / meta.CtVal
	if meta.LotSz > 0 {
		contracts = math.Round(contracts/meta.LotSz) * meta.LotSz
	}
	if contracts < meta.MinSz {
		contracts = meta.MinSz
	}
	szPrecision := precisionFromStep(meta.LotSz)
	pxPrecision := precisionFromStep(meta.TickSz)

	price := req.Price
	if meta.TickSz > 0 {
		price = math.Round(price/meta.TickSz) * meta.TickSz
	}

	tdMode := o.MarginMode
	if o.isSpot() {
		tdMode = "cash" // 现货现金交易，不是保证金/合约模式
	}

	body := map[string]interface{}{
		"instId":  instId,
		"tdMode":  tdMode,
		"side":    strings.ToLower(string(req.Side)),
		"ordType": strings.ToLower(string(req.Type)),
		"sz":      strconv.FormatFloat(contracts, 'f', szPrecision, 64),
	}
	if req.ClientOrderID != "" {
		// OKX 的 clOrdId 只允许字母数字，长度限制32；这里做一次简单清洗，
		// 避免网格引擎生成的 clientID（含"-"）被拒绝
		body["clOrdId"] = sanitizeOKXClientID(req.ClientOrderID)
	}
	if req.Type == OrderTypeLimit {
		body["px"] = strconv.FormatFloat(price, 'f', pxPrecision, 64)
	}

	if o.isSpot() {
		// 现货没有持仓方向/减仓概念：卖单天然受限于实际可用余额，交易所会在
		// 余额不足时直接拒绝。市价买单默认 sz 代表"花费的计价货币数量"而不是
		// "买入的标的币数量"，这里显式指定 tgtCcy=base_ccy 强制 sz 按标的币数量
		// 理解，与我们内部统一的 Quantity 语义保持一致。
		if req.Type == OrderTypeMarket && req.Side == SideBuy {
			body["tgtCcy"] = "base_ccy"
		}
	} else if o.PositionMode == "net" {
		if req.ReduceOnly {
			body["reduceOnly"] = true
		}
	} else {
		// 双向持仓模式：用 posSide 显式指定方向，开平仓由 side+posSide 组合决定，
		// 不需要也不应该再传 reduceOnly
		posSide := "long"
		if req.PositionSide == PositionShort {
			posSide = "short"
		}
		body["posSide"] = posSide
	}

	data, err := o.signedPost(ctx, "/api/v5/trade/order", body)
	if err != nil {
		return nil, err
	}
	var arr []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
	}
	if err := json.Unmarshal(data, &arr); err != nil || len(arr) == 0 {
		return nil, fmt.Errorf("解析下单结果失败: %v", err)
	}
	if arr[0].SCode != "0" {
		return nil, fmt.Errorf("OKX 下单被拒绝 [%s]: %s", arr[0].SCode, arr[0].SMsg)
	}

	return &Order{
		ExchangeOrderID: arr[0].OrdId,
		ClientOrderID:   req.ClientOrderID,
		Symbol:          req.Symbol,
		Side:            req.Side,
		PositionSide:    req.PositionSide,
		Type:            req.Type,
		Price:           req.Price,
		Quantity:        req.Quantity,
		Status:          OrderStatusNew,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}, nil
}

// sanitizeOKXClientID 去掉 OKX 不允许的字符（只保留字母数字），并截断到32位以内
func sanitizeOKXClientID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 32 {
		s = s[len(s)-32:]
	}
	return s
}

func (o *OKXExchange) CancelOrder(ctx context.Context, symbol string, exchangeOrderID string) error {
	instId := o.instId(symbol)
	body := map[string]interface{}{"instId": instId, "ordId": exchangeOrderID}
	_, err := o.signedPost(ctx, "/api/v5/trade/cancel-order", body)
	return err
}

func (o *OKXExchange) GetOpenOrders(ctx context.Context, symbol string) ([]Order, error) {
	instId := o.instId(symbol)
	data, err := o.signedGet(ctx, "/api/v5/trade/orders-pending", url.Values{"instId": {instId}})
	if err != nil {
		return nil, err
	}
	return parseOKXOrders(data, symbol)
}

func (o *OKXExchange) GetOrder(ctx context.Context, symbol string, exchangeOrderID string) (*Order, error) {
	instId := o.instId(symbol)
	params := url.Values{"instId": {instId}, "ordId": {exchangeOrderID}}
	data, err := o.signedGet(ctx, "/api/v5/trade/order", params)
	if err != nil {
		return nil, err
	}
	orders, err := parseOKXOrders(data, symbol)
	if err != nil || len(orders) == 0 {
		return nil, fmt.Errorf("未找到订单 %s: %v", exchangeOrderID, err)
	}
	return &orders[0], nil
}

func parseOKXOrders(data json.RawMessage, symbol string) ([]Order, error) {
	var arr []struct {
		OrdId     string `json:"ordId"`
		ClOrdId   string `json:"clOrdId"`
		Side      string `json:"side"`
		PosSide   string `json:"posSide"`
		OrdType   string `json:"ordType"`
		Px        string `json:"px"`
		Sz        string `json:"sz"`
		AccFillSz string `json:"accFillSz"`
		AvgPx     string `json:"avgPx"`
		State     string `json:"state"`
		CTime     string `json:"cTime"`
		UTime     string `json:"uTime"`
	}
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("解析订单列表失败: %w", err)
	}
	out := make([]Order, 0, len(arr))
	for _, r := range arr {
		status := OrderStatusNew
		switch r.State {
		case "filled":
			status = OrderStatusFilled
		case "partially_filled":
			status = OrderStatusPartial
		case "canceled":
			status = OrderStatusCanceled
		}
		posSide := PositionLong
		if r.PosSide == "short" {
			posSide = PositionShort
		}
		cTimeMs, _ := strconv.ParseInt(r.CTime, 10, 64)
		uTimeMs, _ := strconv.ParseInt(r.UTime, 10, 64)
		out = append(out, Order{
			ExchangeOrderID: r.OrdId,
			ClientOrderID:   r.ClOrdId,
			Symbol:          symbol,
			Side:            Side(r.Side),
			PositionSide:    posSide,
			Type:            OrderType(r.OrdType),
			Price:           parseF(r.Px),
			Quantity:        parseF(r.Sz),
			FilledQuantity:  parseF(r.AccFillSz),
			AvgFillPrice:    parseF(r.AvgPx),
			Status:          status,
			CreatedAt:       time.UnixMilli(cTimeMs),
			UpdatedAt:       time.UnixMilli(uTimeMs),
		})
	}
	return out, nil
}

// GetPositions 现货没有交易所原生"持仓"概念，固定返回空列表。
// 持仓统计由 manager 层调用 strategy.Engine.PositionSummary() 自己合成
// （原因见 exchange/binance_spot.go 顶部注释）。合约模式正常查询。
func (o *OKXExchange) GetPositions(ctx context.Context, symbol string) ([]Position, error) {
	if o.isSpot() {
		return nil, nil
	}
	instId := o.instId(symbol)
	data, err := o.signedGet(ctx, "/api/v5/account/positions", url.Values{"instId": {instId}})
	if err != nil {
		return nil, err
	}
	var arr []struct {
		Pos     string `json:"pos"` // 合约张数（可能为负，取决于持仓模式）
		AvgPx   string `json:"avgPx"`
		MarkPx  string `json:"markPx"`
		Upl     string `json:"upl"`
		Lever   string `json:"lever"`
		Margin  string `json:"margin"`
		LiqPx   string `json:"liqPx"`
		PosSide string `json:"posSide"`
	}
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("解析持仓失败: %w", err)
	}

	meta, metaErr := o.getInstrumentMeta(ctx, instId)
	ctVal := 1.0
	if metaErr == nil && meta.CtVal > 0 {
		ctVal = meta.CtVal
	}

	out := make([]Position, 0, len(arr))
	for _, p := range arr {
		contracts := parseF(p.Pos)
		if contracts == 0 {
			continue
		}
		side := PositionLong
		switch p.PosSide {
		case "short":
			side = PositionShort
		case "long":
			side = PositionLong
		default: // "net"：按正负推断方向
			if contracts < 0 {
				side = PositionShort
			}
		}
		out = append(out, Position{
			Symbol:           symbol,
			PositionSide:     side,
			Quantity:         math.Abs(contracts) * ctVal, // 张数换算回标的币数量，保持与其他交易所语义一致
			EntryPrice:       parseF(p.AvgPx),
			MarkPrice:        parseF(p.MarkPx),
			Leverage:         parseF(p.Lever),
			MarginUsed:       parseF(p.Margin),
			UnrealizedPnL:    parseF(p.Upl),
			LiquidationPrice: parseF(p.LiqPx),
		})
	}
	return out, nil
}

func (o *OKXExchange) GetBalances(ctx context.Context) ([]Balance, error) {
	data, err := o.signedGet(ctx, "/api/v5/account/balance", nil)
	if err != nil {
		return nil, err
	}
	var arr []struct {
		Details []struct {
			Ccy      string `json:"ccy"`
			AvailBal string `json:"availBal"`
			Eq       string `json:"eq"`
		} `json:"details"`
	}
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("解析余额失败: %w", err)
	}
	var out []Balance
	for _, a := range arr {
		for _, d := range a.Details {
			total := parseF(d.Eq)
			if total == 0 {
				continue
			}
			out = append(out, Balance{
				Asset:     d.Ccy,
				Available: parseF(d.AvailBal),
				Total:     total,
			})
		}
	}
	return out, nil
}

func (o *OKXExchange) SetLeverage(ctx context.Context, symbol string, leverage float64) error {
	if o.isSpot() {
		return nil // 现货没有杠杆概念，空操作
	}
	instId := o.instId(symbol)
	body := map[string]interface{}{
		"instId":  instId,
		"lever":   strconv.Itoa(int(leverage)),
		"mgnMode": o.MarginMode,
	}
	if o.PositionMode == "long_short" {
		// 双向持仓模式下，多空两个方向的杠杆需要分别设置
		bodyLong := map[string]interface{}{"instId": instId, "lever": strconv.Itoa(int(leverage)), "mgnMode": o.MarginMode, "posSide": "long"}
		bodyShort := map[string]interface{}{"instId": instId, "lever": strconv.Itoa(int(leverage)), "mgnMode": o.MarginMode, "posSide": "short"}
		if _, err := o.signedPost(ctx, "/api/v5/account/set-leverage", bodyLong); err != nil {
			return err
		}
		_, err := o.signedPost(ctx, "/api/v5/account/set-leverage", bodyShort)
		return err
	}
	_, err := o.signedPost(ctx, "/api/v5/account/set-leverage", body)
	return err
}
