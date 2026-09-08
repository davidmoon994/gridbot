package strategy

import "testing"

// 验证修复：模拟"程序重启20次"，每次都是全新的Engine实例，用完全相同的
// symbol/index/seq参数生成第一笔订单的ClientOrderID，确认20次生成的ID
// 互不相同（修复前：instanceID不存在时，这20个ID会完全一样，必然撞车）。
func TestInstanceIDPreventsRestartCollision(t *testing.T) {
	seen := map[string]bool{}
	for restart := 0; restart < 20; restart++ {
		e := NewEngine(Config{Symbol: "ETHUSDC"})
		// 直接复刻 placeMissingOrders 里买单分支的ID生成公式
		id := e.cfg.Symbol + "-B-" + "-7" + "-" + e.instanceID + "-0"
		if seen[id] {
			t.Fatalf("第%d次模拟重启生成的ID与之前重复: %s", restart+1, id)
		}
		seen[id] = true
	}
	t.Logf("20次模拟重启，生成了%d个互不相同的ID，修复生效", len(seen))
}
