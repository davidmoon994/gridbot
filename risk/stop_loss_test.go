package risk

import (
	"strings"
	"testing"
)

// 每个网格可以单独配置止损线；没配置（0）就用引擎默认值。
func TestShouldForceCloseUsesPerGridStopLoss(t *testing.T) {
	e := NewEngine(DefaultLimits()) // 默认止损线 15，回撤保护默认关闭
	if got := e.DefaultStopLossPct(); got != 15 {
		t.Fatalf("默认止损线应为 15，实际 %.2f", got)
	}

	cases := []struct {
		name      string
		stopLoss  float64 // 网格自己的配置
		curPct    float64
		wantClose bool
		wantInMsg string
	}{
		{"未配置、浮亏10%：用默认15，不触发", 0, -10, false, ""},
		{"未配置、浮亏16%：触发，写默认值15", 0, -16, true, "15.0%"},
		{"配置5、浮亏7.5%：比默认更严，触发", 5, -7.5, true, "5.0%"},
		{"配置5、浮亏4%：未到线，不触发", 5, -4, false, ""},
		{"配置40、浮亏20%：比默认更松，不触发", 40, -20, false, ""},
		{"配置40、浮亏41%：触发", 40, -41, true, "40.0%"},
		{"刚好等于止损线：触发", 5, -5, true, "5.0%"},
		{"盈利状态：永远不触发", 5, 12, false, ""},
	}
	for _, c := range cases {
		got, reason := e.ShouldForceClose(AccountState{CurrentUnrealizedPnLPct: c.curPct, StopLossPct: c.stopLoss})
		if got != c.wantClose {
			t.Errorf("%s: 期望触发=%v 实际=%v (%s)", c.name, c.wantClose, got, reason)
			continue
		}
		if c.wantClose && !strings.Contains(reason, c.wantInMsg) {
			t.Errorf("%s: 原因里应包含 %q，实际 %q", c.name, c.wantInMsg, reason)
		}
	}
}
