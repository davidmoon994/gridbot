package strategy

import (
	"gridbot/exchange"
)

// EMA 计算指数移动平均线，返回与输入等长的序列（前面不足周期的部分用 SMA 近似）
func EMA(closes []float64, period int) []float64 {
	n := len(closes)
	out := make([]float64, n)
	if n == 0 || period <= 0 {
		return out
	}
	k := 2.0 / float64(period+1)
	// 用前 period 个收盘价的简单平均作为初始值
	seed := 0.0
	seedN := period
	if seedN > n {
		seedN = n
	}
	for i := 0; i < seedN; i++ {
		seed += closes[i]
	}
	seed /= float64(seedN)
	prev := seed
	for i := 0; i < n; i++ {
		if i == 0 {
			out[i] = seed
			continue
		}
		prev = closes[i]*k + prev*(1-k)
		out[i] = prev
	}
	return out
}

// LastEMA 返回K线序列最新的EMA值
func LastEMA(klines []exchange.Kline, period int) float64 {
	closes := make([]float64, len(klines))
	for i, k := range klines {
		closes[i] = k.Close
	}
	ema := EMA(closes, period)
	if len(ema) == 0 {
		return 0
	}
	return ema[len(ema)-1]
}

// ATR 计算平均真实波幅（Average True Range），衡量近期波动率大小。
// 网格间距会依据 ATR 动态调整：波动越大，网格越宽，减少无效开平仓次数。
func ATR(klines []exchange.Kline, period int) float64 {
	n := len(klines)
	if n < 2 {
		return 0
	}
	trs := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close
		tr := max3(
			high-low,
			absF(high-prevClose),
			absF(low-prevClose),
		)
		trs = append(trs, tr)
	}
	if len(trs) < period {
		period = len(trs)
	}
	if period == 0 {
		return 0
	}
	// Wilder 平滑：先用前period个TR做简单平均，再逐步平滑
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)
	for i := period; i < len(trs); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}
	return atr
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}
