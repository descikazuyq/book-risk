package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“下调单个合约最大持仓量”补充自动化回归保障，重点覆盖同一账户同时
// 持有其他合约买单与目标合约卖单占用的情形：下调一个合约的限额只能处理这个合约
// 的有效买单剩余量，其他合约订单、卖单占用与既有成交记录一律保持原值。
//
// 固定场景（整数金额口径，全部以公开查询结果为准；未启用日内亏损保护与账户总
// 持仓金额上限）：
//   - 初始现金 1000；A、B 最大持仓量均为 20。
//   - 已生效报价：A 序号1、时刻10、价格10；B 序号1、时刻20、价格5。
//   - 接受 A 买单 10@10（订单1），其中 3 份按价格 9 成交：现金 973、A 持仓 3、
//     订单1 剩余 7 份占 70。
//   - 接受 A 买单 2@12（订单2，占 24），接受 B 买单 4@5（订单3，占 20）：
//     买单共占用 114 现金。
//   - 接受 A 卖单 2@8（订单4，未成交，占用 A 可卖 2 份）：A 可卖 1。
//
// 第一次把 A 限额下调到 10：持仓 3 + 买单剩余 9 = 12 超限，只撤销最后接受的
// A 买单（订单2，剩余 2 份，释放 24）；此后 3+7=10 恰好达到新限额，较早的
// 订单1 保留部分成交状态不得再撤。B 买单接受得更晚仍保持有效，A 卖单数量与
// 可卖占用不变；现金余额仍为 973（已成交部分不退款），占用 90、可用 883。
//
// 第二次把 A 限额下调到 2：持仓 3 本身已超限，撤销订单1 的全部剩余 7 份
// （释放 70），保留已成交 3 份，不自动卖出持仓；占用现金只剩 B 买单的 20，
// A 持仓 3、可卖 1，卖单继续有效。两次撤销记录按实际撤销顺序保存，分别说明
// 各自的限额变化原因，携带当时的 A 报价与各自的新限额，并区分已成交数量与被
// 取消的剩余数量；第二次下调不得改写第一次的记录与既有成交记录。

// lowerMaxSetup 构造下调限额前的共用前置状态，返回引擎与四个订单编号
// （bidA1：A 部分成交买单；bidA2：A 待成交买单；bidB：B 待成交买单；
// sellA：A 有效卖单）。
func lowerMaxSetup(t *testing.T) (e *Engine, bidA1, bidA2, bidB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 20)
	mustSetMax(t, e, "B", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 5}, 1)

	bidA1 = mustBuy(t, e, "A", 10, 10)
	fr, err := e.Fill(Trade{TradeID: 1, OrderID: bidA1, Symbol: "A", Side: Buy, Price: 9, Qty: 3})
	if err != nil {
		t.Fatal(err)
	}
	if fr.Filled != 3 || fr.Remaining != 7 || fr.Status != StatusPartial {
		t.Fatalf("订单1 应部分成交 3 份、剩余 7 份: %+v", fr)
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

// TestSetMaxPositionLowerCancelsOnlyLastAcceptedBuyOfSymbol 覆盖第一次下调：
// A 限额 20 → 10 时只撤销最后接受的 A 买单（订单2），返回的撤销编号只含这一单；
// 较早的部分成交 A 买单、更晚接受的 B 买单与 A 卖单全部保持原状态与占用。
func TestSetMaxPositionLowerCancelsOnlyLastAcceptedBuyOfSymbol(t *testing.T) {
	e, bidA1, bidA2, bidB, sellA := lowerMaxSetup(t)

	recsBefore := len(e.Records())
	canceled, err := e.SetMaxPosition("A", 10)
	if err != nil {
		t.Fatalf("下调 A 限额到 10 不应报错: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != bidA2 {
		t.Fatalf("返回的撤销编号应只包含后来接受的 A 买单 %d，实际 %v", bidA2, canceled)
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 10 {
		t.Fatalf("A 最大持仓量应为 10: max=%d ok=%v", max, ok)
	}

	// 资金：撤单只释放订单2 未成交部分的占用 24，已成交部分的实际金额不退回；
	// 现金余额仍为 973，占用 90（订单1 剩余 70 + B 单 20），可用 883。
	if e.Cash() != 973 || e.ReservedCash() != 90 || e.AvailableCash() != 883 {
		t.Fatalf("撤单后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓与可卖：不自动卖出持仓，也不用待成交卖单提前抵减；A 持仓 3、可卖 1。
	if e.Position("A") != 3 || e.Sellable("A") != 1 || e.Position("B") != 0 {
		t.Fatalf("撤单不得改动持仓与卖单占用: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：订单1 保留部分成交（已成交 3、剩余 7），持仓与剩余量合计
	// 3+7=10 恰好达到新限额，不能再撤；订单2 整单撤销。
	oA1, _ := e.Order(bidA1)
	if oA1.Status != StatusPartial || oA1.Qty != 10 || oA1.Filled != 3 || oA1.Remaining() != 7 {
		t.Fatalf("较早的 A 买单应保留部分成交、剩余 7: %+v", oA1)
	}
	oA2, _ := e.Order(bidA2)
	if oA2.Status != StatusCanceled || oA2.Qty != 2 || oA2.Filled != 0 || oA2.Remaining() != 0 {
		t.Fatalf("后来接受的 A 买单应整单撤销: %+v", oA2)
	}
	if !strings.Contains(oA2.Reason, "10") {
		t.Fatalf("订单2 撤销原因必须说明限额下调至 10: %q", oA2.Reason)
	}
	// B 买单虽然接受得更晚，仍应保持有效；A 卖单数量与占用不变。
	oB, _ := e.Order(bidB)
	if oB.Status != StatusPending || oB.Qty != 4 || oB.Limit != 5 || oB.Remaining() != 4 {
		t.Fatalf("B 买单必须保持有效、剩余 4: %+v", oB)
	}
	oSell, _ := e.Order(sellA)
	if oSell.Status != StatusPending || oSell.Side != Sell || oSell.Qty != 2 ||
		oSell.Limit != 8 || oSell.Remaining() != 2 || oSell.Reason != "" {
		t.Fatalf("A 卖单必须原样保留: %+v", oSell)
	}

	// 新增记录恰好 1 条：订单2 的撤销记录，说明限额下调原因，携带当时的
	// A 报价（序号1、时刻10、价格10）与新限额 10，并区分已成交 0 与被取消的 2。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 {
		t.Fatalf("第一次下调应只新增 1 条撤销记录，实际 %d 条: %+v", len(newRecs), newRecs)
	}
	cr := newRecs[0]
	if cr.Kind != RecordCanceled || cr.OrderID != bidA2 || cr.Symbol != "A" || cr.Side != Buy {
		t.Fatalf("新增记录应为订单2 的撤销记录: %+v", cr)
	}
	if cr.Limit != 12 || cr.Qty != 2 || cr.Filled != 0 || cr.Remaining != 2 {
		t.Fatalf("撤销记录必须保留限价 12、总量 2、已成交 0，并说明取消了 2 份: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 1 || cr.QuoteMoment != 10 || cr.QuotePrice != 10 {
		t.Fatalf("撤销记录的报价快照应锚定 A 序号1、时刻10、价格10: %+v", cr)
	}
	if !cr.MaxPositionValid || cr.MaxPosition != 10 {
		t.Fatalf("撤销记录必须固化新限额 10: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "限额") || !strings.Contains(cr.Reason, "10") {
		t.Fatalf("撤销原因必须说明限额下调至 10: %q", cr.Reason)
	}
}

// TestSetMaxPositionBelowPositionCancelsAllRemainingBuys 覆盖第二次下调：
// A 限额 10 → 2 时持仓 3 本身已超限，撤销较早 A 买单的全部剩余 7 份，保留
// 已成交 3 份，不自动卖出持仓；第一次的撤销记录与既有成交记录保持原值。
func TestSetMaxPositionBelowPositionCancelsAllRemainingBuys(t *testing.T) {
	e, bidA1, bidA2, bidB, sellA := lowerMaxSetup(t)

	// 先完成第一次下调（20 → 10），并留存当时全部记录供后续比对。
	if canceled, err := e.SetMaxPosition("A", 10); err != nil || len(canceled) != 1 || canceled[0] != bidA2 {
		t.Fatalf("第一次下调应只撤销订单2: canceled=%v err=%v", canceled, err)
	}
	recsAfterFirst := e.Records()

	// 第二次下调：持仓 3 > 新限额 2，撤销订单1 的全部剩余 7 份。
	canceled, err := e.SetMaxPosition("A", 2)
	if err != nil {
		t.Fatalf("下调 A 限额到 2 不应报错: %v", err)
	}
	if len(canceled) != 1 || canceled[0] != bidA1 {
		t.Fatalf("返回的撤销编号应只包含较早的 A 买单 %d，实际 %v", bidA1, canceled)
	}
	if max, ok := e.MaxPosition("A"); !ok || max != 2 {
		t.Fatalf("A 最大持仓量应为 2: max=%d ok=%v", max, ok)
	}

	// 资金：再释放订单1 剩余 7 份的占用 70；占用现金只剩 B 买单的 20，
	// 现金余额仍为 973（已成交部分不退款），可用 953。
	if e.Cash() != 973 || e.ReservedCash() != 20 || e.AvailableCash() != 953 {
		t.Fatalf("第二次撤单后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓与可卖：不自动卖出超限持仓，也不用待成交卖单抵减；A 持仓 3、可卖 1。
	if e.Position("A") != 3 || e.Sellable("A") != 1 || e.Position("B") != 0 {
		t.Fatalf("持仓超限也不得自动卖出或改动卖单占用: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：订单1 撤销剩余 7 份但保留总量 10、已成交 3；B 买单与 A 卖单
	// 继续有效。
	oA1, _ := e.Order(bidA1)
	if oA1.Status != StatusCanceled || oA1.Qty != 10 || oA1.Filled != 3 || oA1.Remaining() != 0 {
		t.Fatalf("订单1 应保留总量 10、已成交 3 并撤销剩余 7 份: %+v", oA1)
	}
	if !strings.Contains(oA1.Reason, "持仓 3") || !strings.Contains(oA1.Reason, "新限额 2") {
		t.Fatalf("订单1 撤销原因必须以持仓 3 超过新限额 2 解释: %q", oA1.Reason)
	}
	oB, _ := e.Order(bidB)
	if oB.Status != StatusPending || oB.Remaining() != 4 {
		t.Fatalf("B 买单必须保持有效、剩余 4: %+v", oB)
	}
	oSell, _ := e.Order(sellA)
	if oSell.Status != StatusPending || oSell.Qty != 2 || oSell.Remaining() != 2 || oSell.Reason != "" {
		t.Fatalf("A 卖单必须继续有效: %+v", oSell)
	}

	// 第二次下调只新增 1 条撤销记录：取消订单1 剩余 7 份（已成交 3 份保留），
	// 原因说明持仓已超新限额，携带当时的 A 报价与新限额 2。
	recsNow := e.Records()
	newRecs := recsNow[len(recsAfterFirst):]
	if len(newRecs) != 1 {
		t.Fatalf("第二次下调应只新增 1 条撤销记录，实际 %d 条: %+v", len(newRecs), newRecs)
	}
	cr := newRecs[0]
	if cr.Kind != RecordCanceled || cr.OrderID != bidA1 || cr.Symbol != "A" || cr.Side != Buy {
		t.Fatalf("新增记录应为订单1 的撤销记录: %+v", cr)
	}
	if cr.Limit != 10 || cr.Qty != 10 || cr.Filled != 3 || cr.Remaining != 7 {
		t.Fatalf("撤销记录必须保留总量 10、已成交 3，并说明取消了 7 份: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 1 || cr.QuoteMoment != 10 || cr.QuotePrice != 10 {
		t.Fatalf("撤销记录的报价快照应锚定 A 序号1、时刻10、价格10: %+v", cr)
	}
	if !cr.MaxPositionValid || cr.MaxPosition != 2 {
		t.Fatalf("撤销记录必须固化新限额 2: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "持仓 3") || !strings.Contains(cr.Reason, "新限额 2") {
		t.Fatalf("撤销原因必须说明持仓 3 已超过新限额 2: %q", cr.Reason)
	}

	// 第二次下调不得改写第一次的记录：此前全部记录逐条保持原值，
	// 第一次的撤销记录仍固化当时的新限额 10。
	if len(recsNow) != len(recsAfterFirst)+1 {
		t.Fatalf("第二次下调后记录总数应只增加 1: 前 %d 后 %d", len(recsAfterFirst), len(recsNow))
	}
	for i, prev := range recsAfterFirst {
		if !reflect.DeepEqual(recsNow[i], prev) {
			t.Fatalf("第 %d 条既有记录被第二次下调改写: 原 %+v 现 %+v", i, prev, recsNow[i])
		}
	}
	firstCancel := recsAfterFirst[len(recsAfterFirst)-1]
	if firstCancel.Kind != RecordCanceled || firstCancel.OrderID != bidA2 || firstCancel.MaxPosition != 10 {
		t.Fatalf("第一次的撤销记录必须保持订单2 与新限额 10: %+v", firstCancel)
	}

	// 既有成交记录保持原值：成交编号1 仍记录按价格 9 成交 3 份、订单累计 3、
	// 当时剩余 7，不被后续撤单改写。
	var fillRec *Record
	for i := range recsNow {
		if recsNow[i].Kind == RecordFilled && recsNow[i].TradeID == 1 {
			fillRec = &recsNow[i]
			break
		}
	}
	if fillRec == nil {
		t.Fatal("必须存在成交编号1 的成交记录")
	}
	if fillRec.OrderID != bidA1 || fillRec.TradePrice != 9 || fillRec.Qty != 3 ||
		fillRec.Filled != 3 || fillRec.Remaining != 7 {
		t.Fatalf("既有成交记录必须保持原值: %+v", *fillRec)
	}

	// 两次撤销记录按实际撤销顺序保存：先订单2（限额 10），后订单1（限额 2）。
	var cancelOrder []int64
	for _, r := range recsNow {
		if r.Kind == RecordCanceled {
			cancelOrder = append(cancelOrder, r.OrderID)
		}
	}
	if len(cancelOrder) != 2 || cancelOrder[0] != bidA2 || cancelOrder[1] != bidA1 {
		t.Fatalf("撤销记录应按实际撤销顺序保存（先 %d 后 %d），实际 %v", bidA2, bidA1, cancelOrder)
	}

	// 全部记录中不得出现 B 买单或 A 卖单的撤销。
	for _, r := range recsNow {
		if r.Kind == RecordCanceled && (r.OrderID == bidB || r.OrderID == sellA) {
			t.Fatalf("其他合约买单与卖单不得被限额下调撤销: %+v", r)
		}
	}
}
