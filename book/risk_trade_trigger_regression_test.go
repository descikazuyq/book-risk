package book

import (
	"strings"
	"testing"
)

// 本文件为“部分买入成交入账后才使日内亏损达到上限”补充回归保障。
//
// 关键构造：初始现金 1000，先按价格 10 买入并持有 10 股（现金 900），以净值
// 1000 开启交易日、亏损上限 100；最新有效报价降到 5 时亏损为 50，尚未限制买入。
// 随后接受总量 10、限价 20 的买单（占用现金 200），再提交价格 20、数量 4 的成交：
// 成交必须先完整成功入账（现金 820、持仓 14），成交后按已生效报价 5 估值，净值
// 890、亏损 110 达到上限，风险保护随后撤销该订单尚未成交的 6 股并释放剩余占用
// 120，可用现金回到 820。
//
// 必须同时核对：成交返回结果（累计 4、剩余 6、部分成交）与订单查询（总量 10、
// 已成交 4、已撤销、有效剩余量 0、带风险撤销原因）各自保留、互不覆盖；事件顺序
// 为成交、当日首次触线（来源成交、关联成交编号、估值锚定报价 5）、剩余量撤销
// （取消 6 股并固化亏损 110 与上限 100）；成交实际价格 20 与估值价格 5 分别留存。
//
// 边界：亏损恰好等于上限同样触线；成交后亏损仍低于上限时只完成部分成交，剩余
// 买单与现金占用继续有效，不产生触线或撤销记录；触线撤单后原样重放已入账成交，
// 必须原样返回第一次结果，资金、持仓与记录均不再变化。

// riskTradeSetup 构造共用前置状态：
// 初始现金 1000，A 合约 10 股 @10 已成交入账（cash=900, pos=10, seq1@10）；
// 以净值 1000 开启交易日 1、亏损上限 lossLimit；seq2@5 生效后净值 950、亏损 50；
// 再接受总量 10、限价 20 的买单 bid（现金占用 200，可用 700）。
// 返回引擎与该买单编号；除显式说明外调用前保护尚未触线。
func riskTradeSetup(t *testing.T, lossLimit int64) (e *Engine, bid int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	first := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: first, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 5}, 1) // 净值 950，亏损 50
	if e.RiskStatus().Baseline != 1000 || e.RiskStatus().Equity != 950 ||
		e.RiskStatus().Loss != 50 || e.RiskStatus().Restricted {
		t.Fatalf("开日与降价后的前置风险状态错误: %+v", e.RiskStatus())
	}
	bid = mustBuy(t, e, "A", 10, 20)
	if e.Cash() != 900 || e.ReservedCash() != 200 || e.AvailableCash() != 700 || e.Position("A") != 10 {
		t.Fatalf("挂入限价 20 的买单后前置资金状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	return e, bid
}

// TestRiskPartialBuyFillSettlesThenCancelsRemainder 覆盖主场景：部分买入成交先
// 成功入账，再由同一笔成交触发日内亏损保护、撤销剩余 6 股。逐笔核对资金、持仓、
// 成交结果、订单查询与全部新增记录。
func TestRiskPartialBuyFillSettlesThenCancelsRemainder(t *testing.T) {
	e, bid := riskTradeSetup(t, 100)

	recsBefore := len(e.Records())
	tr := Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 20, Qty: 4}
	res, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("部分成交必须成功入账: %v", err)
	}

	// 成交返回结果反映的是“成交记账完成、风险撤单之前”的订单：累计 4、剩余 6、
	// 部分成交。该结果随后不得被订单撤销改写。
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 20, Qty: 4,
		Filled: 4, Remaining: 6, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 4、剩余 6、部分成交: %+v", res)
	}

	// 资金与持仓：成交价 20×4=80 已结算，现金 820、持仓 14；剩余 6 股占用
	// 6×20=120 被风险撤销全部释放，占用归零、可用等于余额。
	if e.Cash() != 820 || e.Position("A") != 14 || e.Sellable("A") != 14 {
		t.Fatalf("成交入账与剩余撤单后的资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 820 {
		t.Fatalf("剩余 6 股占用的 120 现金必须全部释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}

	// 风险状态：按已生效报价 5 估值，净值 890、亏损 110，已限制增险。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 100 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 890 || st.Loss != 110 {
		t.Fatalf("成交后应触线且按报价 5 估值: %+v", st)
	}

	// 订单查询：总量 10、已成交 4 保留，状态为已撤销、有效剩余量为 0，
	// 并携带风险撤销原因；与成交返回结果（部分成交、剩余 6）并存、互不覆盖。
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Qty != 10 || o.Filled != 4 || o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应保留总量 10、已成交 4，已撤销且有效剩余量为 0: %+v", o)
	}
	if o.Limit != 20 {
		t.Fatalf("订单限价应保留为 20: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 110") || !strings.Contains(o.Reason, "上限 100") {
		t.Fatalf("撤销原因必须按成交后亏损 110 与上限 100 解释，不能用成交前亏损 50: %q", o.Reason)
	}

	// 新增记录恰好 3 条，顺序为：成交 → 当日首次触线 → 剩余量撤销。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（成交、触线、撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：实际成交价 20 单独保存；报价快照是当时已生效的估值价 5
	// （seq2、时刻 2），两者不能混用；剩余 6 取成交记账后、撤单前的口径。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 2 || fr.OrderID != bid || fr.Side != Buy {
		t.Fatalf("第一条应为成交记录: %+v", fr)
	}
	if fr.TradePrice != 20 || fr.Qty != 4 || fr.Filled != 4 || fr.Remaining != 6 {
		t.Fatalf("成交记录应保存成交价 20、本次 4、累计 4、当时剩余 6: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 2 || fr.QuotePrice != 5 {
		t.Fatalf("成交记录的报价定位必须锚定当时已生效价格 5（seq2、时刻 2）: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 890 ||
		fr.RiskLoss != 110 || fr.RiskLimit != 100 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源是该笔成交，关联成交编号 2；估值报价定位取当时已生效的
	// 价格 5 及其序号、时刻；固化亏损 110、上限 100。
	tr2 := newRecs[1]
	if tr2.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", tr2)
	}
	if tr2.RiskTrigger != RiskTriggerTrade || tr2.RiskRefSeq != 2 || tr2.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 2（成交编号），不得挂到报价或其他来源: trigger=%s ref=%d moment=%d",
			tr2.RiskTrigger, tr2.RiskRefSeq, tr2.RiskRefMoment)
	}
	if tr2.RiskDay != 1 || tr2.RiskBaseline != 1000 || tr2.RiskEquity != 890 ||
		tr2.RiskLoss != 110 || tr2.RiskLimit != 100 || !tr2.RiskRestrict {
		t.Fatalf("触线快照必须固化成交后亏损 110 与上限 100: %+v", tr2)
	}
	if len(tr2.RiskQuoteRefs) != 1 ||
		tr2.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 5}) {
		t.Fatalf("触线估值报价定位必须锚定 seq2@时刻2 价格 5: %+v", tr2.RiskQuoteRefs)
	}
	if !strings.Contains(tr2.Reason, "亏损 110") || !strings.Contains(tr2.Reason, "上限 100") {
		t.Fatalf("触线原因必须写明亏损 110 与上限 100: %q", tr2.Reason)
	}

	// 3) 撤销记录：注明取消剩余 6 股，保存成交后亏损 110 与上限 100，
	// 不能使用成交前亏损 50 解释撤单；不带成交价，估值报价仍为 5。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy {
		t.Fatalf("第三条应为该买单的剩余量撤销记录: %+v", cr)
	}
	if cr.Qty != 10 || cr.Filled != 4 || cr.Remaining != 6 {
		t.Fatalf("撤销记录应保留总量 10、已成交 4，并注明本次取消 6 股: %+v", cr)
	}
	if cr.TradePrice != 0 {
		t.Fatalf("撤销记录不得混入成交价 20: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 2 || cr.QuoteMoment != 2 || cr.QuotePrice != 5 {
		t.Fatalf("撤销记录报价定位必须仍是估值价 5（seq2、时刻 2）: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 890 ||
		cr.RiskLoss != 110 || cr.RiskLimit != 100 || !cr.RiskRestrict {
		t.Fatalf("撤销记录必须固化成交后亏损 110 与上限 100（限制已生效）: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "亏损 110") || !strings.Contains(cr.Reason, "上限 100") {
		t.Fatalf("撤销原因必须以亏损 110、上限 100 解释: %q", cr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 限制持续：新买单继续被拒；被撤订单不能再撤销或修改。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制后新买单必须拒绝")
	}
	if err := e.Cancel(bid); err == nil {
		t.Fatal("已被风险保护撤销的订单不能再次撤销")
	}
	if err := e.Modify(bid, 12, 20); err == nil {
		t.Fatal("已被风险保护撤销的订单不能借修改恢复")
	}
}

// TestRiskPartialBuyFillLossExactlyAtLimitTriggers 覆盖边界：同一笔成交使亏损
// 恰好等于上限（110 == 110）时也必须触线并撤销剩余量。
func TestRiskPartialBuyFillLossExactlyAtLimitTriggers(t *testing.T) {
	e, bid := riskTradeSetup(t, 110) // 上限放宽到 110，成交后亏损恰好 110

	res, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 20, Qty: 4})
	if err != nil {
		t.Fatalf("成交必须成功: %v", err)
	}
	if res.Status != StatusPartial || res.Filled != 4 || res.Remaining != 6 {
		t.Fatalf("成交结果应为累计 4、剩余 6、部分成交: %+v", res)
	}

	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 890 || st.Loss != 110 || st.LossLimit != 110 {
		t.Fatalf("亏损恰好等于上限 110 时必须触线: %+v", st)
	}
	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Filled != 4 || o.Remaining() != 0 {
		t.Fatalf("剩余 6 股必须被风险保护撤销: %+v", o)
	}
	if e.Cash() != 820 || e.ReservedCash() != 0 || e.AvailableCash() != 820 || e.Position("A") != 14 {
		t.Fatalf("成交入账且剩余占用释放后的资金持仓错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerTrade || rec.RiskRefSeq != 2 ||
		rec.RiskLoss != 110 || rec.RiskLimit != 110 {
		t.Fatalf("触线记录应关联成交 2 并固化亏损 110、上限 110: %+v", rec)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("恰好达限只能产生一条触线记录")
	}
}

// TestRiskPartialBuyFillBelowLimitKeepsRemainderAndReserve 覆盖未触线路径：
// 成交后亏损仍低于上限时只完成部分成交，剩余买单与现金占用继续有效，
// 不产生触线或撤销记录。
func TestRiskPartialBuyFillBelowLimitKeepsRemainderAndReserve(t *testing.T) {
	e, bid := riskTradeSetup(t, 100)

	// 仅成交 2 股 @20：现金 860、持仓 12，净值 860+60=920、亏损 80 < 100。
	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 20, Qty: 2})
	if err != nil {
		t.Fatalf("成交必须成功: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 20, Qty: 2,
		Filled: 2, Remaining: 8, Status: StatusPartial}) {
		t.Fatalf("成交结果应为累计 2、剩余 8、部分成交: %+v", res)
	}

	st := e.RiskStatus()
	if st.Restricted || st.Equity != 920 || st.Loss != 80 {
		t.Fatalf("亏损 80 未达上限 100，不应触线: %+v", st)
	}

	// 订单仍为部分成交、剩余 8 股有效；剩余占用 8×20=160 继续保留。
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Filled != 2 || o.Qty != 10 || o.Remaining() != 8 || o.Reason != "" {
		t.Fatalf("未触线时剩余买单必须继续有效: %+v", o)
	}
	if e.Cash() != 860 || e.ReservedCash() != 160 || e.AvailableCash() != 700 || e.Position("A") != 12 {
		t.Fatalf("剩余 8 股的现金占用 160 必须继续保留: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 只新增一条成交记录，不得有触线或撤销记录。
	recs := e.Records()[recsBefore:]
	if len(recs) != 1 || recs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", recs)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("未达上限不得产生触线记录")
	}

	// 剩余部分仍可继续成交（占用有效）：再成交 8 股后订单全部成交。
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: bid, Symbol: "A", Side: Buy, Price: 20, Qty: 8})
	if err != nil {
		t.Fatalf("保留的剩余 8 股必须仍可成交: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 10 || res2.Remaining != 0 {
		t.Fatalf("剩余 8 股全部成交后应为全部成交: %+v", res2)
	}
	if o2, _ := e.Order(bid); o2.Status != StatusFilled || e.ReservedCash() != 0 {
		t.Fatalf("全部成交后占用应清零: order=%+v reserved=%d", o2, e.ReservedCash())
	}
}

// TestRiskTriggeredFillReplayReturnsFirstResultUnchanged 覆盖触线撤单后的幂等
// 重放：原样重提交已入账成交必须返回第一次结果（仍为部分成交、剩余 6），
// 现金、持仓、订单与记录均不再变化。
func TestRiskTriggeredFillReplayReturnsFirstResultUnchanged(t *testing.T) {
	e, bid := riskTradeSetup(t, 100)

	tr := Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 20, Qty: 4}
	res1, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("首次成交必须成功: %v", err)
	}

	recordsBefore := len(e.Records())
	cashBefore, reservedBefore := e.Cash(), e.ReservedCash()
	posBefore := e.Position("A")
	orderBefore, _ := e.Order(bid)

	// 触线撤单后原样重放：返回第一次的结果（部分成交、剩余 6），
	// 而不是订单当前的已撤销、剩余 0；两种结果不能相互覆盖。
	res2, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("已入账成交的原样重放必须成功返回原结果: %v", err)
	}
	if res1 != res2 {
		t.Fatalf("重放必须原样返回第一次结果: first=%+v replay=%+v", res1, res2)
	}
	if res2.Status != StatusPartial || res2.Remaining != 6 {
		t.Fatalf("重放结果必须保持首次的部分成交、剩余 6: %+v", res2)
	}

	orderAfter, _ := e.Order(bid)
	if orderAfter != orderBefore {
		t.Fatalf("重放不得改变订单状态: before=%+v after=%+v", orderBefore, orderAfter)
	}
	if orderAfter.Status != StatusCanceled || orderAfter.Remaining() != 0 {
		t.Fatalf("订单查询必须保持已撤销、有效剩余量 0: %+v", orderAfter)
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
}
