package book

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“同一引擎内成交编号在整个账户范围内唯一、不能被不同订单重复借用”
// 补充回归保障。成交编号始终由调用方提供：一张买单已成功部分成交后，另一张
// 本身完全具备成交条件的有效买单（价格不越过限价、剩余量充足、订单未失效），
// 即使提交完全相同的成交价格与数量，也不能借用前一笔成交的编号——无论两张
// 买单是否属于同一合约，编号唯一性都作用于整个账户而不是分合约计算。
//
// 冲突回报必须返回错误与零值成交结果，只追加一条拒绝记录：记录保存本次提交
// 的订单编号、成交编号、合约、方向、价格与数量，原因明确指出该编号已用于另一
// 笔成交；其中报价与最大持仓量快照对应本次申请成交的合约，不能错用最初成交
// 合约的快照。现金余额、可用现金、买单现金占用、各合约持仓以及两张订单的累计
// 成交量、有效剩余量与状态都与提交前一致：另一张订单不能被当作已经成交，第一
// 张订单已入账部分也不能回退，原有接受记录与成交记录原样保留。
//
// 冲突之后，原成交的完整回报重放仍返回第一次的结果（保留第一次入账时的累计
// 成交量、剩余量与状态），不新增记录、不再次记账；另一张订单换用尚未使用的
// 正成交编号并提交原先被拒绝的价格与数量时应正常成交：按实际成交金额扣现金、
// 按该订单限价释放对应数量的现金占用、增加持仓、更新累计成交量/剩余量/状态，
// 只新增一条成交记录，第一张订单保持原状。

// tradeIDConflictLedger 是冲突提交前后必须保持一致的账户与订单快照。
type tradeIDConflictLedger struct {
	cash      int64
	reserved  int64
	available int64
	posA      int64
	posB      int64
	order1    Order
	order2    Order
}

func snapshotTradeIDConflictLedger(e *Engine, o1, o2 int64, symA, symB string) tradeIDConflictLedger {
	ord1, _ := e.Order(o1)
	ord2, _ := e.Order(o2)
	return tradeIDConflictLedger{
		cash:      e.Cash(),
		reserved:  e.ReservedCash(),
		available: e.AvailableCash(),
		posA:      e.Position(symA),
		posB:      e.Position(symB),
		order1:    ord1,
		order2:    ord2,
	}
}

// assertTradeIDReuseRejected 校验另一张有效订单借用已入账成交编号时：返回错误
// 与零值结果、只新增一条拒绝记录；记录完整保留本次提交的订单编号、成交编号、
// 合约、方向、价格与数量，原因明确指出该编号已被原成交（prev）占用。
// 返回该拒绝记录，供调用方进一步校验报价与最大持仓量快照。
func assertTradeIDReuseRejected(t *testing.T, e *Engine, tr, prev Trade, recsBefore int) Record {
	t.Helper()
	res, err := e.Fill(tr)
	if err == nil {
		t.Fatalf("订单 %d 借用成交编号 %d 必须报错", tr.OrderID, tr.TradeID)
	}
	wantSubs := []string{
		fmt.Sprintf("成交编号 %d", tr.TradeID),
		"已用于",
		fmt.Sprintf("订单=%d", prev.OrderID),
		fmt.Sprintf("合约=%s", prev.Symbol),
	}
	for _, s := range wantSubs {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("错误 %q 应包含 %q", err.Error(), s)
		}
	}
	if res != (FillResult{}) {
		t.Fatalf("编号冲突必须返回零值成交结果，实际 %+v", res)
	}
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("编号冲突只能新增一条拒绝记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	rec := recs[len(recs)-1]
	if rec.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rec)
	}
	if rec.OrderID != tr.OrderID || rec.TradeID != tr.TradeID || rec.Symbol != tr.Symbol ||
		rec.Side != tr.Side || rec.TradePrice != tr.Price || rec.Qty != tr.Qty {
		t.Fatalf("拒绝记录必须保留本次提交的订单/成交编号、合约、方向、价格、数量: %+v", rec)
	}
	for _, s := range wantSubs {
		if !strings.Contains(rec.Reason, s) {
			t.Fatalf("拒绝原因 %q 应包含 %q", rec.Reason, s)
		}
	}
	// 全部场景均在未开启交易日的账户中进行：拒绝记录不得携带任何风险快照。
	if rec.RiskDay != 0 || rec.RiskBaseline != 0 || rec.RiskEquity != 0 || rec.RiskLoss != 0 ||
		rec.RiskLimit != 0 || rec.RiskRestrict {
		t.Fatalf("未开日时拒绝记录不应携带风险快照: %+v", rec)
	}
	return rec
}

// TestFillTradeIDReuseByAnotherBuySameSymbol 覆盖同一合约的两张买单：第一张买单
// 已成功部分成交后，第二张有效买单以完全相同的价格和数量借用前一笔成交编号，
// 必须仅因编号冲突被拒绝；冲突记录的报价与限额快照对应该合约，资金、持仓与
// 两张订单全部保持提交前的值。随后原成交重放返回第一次结果，第二张订单换新
// 编号提交原价格与数量后正常成交。
func TestFillTradeIDReuseByAnotherBuySameSymbol(t *testing.T) {
	e, _ := NewEngine(10000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	order1 := mustBuy(t, e, "A", 10, 10) // ID=1：先接受
	order2 := mustBuy(t, e, "A", 10, 10) // ID=2：另一张完全有效的买单

	// 第一张买单先以价格 9 部分成交 4 份：现金 -36=9964，释放 4×限价10 的占用，
	// 买单占用 200-40=160，持仓 A=4。
	first := Trade{TradeID: 1, OrderID: order1, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(first)
	if err != nil {
		t.Fatalf("第一张买单部分成交应成功: %v", err)
	}
	if res1.TradeID != 1 || res1.OrderID != order1 || res1.Price != 9 || res1.Qty != 4 ||
		res1.Filled != 4 || res1.Remaining != 6 || res1.Status != StatusPartial {
		t.Fatalf("第一次成交结果错误: %+v", res1)
	}
	if e.Cash() != 9964 || e.ReservedCash() != 160 || e.AvailableCash() != 9804 || e.Position("A") != 4 {
		t.Fatalf("前置账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(order2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 10 {
		t.Fatalf("第二张订单冲突前应为待成交、剩余 10: %+v", o)
	}

	// 固定冲突前的全部记录：接受 order1、接受 order2、成交编号 1，共三条。
	recsBefore := len(e.Records())
	if recsBefore != 3 {
		t.Fatalf("冲突前应有 3 条记录（接受×2+成交），实际 %d", recsBefore)
	}
	recordsBefore := e.Records()
	before := snapshotTradeIDConflictLedger(e, order1, order2, "A", "A")

	// 第二张订单本身完全具备成交条件（价格 9 不越过限价 10、数量 4 不超过剩余
	// 10、订单待成交未失效），与第一笔成交仅成交编号相同——拒绝只能来自编号冲突。
	conflict := Trade{TradeID: 1, OrderID: order2, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	rec := assertTradeIDReuseRejected(t, e, conflict, first, recsBefore)

	// 快照对应本次申请成交的合约 A。
	if !rec.QuoteValid || rec.QuoteSeq != 1 || rec.QuoteMoment != 10 || rec.QuotePrice != 10 {
		t.Fatalf("拒绝记录的报价快照必须取自合约 A: %+v", rec)
	}
	if !rec.MaxPositionValid || rec.MaxPosition != 100 {
		t.Fatalf("拒绝记录的最大持仓量快照必须取自合约 A: %+v", rec)
	}

	// 现金余额、可用现金、买单现金占用、持仓以及两张订单的累计成交量、剩余量、
	// 状态全部与冲突提交前一致。
	after := snapshotTradeIDConflictLedger(e, order1, order2, "A", "A")
	if after != before {
		t.Fatalf("编号冲突不得改变任何资金、持仓或订单状态:\n之前 %+v\n之后 %+v", before, after)
	}
	if o, _ := e.Order(order2); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 10 {
		t.Fatalf("第二张订单不能被当作已经成交: %+v", o)
	}

	// 原有接受记录与成交记录原样保留，只在末尾追加了那一条拒绝记录。
	recs := e.Records()
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}

	// 冲突后重新提交最初成功入账的完整回报：仍返回第一次成交结果（第一次入账时
	// 的累计成交量 4、剩余量 6、部分成交状态），不新增记录也不再次记账。
	again, err := e.Fill(first)
	if err != nil {
		t.Fatalf("原成交完整重放不应报错: %v", err)
	}
	if again != res1 {
		t.Fatalf("原成交重放必须返回第一次结果: want %+v got %+v", res1, again)
	}
	if len(e.Records()) != recsBefore+1 {
		t.Fatalf("原成交重放不得新增记录，实际 %d 条", len(e.Records()))
	}
	if got := snapshotTradeIDConflictLedger(e, order1, order2, "A", "A"); got != before {
		t.Fatalf("原成交重放不得再次改变资金、持仓或订单: %+v", got)
	}

	// 第二张订单换用尚未使用的正成交编号 2，提交原先被拒绝的价格 9 与数量 4：
	// 按实际金额 36 扣现金，按其限价 10 释放 4 份占用，持仓增加 4。
	res2, err := e.Fill(Trade{TradeID: 2, OrderID: order2, Symbol: "A", Side: Buy, Price: 9, Qty: 4})
	if err != nil {
		t.Fatalf("换新编号后第二张订单应正常成交: %v", err)
	}
	if res2.TradeID != 2 || res2.OrderID != order2 || res2.Price != 9 || res2.Qty != 4 ||
		res2.Filled != 4 || res2.Remaining != 6 || res2.Status != StatusPartial {
		t.Fatalf("第二张订单成交结果错误: %+v", res2)
	}
	if e.Cash() != 9928 || e.ReservedCash() != 120 || e.AvailableCash() != 9808 || e.Position("A") != 8 {
		t.Fatalf("第二张订单成交后账务错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(order1); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("第一张订单必须保持原状: %+v", o)
	}
	if o, _ := e.Order(order2); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("第二张订单应累计成交 4、剩余 6、部分成交: %+v", o)
	}

	// 只在拒绝记录之后新增一条成交记录；拒绝记录与此前各条记录都原样保留。
	recs = e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("换新编号成交后应只比冲突前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 1 ||
		recs[recsBefore].OrderID != order2 {
		t.Fatalf("编号冲突拒绝记录必须保留: %+v", recs[recsBefore])
	}
	fill2 := recs[recsBefore+1]
	if fill2.Kind != RecordFilled || fill2.TradeID != 2 || fill2.OrderID != order2 ||
		fill2.Symbol != "A" || fill2.Side != Buy || fill2.TradePrice != 9 ||
		fill2.Qty != 4 || fill2.Filled != 4 || fill2.Remaining != 6 {
		t.Fatalf("第二张订单的新成交记录错误: %+v", fill2)
	}
}

// TestFillTradeIDReuseByAnotherBuyDifferentSymbol 覆盖不同合约的两张买单，确认
// 成交编号唯一性作用于整个账户而不是各合约分别计算：A 合约买单的已成交编号
// 不能被 B 合约的有效买单借用。冲突拒绝记录的报价与最大持仓量快照必须取自
// 本次申请的 B 合约，不能错用最初成交的 A 合约。
func TestFillTradeIDReuseByAnotherBuyDifferentSymbol(t *testing.T) {
	e, _ := NewEngine(10000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustSetMax(t, e, "B", 50)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 9}, 1)

	orderA := mustBuy(t, e, "A", 10, 10) // ID=1，A 合约
	orderB := mustBuy(t, e, "B", 10, 9)  // ID=2，B 合约的有效买单

	// A 合约买单先以价格 9 部分成交 4 份：现金 9964，买单占用 190-40=150，
	// 持仓 A=4、B=0。
	first := Trade{TradeID: 1, OrderID: orderA, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(first)
	if err != nil {
		t.Fatalf("A 合约买单部分成交应成功: %v", err)
	}
	if res1.Filled != 4 || res1.Remaining != 6 || res1.Status != StatusPartial {
		t.Fatalf("第一次成交结果错误: %+v", res1)
	}
	if e.Cash() != 9964 || e.ReservedCash() != 150 || e.AvailableCash() != 9814 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置账务错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}

	recsBefore := len(e.Records())
	recordsBefore := e.Records()
	before := snapshotTradeIDConflictLedger(e, orderA, orderB, "A", "B")

	// B 合约订单本身完全可成交（价格 9 等于限价 9、数量 4 不超过剩余 10、订单
	// 待成交），提交与第一笔成交完全相同的价格和数量，仅成交编号撞号：跨合约
	// 借用编号必须拒绝，证明唯一性按整个账户计算。
	conflict := Trade{TradeID: 1, OrderID: orderB, Symbol: "B", Side: Buy, Price: 9, Qty: 4}
	rec := assertTradeIDReuseRejected(t, e, conflict, first, recsBefore)

	// 拒绝记录指向 B（本次申请成交的合约），原因里的原成交指向 A。
	if rec.Symbol != "B" {
		t.Fatalf("拒绝记录必须保存本次提交的合约 B，实际 %q", rec.Symbol)
	}
	if !rec.QuoteValid || rec.QuoteSeq != 1 || rec.QuoteMoment != 20 || rec.QuotePrice != 9 {
		t.Fatalf("拒绝记录的报价快照必须取自本次申请合约 B: %+v", rec)
	}
	if !rec.MaxPositionValid || rec.MaxPosition != 50 {
		t.Fatalf("拒绝记录的最大持仓量快照必须取自本次申请合约 B: %+v", rec)
	}
	// 明确不能错用最初成交合约 A（报价 10、限额 100、时刻 10）的快照。
	if rec.QuotePrice == 10 || rec.QuoteMoment == 10 || rec.MaxPosition == 100 {
		t.Fatalf("拒绝记录不得错用最初成交合约 A 的报价/限额快照: %+v", rec)
	}

	after := snapshotTradeIDConflictLedger(e, orderA, orderB, "A", "B")
	if after != before {
		t.Fatalf("跨合约编号冲突不得改变任何资金、持仓或订单状态:\n之前 %+v\n之后 %+v", before, after)
	}
	if o, _ := e.Order(orderB); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 10 {
		t.Fatalf("B 合约订单不能被当作已经成交: %+v", o)
	}
	if o, _ := e.Order(orderA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 合约订单已入账部分不能回退: %+v", o)
	}

	recs := e.Records()
	for i, want := range recordsBefore {
		if !reflect.DeepEqual(recs[i], want) {
			t.Fatalf("第 %d 条原有记录被改写:\nwant %+v\ngot  %+v", i, want, recs[i])
		}
	}

	// 原成交（A 合约）完整重放：返回第一次结果，不新增记录、不再次记账。
	again, err := e.Fill(first)
	if err != nil {
		t.Fatalf("原成交完整重放不应报错: %v", err)
	}
	if again != res1 {
		t.Fatalf("原成交重放必须返回第一次结果: want %+v got %+v", res1, again)
	}
	if len(e.Records()) != recsBefore+1 {
		t.Fatalf("原成交重放不得新增记录，实际 %d 条", len(e.Records()))
	}

	// B 合约订单换用新编号 2 提交原价格 9、原数量 4：按 9×4=36 扣现金并按其
	// 限价 9 释放 36 占用，B 持仓增加 4；A 合约持仓与第一张订单保持原状。
	resB, err := e.Fill(Trade{TradeID: 2, OrderID: orderB, Symbol: "B", Side: Buy, Price: 9, Qty: 4})
	if err != nil {
		t.Fatalf("B 合约订单换新编号后应正常成交: %v", err)
	}
	if resB.TradeID != 2 || resB.OrderID != orderB || resB.Filled != 4 ||
		resB.Remaining != 6 || resB.Status != StatusPartial {
		t.Fatalf("B 合约订单成交结果错误: %+v", resB)
	}
	if e.Cash() != 9928 || e.ReservedCash() != 114 || e.AvailableCash() != 9814 ||
		e.Position("A") != 4 || e.Position("B") != 4 {
		t.Fatalf("B 合约订单成交后账务错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(orderA); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("A 合约第一张订单必须保持原状: %+v", o)
	}
	if o, _ := e.Order(orderB); o.Filled != 4 || o.Remaining() != 6 || o.Status != StatusPartial {
		t.Fatalf("B 合约订单应累计成交 4、剩余 6、部分成交: %+v", o)
	}

	recs = e.Records()
	if len(recs) != recsBefore+2 {
		t.Fatalf("换新编号成交后应只比冲突前多两条记录（拒绝+成交），实际 %d 条",
			len(recs)-recsBefore)
	}
	if recs[recsBefore].Kind != RecordRejected || recs[recsBefore].TradeID != 1 ||
		recs[recsBefore].OrderID != orderB || recs[recsBefore].Symbol != "B" {
		t.Fatalf("跨合约编号冲突拒绝记录必须保留: %+v", recs[recsBefore])
	}
	fillB := recs[recsBefore+1]
	if fillB.Kind != RecordFilled || fillB.TradeID != 2 || fillB.OrderID != orderB ||
		fillB.Symbol != "B" || fillB.TradePrice != 9 || fillB.Qty != 4 ||
		fillB.Filled != 4 || fillB.Remaining != 6 {
		t.Fatalf("B 合约订单的新成交记录错误: %+v", fillB)
	}
}
