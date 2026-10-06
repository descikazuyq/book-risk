package book

import (
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方“首次开启交易日”并把亏损上限设为零时，
// 即使行情没有下跌、当前亏损为零，也必须在开日当次调用内立即限制增险——拒绝此后
// 的一切新买单，并处理（撤销）开日前已经接受的有效买单。重点覆盖账户同时存在
// 部分成交买单、跨合约待成交买单与有效卖单占用的情形，保留现有功能的公开行为。
//
// 固定场景（整数金额口径，全部以公开查询结果为准）：
//   - 初始现金 1000；A、B 最大持仓量均为 100。
//   - 最新已生效报价：A 序号1、时刻10、价格10；B 序号1、时刻20、价格20。
//   - 接受 A 买单 10@10（订单1），其中 4 份按价格 9 成交：现金 964、A 持仓 4，
//     订单1 剩余 6 份占用 60 现金。
//   - 接受 B 买单 3@20（订单2）：占用 3×20=60 现金（两张买单各占 60，合计 120）。
//   - 接受 A 卖单 2@9（订单3，尚未成交）：占用 A 可卖 2 份，可卖数量 4−2=2。
//   - 账户尚未开启交易日，账户总持仓金额上限也未启用。
//
// 以日号 1、亏损上限 0 开日应成功：基准净值与当前净值均为 964+4×10=1004、亏损 0，
// 但 0 >= 0 立即触线——先增加一条来源为开日的触线记录（日号 1、净值 1004、亏损 0、
// 上限 0、已限制；估值报价只包含有持仓的 A：序号1、时刻10、价格10，不能把只有挂单
// 的 B 算进持仓估值），再按买单编号从大到小依次撤销订单2（B 取消 3 份）与订单1
// （A 取消剩余 6 份，已成交 4 份保留）；编号更大的 A 卖单继续有效。撤单后现金仍为
// 964、A 持仓仍为 4、A 买单累计成交仍为 4，买单现金占用降为 0、可用现金变为 964，
// A 可卖数量仍为 2，已经入账的成交不重新计价。开日后其他条件均满足的新买单仍被
// 风险保护拒绝。
//
// 边界：同样的开日前状态使用正数亏损上限 1 时，零亏损不触线，不新增触线或撤销记录，
// 订单与占用保持原值；负数亏损上限必须报错，账户仍未开日，也不撤单、不释放占用、
// 不新增记录。

// startDayZeroLimitSetup 构造成零上限开日前的共用前置状态（尚未开日、未设金额上限），
// 返回引擎与三个订单编号（bidA：A 部分成交买单；bidB：B 待成交买单；sellA：A 有效卖单）。
func startDayZeroLimitSetup(t *testing.T) (e *Engine, bidA, bidB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	bidA = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bidA, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	bidB = mustBuy(t, e, "B", 3, 20)
	sellA = mustSell(t, e, "A", 2, 9)

	// 题目给定的前置资金、持仓与占用口径，逐笔固化：
	// 现金 964；A 买单剩余 6 份占 60、B 买单 3 份占 60，合计占用 120，可用 844。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("前置持仓错误: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}
	oA, _ := e.Order(bidA)
	if oA.Status != StatusPartial || oA.Qty != 10 || oA.Filled != 4 || oA.Remaining() != 6 {
		t.Fatalf("A 买单应为部分成交、剩余 6 份: %+v", oA)
	}
	oB, _ := e.Order(bidB)
	if oB.Status != StatusPending || oB.Qty != 3 || oB.Filled != 0 || oB.Remaining() != 3 {
		t.Fatalf("B 买单应为待成交、剩余 3 份: %+v", oB)
	}
	oS, _ := e.Order(sellA)
	if oS.Status != StatusPending || oS.Qty != 2 || oS.Limit != 9 || oS.Remaining() != 2 {
		t.Fatalf("A 卖单应为待成交、占用 2 份: %+v", oS)
	}
	if e.RiskStatus().Open {
		t.Fatal("前置状态必须尚未开启交易日")
	}
	if e.PositionAmountStatus().Enabled {
		t.Fatal("前置状态必须尚未启用账户总持仓金额上限")
	}
	return e, bidA, bidB, sellA
}

// TestRiskStartDayZeroLimitImmediatelyRestrictsAndCancelsBuys 覆盖主路径：零亏损上限
// 在零亏损时也立即触线，先记一条开日触线记录，再按编号降序撤销两张买单的剩余量，
// 部分成交的已成交部分与编号更大的有效卖单原样保留。
func TestRiskStartDayZeroLimitImmediatelyRestrictsAndCancelsBuys(t *testing.T) {
	e, bidA, bidB, sellA := startDayZeroLimitSetup(t)

	recsBefore := len(e.Records())

	// 零上限开日必须成功：即使行情未下跌、亏损为 0，也在本次调用内立即限制增险。
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatalf("零亏损上限开日必须成功: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 {
		t.Fatalf("交易日应已开启: %+v", st)
	}
	if st.Baseline != 1004 || st.Equity != 1004 || st.Loss != 0 || st.LossLimit != 0 {
		t.Fatalf("基准净值与当前净值都应为 1004、亏损 0、上限 0: %+v", st)
	}
	if !st.Restricted {
		t.Fatalf("零上限开日即使零亏损也必须立即进入已限制状态: %+v", st)
	}

	// 资金与持仓：买单占用全部释放（120→0）、可用现金变为 964；现金余额 964、
	// A 持仓 4 与卖单占用的 2 份可卖数量保留；B 始终无持仓。
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 {
		t.Fatalf("撤单只释放买单占用：现金仍为 964、占用归零、可用 964: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("触线撤单不得改动持仓与卖单占用: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：先撤编号更大的 B 单（3 份全部取消），再撤 A 单剩余 6 份
	// （总量 10、累计成交 4 保留）；编号更大的 A 卖单继续有效。
	oB, _ := e.Order(bidB)
	if oB.Status != StatusCanceled || oB.Qty != 3 || oB.Filled != 0 || oB.Remaining() != 0 {
		t.Fatalf("B 买单应整单撤销、有效剩余量为 0: %+v", oB)
	}
	if !strings.Contains(oB.Reason, "亏损 0") || !strings.Contains(oB.Reason, "上限 0") {
		t.Fatalf("B 买单撤销原因必须以亏损 0、上限 0 解释: %q", oB.Reason)
	}
	oA, _ := e.Order(bidA)
	if oA.Status != StatusCanceled || oA.Qty != 10 || oA.Filled != 4 || oA.Remaining() != 0 {
		t.Fatalf("A 买单应保留总量 10、累计成交 4 并撤销剩余 6 份: %+v", oA)
	}
	if !strings.Contains(oA.Reason, "亏损 0") || !strings.Contains(oA.Reason, "上限 0") {
		t.Fatalf("A 买单撤销原因必须以亏损 0、上限 0 解释: %q", oA.Reason)
	}
	oS, _ := e.Order(sellA)
	if oS.Status != StatusPending || oS.Side != Sell || oS.Qty != 2 ||
		oS.Limit != 9 || oS.Remaining() != 2 || oS.Reason != "" {
		t.Fatalf("编号更大的 A 卖单必须原样保留、不被风险保护撤销: %+v", oS)
	}

	// 本次开日先增加一条触线记录，再依次增加两条买单撤销记录，恰好 3 条。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("零上限开日应只新增 3 条记录（触线、撤B、撤A），实际 %d 条: %+v",
			len(newRecs), newRecs)
	}

	// 1) 触线记录：来源为开日，日号 1、净值 1004、亏损 0、上限 0、已限制。
	tr := newRecs[0]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", tr)
	}
	if tr.RiskTrigger != RiskTriggerStartDay || tr.RiskRefSeq != 0 || tr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是开日且不关联报价序号或成交编号: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 1004 || tr.RiskEquity != 1004 ||
		tr.RiskLoss != 0 || tr.RiskLimit != 0 || !tr.RiskRestrict {
		t.Fatalf("触线记录必须固化日号1、净值1004、亏损0、上限0与已限制状态: %+v", tr)
	}
	if !strings.Contains(tr.Reason, "开日") || !strings.Contains(tr.Reason, "亏损 0") ||
		!strings.Contains(tr.Reason, "上限 0") {
		t.Fatalf("触线原因必须写明由开日引发、亏损 0 与上限 0: %q", tr.Reason)
	}
	// 估值报价只包含有持仓的 A（序号1、时刻10、价格10）；只有挂单的 B 不得计入。
	if len(tr.RiskQuoteRefs) != 1 {
		t.Fatalf("估值报价应只包含有持仓的 A，实际 %+v", tr.RiskQuoteRefs)
	}
	if tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 10}) {
		t.Fatalf("触线估值报价必须定位 A 序号1、时刻10、价格10: %+v", tr.RiskQuoteRefs)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 撤单顺序：先 B 单（编号 2）后 A 单（编号 1）。
	if newRecs[1].OrderID != bidB || newRecs[2].OrderID != bidA {
		t.Fatalf("撤单必须按买单编号从大到小：先 %d 后 %d，实际先 %d 后 %d",
			bidB, bidA, newRecs[1].OrderID, newRecs[2].OrderID)
	}

	// 2) B 单撤销记录：取消 3 份，保存开日时的风险快照，报价快照为 B 的 seq1。
	crB := newRecs[1]
	if crB.Kind != RecordCanceled || crB.Symbol != "B" || crB.Side != Buy {
		t.Fatalf("第二条应为 B 买单撤销记录: %+v", crB)
	}
	if crB.Qty != 3 || crB.Filled != 0 || crB.Remaining != 3 {
		t.Fatalf("B 撤销记录必须说明取消了 3 份: %+v", crB)
	}
	if !crB.QuoteValid || crB.QuoteSeq != 1 || crB.QuoteMoment != 20 || crB.QuotePrice != 20 {
		t.Fatalf("B 撤销记录报价快照应锚定 B 序号1、时刻20、价格20: %+v", crB)
	}
	if crB.RiskDay != 1 || crB.RiskBaseline != 1004 || crB.RiskEquity != 1004 ||
		crB.RiskLoss != 0 || crB.RiskLimit != 0 || !crB.RiskRestrict {
		t.Fatalf("B 撤销记录必须保存开日时的风险快照: %+v", crB)
	}

	// 3) A 单撤销记录：取消剩余 6 份、累计成交 4 保留，同样保存开日风险快照，
	// 报价快照为 A 的 seq1@时刻10 价格10。
	crA := newRecs[2]
	if crA.Kind != RecordCanceled || crA.Symbol != "A" || crA.Side != Buy {
		t.Fatalf("第三条应为 A 买单撤销记录: %+v", crA)
	}
	if crA.Qty != 10 || crA.Filled != 4 || crA.Remaining != 6 {
		t.Fatalf("A 撤销记录必须保留总量 10、已成交 4，并说明取消了 6 份: %+v", crA)
	}
	if !crA.QuoteValid || crA.QuoteSeq != 1 || crA.QuoteMoment != 10 || crA.QuotePrice != 10 {
		t.Fatalf("A 撤销记录报价快照应锚定 A 序号1、时刻10、价格10: %+v", crA)
	}
	if crA.RiskDay != 1 || crA.RiskBaseline != 1004 || crA.RiskEquity != 1004 ||
		crA.RiskLoss != 0 || crA.RiskLimit != 0 || !crA.RiskRestrict {
		t.Fatalf("A 撤销记录必须保存开日时的风险快照: %+v", crA)
	}

	// 全部记录中不得出现卖单撤销。
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && r.OrderID == sellA {
			t.Fatalf("有效卖单 %d 不得被风险保护撤销: %+v", sellA, r)
		}
	}

	// 开日前已入账的成交记录保持原值：成交价 9、本量 4、累计 4，报价快照为
	// 成交时的 A seq1@(10,10)，且不得事后补写开日风险快照。
	var fillRec Record
	for _, r := range e.Records() {
		if r.Kind == RecordFilled && r.TradeID == 1 {
			fillRec = r
		}
	}
	if fillRec.TradeID != 1 {
		t.Fatal("必须能找到成交编号 1 的原成交记录")
	}
	if fillRec.OrderID != bidA || fillRec.TradePrice != 9 || fillRec.Qty != 4 || fillRec.Filled != 4 {
		t.Fatalf("原成交记录内容必须保持不变: %+v", fillRec)
	}
	if !fillRec.QuoteValid || fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("原成交记录报价快照必须保留成交时的 A seq1@(10,10): %+v", fillRec)
	}
	if fillRec.RiskDay != 0 || fillRec.RiskRestrict {
		t.Fatalf("开日前的成交记录不得被事后补写风险快照: %+v", fillRec)
	}

	// 限制持续到下一交易日：开日后其他下单条件均满足的新买单仍因风险保护被拒绝，
	// 拒绝记录固化开日风险快照，且不产生第二条触线记录。
	recsBeforeBuy := len(e.Records())
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("零上限开日触线后，合规新买单也必须拒绝")
	} else if !strings.Contains(err.Error(), "亏损保护已触发") {
		t.Fatalf("新买单必须因亏损保护被拒绝，实际: %v", err)
	}
	rj := e.Records()[recsBeforeBuy]
	if rj.Kind != RecordRejected || rj.Symbol != "A" || rj.Side != Buy {
		t.Fatalf("新买单应留下拒绝记录: %+v", rj)
	}
	if rj.RiskDay != 1 || rj.RiskBaseline != 1004 || rj.RiskEquity != 1004 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 0 || !rj.RiskRestrict {
		t.Fatalf("拒绝记录必须固化开日时的风险快照: %+v", rj)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("拒绝新买单不得再产生触线记录")
	}
	// 被拒买单不消耗编号、不改变资金占用。
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 {
		t.Fatalf("拒绝新买单不得改变资金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}

	// 已撤销买单不能再次撤销，也不能借修改恢复。
	if err := e.Cancel(bidB); err == nil {
		t.Fatal("已被风险保护撤销的 B 买单不能再次撤销")
	}
	if err := e.Modify(bidA, 8, 10); err == nil {
		t.Fatal("已被风险保护撤销的 A 买单不能借修改恢复")
	}
}

// TestRiskStartDayPositiveLimitWithZeroLossChangesNothing 钉住对照边界：同样的开日前
// 状态使用正数亏损上限 1 时，零亏损不触线——不新增触线或撤销记录，两张买单与卖单、
// 现金占用 120 全部保持原值，合规新买单仍可接受。
func TestRiskStartDayPositiveLimitWithZeroLossChangesNothing(t *testing.T) {
	e, bidA, bidB, sellA := startDayZeroLimitSetup(t)

	recsBefore := len(e.Records())
	if err := e.StartTradingDay(1, 1); err != nil {
		t.Fatalf("正数亏损上限开日必须成功: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1004 || st.Equity != 1004 ||
		st.Loss != 0 || st.LossLimit != 1 || st.Restricted {
		t.Fatalf("零亏损低于正数上限 1 时不得触线: %+v", st)
	}

	// 两张买单与卖单全部保持原状态；买单现金占用 120 不释放。
	if o, _ := e.Order(bidA); o.Status != StatusPartial || o.Qty != 10 || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 买单应保持部分成交、剩余 6 份: %+v", o)
	}
	if o, _ := e.Order(bidB); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("B 买单应保持待成交、剩余 3 份: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单应保持有效、占用 2 份: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("未触线时资金、持仓与占用必须保持原值: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("零亏损正数上限开日不得新增任何记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("零亏损低于正数上限时不得产生触线记录")
	}

	// 未触线：其他条件满足的新买单正常接受并占用现金（1×1=1，占用 120→121）。
	newBid := mustBuy(t, e, "B", 1, 1)
	if o, _ := e.Order(newBid); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("未触线时新买单应正常接受: %+v", o)
	}
	if e.ReservedCash() != 121 || e.AvailableCash() != 843 {
		t.Fatalf("新买单应占用 1 现金: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
}

// TestRiskStartDayNegativeLimitRejectedKeepsState 覆盖负亏损上限开日：必须报错，
// 账户仍未开日，不撤单、不释放占用，也不新增任何记录。
func TestRiskStartDayNegativeLimitRejectedKeepsState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
	}{
		{"负上限1", -1},
		{"负上限100", -100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, bidA, bidB, sellA := startDayZeroLimitSetup(t)

			recsBefore := len(e.Records())
			if err := e.StartTradingDay(1, tc.limit); err == nil {
				t.Fatalf("负亏损上限 %d 开日必须报错", tc.limit)
			} else if !strings.Contains(err.Error(), "亏损上限不能为负") {
				t.Fatalf("错误必须说明亏损上限不能为负，实际: %v", err)
			}

			// 账户仍未开日。
			if st := e.RiskStatus(); st.Open || st.Restricted {
				t.Fatalf("负上限开日失败后必须仍处于未开日状态: %+v", st)
			}

			// 不撤单：两张买单与卖单保持原状态。
			if o, _ := e.Order(bidA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
				t.Fatalf("负上限失败后 A 买单必须保持有效、剩余 6 份: %+v", o)
			}
			if o, _ := e.Order(bidB); o.Status != StatusPending || o.Remaining() != 3 {
				t.Fatalf("负上限失败后 B 买单必须保持有效、剩余 3 份: %+v", o)
			}
			if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
				t.Fatalf("负上限失败后 A 卖单必须保持有效: %+v", o)
			}
			// 不释放占用、不改持仓；不新增任何记录（含拒绝记录与触线记录）。
			if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
				e.Position("A") != 4 || e.Sellable("A") != 2 {
				t.Fatalf("负上限失败后资金、持仓与占用必须保持原值: cash=%d reserved=%d available=%d pos=%d sellable=%d",
					e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
			}
			if len(e.Records()) != recsBefore {
				t.Fatalf("负上限失败不得新增任何记录，实际新增 %d 条", len(e.Records())-recsBefore)
			}
			if countRiskRecords(e) != 0 {
				t.Fatal("负上限失败不得产生触线记录")
			}

			// 修正为零上限后开日行为仍与主路径一致：立即触线并按编号降序撤单。
			if err := e.StartTradingDay(1, 0); err != nil {
				t.Fatalf("非法尝试后零上限开日仍应成功: %v", err)
			}
			if !e.RiskStatus().Restricted || e.RiskStatus().Equity != 1004 {
				t.Fatalf("非法尝试后零上限开日仍应在净值 1004、亏损 0 时立即限制: %+v", e.RiskStatus())
			}
			if o, _ := e.Order(bidB); o.Status != StatusCanceled || o.Remaining() != 0 {
				t.Fatalf("B 买单必须被保护撤销: %+v", o)
			}
			if o, _ := e.Order(bidA); o.Status != StatusCanceled || o.Filled != 4 || o.Remaining() != 0 {
				t.Fatalf("A 买单剩余 6 份必须被保护撤销、已成交 4 份保留: %+v", o)
			}
			if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
				t.Fatalf("A 卖单必须保持有效: %+v", o)
			}
			if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 ||
				e.Position("A") != 4 || e.Sellable("A") != 2 {
				t.Fatalf("保护撤单后资金口径应与主路径一致: cash=%d reserved=%d available=%d pos=%d sellable=%d",
					e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
			}
			if countRiskRecords(e) != 1 {
				t.Fatal("非法尝试不得占用触线记录，合法开日只产生一条触线记录")
			}
		})
	}
}
