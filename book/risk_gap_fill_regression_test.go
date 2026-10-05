package book

import (
	"strings"
	"testing"
)

// 本文件为“行情缺口期间已接受买单的成交”补充回归保障：缺口阻止新买单，但已接受
// 订单仍可按有效参数成交；成交后的净值只能使用最新已生效报价，等待补齐的报价
// 不能提前参与判断。
//
// 关键构造：初始现金 1000，合约 A 最新报价 seq1@时刻10 价格 8；接受总量 10、
// 限价 10 的买单（占用现金 100，余额仍为 1000），开启亏损上限为 lossLimit 的
// 交易日（无持仓，基准净值 1000）。随后 seq3@时刻30 价格 20 先到并等待（seq2
// 缺失），最新报价与风险状态不得改变。此时为该买单提交价格 10、数量 5 的成交：
// 必须成功入账，不能因缺口拒绝，也不能按等待中的价格 20 估值。
//
// 成交后现金 950、持仓 5，按已生效报价 8 估值净值 990、亏损 10：
//   - 上限 10：亏损恰好达限，立即触发保护并撤销该买单剩余 5 股，占用现金归零；
//     成交返回仍定格在记账完成时的部分成交（累计 5、剩余 5），订单查询则为已
//     撤销、有效剩余 0 并带日内亏损保护撤销原因；记录依次为成交、当日首次触线
//     （来源为这笔成交，估值锚定 seq1@时刻10 价格 8）、剩余量撤销；整个处理
//     结束后最新报价仍为 seq1，缺口仍在，等待的 seq3 不被这次成交消耗。
//   - 上限 11：亏损 10 低于上限，订单保持部分成交，剩余 5 股继续占用 50 现金，
//     没有触线或保护撤销记录。
//
// 两种情况下，缺口期间申请新的买单仍按已有规则拒绝。

// riskGapFillSetup 构造共用前置状态：
// 初始现金 1000，合约 A 限额 100，最新报价 seq1@时刻10 价格 8；
// 接受总量 10、限价 10 的买单 bid（占用现金 100，可用 900，无持仓）；
// 开启交易日 1、亏损上限 lossLimit（基准净值 1000）；
// 再收到 seq3@时刻30 价格 20，因 seq2 缺失进入等待。
// 返回引擎与该买单编号；调用前缺口存在、最新报价仍为 seq1、保护未触线。
func riskGapFillSetup(t *testing.T, lossLimit int64) (e *Engine, bid int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 8}, 1)
	bid = mustBuy(t, e, "A", 10, 10)
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 1000 || e.ReservedCash() != 100 || e.AvailableCash() != 900 || e.Position("A") != 0 {
		t.Fatalf("下单并开日后的前置资金状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 1000 ||
		st.Equity != 1000 || st.Loss != 0 || st.Restricted {
		t.Fatalf("开日后的前置风险状态错误: %+v", st)
	}

	// seq3 跨号先到：只进入等待，最新报价、净值与风险状态都不能改变。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3 跨号必须进入等待并形成缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Moment != 10 || q.Price != 8 {
		t.Fatalf("等待报价不得推进最新报价: %+v ok=%v", q, ok)
	}
	if st := e.RiskStatus(); st.Equity != 1000 || st.Loss != 0 || st.Restricted {
		t.Fatalf("等待报价不得提前参与估值或改变风险状态: %+v", st)
	}
	return e, bid
}

// TestRiskGapFillSettlesThenTriggersOnAppliedQuote 覆盖主场景：缺口期间部分买入
// 成交先成功入账，成交后按最新已生效报价 8（而非等待中的 20）估值，亏损恰好
// 达到上限 10，同一调用随后触发保护并撤销剩余 5 股。逐笔核对资金、持仓、成交
// 结果、订单查询、全部新增记录以及缺口状态的保持。
func TestRiskGapFillSettlesThenTriggersOnAppliedQuote(t *testing.T) {
	e, bid := riskGapFillSetup(t, 10)

	recsBefore := len(e.Records())
	tr := Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 5}
	res, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("缺口期间已接受买单的成交必须成功入账，不能因缺口拒绝: %v", err)
	}

	// 成交返回结果定格在“成交记账完成、风险撤单之前”：累计 5、剩余 5、部分成交。
	if res != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 5,
		Filled: 5, Remaining: 5, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 5、剩余 5、部分成交: %+v", res)
	}

	// 资金与持仓：成交价 10×5=50 已结算，现金 950、持仓 5；剩余 5 股的占用
	// 50 被风险撤销全部释放，占用归零、可用等于余额。
	if e.Cash() != 950 || e.Position("A") != 5 || e.Sellable("A") != 5 {
		t.Fatalf("成交入账与剩余撤单后的资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 950 {
		t.Fatalf("剩余 5 股占用的 50 现金必须全部释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}

	// 风险状态：净值必须按最新已生效报价 8 估值（950+5×8=990），亏损恰好 10
	// 达到上限并限制；若误用等待中的价格 20，净值为 1050、不会触线。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 10 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 990 || st.Loss != 10 {
		t.Fatalf("成交后应按已生效报价 8 估值并触线（净值 990、亏损 10）: %+v", st)
	}

	// 订单查询：总量 10、已成交 5 保留，状态为已撤销、有效剩余量为 0，
	// 并携带日内亏损保护撤销原因；与成交返回结果（部分成交、剩余 5）并存。
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Qty != 10 || o.Filled != 5 || o.Limit != 10 || o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应保留总量 10、已成交 5，已撤销且有效剩余量为 0: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 10") || !strings.Contains(o.Reason, "上限 10") {
		t.Fatalf("撤销原因必须按成交后亏损 10 与上限 10 解释: %q", o.Reason)
	}

	// 新增记录恰好 3 条，顺序为：成交 → 当日首次触线 → 剩余量撤销。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（成交、触线、撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：实际成交价 10 单独保存；报价快照是当时已生效的估值价 8
	// （seq1、时刻 10），不得写成等待中的 seq3@时刻30 价格 20。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 1 || fr.OrderID != bid || fr.Side != Buy {
		t.Fatalf("第一条应为成交记录: %+v", fr)
	}
	if fr.TradePrice != 10 || fr.Qty != 5 || fr.Filled != 5 || fr.Remaining != 5 {
		t.Fatalf("成交记录应保存成交价 10、本次 5、累计 5、当时剩余 5: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 1 || fr.QuoteMoment != 10 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定已生效的 seq1@时刻10 价格 8，不得使用等待报价: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 990 ||
		fr.RiskLoss != 10 || fr.RiskLimit != 10 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源是这笔成交（关联成交编号 1）；估值报价定位取已生效的
	// seq1@时刻10 价格 8，不得把等待中的 seq3@价格20 写成估值依据。
	tr2 := newRecs[1]
	if tr2.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", tr2)
	}
	if tr2.RiskTrigger != RiskTriggerTrade || tr2.RiskRefSeq != 1 || tr2.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 1（成交编号），不得挂到报价或其他来源: trigger=%s ref=%d moment=%d",
			tr2.RiskTrigger, tr2.RiskRefSeq, tr2.RiskRefMoment)
	}
	if tr2.RiskDay != 1 || tr2.RiskBaseline != 1000 || tr2.RiskEquity != 990 ||
		tr2.RiskLoss != 10 || tr2.RiskLimit != 10 || !tr2.RiskRestrict {
		t.Fatalf("触线快照必须固化成交后净值 990、亏损 10 与上限 10: %+v", tr2)
	}
	if len(tr2.RiskQuoteRefs) != 1 ||
		tr2.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 8}) {
		t.Fatalf("触线估值报价定位必须锚定 seq1@时刻10 价格 8，不得使用等待中的 seq3@价格20: %+v", tr2.RiskQuoteRefs)
	}
	if !strings.Contains(tr2.Reason, "亏损 10") || !strings.Contains(tr2.Reason, "上限 10") {
		t.Fatalf("触线原因必须写明亏损 10 与上限 10: %q", tr2.Reason)
	}

	// 3) 撤销记录：注明取消剩余 5 股，固化成交后亏损 10 与上限 10；
	// 不带成交价，估值报价仍为 seq1@时刻10 价格 8。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy {
		t.Fatalf("第三条应为该买单的剩余量撤销记录: %+v", cr)
	}
	if cr.Qty != 10 || cr.Filled != 5 || cr.Remaining != 5 {
		t.Fatalf("撤销记录应保留总量 10、已成交 5，并注明本次取消 5 股: %+v", cr)
	}
	if cr.TradePrice != 0 {
		t.Fatalf("撤销记录不得混入成交价 10: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 1 || cr.QuoteMoment != 10 || cr.QuotePrice != 8 {
		t.Fatalf("撤销记录报价定位必须仍是已生效的 seq1@时刻10 价格 8: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 990 ||
		cr.RiskLoss != 10 || cr.RiskLimit != 10 || !cr.RiskRestrict {
		t.Fatalf("撤销记录必须固化成交后亏损 10 与上限 10（限制已生效）: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "亏损 10") || !strings.Contains(cr.Reason, "上限 10") {
		t.Fatalf("撤销原因必须以亏损 10、上限 10 解释: %q", cr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 整个成交处理结束后：最新报价仍为 seq1，缺口仍在，等待的 seq3 没有被
	// 这次成交消耗。
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Moment != 10 || q.Price != 8 {
		t.Fatalf("成交处理不得推进最新报价，应仍为 seq1@时刻10 价格 8: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("成交处理不得消除行情缺口")
	}

	// 缺口期间申请新的买单仍按已有规则拒绝（此处保护已触发，拒绝原因来自
	// 日内亏损保护）；该拒绝与本批事件记录无关，放在记录断言之后。
	if _, err := e.Buy("A", 1, 1); err == nil ||
		!strings.Contains(err.Error(), "日内亏损保护") {
		t.Fatalf("触线限制后缺口期间的新买单必须拒绝: %v", err)
	}

	// 等待中的 seq3 仍然保留：补齐 seq2 后 seq2、seq3 依次生效（2 条），
	// 缺口消除、最新报价推进到 seq3@时刻30 价格 20；限制不因价格回升而解除。
	if n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 9}); err != nil || n != 2 {
		t.Fatalf("等待报价必须保留，补齐 seq2 后应新生效 2 条: n=%d err=%v", n, err)
	}
	if e.HasGap("A") {
		t.Fatal("seq2 补齐后缺口必须消除")
	}
	if q, _ := e.CurrentQuote("A"); q.Seq != 3 || q.Moment != 30 || q.Price != 20 {
		t.Fatalf("最新报价应推进到 seq3@时刻30 价格 20: %+v", q)
	}
	if st := e.RiskStatus(); !st.Restricted || st.Equity != 1050 || st.Loss != 0 {
		t.Fatalf("价格回升后净值按 seq3@20 估值（1050），但限制必须保留: %+v", st)
	}
}

// TestRiskGapFillBelowLimitKeepsPartialOrderAndReserve 覆盖未触线路径：亏损上限
// 为 11 时，同样这笔成交只造成 10 的亏损（低于上限），订单保持部分成交，
// 剩余 5 股继续占用 50 现金，没有触线或保护撤销记录。
func TestRiskGapFillBelowLimitKeepsPartialOrderAndReserve(t *testing.T) {
	e, bid := riskGapFillSetup(t, 11)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 5})
	if err != nil {
		t.Fatalf("缺口期间已接受买单的成交必须成功入账: %v", err)
	}
	if res != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 5,
		Filled: 5, Remaining: 5, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 5、剩余 5、部分成交: %+v", res)
	}

	// 净值仍按已生效报价 8 估值：950+5×8=990，亏损 10 低于上限 11，不触线。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 990 || st.Loss != 10 || st.LossLimit != 11 {
		t.Fatalf("亏损 10 未达上限 11，不应触线: %+v", st)
	}

	// 订单保持部分成交、剩余 5 股有效；剩余占用 5×10=50 继续保留。
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Qty != 10 || o.Filled != 5 || o.Remaining() != 5 || o.Reason != "" {
		t.Fatalf("未触线时订单必须保持部分成交、剩余 5 股有效: %+v", o)
	}
	if e.Cash() != 950 || e.ReservedCash() != 50 || e.AvailableCash() != 900 || e.Position("A") != 5 {
		t.Fatalf("剩余 5 股的现金占用 50 必须继续保留: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 只新增一条成交记录，不得有触线或保护撤销记录。
	recs := e.Records()[recsBefore:]
	if len(recs) != 1 || recs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", recs)
	}
	if recs[0].QuoteSeq != 1 || recs[0].QuoteMoment != 10 || recs[0].QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定已生效的 seq1@时刻10 价格 8: %+v", recs[0])
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("未达上限不得产生触线记录")
	}

	// 最新报价仍为 seq1，缺口仍在，等待的 seq3 不被这次成交消耗。
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Price != 8 {
		t.Fatalf("成交处理不得推进最新报价: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("成交处理不得消除行情缺口")
	}

	// 缺口期间申请新的买单仍按已有规则拒绝（未触线，拒绝原因来自行情缺口）。
	if _, err := e.Buy("A", 1, 1); err == nil || !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("缺口期间的新买单必须按缺口规则拒绝: %v", err)
	}
}
