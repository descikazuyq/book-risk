package book

import (
	"strconv"
	"strings"
	"testing"
)

// 本文件为“同一引擎内成交编号账户级唯一、不能被不同订单重复使用”补充回归保障：
// 未开启交易日的账户中，一张买单已成功部分成交后，另一张完全具备成交条件的有效
// 买单即使提交完全相同的成交价格和数量，也不能借用前一笔成交编号——冲突回报必须
// 返回错误与零值成交结果，只追加一条拒绝记录（保留本次提交的订单编号、成交编号、
// 合约、方向、价格、数量，原因指明该编号已用于另一笔成交），记录中的报价与最大
// 持仓量快照对应本次申请成交的合约；现金余额、可用现金、买单现金占用、各合约持仓
// 以及两张订单的累计成交量、有效剩余量与状态全部保持不变，第二张订单不能被当作
// 已成交，第一张订单已入账部分不能回退，原有接受与成交记录原样保留。
//
// 冲突后重新提交最初成功入账的完整回报：按幂等规则返回第一次的成交结果，不新增
// 记录、不再次改变资金/持仓/订单，结果保留第一次入账时的累计成交量、剩余量与
// 状态。第二张订单换用一个尚未使用的正成交编号、再提交原先被拒的价格和数量时，
// 必须按普通成交正常入账。
//
// 编号唯一性作用于整个账户而非各合约分别计算，因此同合约两张买单与不同合约的
// 买单两种情形都要覆盖。

// assertTradeIDReuseRejected 校验另一张有效订单借用已成交编号时被整笔拒绝：
// 返回错误与零值 FillResult；只新增一条拒绝记录；记录保留本次提交的订单编号、
// 成交编号、合约、方向、价格与数量，原因明确指出该编号已用于另一笔成交并给出
// 原订单与原合约；报价与最大持仓量快照锚定本次申请合约（wantQuote/wantMax），
// 未开日时不得携带风险快照。
func assertTradeIDReuseRejected(t *testing.T, e *Engine, tr Trade, recsBefore int,
	prevOrderID int64, prevSymbol string, wantQuote Quote, wantMax int64) {
	t.Helper()
	res, err := e.Fill(tr)
	if err == nil {
		t.Fatalf("订单 %d 借用已成交编号 %d 必须报错: %+v", tr.OrderID, tr.TradeID, tr)
	}
	for _, s := range []string{"成交编号", "已用于不同内容的成交", "订单="} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("冲突错误 %q 应包含 %q（指明编号已用于另一笔成交）", err.Error(), s)
		}
	}
	if !strings.Contains(err.Error(), prevSymbol) {
		t.Fatalf("冲突错误 %q 应指明原成交合约 %s", err.Error(), prevSymbol)
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(prevOrderID, 10)) {
		t.Fatalf("冲突错误 %q 应指明原成交订单 %d", err.Error(), prevOrderID)
	}
	if res != (FillResult{}) {
		t.Fatalf("编号冲突必须返回零值成交结果，实际 %+v", res)
	}

	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("编号冲突只能新增一条记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	rec := recs[len(recs)-1]
	if rec.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rec)
	}
	// 记录保存本次提交（第二张订单）的内容，而不是原成交的内容。
	if rec.OrderID != tr.OrderID || rec.TradeID != tr.TradeID || rec.Symbol != tr.Symbol ||
		rec.Side != tr.Side || rec.TradePrice != tr.Price || rec.Qty != tr.Qty {
		t.Fatalf("拒绝记录必须保留本次提交的订单/成交编号、合约、方向、价格、数量: %+v", rec)
	}
	if tr.Symbol != prevSymbol && rec.Symbol == prevSymbol {
		t.Fatalf("跨合约冲突时拒绝记录不得错用最初成交的合约 %s: %+v", prevSymbol, rec)
	}
	if !strings.Contains(rec.Reason, "已用于不同内容的成交") {
		t.Fatalf("拒绝原因必须指明编号已用于另一笔成交: %q", rec.Reason)
	}
	// 报价与最大持仓量快照对应本次申请成交的合约，不能错用最初成交的合约。
	if !rec.QuoteValid || rec.QuoteSeq != wantQuote.Seq ||
		rec.QuoteMoment != wantQuote.Moment || rec.QuotePrice != wantQuote.Price {
		t.Fatalf("拒绝记录报价快照必须锚定本次申请合约 %s 的 %+v: %+v",
			tr.Symbol, wantQuote, rec)
	}
	if !rec.MaxPositionValid || rec.MaxPosition != wantMax {
		t.Fatalf("拒绝记录最大持仓量快照必须锚定本次申请合约 %s 的 %d: %+v",
			tr.Symbol, wantMax, rec)
	}
	if rec.RiskDay != 0 || rec.RiskBaseline != 0 || rec.RiskLimit != 0 || rec.RiskRestrict {
		t.Fatalf("未开启交易日时拒绝记录不得携带风险快照: %+v", rec)
	}
}

// TestFillTradeIDReuseRejectedSameSymbol 覆盖同一合约的两张买单：第一张买单已
// 部分成交，第二张有效买单以完全相同的价格和数量借用前一笔成交编号，必须因
// 编号冲突被拒绝（而不是因价格越限、剩余量不足或订单失效）；随后原成交完整
// 回报幂等重放，第二张订单换新编号后按原价格数量正常成交。
func TestFillTradeIDReuseRejectedSameSymbol(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// 第一张买单：总量 10、限价 10；先以 9×4 部分成交。
	o1 := mustBuy(t, e, "A", 10, 10)
	first := Trade{TradeID: 1, OrderID: o1, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(first)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Filled != 4 || res1.Remaining != 6 || res1.Status != StatusPartial {
		t.Fatalf("前置成交结果错误: %+v", res1)
	}
	// cash=1000-36=964，reserved=100-40=60，持仓 A=4。
	if e.Cash() != 964 || e.ReservedCash() != 60 || e.AvailableCash() != 904 || e.Position("A") != 4 {
		t.Fatalf("前置资金状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 第二张买单：总量 6、限价 10，与第一张剩余量互不影响；占用现金 60。
	o2 := mustBuy(t, e, "A", 6, 10)
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 {
		t.Fatalf("第二单接受后资金状态错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if o, _ := e.Order(o2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("第二单应为待成交、剩余 6: %+v", o)
	}
	// 第二单具备正常成交条件：价格 9 不高于限价 10、数量 4 不超过剩余 6，
	// 因此随后若被拒绝只可能来自成交编号冲突。
	conflict := Trade{TradeID: 1, OrderID: o2, Symbol: "A", Side: Buy, Price: 9, Qty: 4}

	recsBefore := len(e.Records())
	assertTradeIDReuseRejected(t, e, conflict, recsBefore, o1, "A",
		Quote{Seq: 1, Moment: 10, Price: 10}, 100)

	// 提交前后资金、持仓完全一致：第二单不能被当作已成交，第一单已入账部分不回退。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 || e.Position("A") != 4 {
		t.Fatalf("编号冲突不得改变资金或持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(o1); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("第一张订单已入账部分不得回退: %+v", o)
	}
	if o, _ := e.Order(o2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("第二张订单不得被当作已成交: %+v", o)
	}
	// 原有接受记录与成交记录原样保留，仅在末尾多一条拒绝记录。
	recs := e.Records()
	if recs[0].Kind != RecordAccepted || recs[0].OrderID != o1 {
		t.Fatalf("第一张订单的接受记录必须原样保留: %+v", recs[0])
	}
	if recs[1].Kind != RecordFilled || recs[1].OrderID != o1 || recs[1].TradeID != 1 ||
		recs[1].Filled != 4 || recs[1].Remaining != 6 || recs[1].TradePrice != 9 {
		t.Fatalf("第一张订单的成交记录必须原样保留: %+v", recs[1])
	}
	if recs[2].Kind != RecordAccepted || recs[2].OrderID != o2 {
		t.Fatalf("第二张订单的接受记录必须原样保留: %+v", recs[2])
	}

	// 冲突后重新提交最初成功入账的完整回报：返回第一次的成交结果，
	// 不新增记录，也不再次改变资金、持仓或订单。
	again, err := e.Fill(first)
	if err != nil {
		t.Fatalf("原成交完整回报重放不应报错: %v", err)
	}
	if again != res1 {
		t.Fatalf("重放必须返回第一次入账时的结果（累计 4、剩余 6、部分成交）: %+v vs %+v",
			again, res1)
	}
	if len(e.Records()) != recsBefore+1 {
		t.Fatalf("幂等重放不得新增记录: %d vs %d", len(e.Records()), recsBefore+1)
	}
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.Position("A") != 4 {
		t.Fatalf("幂等重放不得再次改变资金或持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(o1); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("重放结果必须保留第一次入账时的成交量/剩余量/状态: %+v", o)
	}
	if o, _ := e.Order(o2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("重放不得把第二张订单记为已成交: %+v", o)
	}

	// 第二张订单换用尚未使用的正成交编号 2，提交原先被拒的价格与数量：正常成交。
	rescued := Trade{TradeID: 2, OrderID: o2, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res2, err := e.Fill(rescued)
	if err != nil {
		t.Fatalf("换新编号后提交原价格数量应正常成交: %v", err)
	}
	if res2.TradeID != 2 || res2.OrderID != o2 || res2.Price != 9 || res2.Qty != 4 ||
		res2.Filled != 4 || res2.Remaining != 2 || res2.Status != StatusPartial {
		t.Fatalf("第二单成交结果错误: %+v", res2)
	}
	// 按实际成交金额 9×4=36 扣现金，按第二单限价 10 释放 4 股的现金占用，
	// 持仓增加 4：cash=928，reserved=120-40=80，available=848，posA=8。
	if e.Cash() != 928 || e.ReservedCash() != 80 || e.AvailableCash() != 848 || e.Position("A") != 8 {
		t.Fatalf("第二单成交后资金持仓错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(o2); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 2 {
		t.Fatalf("第二单应累计成交 4、剩余 2、部分成交: %+v", o)
	}
	if o, _ := e.Order(o1); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("第一张订单必须保持原状: %+v", o)
	}
	// 只新增一条成交记录；此前的拒绝记录保留。
	recs = e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("换编号成交后应只比冲突前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 1 ||
		recs[recsBefore].OrderID != o2 {
		t.Fatalf("冲突拒绝记录必须保留: %+v", recs[recsBefore])
	}
	filled := recs[recsBefore+1]
	if filled.Kind != RecordFilled || filled.OrderID != o2 || filled.TradeID != 2 ||
		filled.Symbol != "A" || filled.Side != Buy || filled.TradePrice != 9 ||
		filled.Qty != 4 || filled.Filled != 4 || filled.Remaining != 2 {
		t.Fatalf("新成交记录错误: %+v", filled)
	}
}

// TestFillTradeIDReuseRejectedAcrossSymbols 覆盖不同合约的买单：成交编号唯一性
// 作用于整个账户，第二张买单即使属于另一个合约也不能借用第一笔成交编号。
// 两个合约使用不同的报价时刻/价格与持仓限额，以确认冲突拒绝记录中的报价与
// 最大持仓量快照锚定本次申请成交的合约（B），而不是最初成交的合约（A）。
func TestFillTradeIDReuseRejectedAcrossSymbols(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustSetMax(t, e, "B", 50)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 8}, 1)

	// A 合约第一张买单部分成交 4 股 @8：cash=968，reserved=60，posA=4。
	o1 := mustBuy(t, e, "A", 10, 10)
	first := Trade{TradeID: 1, OrderID: o1, Symbol: "A", Side: Buy, Price: 8, Qty: 4}
	res1, err := e.Fill(first)
	if err != nil {
		t.Fatal(err)
	}
	if res1.Filled != 4 || res1.Remaining != 6 || res1.Status != StatusPartial {
		t.Fatalf("前置成交结果错误: %+v", res1)
	}
	if e.Cash() != 968 || e.ReservedCash() != 60 || e.AvailableCash() != 908 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置资金持仓错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}

	// B 合约第二张买单：限价 8、总量 6，占用 48 现金；价格 8、数量 4 完全可成交。
	o2 := mustBuy(t, e, "B", 6, 8)
	if e.Cash() != 968 || e.ReservedCash() != 108 || e.AvailableCash() != 860 {
		t.Fatalf("第二单接受后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 与第一笔完全相同的成交价格和数量，但订单与合约不同：编号冲突必须拒绝。
	conflict := Trade{TradeID: 1, OrderID: o2, Symbol: "B", Side: Buy, Price: 8, Qty: 4}

	recsBefore := len(e.Records())
	assertTradeIDReuseRejected(t, e, conflict, recsBefore, o1, "A",
		Quote{Seq: 1, Moment: 20, Price: 8}, 50)

	// 两个合约的资金与持仓都保持提交前的值。
	if e.Cash() != 968 || e.ReservedCash() != 108 || e.AvailableCash() != 860 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("跨合约编号冲突不得改变资金或任一合约持仓: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(o1); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 合约第一张订单已入账部分不得回退: %+v", o)
	}
	if o, _ := e.Order(o2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("B 合约第二张订单不得被当作已成交: %+v", o)
	}
	// 原有接受与成交记录原样保留。
	recs := e.Records()
	if recs[0].Kind != RecordAccepted || recs[0].Symbol != "A" || recs[0].OrderID != o1 {
		t.Fatalf("A 单接受记录必须原样保留: %+v", recs[0])
	}
	if recs[1].Kind != RecordFilled || recs[1].Symbol != "A" || recs[1].TradeID != 1 ||
		recs[1].Filled != 4 || recs[1].Remaining != 6 {
		t.Fatalf("A 单成交记录必须原样保留: %+v", recs[1])
	}
	if recs[2].Kind != RecordAccepted || recs[2].Symbol != "B" || recs[2].OrderID != o2 {
		t.Fatalf("B 单接受记录必须原样保留: %+v", recs[2])
	}

	// 重新提交 A 合约最初成功入账的完整回报：返回第一次结果，不新增记录、不再记账。
	again, err := e.Fill(first)
	if err != nil {
		t.Fatalf("原成交完整回报重放不应报错: %v", err)
	}
	if again != res1 {
		t.Fatalf("重放必须返回第一次入账时的结果: %+v vs %+v", again, res1)
	}
	if len(e.Records()) != recsBefore+1 {
		t.Fatalf("幂等重放不得新增记录: %d vs %d", len(e.Records()), recsBefore+1)
	}
	if e.Cash() != 968 || e.ReservedCash() != 108 || e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("幂等重放不得改变资金或持仓: cash=%d reserved=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(o1); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("A 单必须保留第一次入账时的成交量/剩余量/状态: %+v", o)
	}
	if o, _ := e.Order(o2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 6 {
		t.Fatalf("B 单不得被当作已成交: %+v", o)
	}

	// B 单换用尚未使用的正成交编号 2，提交原先被拒的价格与数量：正常成交。
	rescued := Trade{TradeID: 2, OrderID: o2, Symbol: "B", Side: Buy, Price: 8, Qty: 4}
	res2, err := e.Fill(rescued)
	if err != nil {
		t.Fatalf("B 单换新编号后应正常成交: %v", err)
	}
	if res2.TradeID != 2 || res2.OrderID != o2 || res2.Price != 8 || res2.Qty != 4 ||
		res2.Filled != 4 || res2.Remaining != 2 || res2.Status != StatusPartial {
		t.Fatalf("B 单成交结果错误: %+v", res2)
	}
	// 按实际金额 8×4=32 扣现金，按 B 单限价 8 释放 4 股占用 32：
	// cash=936，reserved=108-32=76，posB=4，posA 保持 4。
	if e.Cash() != 936 || e.ReservedCash() != 76 || e.AvailableCash() != 860 ||
		e.Position("A") != 4 || e.Position("B") != 4 {
		t.Fatalf("B 单成交后资金持仓错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(o2); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 2 {
		t.Fatalf("B 单应累计成交 4、剩余 2、部分成交: %+v", o)
	}
	if o, _ := e.Order(o1); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 单必须保持原状: %+v", o)
	}
	// 只新增一条成交记录；冲突拒绝记录保留。
	recs = e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("换编号成交后应只比冲突前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 1 ||
		recs[recsBefore].OrderID != o2 || recs[recsBefore].Symbol != "B" {
		t.Fatalf("冲突拒绝记录必须保留且属于 B 单: %+v", recs[recsBefore])
	}
	filled := recs[recsBefore+1]
	if filled.Kind != RecordFilled || filled.OrderID != o2 || filled.TradeID != 2 ||
		filled.Symbol != "B" || filled.Side != Buy || filled.TradePrice != 8 ||
		filled.Qty != 4 || filled.Filled != 4 || filled.Remaining != 2 {
		t.Fatalf("B 单新成交记录错误: %+v", filled)
	}
}
