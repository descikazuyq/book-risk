package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“下调单个合约最大持仓量”（SetMaxPosition）补充自动化回归保障：
// 重点覆盖同一账户同时持有其他合约买单与目标合约卖单占用时，下调一个合约的
// 限额只处理这个合约的有效买单剩余量，全部结论以公开的订单、资金、持仓与
// 事件记录查询为准（未启用日内亏损保护与账户总持仓金额上限）。
//
// 固定场景（整数金额口径）：
//   - 初始现金 1000；A、B 最大持仓量均为 20。
//   - 已生效报价：A 序号1、时刻10、价格10；B 序号1、时刻20、价格5。
//   - 接受 A 买单 10@10（订单1），其中 3 份按价格 9 成交：现金 973、A 持仓 3。
//   - 接受 A 买单 2@12（订单2），再接受 B 买单 4@5（订单3）。
//   - 接受 A 卖单 2@8（订单4，占用 A 可卖 2 份，尚未成交）。
//   - 前置口径：A 持仓 3、可卖 1；现金余额 973；买单共占用现金 114
//     （订单1 剩余 7 份占 70，订单2 占 24，订单3 占 20），可用 859。
//
// 第一次下调 A 限额到 10：持仓 3 + A 买单剩余 9 = 12 超限，只撤销最后接受的
// A 买单（订单2），返回的撤销编号只含这一单；订单1 保留部分成交状态（已成交 3、
// 剩余 7），持仓与剩余量合计恰好达到新限额即停止。B 买单（订单3）虽然接受得更晚，
// 仍保持有效；A 卖单数量与可卖占用不变。撤单释放 24 现金：余额仍为 973（已成交
// 部分的实际金额不退回），占用 90，可用 883。
//
// 第二次下调 A 限额到 2：持仓 3 本身已超过新限额（待成交卖单不能提前抵减持仓），
// 撤销订单1 的全部剩余 7 份，保留其已成交 3 份，不自动卖出持仓。此后占用现金只剩
// B 买单的 20，A 持仓 3、可卖 1，卖单继续有效。两次撤销记录按实际撤销顺序保存，
// 分别说明限额变化原因、携带各自当时的 A 报价与新限额快照，并区分已成交数量与被
// 取消的剩余数量；第二次下调不改写第一次的记录，既有成交记录也保持原值。

// lowerMaxSetup 构造下调限额前的共用前置状态，返回引擎与四个订单编号
// （bidA1：A 部分成交买单；bidA2：A 待成交买单；bidB：B 待成交买单；sellA：A 有效卖单）。
func lowerMaxSetup(t *testing.T) (e *Engine, bidA1, bidA2, bidB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 20)
	mustSetMax(t, e, "B", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 5}, 1)

	bidA1 = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bidA1, Symbol: "A", Side: Buy, Price: 9, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	bidA2 = mustBuy(t, e, "A", 2, 12)
	bidB = mustBuy(t, e, "B", 4, 5)
	sellA = mustSell(t, e, "A", 2, 8)

	// 题目给定的前置资金与持仓口径，逐笔固化。
	if e.Cash() != 973 || e.ReservedCash() != 114 || e.AvailableCash() != 859 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 3 || e.Sellable("A") != 1 || e.Position("B") != 0 {
		t.Fatalf("前置持仓错误: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}
	return e, bidA1, bidA2, bidB, sellA
}

// TestSetMaxPositionLowerOnlyCancelsTargetSymbolBuys 覆盖主场景：
// 两次下调 A 的限额，第一次只撤最后接受的 A 买单，第二次因持仓本身超限撤掉
// 较早 A 买单的全部剩余量；B 买单与 A 卖单始终不受影响，撤销记录各自固化快照。
func TestSetMaxPositionLowerOnlyCancelsTargetSymbolBuys(t *testing.T) {
	e, bidA1, bidA2, bidB, sellA := lowerMaxSetup(t)

	// ---- 第一次下调：A 限额 20 -> 10 ----
	recsBefore10 := len(e.Records())
	canceled10, err := e.SetMaxPosition("A", 10)
	if err != nil {
		t.Fatalf("下调 A 限额到 10 不应报错: %v", err)
	}
	// 返回的撤销编号只包含后来接受的 A 买单（订单2）这一单。
	if len(canceled10) != 1 || canceled10[0] != bidA2 {
		t.Fatalf("下调到 10 应只撤销订单 %d，实际 %v", bidA2, canceled10)
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 10 {
		t.Fatalf("A 限额应已更新为 10: max=%d ok=%v", max, ok)
	}
	if max, ok := e.MaxPosition("B"); !ok || max != 20 {
		t.Fatalf("B 限额不受 A 下调影响，应保持 20: max=%d ok=%v", max, ok)
	}

	// 资金：撤单释放 2×12=24 占用；已成交部分的实际金额（3×9=27）不退回，
	// 现金余额仍为 973，占用 90，可用 883。
	if e.Cash() != 973 || e.ReservedCash() != 90 || e.AvailableCash() != 883 {
		t.Fatalf("下调到 10 后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓与可卖：A 持仓 3、可卖 1 不变，待成交卖单不提前抵减持仓。
	if e.Position("A") != 3 || e.Sellable("A") != 1 || e.Position("B") != 0 {
		t.Fatalf("下调到 10 后持仓错误: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：订单1 保留部分成交（已成交 3、剩余 7），持仓与剩余量合计
	// 恰好达到新限额 10，不能继续撤单；订单2 整单撤销；B 买单与 A 卖单原样保留。
	o1, _ := e.Order(bidA1)
	if o1.Status != StatusPartial || o1.Qty != 10 || o1.Filled != 3 || o1.Remaining() != 7 {
		t.Fatalf("较早 A 买单应保留部分成交、剩余 7: %+v", o1)
	}
	o2, _ := e.Order(bidA2)
	if o2.Status != StatusCanceled || o2.Qty != 2 || o2.Filled != 0 || o2.Remaining() != 0 {
		t.Fatalf("后来 A 买单应整单撤销: %+v", o2)
	}
	if !strings.Contains(o2.Reason, "10") {
		t.Fatalf("订单2 撤销原因应说明限额下调至 10: %q", o2.Reason)
	}
	oB, _ := e.Order(bidB)
	if oB.Status != StatusPending || oB.Qty != 4 || oB.Remaining() != 4 {
		t.Fatalf("B 买单接受得更晚仍应保持有效: %+v", oB)
	}
	oSell, _ := e.Order(sellA)
	if oSell.Status != StatusPending || oSell.Side != Sell || oSell.Qty != 2 ||
		oSell.Limit != 8 || oSell.Remaining() != 2 || oSell.Reason != "" {
		t.Fatalf("A 卖单数量与可卖占用不得改变: %+v", oSell)
	}

	// 第一次下调恰好新增一条撤销记录：说明限额下调原因，携带当时 A 报价
	// （序号1、时刻10、价格10）与新限额 10 快照，已成交 0、被取消剩余 2。
	newRecs10 := e.Records()[recsBefore10:]
	if len(newRecs10) != 1 {
		t.Fatalf("下调到 10 应只新增 1 条撤销记录，实际 %d 条: %+v", len(newRecs10), newRecs10)
	}
	cr2 := newRecs10[0]
	if cr2.Kind != RecordCanceled || cr2.OrderID != bidA2 || cr2.Symbol != "A" || cr2.Side != Buy {
		t.Fatalf("应为订单2 的撤销记录: %+v", cr2)
	}
	if cr2.Qty != 2 || cr2.Filled != 0 || cr2.Remaining != 2 {
		t.Fatalf("订单2 撤销记录应区分已成交 0 与被取消剩余 2: %+v", cr2)
	}
	if !cr2.QuoteValid || cr2.QuoteSeq != 1 || cr2.QuoteMoment != 10 || cr2.QuotePrice != 10 {
		t.Fatalf("订单2 撤销记录应携带当时 A 报价（序号1、时刻10、价格10）: %+v", cr2)
	}
	if !cr2.MaxPositionValid || cr2.MaxPosition != 10 {
		t.Fatalf("订单2 撤销记录应携带新限额 10 快照: %+v", cr2)
	}
	if !strings.Contains(cr2.Reason, "10") {
		t.Fatalf("订单2 撤销原因应说明限额下调至 10: %q", cr2.Reason)
	}

	// ---- 第二次下调：A 限额 10 -> 2 ----
	recsBefore2 := len(e.Records())
	canceled2, err := e.SetMaxPosition("A", 2)
	if err != nil {
		t.Fatalf("下调 A 限额到 2 不应报错: %v", err)
	}
	// 持仓 3 本身已超过新限额 2（待成交卖单不抵减持仓）：撤销订单1 的全部
	// 剩余 7 份，保留已成交 3 份，不自动卖出持仓。
	if len(canceled2) != 1 || canceled2[0] != bidA1 {
		t.Fatalf("下调到 2 应只撤销订单 %d，实际 %v", bidA1, canceled2)
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 2 {
		t.Fatalf("A 限额应已更新为 2: max=%d ok=%v", max, ok)
	}

	// 资金：再释放 7×10=70，占用现金只剩 B 买单的 20；余额仍为 973，可用 953。
	if e.Cash() != 973 || e.ReservedCash() != 20 || e.AvailableCash() != 953 {
		t.Fatalf("下调到 2 后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓不自动卖出：A 持仓仍为 3、可卖仍为 1，原有卖单继续有效。
	if e.Position("A") != 3 || e.Sellable("A") != 1 || e.Position("B") != 0 {
		t.Fatalf("下调到 2 后持仓错误: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}
	o1, _ = e.Order(bidA1)
	if o1.Status != StatusCanceled || o1.Qty != 10 || o1.Filled != 3 || o1.Remaining() != 0 {
		t.Fatalf("订单1 应保留已成交 3 并撤销剩余 7: %+v", o1)
	}
	if !strings.Contains(o1.Reason, "持仓 3") || !strings.Contains(o1.Reason, "2") {
		t.Fatalf("订单1 撤销原因应说明持仓 3 已超过新限额 2: %q", o1.Reason)
	}
	oB, _ = e.Order(bidB)
	if oB.Status != StatusPending || oB.Remaining() != 4 {
		t.Fatalf("B 买单在第二次下调后仍应保持有效: %+v", oB)
	}
	oSell, _ = e.Order(sellA)
	if oSell.Status != StatusPending || oSell.Remaining() != 2 || oSell.Reason != "" {
		t.Fatalf("A 卖单在第二次下调后仍应继续有效: %+v", oSell)
	}

	// 第二次下调恰好新增一条撤销记录：说明持仓超限原因，携带当时 A 报价
	// 与新限额 2 快照，区分已成交 3 与被取消剩余 7。
	newRecs2 := e.Records()[recsBefore2:]
	if len(newRecs2) != 1 {
		t.Fatalf("下调到 2 应只新增 1 条撤销记录，实际 %d 条: %+v", len(newRecs2), newRecs2)
	}
	cr1 := newRecs2[0]
	if cr1.Kind != RecordCanceled || cr1.OrderID != bidA1 || cr1.Symbol != "A" || cr1.Side != Buy {
		t.Fatalf("应为订单1 的撤销记录: %+v", cr1)
	}
	if cr1.Qty != 10 || cr1.Filled != 3 || cr1.Remaining != 7 {
		t.Fatalf("订单1 撤销记录应区分已成交 3 与被取消剩余 7: %+v", cr1)
	}
	if !cr1.QuoteValid || cr1.QuoteSeq != 1 || cr1.QuoteMoment != 10 || cr1.QuotePrice != 10 {
		t.Fatalf("订单1 撤销记录应携带当时 A 报价（序号1、时刻10、价格10）: %+v", cr1)
	}
	if !cr1.MaxPositionValid || cr1.MaxPosition != 2 {
		t.Fatalf("订单1 撤销记录应携带新限额 2 快照: %+v", cr1)
	}
	if !strings.Contains(cr1.Reason, "持仓 3") || !strings.Contains(cr1.Reason, "2") {
		t.Fatalf("订单1 撤销原因应说明持仓 3 已超过新限额 2: %q", cr1.Reason)
	}

	// 两次撤销记录按实际撤销顺序保存：先订单2（限额 10），后订单1（限额 2）。
	var cancelOrder []int64
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled {
			cancelOrder = append(cancelOrder, r.OrderID)
		}
	}
	if !reflect.DeepEqual(cancelOrder, []int64{bidA2, bidA1}) {
		t.Fatalf("撤销记录应按实际撤销顺序保存为 [%d %d]，实际 %v", bidA2, bidA1, cancelOrder)
	}

	// 第二次下调不能改写第一次的撤销记录。
	if got := e.Records()[recsBefore10]; !reflect.DeepEqual(got, cr2) {
		t.Fatalf("第二次下调不得改写第一次的撤销记录:\n第一次: %+v\n现在:   %+v", cr2, got)
	}

	// 既有成交记录保持原值：成交 3 份、价格 9，快照仍为当时的限额 20。
	var fillRec *Record
	recs := e.Records()
	for i := range recs {
		if recs[i].Kind == RecordFilled && recs[i].TradeID == 1 {
			fillRec = &recs[i]
		}
	}
	if fillRec == nil {
		t.Fatal("应存在订单1 的成交记录")
	}
	if fillRec.OrderID != bidA1 || fillRec.Qty != 3 || fillRec.TradePrice != 9 ||
		fillRec.Filled != 3 || fillRec.Remaining != 7 {
		t.Fatalf("成交记录应保持成交 3 份、价格 9、当时剩余 7: %+v", *fillRec)
	}
	if !fillRec.MaxPositionValid || fillRec.MaxPosition != 20 {
		t.Fatalf("成交记录的限额快照应保持成交时的 20，不被后续下调改写: %+v", *fillRec)
	}

	// 全部记录中不得出现 B 买单或 A 卖单的撤销。
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && (r.OrderID == bidB || r.OrderID == sellA) {
			t.Fatalf("B 买单与 A 卖单不得被 A 限额下调撤销: %+v", r)
		}
	}
}

// TestSetMaxPositionLowerToCurrentUsageCancelsNothing 钉住边界：
// 下调后的新限额恰好等于当前“持仓 + 有效买单剩余量”时不撤任何单。
func TestSetMaxPositionLowerToCurrentUsageCancelsNothing(t *testing.T) {
	e, bidA1, bidA2, bidB, sellA := lowerMaxSetup(t)

	// 持仓 3 + A 买单剩余 9 = 12，下调到 12 恰好用满额度。
	recsBefore := len(e.Records())
	canceled, err := e.SetMaxPosition("A", 12)
	if err != nil {
		t.Fatalf("下调 A 限额到 12 不应报错: %v", err)
	}
	if len(canceled) != 0 {
		t.Fatalf("新限额恰好等于当前占用时不应撤单，实际 %v", canceled)
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 12 {
		t.Fatalf("A 限额应已更新为 12: max=%d ok=%v", max, ok)
	}
	if e.Cash() != 973 || e.ReservedCash() != 114 || e.AvailableCash() != 859 {
		t.Fatalf("未撤单时资金占用必须保持原值: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 3 || e.Sellable("A") != 1 {
		t.Fatalf("未撤单时持仓与可卖必须保持原值: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	for _, id := range []int64{bidA1, bidA2, bidB, sellA} {
		o, _ := e.Order(id)
		if o.Status == StatusCanceled {
			t.Fatalf("订单 %d 不应被撤销: %+v", id, o)
		}
	}
	if o, _ := e.Order(bidA1); o.Status != StatusPartial || o.Remaining() != 7 {
		t.Fatalf("订单1 应保持部分成交、剩余 7: %+v", o)
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("恰好用满额度的下调不得追加任何记录")
	}
}
