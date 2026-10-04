package book

import (
	"strings"
	"testing"
)

// 本文件为“交易日内下调日内亏损上限”补充自动化回归保障：新上限碰到当前亏损时，
// 有效买单必须立即按现有规则（订单编号从大到小、跨全部合约、只撤未成交部分）撤销，
// 重点覆盖跨合约订单、部分成交买单与有效卖单同时存在的情况。
//
// 固定构造（全部经公开入口驱动，金额一律整数最小单位）：
//   - 初始现金 1000；A、B 合约最大持仓量均为 100。
//   - 已生效报价：A 序号 1、时刻 10、价格 10；B 序号 1、时刻 20、价格 20。
//   - 接受 A 买单（编号 1）：总量 10、限价 10，其中 4 份按价格 9 成交；
//   - 接受 B 买单（编号 2）：数量 2、限价 20（未成交）；
//   - 接受 A 卖单（编号 3）：数量 2、限价 1（未成交）。
//
// 此时现金 964（成交 4×9 已结算），A 持仓 4，买单占用现金 100
// （A 剩余 6×10=60，B 2×20=40），A 可卖数量 2（持仓 4 减去卖单占用 2）。
// 以亏损上限 100 开启交易日 1：基准净值 = 964 + 4×10 = 1004（B 无持仓不估值，
// 买单占用的现金是余额的一部分不重复扣减）。A 序号 2、时刻 30、价格 5 生效后，
// 净值 = 964 + 4×5 = 984、亏损 20，保护尚未触发。
//
// 上限先调到 21（亏损 20 仍低于上限）：只更新当前上限，订单与占用保持原值，
// 不产生触线或撤销记录；再调到 20（亏损恰好达到上限）：立即进入限制增险状态，
// 先记录本交易日首次触线（来源调限额），再按订单编号从大到小撤销 B 买单全部 2 份、
// A 买单剩余 6 份。A 已成交的 4 份及其入账现金保留，买单现金占用归零、现金仍 964；
// A 卖单继续有效，持仓 4、可卖数量 2 不变；此后合规新买单也因保护被拒绝。
//
// 触线记录必须保存基准 1004、净值 984、亏损 20、新上限 20，估值报价只包含有持仓的
// A（序号 2、时刻 30、价格 5），不能把仅有待成交买单的 B 报价算进去；两条撤销记录
// 也固化触线时的亏损 20 与新上限 20。负上限调整必须报错且不改变任何业务状态，
// 也不追加触线、撤销记录。

// riskAdjustLimitCrossSetup 构造上述前置状态并停在“A 序号 2 报价已生效、亏损 20、
// 上限仍为 100、尚未触线”的时刻。返回三个订单编号：buyA=1、buyB=2、sellA=3。
func riskAdjustLimitCrossSetup(t *testing.T) (e *Engine, buyA, buyB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	buyA = mustBuy(t, e, "A", 10, 10) // 编号 1，占用 100
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyA, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatalf("A 买单 4 份按价格 9 成交必须成功: %v", err)
	}
	buyB = mustBuy(t, e, "B", 2, 20)  // 编号 2，追加占用 40
	sellA = mustSell(t, e, "A", 2, 1) // 编号 3，占用可卖数量 2

	if buyA != 1 || buyB != 2 || sellA != 3 {
		t.Fatalf("订单编号应为 1/2/3，实际 %d/%d/%d", buyA, buyB, sellA)
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("开日前资金状态错误: cash=%d reserved=%d available=%d（期望 964/100/864）",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("开日前持仓状态错误: A pos=%d sellable=%d, B pos=%d（期望 4/2/0）",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatalf("开启交易日失败: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1004 || st.LossLimit != 100 || st.Restricted {
		t.Fatalf("开日后基准风险状态错误: %+v", st)
	}

	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 30, Price: 5}, 1) // 净值 984，亏损 20
	st = e.RiskStatus()
	if st.Baseline != 1004 || st.Equity != 984 || st.Loss != 20 || st.Restricted {
		t.Fatalf("A 序号 2 报价生效后应净值 984、亏损 20、尚未触线: %+v", st)
	}
	return e, buyA, buyB, sellA
}

// TestRiskLowerLimitToCurrentLossCancelsCrossSymbolBuysKeepsSell 是主场景：
// 21（不触线）→ 20（恰好碰到亏损 20）立即触线，跨合约按编号从大到小撤单，
// 部分成交与有效卖单保留。逐笔核对风险状态、订单、资金、持仓与全部事件记录。
func TestRiskLowerLimitToCurrentLossCancelsCrossSymbolBuysKeepsSell(t *testing.T) {
	e, buyA, buyB, sellA := riskAdjustLimitCrossSetup(t)

	// 第一步：上限调到 21，亏损 20 仍低于上限，只更新上限本身。
	recsBefore21 := len(e.Records())
	if err := e.SetLossLimit(21); err != nil {
		t.Fatalf("上调式不触线调整不得报错: %v", err)
	}
	st := e.RiskStatus()
	if st.Restricted || st.LossLimit != 21 || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("上限 21 不应触线，风险状态应为亏损 20/上限 21/未限制: %+v", st)
	}
	if len(e.Records()) != recsBefore21 {
		t.Fatalf("亏损未达新上限时不得产生任何记录，原有 %d 现有 %d", recsBefore21, len(e.Records()))
	}
	// 订单与占用保持原值：A 买单部分成交剩余 6，B 买单 2 份待成交，卖单有效。
	if o, _ := e.Order(buyA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("调到 21 后 A 买单应保持部分成交、剩余 6: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("调到 21 后 B 买单应保持待成交、剩余 2: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("调到 21 后 A 卖单应保持有效: %+v", o)
	}
	if e.ReservedCash() != 100 || e.Cash() != 964 || e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("调到 21 不得改变资金或持仓: cash=%d reserved=%d posA=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}

	// 第二步：上限调到 20，亏损恰好达到上限，立即限制增险并撤单。
	recsBefore20 := len(e.Records())
	if err := e.SetLossLimit(20); err != nil {
		t.Fatalf("下调到 20 应成功（触线由引擎内部处理）: %v", err)
	}
	st = e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1004 {
		t.Fatalf("触线后开日基准不得改变: %+v", st)
	}
	if !st.Restricted || st.Equity != 984 || st.Loss != 20 || st.LossLimit != 20 {
		t.Fatalf("亏损 20 恰好达到新上限 20 时必须立即触线: %+v", st)
	}

	// 已成交的 4 份及其入账现金保留；买单占用归零，现金余额不变。
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 {
		t.Fatalf("撤单后现金仍为 964、买单占用必须归零: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("A 持仓 4 与可卖数量 2 必须保留，B 仍空仓: A pos=%d sellable=%d B pos=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}

	// 订单查询：A 买单保留总量 10、已成交 4，已撤销且剩余量 0；B 买单 2 份全部撤销；
	// A 卖单继续有效。
	oA, _ := e.Order(buyA)
	if oA.Symbol != "A" || oA.Side != Buy || oA.Limit != 10 || oA.Qty != 10 ||
		oA.Filled != 4 || oA.Status != StatusCanceled || oA.Remaining() != 0 {
		t.Fatalf("A 买单应保留成交 4、撤销剩余 6: %+v", oA)
	}
	oB, _ := e.Order(buyB)
	if oB.Symbol != "B" || oB.Side != Buy || oB.Limit != 20 || oB.Qty != 2 ||
		oB.Filled != 0 || oB.Status != StatusCanceled || oB.Remaining() != 0 {
		t.Fatalf("B 买单应整体撤销 2 份: %+v", oB)
	}
	oS, _ := e.Order(sellA)
	if oS.Symbol != "A" || oS.Side != Sell || oS.Limit != 1 || oS.Qty != 2 ||
		oS.Filled != 0 || oS.Status != StatusPending || oS.Remaining() != 2 {
		t.Fatalf("A 卖单必须继续有效（数量 2、限价 1）: %+v", oS)
	}

	// 新增记录恰好 3 条，顺序：当日首次触线 → 撤销 B（编号 2）→ 撤销 A（编号 1）。
	newRecs := e.Records()[recsBefore20:]
	if len(newRecs) != 3 {
		t.Fatalf("调到 20 应只新增 3 条记录（触线、撤 B、撤 A），实际 %d 条", len(newRecs))
	}

	// 1) 触线记录：明确由调整上限引发；保存基准 1004、净值 984、亏损 20、新上限 20。
	tr := newRecs[0]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第一条应为当日首次触线记录: %+v", tr)
	}
	if tr.RiskTrigger != RiskTriggerAdjustLimit || tr.RiskRefSeq != 0 || tr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是调限额且不挂报价/成交编号: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 1004 || tr.RiskEquity != 984 ||
		tr.RiskLoss != 20 || tr.RiskLimit != 20 || !tr.RiskRestrict {
		t.Fatalf("触线记录必须固化基准 1004、净值 984、亏损 20、新上限 20: %+v", tr)
	}
	if !strings.Contains(tr.Reason, "调限额") || !strings.Contains(tr.Reason, "亏损 20") ||
		!strings.Contains(tr.Reason, "上限 20") {
		t.Fatalf("触线原因必须写明由调限额引发、亏损 20、上限 20: %q", tr.Reason)
	}
	// 估值报价只包含有持仓的 A（序号 2、时刻 30、价格 5）；仅有待成交买单的 B
	// 既无持仓也不得因其报价序号 1 被算入。
	if len(tr.RiskQuoteRefs) != 1 ||
		tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 30, Price: 5}) {
		t.Fatalf("触线估值报价只能定位 A 序号2/时刻30/价格5，不能带入 B: %+v", tr.RiskQuoteRefs)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 2) 先撤编号更大的 B 买单：取消 2 份，固化触线时亏损 20 与新上限 20；
	// 记录的报价快照取 B 自身已生效报价（序号 1、时刻 20、价格 20）。
	crB := newRecs[1]
	if crB.Kind != RecordCanceled || crB.OrderID != buyB || crB.Symbol != "B" || crB.Side != Buy {
		t.Fatalf("第二条应为 B 买单（编号 2）的撤销记录: %+v", crB)
	}
	if crB.Qty != 2 || crB.Filled != 0 || crB.Remaining != 2 {
		t.Fatalf("B 撤销记录应说明取消 2 份（总量 2、已成交 0）: %+v", crB)
	}
	if !crB.QuoteValid || crB.QuoteSeq != 1 || crB.QuoteMoment != 20 || crB.QuotePrice != 20 {
		t.Fatalf("B 撤销记录报价定位应为 B 序号1/时刻20/价格20: %+v", crB)
	}
	if crB.RiskDay != 1 || crB.RiskBaseline != 1004 || crB.RiskEquity != 984 ||
		crB.RiskLoss != 20 || crB.RiskLimit != 20 || !crB.RiskRestrict {
		t.Fatalf("B 撤销记录必须固化触线时亏损 20 与新上限 20（限制已生效）: %+v", crB)
	}
	if !strings.Contains(crB.Reason, "亏损 20") || !strings.Contains(crB.Reason, "上限 20") {
		t.Fatalf("B 撤销原因必须以亏损 20、上限 20 解释: %q", crB.Reason)
	}

	// 3) 再撤 A 买单的未成交部分：取消 6 份，已成交 4 在记录中保留。
	crA := newRecs[2]
	if crA.Kind != RecordCanceled || crA.OrderID != buyA || crA.Symbol != "A" || crA.Side != Buy {
		t.Fatalf("第三条应为 A 买单（编号 1）的撤销记录: %+v", crA)
	}
	if crA.Qty != 10 || crA.Filled != 4 || crA.Remaining != 6 {
		t.Fatalf("A 撤销记录应保留总量 10、已成交 4，并说明取消 6 份: %+v", crA)
	}
	if !crA.QuoteValid || crA.QuoteSeq != 2 || crA.QuoteMoment != 30 || crA.QuotePrice != 5 {
		t.Fatalf("A 撤销记录报价定位应为 A 序号2/时刻30/价格5: %+v", crA)
	}
	if crA.RiskDay != 1 || crA.RiskBaseline != 1004 || crA.RiskEquity != 984 ||
		crA.RiskLoss != 20 || crA.RiskLimit != 20 || !crA.RiskRestrict {
		t.Fatalf("A 撤销记录必须固化触线时亏损 20 与新上限 20（限制已生效）: %+v", crA)
	}
	if !strings.Contains(crA.Reason, "亏损 20") || !strings.Contains(crA.Reason, "上限 20") {
		t.Fatalf("A 撤销原因必须以亏损 20、上限 20 解释: %q", crA.Reason)
	}

	// 限制持续到下一交易日：合规新买单（资金、限额、报价、缺口均满足）也必须拒绝。
	recsBeforeBuy := len(e.Records())
	if id, err := e.Buy("B", 1, 20); err == nil || id != 0 {
		t.Fatalf("触线后合规新买单也必须拒绝，实际 id=%d err=%v", id, err)
	}
	if e.ReservedCash() != 0 || e.Cash() != 964 {
		t.Fatalf("被拒绝的买单不得占用现金: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	rj := e.Records()[recsBeforeBuy]
	if rj.Kind != RecordRejected || rj.Symbol != "B" || rj.Side != Buy ||
		rj.RiskLoss != 20 || rj.RiskLimit != 20 || !rj.RiskRestrict || rj.RiskDay != 1 {
		t.Fatalf("拒单记录必须固化限制状态（亏损 20、上限 20、已限制）: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "亏损保护") {
		t.Fatalf("拒单原因必须指向日内亏损保护: %q", rj.Reason)
	}

	// 负上限调整必须报错：风险状态、订单、占用与事件记录全部不变。
	recsBeforeNeg := len(e.Records())
	if err := e.SetLossLimit(-1); err == nil {
		t.Fatal("负亏损上限必须报错")
	} else if !strings.Contains(err.Error(), "不能为负") {
		t.Fatalf("错误信息应说明上限不能为负: %v", err)
	}
	st = e.RiskStatus()
	if !st.Restricted || st.LossLimit != 20 || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("负上限报错后风险状态必须不变: %+v", st)
	}
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("负上限报错后资金持仓必须不变: cash=%d reserved=%d posA=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("负上限报错后卖单必须仍有效: %+v", o)
	}
	if len(e.Records()) != recsBeforeNeg {
		t.Fatalf("负上限报错不得追加任何记录，原有 %d 现有 %d", recsBeforeNeg, len(e.Records()))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("负上限报错不得追加触线记录")
	}
}

// TestRiskLowerLimitStillAboveLossOnlyUpdatesLimit 单独固定“下调但尚未碰到当前亏损”
// 语义：100 → 21 只更新上限，即使再调回 100 也不触线；订单、占用、记录保持不变。
func TestRiskLowerLimitStillAboveLossOnlyUpdatesLimit(t *testing.T) {
	e, buyA, buyB, sellA := riskAdjustLimitCrossSetup(t)

	recsBefore := len(e.Records())
	if err := e.SetLossLimit(21); err != nil {
		t.Fatalf("调到 21 不得报错: %v", err)
	}
	if err := e.SetLossLimit(100); err != nil {
		t.Fatalf("调回 100 不得报错: %v", err)
	}
	st := e.RiskStatus()
	if st.Restricted || st.LossLimit != 100 || st.Equity != 984 || st.Loss != 20 || st.Baseline != 1004 {
		t.Fatalf("亏损始终未碰上限，不得触线: %+v", st)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("未触线的限额调整不得追加记录，原有 %d 现有 %d", recsBefore, len(e.Records()))
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("未触线调整不得改变资金与持仓: cash=%d reserved=%d posA=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(buyA); o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 买单应保持部分成交、剩余 6: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单应保持待成交、剩余 2: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单应保持有效: %+v", o)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("未碰上限不得产生触线记录")
	}
}

// TestRiskNegativeLimitBeforeTriggerLeavesEverythingUntouched 覆盖触线前的负上限
// 调整：报错且不改变风险状态、订单或占用，也不追加触线、撤销记录。
func TestRiskNegativeLimitBeforeTriggerLeavesEverythingUntouched(t *testing.T) {
	e, buyA, buyB, sellA := riskAdjustLimitCrossSetup(t)

	recsBefore := len(e.Records())
	if err := e.SetLossLimit(-1); err == nil {
		t.Fatal("负亏损上限必须报错")
	} else if !strings.Contains(err.Error(), "不能为负") {
		t.Fatalf("错误信息应说明上限不能为负: %v", err)
	}
	st := e.RiskStatus()
	if st.Restricted || st.Open != true || st.LossLimit != 100 || st.Equity != 984 || st.Loss != 20 {
		t.Fatalf("负上限报错后风险状态必须保持亏损 20/上限 100/未限制: %+v", st)
	}
	if e.Cash() != 964 || e.ReservedCash() != 100 || e.AvailableCash() != 864 {
		t.Fatalf("负上限报错后资金占用必须不变: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if o, _ := e.Order(buyA); o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 买单必须保持部分成交、剩余 6: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("B 买单必须保持待成交、剩余 2: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单必须保持有效: %+v", o)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("负上限报错不得追加任何记录，原有 %d 现有 %d", recsBefore, len(e.Records()))
	}
	for _, r := range e.Records() {
		if r.Kind == RecordRiskTriggered || r.Kind == RecordCanceled {
			t.Fatalf("负上限报错不得追加触线或撤销记录: %+v", r)
		}
	}
}
