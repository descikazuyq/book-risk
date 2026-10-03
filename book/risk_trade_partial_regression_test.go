package book

import (
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方提交成交时，一笔部分买入成交本身
// 就可能使日内亏损达到上限。这种情况下成交必须先成功入账，风险保护随后只撤销
// 该订单尚未成交的部分；调用方既要能从成交返回确认成交成功，又要能从订单查询
// 看清剩余量为何取消，两种结果不能相互覆盖。
//
// 可核对账目的主场景：
// 初始现金 1000，先以价格 10 买入并持有 10 份（现金 900、持仓 10），以净值
// 1000 开启交易日（日号 1，亏损上限 100）；最新有效报价 seq2（时刻 2）降到 5
// 时净值 950、亏损 50，尚未限制买入。随后接受总量 10、限价 20 的买单（占用
// 200 现金），再提交价格 20、数量 4 的成交：
//   - 成交成功：现金 900-80=820，持仓 10+4=14，成交结果为累计成交 4、剩余 6、
//     部分成交；
//   - 成交后按已生效报价 5 估值：820 + 14×5 = 890，亏损 1000-890=110，达到
//     上限 100，由该笔成交触发当日首次触线；
//   - 风险保护撤销订单剩余 6 份，剩余部分占用的 6×20=120 现金全部释放，
//     占用归零、可用现金 820。
//
// 事件顺序必须是：先保存成交，再记录当日首次风险触线（来源为该笔成交、关联
// 成交编号，估值报价定位锚定当时已生效的 seq2@时刻2 价格 5），最后记录剩余量
// 撤销（取消 6 份，并保存成交后的亏损 110 与上限 100，不能用成交前亏损 50
// 解释撤单）；成交实际价格 20 与估值价格 5 在成交记录上各自保留，不能混用。
//
// 边界：同一笔成交使亏损恰好等于上限时同样触线；成交后亏损仍低于上限时只完成
// 部分成交，剩余买单与现金占用继续有效，不产生触线或撤销记录。触线撤单后原样
// 重提交已入账成交，必须返回第一次的结果，现金、持仓与记录均不再变化。

// riskTradePartialSetup 构造主场景共用前置：
// cash=900、持仓 A=10、报价 seq1@10；开日（日号 1，基准 1000）后报价 seq2@5，
// 净值 950、亏损 50，尚未限制买入；再接受总量 10、限价 20 的买单（编号 2，
// 占用 200 现金）。返回引擎与买单编号。
func riskTradePartialSetup(t *testing.T, lossLimit int64) (e *Engine, buyID int64) {
	t.Helper()
	e = setupHolding(t, 10) // cash=900, pos A=10, quote seq1@10
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 5}, 1) // 净值 950，亏损 50
	if st := e.RiskStatus(); st.Restricted || st.Equity != 950 || st.Loss != 50 {
		t.Fatalf("报价跌到 5 后亏损 50 不应触线: %+v", st)
	}
	buyID = mustBuy(t, e, "A", 10, 20) // 总量 10、限价 20，占用 200
	if e.Cash() != 900 || e.ReservedCash() != 200 || e.AvailableCash() != 700 {
		t.Fatalf("买单接受后资金占用错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	return e, buyID
}

// TestRiskTradeTriggerPartialFillSettlesThenCancelsRemaining 覆盖主场景：
// 部分买入成交先入账并返回部分成交结果，成交致亏损 110 超过上限 100 后风险
// 保护撤销剩余 6 份；资金、持仓、订单状态与事件记录逐项可核对。
func TestRiskTradeTriggerPartialFillSettlesThenCancelsRemaining(t *testing.T) {
	e, buyID := riskTradePartialSetup(t, 100)

	tr := Trade{TradeID: 2, OrderID: buyID, Symbol: "A", Side: Buy, Price: 20, Qty: 4}
	recordsBefore := len(e.Records())
	res, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("致亏成交必须先成功入账: %v", err)
	}

	// 成交返回结果锚定成交发生的那一刻：累计成交 4、剩余 6、部分成交。
	wantRes := FillResult{TradeID: 2, OrderID: buyID, Price: 20, Qty: 4,
		Filled: 4, Remaining: 6, Status: StatusPartial}
	if res != wantRes {
		t.Fatalf("成交返回结果错误: %+v，期望 %+v", res, wantRes)
	}

	// 账目：成交金额 80 已扣，剩余 6 份占用的 120 现金被撤单全部释放。
	if e.Cash() != 820 || e.ReservedCash() != 0 || e.AvailableCash() != 820 {
		t.Fatalf("触线撤单后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 14 {
		t.Fatalf("成交入账后持仓应为 14，实际 %d", e.Position("A"))
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 890 ||
		st.Loss != 110 || st.LossLimit != 100 || !st.Restricted {
		t.Fatalf("成交后净值 890、亏损 110 必须触线: %+v", st)
	}

	// 订单查询保留总量 10、已成交 4 与风险撤销原因，有效剩余量为零。
	ord, ok := e.Order(buyID)
	if !ok {
		t.Fatalf("订单 %d 必须仍可查询", buyID)
	}
	if ord.Qty != 10 || ord.Filled != 4 || ord.Status != StatusCanceled || ord.Remaining() != 0 {
		t.Fatalf("订单查询应保留总量 10、已成交 4、撤销态且有效剩余为零: %+v", ord)
	}
	if !strings.Contains(ord.Reason, "亏损 110") || !strings.Contains(ord.Reason, "上限 100") {
		t.Fatalf("订单必须保留风险撤销原因（亏损 110、上限 100）: %q", ord.Reason)
	}

	// 成交结果与订单查询是两个视角，不能相互覆盖：成交那一刻是部分成交、剩余 6，
	// 随后保护把订单剩余量取消，查询必须反映撤销态与零有效剩余。
	if res.Status != StatusPartial || ord.Status != StatusCanceled {
		t.Fatalf("成交结果须为部分成交而订单须为已撤销: res.Status=%s ord.Status=%s",
			res.Status, ord.Status)
	}
	if res.Remaining != 6 || ord.Remaining() != 0 || res.Filled != 4 || ord.Filled != 4 {
		t.Fatalf("成交结果剩余 6 与订单有效剩余 0 必须并存: res=%+v ord=%+v", res, ord)
	}

	// 本操作恰好新增三条记录：成交 → 当日首次触线 → 剩余量撤销。
	newRecs := e.Records()[recordsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应新增成交、触线、撤销共 3 条记录，实际 %d 条: %+v", len(newRecs), newRecs)
	}

	// 第一条：成交。成交实际价格 20（TradePrice）与当时估值价格 5（报价快照）
	// 各自保留；风险快照取自成交已入账、保护尚未执行的瞬间（亏损 110、未限制）。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 2 || fr.OrderID != buyID ||
		fr.Qty != 4 || fr.Filled != 4 || fr.Remaining != 6 {
		t.Fatalf("成交记录要素错误: %+v", fr)
	}
	if fr.TradePrice != 20 {
		t.Fatalf("成交实际价格必须保留为 20: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 2 || fr.QuotePrice != 5 {
		t.Fatalf("成交记录估值报价必须锚定 seq2@时刻2 价格 5: %+v", fr)
	}
	if fr.TradePrice == fr.QuotePrice {
		t.Fatalf("成交实际价格 20 与估值价格 5 不能混用: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 890 ||
		fr.RiskLoss != 110 || fr.RiskLimit != 100 || fr.RiskRestrict {
		t.Fatalf("成交记录风险快照应反映成交后触线前的瞬间: %+v", fr)
	}

	// 第二条：当日首次风险触线。来源为该笔成交、关联成交编号 2；估值报价定位
	// 取当时已生效的 seq2@时刻2 价格 5。
	trig := newRecs[1]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第二条必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerTrade || trig.RiskRefSeq != 2 || trig.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交并关联成交编号 2: trigger=%s ref=%d moment=%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if !strings.Contains(trig.Reason, trig.RiskTrigger.String()) {
		t.Fatalf("触线原因应注明由成交引发: %q", trig.Reason)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1000 || trig.RiskEquity != 890 ||
		trig.RiskLoss != 110 || trig.RiskLimit != 100 || !trig.RiskRestrict {
		t.Fatalf("触线记录风险快照错误: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 5}) {
		t.Fatalf("触线估值报价定位必须锚定 seq2@时刻2 价格 5: %+v", trig.RiskQuoteRefs)
	}

	// 第三条：剩余量撤销。取消 6 份，保存成交后的亏损 110 与上限 100，
	// 不能使用成交前亏损 50。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != buyID || cr.Side != Buy ||
		cr.Qty != 10 || cr.Filled != 4 || cr.Remaining != 6 {
		t.Fatalf("撤销记录必须注明取消剩余 6 份并保留总量 10、已成交 4: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 890 ||
		cr.RiskLoss != 110 || cr.RiskLimit != 100 || !cr.RiskRestrict {
		t.Fatalf("撤销记录必须固化成交后的亏损 110 与上限 100: %+v", cr)
	}
	if cr.RiskLoss == 50 {
		t.Fatalf("撤单不得用成交前亏损 50 解释: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 2 || cr.QuoteMoment != 2 || cr.QuotePrice != 5 {
		t.Fatalf("撤销记录报价快照应锚定 seq2@时刻2 价格 5: %+v", cr)
	}
	if !strings.Contains(cr.Reason, "亏损 110") || !strings.Contains(cr.Reason, "上限 100") {
		t.Fatalf("撤销原因必须注明亏损 110 达到上限 100: %q", cr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 触线撤单后原样重提交已入账成交：返回第一次结果，现金、持仓与记录均不再变化。
	res2, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("已入账成交幂等重放不应报错: %v", err)
	}
	if res2 != res {
		t.Fatalf("幂等重放必须返回第一次结果: %+v，第一次 %+v", res2, res)
	}
	if len(e.Records()) != recordsBefore+3 {
		t.Fatalf("幂等重放不得新增任何记录，实际 %d 条", len(e.Records())-recordsBefore)
	}
	if e.Cash() != 820 || e.ReservedCash() != 0 || e.AvailableCash() != 820 ||
		e.Position("A") != 14 {
		t.Fatalf("幂等重放不得改变资金与持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if ord2, _ := e.Order(buyID); ord2.Status != StatusCanceled || ord2.Filled != 4 ||
		ord2.Remaining() != 0 || ord2.Reason != ord.Reason {
		t.Fatalf("幂等重放不得改变订单状态与撤销原因: %+v", ord2)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("幂等重放不得新增触线记录")
	}
}

// TestRiskTradeTriggerAtExactLimitCancelsRemaining 覆盖边界：同一笔部分成交使
// 亏损恰好等于上限（110 == 110）时同样触线并撤销剩余量。
func TestRiskTradeTriggerAtExactLimitCancelsRemaining(t *testing.T) {
	e, buyID := riskTradePartialSetup(t, 110) // 上限 110：报价 5 时亏损 50 未触线

	res, err := e.Fill(Trade{TradeID: 2, OrderID: buyID, Symbol: "A", Side: Buy, Price: 20, Qty: 4})
	if err != nil {
		t.Fatalf("成交必须先成功入账: %v", err)
	}
	if res.Status != StatusPartial || res.Filled != 4 || res.Remaining != 6 {
		t.Fatalf("成交结果应为部分成交、累计 4、剩余 6: %+v", res)
	}

	// 亏损 110 恰好等于上限 110：达到即触线（>= 语义）。
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 890 || st.Loss != 110 || st.LossLimit != 110 {
		t.Fatalf("亏损恰好等于上限也必须触线: %+v", st)
	}
	ord, _ := e.Order(buyID)
	if ord.Status != StatusCanceled || ord.Qty != 10 || ord.Filled != 4 || ord.Remaining() != 0 {
		t.Fatalf("剩余 6 份必须被风险保护撤销: %+v", ord)
	}
	if !strings.Contains(ord.Reason, "亏损 110") || !strings.Contains(ord.Reason, "上限 110") {
		t.Fatalf("撤销原因应固化亏损 110 与上限 110: %q", ord.Reason)
	}
	if e.Cash() != 820 || e.ReservedCash() != 0 || e.AvailableCash() != 820 ||
		e.Position("A") != 14 {
		t.Fatalf("触线撤单后账目错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	recs := e.Records()
	if len(recs) < 3 {
		t.Fatalf("应留下成交、触线、撤销三条记录，实际 %d 条", len(recs))
	}
	trig, cr := recs[len(recs)-2], recs[len(recs)-1]
	if trig.Kind != RecordRiskTriggered || trig.RiskTrigger != RiskTriggerTrade ||
		trig.RiskRefSeq != 2 || trig.RiskLoss != 110 || trig.RiskLimit != 110 {
		t.Fatalf("恰好达限的触线记录错误: %+v", trig)
	}
	if cr.Kind != RecordCanceled || cr.Remaining != 6 || cr.RiskLoss != 110 || cr.RiskLimit != 110 {
		t.Fatalf("恰好达限的撤销记录错误: %+v", cr)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}
}

// TestRiskPartialFillBelowLimitKeepsRemainingBuyAndReservation 覆盖反向边界：
// 成交后亏损 110 仍低于上限 120 时，只完成部分成交，剩余买单与现金占用继续
// 有效，不产生触线或撤销记录。
func TestRiskPartialFillBelowLimitKeepsRemainingBuyAndReservation(t *testing.T) {
	e, buyID := riskTradePartialSetup(t, 120) // 上限 120：成交后亏损 110 仍低于上限

	recordsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: buyID, Symbol: "A", Side: Buy, Price: 20, Qty: 4})
	if err != nil {
		t.Fatalf("未达上限时成交必须成功: %v", err)
	}
	if res.Status != StatusPartial || res.Filled != 4 || res.Remaining != 6 {
		t.Fatalf("应只完成部分成交、剩余 6 份继续有效: %+v", res)
	}

	// 亏损 110 < 上限 120：不触线，不限制。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 890 || st.Loss != 110 || st.LossLimit != 120 {
		t.Fatalf("亏损低于上限时不得触线: %+v", st)
	}

	// 订单保持部分成交，总量 10、已成交 4、有效剩余 6，无撤销原因。
	ord, _ := e.Order(buyID)
	if ord.Status != StatusPartial || ord.Qty != 10 || ord.Filled != 4 ||
		ord.Remaining() != 6 || ord.Reason != "" {
		t.Fatalf("剩余买单必须继续有效: %+v", ord)
	}
	// 剩余 6 份 × 限价 20 = 120 现金占用继续保留；可用现金 820-120=700。
	if e.Cash() != 820 || e.ReservedCash() != 120 || e.AvailableCash() != 700 {
		t.Fatalf("剩余买单现金占用必须继续有效: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 14 {
		t.Fatalf("成交入账后持仓应为 14，实际 %d", e.Position("A"))
	}

	// 本次操作只新增一条成交记录，不产生触线或撤销记录。
	newRecs := e.Records()[recordsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("低于上限时只能新增一条成交记录: %+v", newRecs)
	}
	if newRecs[0].TradePrice != 20 || newRecs[0].Remaining != 6 {
		t.Fatalf("成交记录要素错误: %+v", newRecs[0])
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("亏损低于上限不得产生触线记录")
	}
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled {
			t.Fatalf("亏损低于上限不得产生撤销记录: %+v", r)
		}
	}
}
