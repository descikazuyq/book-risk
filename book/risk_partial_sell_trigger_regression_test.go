package book

import (
	"strings"
	"testing"
)

// 本文件为“卖单部分成交入账后才使日内亏损首次达到上限”补充回归保障，
// 防止成交结算与风险撤单相互覆盖。
//
// 关键构造：初始现金 1000，先按价格 10 买入 A 合约 10 单位并成交（现金 900、
// 持仓 10），再开启交易日 1、亏损上限 30（基准净值 1000）。A 最新有效报价
// 降到 8 后净值 980、亏损 20，保护尚未触发；B 已有价格 5 的有效报价、持仓为 0。
// 随后依次接受 A 的 2 单位限价 8 买单（占用 16）、B 的 3 单位限价 5 买单
// （占用 15）与 A 的 6 单位限价 3 卖单（占用 6 单位可卖持仓），两笔买单共占用
// 31 现金。卖单按价格 3 成交 2 单位：成交价只用于现金结算（现金 906、持仓 8），
// 剩余持仓仍按有效报价 8 估值，净值 970、亏损 30 恰好达到上限，账户进入限制。
//
// 必须同时核对：成交先完整成功入账（返回累计成交 2、剩余 4、部分成交），风险撤单
// 在此之后才发生；两笔买单按订单编号从大到小撤销（B 单先、A 单后），31 现金
// 占用归零；已入账卖出保留，卖单仍为部分成交并继续占用剩余 4 单位，A 可卖数量
// 为 4——风险保护既不能撤掉卖单，也不能释放其剩余持仓占用。新增记录顺序为卖出
// 成交、当日首次触线（来源成交、关联本次成交编号；报价定位只有持仓的 A，B 只有
// 未成交买单不进入持仓估值）、两笔买单撤销；撤单原因必须以亏损触线解释，不能写成
// 持仓限额不足。成交价 3（现金结算）与有效报价 8（剩余持仓估值）分别留存。
//
// 边界：同样的部分卖出在亏损上限 31 时只完成结算：卖单剩余占用正确保留，两笔
// 买单与 31 现金占用继续有效，不增加触线或风险撤销记录。

// riskPartialSellSetup 构造共用前置状态并返回引擎与三笔订单编号
// （buyA=A 的 2@8 买单、buyB=B 的 3@5 买单、sellID=A 的 6@3 卖单）。
// 完成时现金 900、两笔买单占用 31、A 持仓 10（其中 6 单位被卖单占用，可卖 4）。
func riskPartialSellSetup(t *testing.T, lossLimit int64) (e *Engine, buyA, buyB, sellID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	first := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: first, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 900 || e.Position("A") != 10 {
		t.Fatalf("前置建仓后应为现金 900、持仓 10: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 8}, 1) // 净值 980，亏损 20
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 5}, 1)
	if st := e.RiskStatus(); st.Baseline != 1000 || st.Equity != 980 || st.Loss != 20 || st.Restricted {
		t.Fatalf("开日与降价后的前置风险状态错误: %+v", st)
	}

	buyA = mustBuy(t, e, "A", 2, 8)
	buyB = mustBuy(t, e, "B", 3, 5)
	sellID = mustSell(t, e, "A", 6, 3)
	if e.Cash() != 900 || e.ReservedCash() != 31 || e.AvailableCash() != 869 {
		t.Fatalf("三笔委托接受后两笔买单应共占用 31 现金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 || e.Sellable("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("卖单接受后只占用可卖持仓、不扣减持仓: posA=%d sellableA=%d posB=%d",
			e.Position("A"), e.Sellable("A"), e.Position("B"))
	}
	return e, buyA, buyB, sellID
}

// TestRiskPartialSellFillSettlesThenCancelsBuys 覆盖主场景：卖单部分成交先成功
// 入账使亏损首次达到上限，风险保护随后只撤销买单，成交结算与卖单剩余占用都保留。
func TestRiskPartialSellFillSettlesThenCancelsBuys(t *testing.T) {
	e, buyA, buyB, sellID := riskPartialSellSetup(t, 30)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sellID, Symbol: "A", Side: Sell, Price: 3, Qty: 2})
	if err != nil {
		t.Fatalf("卖单部分成交必须成功入账: %v", err)
	}

	// 成交返回结果反映“成交记账完成、风险撤单之前”的卖单：累计 2、剩余 4、
	// 部分成交；风险撤单只针对买单，不得改写该结果。
	if res != (FillResult{TradeID: 2, OrderID: sellID, Price: 3, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 2、剩余 4、部分成交: %+v", res)
	}

	// 成交价 3×2=6 只用于现金结算：现金 906；两笔买单 31 占用被风险撤销全部
	// 释放，占用归零、可用等于余额。
	if e.Cash() != 906 || e.ReservedCash() != 0 || e.AvailableCash() != 906 {
		t.Fatalf("卖出入账 6 且买单占用全部释放后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// A 持仓 8（成交价不影响估值口径）；卖单剩余 4 单位继续占用，可卖为 4。
	if e.Position("A") != 8 || e.Sellable("A") != 4 {
		t.Fatalf("卖出成交 2 后持仓应为 8，卖单剩余 4 单位继续占用、可卖 4: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if e.Position("B") != 0 {
		t.Fatalf("B 从未成交，持仓应保持 0: pos=%d", e.Position("B"))
	}

	// 风险状态：剩余持仓按最新有效报价 8 估值（不是成交价 3），
	// 净值 906+8×8=970、亏损 30 恰好达到上限 30，账户进入限制。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 30 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 970 || st.Loss != 30 {
		t.Fatalf("成交后应按有效报价 8 估值并恰好触线: %+v", st)
	}

	// 卖单：总量 6、限价 3、已成交 2 保留，仍为部分成交、有效剩余量 4，
	// 不带任何撤销原因——风险保护不能撤掉卖单或释放其剩余持仓占用。
	so, ok := e.Order(sellID)
	if !ok {
		t.Fatal("卖单必须仍可查询")
	}
	if so.Qty != 6 || so.Limit != 3 || so.Filled != 2 || so.Status != StatusPartial || so.Remaining() != 4 {
		t.Fatalf("卖单应保持部分成交、剩余 4 继续有效: %+v", so)
	}
	if so.Reason != "" {
		t.Fatalf("风险保护不得撤销卖单，卖单不应带撤销原因: %q", so.Reason)
	}

	// 两笔买单均已撤销、有效剩余量为 0，并携带亏损触线的撤销原因
	// （不能写成持仓限额不足）。
	for _, c := range []struct {
		id         int64
		symbol     string
		limit, qty int64
	}{
		{id: buyB, symbol: "B", limit: 5, qty: 3},
		{id: buyA, symbol: "A", limit: 8, qty: 2},
	} {
		o, ok := e.Order(c.id)
		if !ok {
			t.Fatalf("买单 %d 必须仍可查询", c.id)
		}
		if o.Symbol != c.symbol || o.Limit != c.limit || o.Qty != c.qty || o.Filled != 0 ||
			o.Status != StatusCanceled || o.Remaining() != 0 {
			t.Fatalf("买单 %d 应保留原委托参数、已撤销且有效剩余量为 0: %+v", c.id, o)
		}
		if !strings.Contains(o.Reason, "亏损 30") || !strings.Contains(o.Reason, "上限 30") {
			t.Fatalf("买单 %d 撤销原因必须以亏损 30、上限 30 解释: %q", c.id, o.Reason)
		}
		if strings.Contains(o.Reason, "持仓限额") {
			t.Fatalf("本次撤单由亏损触线造成，不能写成持仓限额不足: %q", o.Reason)
		}
	}

	// 新增记录恰好 4 条：卖出成交 → 当日首次触线 → B 买单撤销 → A 买单撤销
	// （按订单编号从大到小：buyB > buyA）。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 4 {
		t.Fatalf("应只新增 4 条记录（成交、触线、两笔撤单），实际 %d 条", len(newRecs))
	}
	kinds := []RecordKind{RecordFilled, RecordRiskTriggered, RecordCanceled, RecordCanceled}
	for i, want := range kinds {
		if newRecs[i].Kind != want {
			t.Fatalf("第 %d 条记录类型应为 %s，实际 %s: %+v", i+1, want, newRecs[i].Kind, newRecs[i])
		}
	}

	// 1) 成交记录：成交价 3 用于结算并单独保存；报价快照是当时已生效的估值价 8
	// （seq2、时刻 2），两者不能混用；剩余 4 取成交记账后、撤单前口径。
	fr := newRecs[0]
	if fr.TradeID != 2 || fr.OrderID != sellID || fr.Side != Sell || fr.Symbol != "A" {
		t.Fatalf("第一条应为卖单成交记录: %+v", fr)
	}
	if fr.TradePrice != 3 || fr.Qty != 2 || fr.Filled != 2 || fr.Remaining != 4 {
		t.Fatalf("成交记录应保存成交价 3、本次 2、累计 2、当时剩余 4: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 2 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录报价定位必须锚定有效报价 8（seq2、时刻 2），不能用成交价 3: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 970 ||
		fr.RiskLoss != 30 || fr.RiskLimit != 30 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源为本次成交、关联成交编号 2；保存基准 1000、净值 970、
	// 亏损与上限 30；估值报价定位只有持仓的 A（seq2、时刻 2、价格 8）——
	// B 只有未成交买单，不进入持仓估值报价。
	rr := newRecs[1]
	if rr.RiskTrigger != RiskTriggerTrade || rr.RiskRefSeq != 2 || rr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 2（成交编号），不得挂到报价或其他来源: trigger=%s ref=%d moment=%d",
			rr.RiskTrigger, rr.RiskRefSeq, rr.RiskRefMoment)
	}
	if rr.RiskDay != 1 || rr.RiskBaseline != 1000 || rr.RiskEquity != 970 ||
		rr.RiskLoss != 30 || rr.RiskLimit != 30 || !rr.RiskRestrict {
		t.Fatalf("触线快照必须固化基准 1000、净值 970、亏损与上限 30: %+v", rr)
	}
	if len(rr.RiskQuoteRefs) != 1 ||
		rr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 8}) {
		t.Fatalf("触线估值报价只能包含持仓合约 A 的 seq2@时刻2 价格 8，B 不得进入: %+v", rr.RiskQuoteRefs)
	}
	if !strings.Contains(rr.Reason, "成交") || !strings.Contains(rr.Reason, "亏损 30") ||
		!strings.Contains(rr.Reason, "上限 30") {
		t.Fatalf("触线原因必须写明由成交引发、亏损 30 与上限 30: %q", rr.Reason)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 3) 第一笔撤销必须是编号更大的 B 买单；其报价快照为 B 的有效报价 5。
	crB := newRecs[2]
	if crB.OrderID != buyB || crB.Symbol != "B" || crB.Side != Buy {
		t.Fatalf("第三笔记录应为编号更大的 B 买单撤销: %+v", crB)
	}
	if crB.Limit != 5 || crB.Qty != 3 || crB.Filled != 0 || crB.Remaining != 3 {
		t.Fatalf("B 买单撤销记录应保存限价 5、总量 3、本次取消 3: %+v", crB)
	}
	if crB.TradePrice != 0 {
		t.Fatalf("买单撤销记录不得混入卖出成交价 3: %+v", crB)
	}
	if !crB.QuoteValid || crB.QuoteSeq != 1 || crB.QuoteMoment != 1 || crB.QuotePrice != 5 {
		t.Fatalf("B 撤单记录报价定位必须锚定 B 的有效报价 5: %+v", crB)
	}
	if crB.RiskDay != 1 || crB.RiskBaseline != 1000 || crB.RiskEquity != 970 ||
		crB.RiskLoss != 30 || crB.RiskLimit != 30 || !crB.RiskRestrict {
		t.Fatalf("B 撤单记录必须固化亏损 30、上限 30（限制已生效）: %+v", crB)
	}
	if !strings.Contains(crB.Reason, "亏损 30") || !strings.Contains(crB.Reason, "上限 30") ||
		strings.Contains(crB.Reason, "持仓限额") {
		t.Fatalf("B 撤单原因必须是亏损触线、不能写成持仓限额不足: %q", crB.Reason)
	}

	// 4) 第二笔撤销为 A 买单；其报价快照为 A 的有效报价 8（同样不是成交价 3）。
	crA := newRecs[3]
	if crA.OrderID != buyA || crA.Symbol != "A" || crA.Side != Buy {
		t.Fatalf("第四笔记录应为 A 买单撤销: %+v", crA)
	}
	if crA.Limit != 8 || crA.Qty != 2 || crA.Filled != 0 || crA.Remaining != 2 {
		t.Fatalf("A 买单撤销记录应保存限价 8、总量 2、本次取消 2: %+v", crA)
	}
	if !crA.QuoteValid || crA.QuoteSeq != 2 || crA.QuoteMoment != 2 || crA.QuotePrice != 8 {
		t.Fatalf("A 撤单记录报价定位必须锚定有效报价 8，不能用成交价 3: %+v", crA)
	}
	if !strings.Contains(crA.Reason, "亏损 30") || !strings.Contains(crA.Reason, "上限 30") ||
		strings.Contains(crA.Reason, "持仓限额") {
		t.Fatalf("A 撤单原因必须是亏损触线、不能写成持仓限额不足: %q", crA.Reason)
	}

	// 卖单剩余 4 单位之后仍可按合法成交价（>= 限价 3）结算：成交 4@3 后订单
	// 全部成交，持仓 4、可卖 4，占用早已为 0 的现金状态不回退。
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: sellID, Symbol: "A", Side: Sell, Price: 3, Qty: 4})
	if err != nil {
		t.Fatalf("卖单剩余部分必须仍能合法成交结算: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 6 || res2.Remaining != 0 {
		t.Fatalf("剩余 4 单位成交后应为全部成交、累计 6: %+v", res2)
	}
	if e.Cash() != 918 || e.ReservedCash() != 0 || e.AvailableCash() != 918 ||
		e.Position("A") != 4 || e.Sellable("A") != 4 {
		t.Fatalf("剩余卖单全部成交后资金持仓错误: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	if so2, _ := e.Order(sellID); so2.Status != StatusFilled || so2.Remaining() != 0 {
		t.Fatalf("卖单查询应为全部成交: %+v", so2)
	}

	// 限制持续到下一交易日：任何合约的新买单仍被拒绝，且不会新增触线记录。
	if _, err := e.Buy("A", 1, 8); err == nil {
		t.Fatal("触线限制后 A 新买单必须拒绝")
	}
	if _, err := e.Buy("B", 1, 5); err == nil {
		t.Fatal("触线限制后 B 新买单必须拒绝")
	}
	if !e.RiskStatus().Restricted || countRiskRecords(e) != 1 {
		t.Fatalf("拒绝新买单不得解除限制或重复触线: %+v", e.RiskStatus())
	}
}

// TestRiskPartialSellFillBelowLimitOnlySettles 覆盖未触线路径：亏损上限为 31 时
// 同样的部分卖出（亏损 30）只完成结算——卖单剩余占用保留，两笔买单与 31 现金
// 占用继续有效，不产生触线或风险撤销记录。
func TestRiskPartialSellFillBelowLimitOnlySettles(t *testing.T) {
	e, buyA, buyB, sellID := riskPartialSellSetup(t, 31)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sellID, Symbol: "A", Side: Sell, Price: 3, Qty: 2})
	if err != nil {
		t.Fatalf("卖单部分成交必须成功入账: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: sellID, Price: 3, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 2、剩余 4、部分成交: %+v", res)
	}

	// 成交价 3 结算现金（906），持仓按有效报价 8 估值：净值 970、亏损 30 < 31。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 970 || st.Loss != 30 || st.LossLimit != 31 {
		t.Fatalf("亏损 30 未达上限 31，不应触线: %+v", st)
	}

	// 卖单仍为部分成交，剩余 4 单位继续占用可卖持仓（持仓 8、可卖 4）。
	so, _ := e.Order(sellID)
	if so.Status != StatusPartial || so.Filled != 2 || so.Qty != 6 || so.Limit != 3 ||
		so.Remaining() != 4 || so.Reason != "" {
		t.Fatalf("未触线时卖单剩余占用必须保留: %+v", so)
	}
	if e.Position("A") != 8 || e.Sellable("A") != 4 {
		t.Fatalf("卖出结算后持仓 8、卖单继续占用 4、可卖 4: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}

	// 两笔买单继续有效，31 现金占用保持不变（可用 875）。
	oa, _ := e.Order(buyA)
	if oa.Status != StatusPending || oa.Filled != 0 || oa.Remaining() != 2 {
		t.Fatalf("A 买单必须保持待成交、剩余 2: %+v", oa)
	}
	ob, _ := e.Order(buyB)
	if ob.Status != StatusPending || ob.Filled != 0 || ob.Remaining() != 3 {
		t.Fatalf("B 买单必须保持待成交、剩余 3: %+v", ob)
	}
	if e.Cash() != 906 || e.ReservedCash() != 31 || e.AvailableCash() != 875 {
		t.Fatalf("两笔买单的 31 现金占用必须继续保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}

	// 只新增一条成交记录，不得有触线或撤销记录。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", newRecs)
	}
	if newRecs[0].TradePrice != 3 || !newRecs[0].QuoteValid || newRecs[0].QuotePrice != 8 {
		t.Fatalf("成交记录仍须区分成交价 3（结算）与有效报价 8（估值）: %+v", newRecs[0])
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("未达上限不得产生触线记录")
	}

	// 未触发限制时新买单仍可接受（现金允许范围内），与上限 30 场景形成对照。
	if _, err := e.Buy("A", 1, 8); err != nil {
		t.Fatalf("亏损 30 未达上限 31 时新买单不应被拒绝: %v", err)
	}
	if e.RiskStatus().Restricted || countRiskRecords(e) != 0 {
		t.Fatalf("接受新买单不得触发风险保护: %+v", e.RiskStatus())
	}
}
