package book

import (
	"strings"
	"testing"
)

// 本文件为“同一张买单先有成交、再修改总量与限价、随后再次部分成交，而第二笔成交
// 恰好触发日内亏损保护”补充回归保障，固定三类各自独立的时点口径：
//
//   - 修改不重算历史：第一次成交仍按原成交价 9 记账，累计成交量不被新总量改写；
//   - 修改只重定未成交部分：剩余 6 股按新总量 8、新限价 12 占用 72 现金；
//   - 第二笔成交先按实际成交价 12 结算现金，再按最新已生效报价 8 重算风险；
//     触线后撤销的是按新委托参数计算的剩余 3 股，成交返回结果定格在撤单之前，
//     订单查询反映整次操作结束后的已撤销状态。
//
// 共用构造（初始现金 1000）：报价 seq1@时刻10 价格 10，无持仓时开启交易日 1；
// 买单总量 10、限价 10，先以 9 成交 2（现金 982、持仓 2、剩余 8 占用 80），
// 再改为总量 8、限价 12（现金不变、持仓不变，剩余 6 占用 72）；报价 seq2@时刻20
// 降到 8（净值 998、亏损 2，报价变化本身不触线）；再以 12 成交 3：现金 946、
// 持仓 5，净值 986、亏损 14。亏损上限 14 时恰好触线；上限 15 时不撤单。
// 成交价（9、12）与估值报价（10、8）自始至终分别记账，不得互相顶替。

// modifiedBuyRiskSetup 构造上述前置状态并完成到“第二次成交之前”：
// 已发生接受、第一次成交、修改三条记录；报价 8 已生效但因亏损仅 2 尚未触线。
// 返回引擎、买单编号、第一次成交的历史结果与第二次成交前的记录条数。
func modifiedBuyRiskSetup(t *testing.T, lossLimit int64) (e *Engine, bid int64, first FillResult, recsBeforeSecondFill int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	bid = mustBuy(t, e, "A", 10, 10)

	// 第一次成交：成交价 9（低于限价 10），只按实际价 9 结算，不能按限价扣现金。
	var err error
	first, err = e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 2})
	if err != nil {
		t.Fatalf("第一次成交必须成功: %v", err)
	}
	if first != (FillResult{TradeID: 1, OrderID: bid, Price: 9, Qty: 2,
		Filled: 2, Remaining: 8, Status: StatusPartial}) {
		t.Fatalf("第一次成交结果应为累计 2、剩余 8、部分成交: %+v", first)
	}
	if e.Cash() != 982 || e.ReservedCash() != 80 || e.AvailableCash() != 902 || e.Position("A") != 2 {
		t.Fatalf("第一次成交后应按成交价 9 结算、剩余 8 股仍按旧限价 10 占用 80: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 修改为总量 8、限价 12：已成交 2 股与现金余额一律不动，剩余 6 股改按
	// 12 占用 72（替换原 8×10=80），可用现金相应回升到 910。
	if err := e.Modify(bid, 8, 12); err != nil {
		t.Fatalf("修改部分成交买单必须成功: %v", err)
	}
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Qty != 8 || o.Limit != 12 || o.Filled != 2 || o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("修改后订单应为总量 8、限价 12、已成交 2、剩余 6: %+v", o)
	}
	if e.Cash() != 982 || e.ReservedCash() != 72 || e.AvailableCash() != 910 || e.Position("A") != 2 {
		t.Fatalf("修改只能替换未成交部分占用，历史现金 982 与持仓 2 不变: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 报价降到 8：持仓 2 股按新报价估值，净值 998、亏损 2；低于任一待测上限，
	// 报价变化本身不得产生触线或撤单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 8}, 1)
	st := e.RiskStatus()
	if !st.Open || st.Baseline != 1000 || st.Equity != 998 || st.Loss != 2 || st.Restricted {
		t.Fatalf("报价降到 8 后应为净值 998、亏损 2、尚未限制: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("报价 8 只造成亏损 2，不得提前产生触线记录")
	}

	recsBeforeSecondFill = len(e.Records()) // 接受、第一次成交、修改 共 3 条
	return e, bid, first, recsBeforeSecondFill
}

// TestRiskModifiedBuySecondPartialFillSettlesThenCancelsNewRemainder 覆盖主场景：
// 上限 14 时，修改后以新限价 12 成交 3 股恰好把亏损做到 14。逐笔核对历史成交不
// 重算、本次按成交价结算、持仓按报价 8 估值、成交结果与订单查询两个时点，以及
// 成交→触线→剩余量撤销的完整记录链。
func TestRiskModifiedBuySecondPartialFillSettlesThenCancelsNewRemainder(t *testing.T) {
	e, bid, first, recsBefore := modifiedBuyRiskSetup(t, 14)

	// 成交价 12 恰好等于修改后的新限价：按旧限价 10 本应拒绝，能成交即说明
	// 成交校验使用的是已生效的新参数。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3})
	if err != nil {
		t.Fatalf("第二次成交必须成功入账: %v", err)
	}

	// 返回结果固定在“本次成交记账完成、风险撤单之前”：累计 5、剩余 3、部分成交，
	// 不被同一调用随后发生的撤单回填。
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 12, Qty: 3,
		Filled: 5, Remaining: 3, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 5、剩余 3、部分成交: %+v", res)
	}

	// 资金与持仓：本次按实际成交价 12×3=36 结算（982-36=946），不是按限价
	// 重算历史；剩余 3 股的新占用 3×12=36 被风险撤销全部释放，可用等于余额。
	// 累计买入 5 股（2@9 + 3@12）全部保留在持仓中。
	if e.Cash() != 946 || e.Position("A") != 5 || e.Sellable("A") != 5 {
		t.Fatalf("成交入账与剩余撤单后资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 946 {
		t.Fatalf("按新参数计算的剩余 3 股占用 36 必须随保护撤单全部释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}

	// 风险状态：持仓按最新已生效报价 8 估值，净值 946+5×8=986、亏损 14，
	// 恰好等于上限 14（>= 比较）。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 14 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 986 || st.Loss != 14 {
		t.Fatalf("第二次成交后应恰好触线且按报价 8 估值: %+v", st)
	}

	// 订单查询反映整次操作结束后：已撤销、有效剩余量 0；新总量 8 与新限价 12
	// 保留，已成交 5 保留，并携带以亏损 14、上限 14 解释的撤销原因。
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Qty != 8 || o.Limit != 12 || o.Filled != 5 || o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应保留总量 8、限价 12、已成交 5，已撤销且有效剩余量为 0: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 14") || !strings.Contains(o.Reason, "上限 14") {
		t.Fatalf("撤销原因必须按成交后亏损 14 与上限 14 解释: %q", o.Reason)
	}

	// 第二次成交只新增 3 条记录，顺序为：成交 → 当日首次触线 → 剩余量撤销。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（成交、触线、撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：成交价 12 单独保存；报价快照是当时已生效的估值价 8
	// （seq2、时刻 20），两个价格不能混用；剩余 3 取成交记账后、撤单前口径。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 2 || fr.OrderID != bid || fr.Side != Buy {
		t.Fatalf("第一条应为第二次成交记录: %+v", fr)
	}
	if fr.TradePrice != 12 || fr.Qty != 3 || fr.Filled != 5 || fr.Remaining != 3 {
		t.Fatalf("成交记录应保存成交价 12、本次 3、累计 5、当时剩余 3: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 20 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定估值价 8（seq2、时刻 20），不能写成成交价 12: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 986 ||
		fr.RiskLoss != 14 || fr.RiskLimit != 14 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源只能是本次成交（关联成交编号 2），不得归因于此前的修改
	// 或报价 8 的变化；估值报价定位保存报价 8 的序号 2、时刻 20 与价格。
	tr := newRecs[1]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", tr)
	}
	if tr.RiskTrigger != RiskTriggerTrade || tr.RiskRefSeq != 2 || tr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 2（成交编号），不得挂到修改或报价上: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 1000 || tr.RiskEquity != 986 ||
		tr.RiskLoss != 14 || tr.RiskLimit != 14 || !tr.RiskRestrict {
		t.Fatalf("触线快照必须固化成交后亏损 14 与上限 14: %+v", tr)
	}
	if len(tr.RiskQuoteRefs) != 1 ||
		tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 20, Price: 8}) {
		t.Fatalf("触线估值报价定位必须锚定 seq2@时刻20 价格 8: %+v", tr.RiskQuoteRefs)
	}
	if !strings.Contains(tr.Reason, "亏损 14") || !strings.Contains(tr.Reason, "上限 14") {
		t.Fatalf("触线原因必须写明亏损 14 与上限 14: %q", tr.Reason)
	}

	// 3) 撤销记录：按修改后的委托参数说明实际取消剩余 3 股（总量 8、限价 12、
	// 已成交 5）；不得混入成交价 12 充当限价之外的字段——TradePrice 必须为零，
	// 估值报价仍为 8。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy {
		t.Fatalf("第三条应为该买单的剩余量撤销记录: %+v", cr)
	}
	if cr.Limit != 12 || cr.Qty != 8 || cr.Filled != 5 || cr.Remaining != 3 {
		t.Fatalf("撤销记录应保留新总量 8、新限价 12、已成交 5，并注明本次取消 3 股: %+v", cr)
	}
	if cr.TradePrice != 0 {
		t.Fatalf("撤销记录不得混入成交价 12: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 2 || cr.QuoteMoment != 20 || cr.QuotePrice != 8 {
		t.Fatalf("撤销记录报价定位必须仍是估值价 8（seq2、时刻 20）: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 986 ||
		cr.RiskLoss != 14 || cr.RiskLimit != 14 || !cr.RiskRestrict {
		t.Fatalf("撤销记录必须固化成交后亏损 14 与上限 14（限制已生效）: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "亏损 14") || !strings.Contains(cr.Reason, "上限 14") {
		t.Fatalf("撤销原因必须以亏损 14、上限 14 解释: %q", cr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 历史记录保留原内容：第一次成交仍按成交价 9、当时总量 10 下的剩余 8 记账；
	// 修改记录保存修改前 10/10 与修改后 8/12，两者都不被后续成交与撤单改写。
	hf := recs[1]
	if hf.Kind != RecordFilled || hf.TradeID != 1 || hf.TradePrice != 9 ||
		hf.Qty != 2 || hf.Filled != 2 || hf.Remaining != 8 {
		t.Fatalf("第一次成交记录必须保留成交价 9、本次 2、累计 2、当时剩余 8: %+v", hf)
	}
	if !hf.QuoteValid || hf.QuoteSeq != 1 || hf.QuoteMoment != 10 || hf.QuotePrice != 10 {
		t.Fatalf("第一次成交记录的报价定位必须保留当时的估值价 10: %+v", hf)
	}
	if hf.RiskEquity != 1002 || hf.RiskLoss != 0 || hf.RiskRestrict {
		t.Fatalf("第一次成交记录应保留成交当时的风险快照（净值 1002、未限制）: %+v", hf)
	}
	mr := recs[2]
	if mr.Kind != RecordModified || mr.Limit != 12 || mr.Qty != 8 ||
		mr.Filled != 2 || mr.Remaining != 6 ||
		mr.OldLimit != 10 || mr.OldQty != 10 || mr.OldFilled != 2 {
		t.Fatalf("修改记录必须保留新参数 8/12、已成交 2 与旧参数 10/10: %+v", mr)
	}
	if !mr.QuoteValid || mr.QuoteSeq != 1 || mr.QuoteMoment != 10 || mr.QuotePrice != 10 {
		t.Fatalf("修改记录的报价定位必须保留修改当时的估值价 10（seq1、时刻 10）: %+v", mr)
	}

	// 幂等重放必须返回各自第一次的结果：第一笔停留在修改前的剩余 8，第二笔停留
	// 在撤单前的累计 5、剩余 3；订单当前的已撤销状态不回填任何历史结果，资金、
	// 持仓、占用与记录均不再变化。
	recordsBefore := len(e.Records())
	cashBefore, reservedBefore := e.Cash(), e.ReservedCash()
	posBefore := e.Position("A")
	replay1, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 2})
	if err != nil {
		t.Fatalf("第一次成交的原样重放必须成功返回原结果: %v", err)
	}
	if replay1 != first {
		t.Fatalf("第一次成交重放必须原样返回首次结果（剩余 8 不被修改与撤单改写）: first=%+v replay=%+v",
			first, replay1)
	}
	replay2, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3})
	if err != nil {
		t.Fatalf("第二次成交的原样重放必须成功返回原结果: %v", err)
	}
	if replay2 != res {
		t.Fatalf("第二次成交重放必须原样返回首次结果（部分成交、剩余 3）: first=%+v replay=%+v",
			res, replay2)
	}
	if e.Cash() != cashBefore || e.ReservedCash() != reservedBefore || e.Position("A") != posBefore {
		t.Fatalf("重放不得改变资金与持仓: cash=%d(before %d) reserved=%d(before %d) pos=%d(before %d)",
			e.Cash(), cashBefore, e.ReservedCash(), reservedBefore, e.Position("A"), posBefore)
	}
	if len(e.Records()) != recordsBefore {
		t.Fatalf("重放不得新增任何记录: before=%d after=%d", recordsBefore, len(e.Records()))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("重放不得重复产生触线记录")
	}

	// 限制持续：新买单继续被拒；已撤销订单不能再撤销，也不能借修改恢复；
	// 为它提交新成交编号的成交只被拒绝，已入账的 5 股与现金不变。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制后新买单必须拒绝")
	}
	if err := e.Cancel(bid); err == nil {
		t.Fatal("已被风险保护撤销的订单不能再次撤销")
	}
	if err := e.Modify(bid, 8, 12); err == nil {
		t.Fatal("已被风险保护撤销的订单不能借修改恢复")
	}
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 1}); err == nil {
		t.Fatal("已撤销订单的新成交必须拒绝")
	} else if !strings.Contains(err.Error(), "已撤销") {
		t.Fatalf("拒绝原因应说明订单已撤销: %v", err)
	}
	if e.Cash() != 946 || e.Position("A") != 5 || e.ReservedCash() != 0 {
		t.Fatalf("拒绝不得改变账实: cash=%d pos=%d reserved=%d", e.Cash(), e.Position("A"), e.ReservedCash())
	}
}

// TestRiskModifiedBuySecondPartialFillBelowLimitKeepsRemainderAndReserve 覆盖未触线
// 的相邻情况：亏损上限 15 时，同样的第二次成交只造成亏损 14，不撤单——订单仍为
// 部分成交、剩余 3 股有效并按新限价占用 36 现金，随后还能继续成交。
func TestRiskModifiedBuySecondPartialFillBelowLimitKeepsRemainderAndReserve(t *testing.T) {
	e, bid, _, recsBefore := modifiedBuyRiskSetup(t, 15)

	res, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3})
	if err != nil {
		t.Fatalf("第二次成交必须成功: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 12, Qty: 3,
		Filled: 5, Remaining: 3, Status: StatusPartial}) {
		t.Fatalf("成交结果应为累计 5、剩余 3、部分成交: %+v", res)
	}

	// 亏损 14 低于上限 15：不触线，不产生触线或撤销记录。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 986 || st.Loss != 14 || st.LossLimit != 15 {
		t.Fatalf("亏损 14 未达上限 15，不应触线: %+v", st)
	}

	// 订单仍为部分成交，新总量 8、新限价 12 保留，剩余 3 股有效、无撤销原因。
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Qty != 8 || o.Limit != 12 || o.Filled != 5 ||
		o.Remaining() != 3 || o.Reason != "" {
		t.Fatalf("未触线时剩余买单必须按新参数继续有效: %+v", o)
	}
	// 现金按成交价结算为 946；剩余 3 股按新限价 12 占用 36，可用 910。
	if e.Cash() != 946 || e.ReservedCash() != 36 || e.AvailableCash() != 910 || e.Position("A") != 5 {
		t.Fatalf("剩余 3 股的新占用 36 必须继续保留: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 只新增一条成交记录，不得有触线或撤销。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", newRecs)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("亏损 14 低于上限 15，不得产生触线记录")
	}

	// 保留的剩余 3 股仍可继续成交（占用有效）：按不高于新限价 12 的价格 8 再成交
	// 3 股后订单全部成交、占用清零；成交价 8 结算不改变估值报价，仍不触线。
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: bid, Symbol: "A", Side: Buy, Price: 8, Qty: 3})
	if err != nil {
		t.Fatalf("保留的剩余 3 股必须仍可成交: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 8 || res2.Remaining != 0 {
		t.Fatalf("剩余 3 股全部成交后应为全部成交、累计 8: %+v", res2)
	}
	o2, _ := e.Order(bid)
	if o2.Status != StatusFilled || o2.Remaining() != 0 || e.ReservedCash() != 0 {
		t.Fatalf("全部成交后订单应为已成交且占用清零: order=%+v reserved=%d", o2, e.ReservedCash())
	}
	if e.Cash() != 922 || e.AvailableCash() != 922 || e.Position("A") != 8 {
		t.Fatalf("第三次成交应按成交价 8 结算 24：cash=%d available=%d pos=%d",
			e.Cash(), e.AvailableCash(), e.Position("A"))
	}
	if st := e.RiskStatus(); st.Restricted || st.Equity != 986 || st.Loss != 14 {
		t.Fatalf("按报价 8 成交不改变净值 986 与亏损 14，仍不应触线: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("全过程亏损均低于上限 15，不得产生触线记录")
	}
}
