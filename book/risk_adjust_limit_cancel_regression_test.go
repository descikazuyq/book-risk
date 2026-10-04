package book

import (
	"strings"
	"testing"
)

// 本文件为“交易日内下调日内亏损上限”补充自动化回归保障：新上限碰到当前亏损时，
// 有效买单必须立即按既有规则撤销，重点覆盖跨合约订单、部分成交买单与有效卖单
// 同时存在的情形。
//
// 固定场景（整数金额口径，全部以公开查询结果为准）：
//   - 初始现金 1000；A、B 最大持仓量均为 100。
//   - 已生效报价：A 序号1、时刻10、价格10；B 序号1、时刻20、价格20。
//   - 接受 A 买单 10@10（订单1），其中 4 份按价格 9 成交：现金 964、A 持仓 4、
//     买单占用现金 100（订单1 剩余 6 份占 60，B 单占 40）。
//   - 接受 B 买单 2@20（订单2），接受 A 卖单 2@1（订单3，占用 A 可卖 2 份）。
//   - 以亏损上限 100 开启交易日 1：现金 964 + A 持仓 4×报价10=40，基准净值 1004。
//   - A 序号2、时刻30、价格5 生效：净值 964+20=984，亏损 20，尚未触发。
//
// 先把上限调到 21：亏损 20 < 21，只能更新当前上限，订单与占用保持原值，不产生
// 触线或撤销记录；再调到 20：亏损 20 >= 20，立即进入限制增险状态——先记录本交易
// 日首次触线（来源为调限额，基准 1004、净值 984、亏损 20、新上限 20；估值报价只
// 有持仓的 A：序号2、时刻30、价格5，不能把仅有待成交买单的 B 算进去），再按订单
// 编号从大到小撤销 B 买单 2 份与 A 买单剩余 6 份。A 已成交的 4 份与入账现金保留，
// 买单现金占用归零、现金仍为 964；A 卖单继续有效，持仓 4、可卖数量 2，此后合规的
// 新买单也因保护被拒绝。负上限调整必须报错且不改变任何状态、不追加记录。

// adjustLimitSetup 构造调上限前的共用前置状态，返回引擎与三个订单编号
// （bidA：A 部分成交买单；bidB：B 待成交买单；sellA：A 有效卖单）。
func adjustLimitSetup(t *testing.T) (e *Engine, bidA, bidB, sellA int64) {
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
	bidB = mustBuy(t, e, "B", 2, 20)
	sellA = mustSell(t, e, "A", 2, 1)

	// 题目给定的前置资金与持仓口径，逐笔固化。
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("前置持仓错误: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 1004 ||
		st.Equity != 1004 || st.Loss != 0 || st.LossLimit != 100 || st.Restricted {
		t.Fatalf("开日后基准净值应为 1004 且未限制: %+v", st)
	}

	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 30, Price: 5}, 1)
	if st := e.RiskStatus(); st.Equity != 984 || st.Loss != 20 || st.Restricted {
		t.Fatalf("A 报价降到 5 后净值应为 984、亏损 20，保护尚未触发: %+v", st)
	}
	return e, bidA, bidB, sellA
}

// TestRiskLowerLimitToCurrentLossCancelsCrossContractBuys 覆盖主场景：
// 21 不触线只改上限；20 触线后先记录首次触线，再按编号降序跨合约撤销买单，
// 部分成交的已成交部分与有效卖单保留。
func TestRiskLowerLimitToCurrentLossCancelsCrossContractBuys(t *testing.T) {
	e, bidA, bidB, sellA := adjustLimitSetup(t)

	// 调到 21：亏损 20 未达新上限，只更新上限；订单、占用保持原值。
	recsBefore21 := len(e.Records())
	if err := e.SetLossLimit(21); err != nil {
		t.Fatalf("上调式调整到 21 不应报错: %v", err)
	}
	if st := e.RiskStatus(); st.LossLimit != 21 || st.Restricted || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("调到 21 后只能更新当前上限: %+v", st)
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("调到 21 不得改变资金占用: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if o, _ := e.Order(bidA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 买单应保持部分成交、剩余 6: %+v", o)
	}
	if o, _ := e.Order(bidB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单应保持待成交、剩余 2: %+v", o)
	}
	if len(e.Records()) != recsBefore21 {
		t.Fatal("调到 21 不得产生触线、撤销或任何记录")
	}

	// 调到 20：亏损恰好等于新上限，立即触线。
	if err := e.SetLossLimit(20); err != nil {
		t.Fatalf("调到 20 不应报错: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1004 || st.LossLimit != 20 {
		t.Fatalf("触线后日号、基准与新上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("亏损 20 恰好达到新上限 20 时必须立即限制增险: %+v", st)
	}

	// 资金：买单现金占用归零，已成交入账现金保留，现金余额仍为 964。
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 {
		t.Fatalf("买单占用应全部释放、现金余额保持 964: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓与可卖数量：A 已成交 4 份保留，卖单占用不变；B 始终无持仓。
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("触线撤单不得改动持仓与卖单占用: A.pos=%d A.sellable=%d B.pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：B 单 2 份全部撤销；A 单保留总量 10、已成交 4，剩余 6 份被撤销；
	// A 卖单继续有效。
	oB, _ := e.Order(bidB)
	if oB.Status != StatusCanceled || oB.Qty != 2 || oB.Filled != 0 || oB.Remaining() != 0 {
		t.Fatalf("B 买单应整单撤销且有效剩余量为 0: %+v", oB)
	}
	oA, _ := e.Order(bidA)
	if oA.Status != StatusCanceled || oA.Qty != 10 || oA.Filled != 4 || oA.Remaining() != 0 {
		t.Fatalf("A 买单应保留总量 10、已成交 4 并撤销剩余 6 份: %+v", oA)
	}
	if !strings.Contains(oA.Reason, "亏损 20") || !strings.Contains(oA.Reason, "上限 20") {
		t.Fatalf("A 买单撤销原因必须以亏损 20、新上限 20 解释: %q", oA.Reason)
	}
	oSell, _ := e.Order(sellA)
	if oSell.Status != StatusPending || oSell.Side != Sell || oSell.Qty != 2 ||
		oSell.Limit != 1 || oSell.Remaining() != 2 || oSell.Reason != "" {
		t.Fatalf("有效卖单必须原样保留、不被风险保护撤销: %+v", oSell)
	}

	// 新增记录恰好 3 条：首次触线 → 撤 B 单 → 撤 A 单（编号从大到小）。
	newRecs := e.Records()[recsBefore21:]
	if len(newRecs) != 3 {
		t.Fatalf("调到 20 后应只新增 3 条记录（触线、撤B、撤A），实际 %d 条: %+v", len(newRecs), newRecs)
	}

	// 1) 触线记录：明确由调整上限引发；基准、净值、亏损、新上限齐备。
	tr := newRecs[0]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第一条应为当日首次触线记录: %+v", tr)
	}
	if tr.RiskTrigger != RiskTriggerAdjustLimit || tr.RiskRefSeq != 0 || tr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是调限额且不关联报价或成交: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 1004 || tr.RiskEquity != 984 ||
		tr.RiskLoss != 20 || tr.RiskLimit != 20 || !tr.RiskRestrict {
		t.Fatalf("触线记录必须固化基准1004、净值984、亏损20、新上限20: %+v", tr)
	}
	// 估值报价只包含有持仓的 A；只有待成交买单的 B 不得计入。
	if len(tr.RiskQuoteRefs) != 1 {
		t.Fatalf("估值报价应只包含有持仓的 A，实际 %+v", tr.RiskQuoteRefs)
	}
	if tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 30, Price: 5}) {
		t.Fatalf("触线估值报价必须定位 A 序号2、时刻30、价格5: %+v", tr.RiskQuoteRefs)
	}
	if !strings.Contains(tr.Reason, "调限额") || !strings.Contains(tr.Reason, "亏损 20") ||
		!strings.Contains(tr.Reason, "上限 20") {
		t.Fatalf("触线原因必须写明由调限额引发、亏损 20 与上限 20: %q", tr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 2) 撤销 B 单：取消 2 份，保留触线时亏损 20、新上限 20；报价快照为 B 的 seq1。
	crB := newRecs[1]
	if crB.Kind != RecordCanceled || crB.OrderID != bidB || crB.Symbol != "B" || crB.Side != Buy {
		t.Fatalf("第二条应为 B 买单撤销记录: %+v", crB)
	}
	if crB.Qty != 2 || crB.Filled != 0 || crB.Remaining != 2 {
		t.Fatalf("B 撤销记录必须说明取消了 2 份: %+v", crB)
	}
	if !crB.QuoteValid || crB.QuoteSeq != 1 || crB.QuoteMoment != 20 || crB.QuotePrice != 20 {
		t.Fatalf("B 撤销记录的报价快照应锚定 B 序号1、时刻20、价格20: %+v", crB)
	}
	if crB.RiskDay != 1 || crB.RiskBaseline != 1004 || crB.RiskEquity != 984 ||
		crB.RiskLoss != 20 || crB.RiskLimit != 20 || !crB.RiskRestrict {
		t.Fatalf("B 撤销记录必须保留触线时亏损 20 与新上限 20: %+v", crB)
	}
	if !strings.Contains(crB.Reason, "亏损 20") || !strings.Contains(crB.Reason, "上限 20") {
		t.Fatalf("B 撤销原因必须以亏损 20、上限 20 解释: %q", crB.Reason)
	}

	// 3) 撤销 A 单：取消剩余 6 份（已成交 4 份保留），同样固化亏损 20、新上限 20；
	// 报价快照为 A 的 seq2@时刻30 价格5。
	crA := newRecs[2]
	if crA.Kind != RecordCanceled || crA.OrderID != bidA || crA.Symbol != "A" || crA.Side != Buy {
		t.Fatalf("第三条应为 A 买单撤销记录: %+v", crA)
	}
	if crA.Qty != 10 || crA.Filled != 4 || crA.Remaining != 6 {
		t.Fatalf("A 撤销记录必须保留总量 10、已成交 4，并说明取消了 6 份: %+v", crA)
	}
	if !crA.QuoteValid || crA.QuoteSeq != 2 || crA.QuoteMoment != 30 || crA.QuotePrice != 5 {
		t.Fatalf("A 撤销记录的报价快照应锚定 A 序号2、时刻30、价格5: %+v", crA)
	}
	if crA.RiskDay != 1 || crA.RiskBaseline != 1004 || crA.RiskEquity != 984 ||
		crA.RiskLoss != 20 || crA.RiskLimit != 20 || !crA.RiskRestrict {
		t.Fatalf("A 撤销记录必须保留触线时亏损 20 与新上限 20: %+v", crA)
	}
	if !strings.Contains(crA.Reason, "亏损 20") || !strings.Contains(crA.Reason, "上限 20") {
		t.Fatalf("A 撤销原因必须以亏损 20、上限 20 解释: %q", crA.Reason)
	}

	// 全部记录中不得出现卖单撤销。
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && r.OrderID == sellA {
			t.Fatalf("有效卖单 %d 不得被风险保护撤销: %+v", sellA, r)
		}
	}

	// 限制持续：即使完全合规（资金、持仓限额、行情连续均满足），新买单也被拒绝，
	// 拒绝记录固化当日亏损与上限。
	recsBeforeBuy := len(e.Records())
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制后合规新买单也必须拒绝")
	}
	rj := e.Records()[recsBeforeBuy]
	if rj.Kind != RecordRejected || rj.Symbol != "A" || rj.Side != Buy {
		t.Fatalf("新买单应留下拒绝记录: %+v", rj)
	}
	if rj.RiskDay != 1 || rj.RiskLoss != 20 || rj.RiskLimit != 20 || !rj.RiskRestrict {
		t.Fatalf("拒绝记录必须固化触线后风险快照: %+v", rj)
	}

	// 已被撤销的买单不能再撤销或借修改恢复。
	if err := e.Cancel(bidB); err == nil {
		t.Fatal("已被风险保护撤销的 B 买单不能再次撤销")
	}
	if err := e.Modify(bidA, 12, 10); err == nil {
		t.Fatal("已被风险保护撤销的 A 买单不能借修改恢复")
	}

	// 调高上限不能解除整日限制，也不产生第二条触线记录。
	if err := e.SetLossLimit(100); err != nil {
		t.Fatal(err)
	}
	if !e.RiskStatus().Restricted {
		t.Fatal("触线后调高上限不得解除限制")
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("限制保留期间不得重复产生触线记录")
	}
}

// TestRiskLowerLimitJustAboveLossChangesNothing 单独钉住边界：
// 新上限严格大于当前亏损（21 > 20）时，即使是“下调”，也不得触线或撤单。
func TestRiskLowerLimitJustAboveLossChangesNothing(t *testing.T) {
	e, bidA, bidB, sellA := adjustLimitSetup(t)

	recsBefore := len(e.Records())
	if err := e.SetLossLimit(21); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); st.Restricted || st.LossLimit != 21 {
		t.Fatalf("亏损 20 低于新上限 21 时不应触线: %+v", st)
	}
	// 两笔买单与卖单全部保持原状态与占用。
	if o, _ := e.Order(bidA); o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 买单应保持有效、剩余 6: %+v", o)
	}
	if o, _ := e.Order(bidB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单应保持有效、剩余 2: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单应保持有效: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("未触线时现金占用必须保持原值: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if len(e.Records()) != recsBefore || countRiskRecords(e) != 0 {
		t.Fatal("新上限高于当前亏损时不得追加触线、撤销或任何记录")
	}

	// 未触线下新买单只要合规仍可接受（占用现金 1，可用 863）。
	newBid := mustBuy(t, e, "B", 1, 1)
	if e.ReservedCash() != 101 {
		t.Fatalf("未触线时新买单应正常占用现金: reserved=%d", e.ReservedCash())
	}
	// 触线撤单仍按编号从大到小：新买单（编号更大）先撤，再撤 B、A。
	if err := e.SetLossLimit(20); err != nil {
		t.Fatal(err)
	}
	var canceled []int64
	sawTrigger := false
	for _, r := range e.Records()[recsBefore:] {
		if r.Kind == RecordRiskTriggered {
			sawTrigger = true
			continue
		}
		if sawTrigger && r.Kind == RecordCanceled {
			canceled = append(canceled, r.OrderID)
		}
	}
	want := []int64{newBid, bidB, bidA}
	if len(canceled) != len(want) {
		t.Fatalf("触线应撤销 3 笔买单，实际 %v", canceled)
	}
	for i := range want {
		if canceled[i] != want[i] {
			t.Fatalf("撤单必须按订单编号从大到小 %v，实际 %v", want, canceled)
		}
	}
}

// TestRiskNegativeLimitAdjustmentRejectedAndKeepsState 覆盖负上限调整：
// 报错且不改变风险状态、订单或占用，不追加触线、撤销记录。
func TestRiskNegativeLimitAdjustmentRejectedAndKeepsState(t *testing.T) {
	e, bidA, bidB, sellA := adjustLimitSetup(t)

	// 触线前：负上限报错，上限保持 100，无任何记录追加。
	recsBefore := len(e.Records())
	if err := e.SetLossLimit(-1); err == nil {
		t.Fatal("负亏损上限必须报错")
	}
	st := e.RiskStatus()
	if !st.Open || st.LossLimit != 100 || st.Restricted || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("负上限失败后风险状态必须保持: %+v", st)
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("负上限失败后资金占用必须保持: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if o, _ := e.Order(bidA); o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("负上限失败后 A 买单必须保持有效: %+v", o)
	}
	if o, _ := e.Order(bidB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("负上限失败后 B 买单必须保持有效: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending {
		t.Fatalf("负上限失败后 A 卖单必须保持有效: %+v", o)
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("负上限失败不得追加任何记录（含拒绝记录）")
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("负上限失败不得追加触线记录")
	}

	// 触线后再调负上限：限制、新上限 20、撤单结果与记录数同样保持不变。
	if err := e.SetLossLimit(20); err != nil {
		t.Fatal(err)
	}
	triggeredRecs := len(e.Records())
	if !e.RiskStatus().Restricted {
		t.Fatal("前置必须已触线")
	}
	if err := e.SetLossLimit(-5); err == nil {
		t.Fatal("触线后负上限同样必须报错")
	}
	st2 := e.RiskStatus()
	if !st2.Restricted || st2.LossLimit != 20 || st2.Loss != 20 {
		t.Fatalf("触线后负上限失败不得改变风险状态: %+v", st2)
	}
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("触线后负上限失败不得改变资金与持仓: cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	if len(e.Records()) != triggeredRecs {
		t.Fatal("触线后负上限失败不得追加任何记录")
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("负上限失败不得追加触线记录")
	}
}
