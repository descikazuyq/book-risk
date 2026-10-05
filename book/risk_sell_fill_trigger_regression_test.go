package book

import (
	"strings"
	"testing"
)

// 本文件为“卖单部分成交入账后才使日内亏损首次达到上限”补充回归保障：
// 成交结算与随后的风险撤单必须各司其职、互不覆盖。
//
// 关键构造：初始现金 1000，先按价格 10 买入并持有 A 10 股（现金 900），以净值
// 1000 开启交易日 1、亏损上限 30；A 最新有效报价降到 8 时净值 980、亏损 20，
// 保护尚未触发。随后依次接受 A 的 2 股限价 8 买单、B 的 3 股限价 5 买单（B 已有
// 价格 5 的有效报价且无持仓）以及 A 的 6 股限价 3 卖单，两笔买单共占用 31 现金。
// 卖单按价格 3 成交 2 股：现金 906、A 持仓 8，按报价 8 估值净值 970、亏损 30
// 恰好达到上限，账户进入限制状态。
//
// 必须同时核对：成交先完整入账（成交返回部分成交、累计 2、剩余 4），随后风险保护
// 按订单编号从大到小只撤两笔买单（现金占用归零、可用 906），已入账的卖出保留，
// 卖单仍为部分成交并继续占用 4 股可卖数量（可卖 4），其剩余部分之后仍可成交；
// 新买单仍被拒绝。新增记录顺序为成交 → 当日首次触线（来源成交、关联成交编号、
// 估值锚定 A 的报价 8，B 无持仓不进入报价定位）→ 两笔买单撤销；撤单原因与风险
// 快照都按亏损触线解释，不能写成持仓限额不足。
//
// 边界：同一部分卖出在亏损上限 31 时只完成结算，卖单剩余占用正确保留，两笔买单
// 与 31 现金占用保持有效，不增加触线或风险撤销记录。成交价 3 只用于现金结算，
// 剩余持仓估值始终用有效报价 8，两者不能混用。

// riskSellSetup 构造共用前置状态：
// 初始现金 1000，A 10 股 @10 已成交入账（cash=900, pos=10, seq1@时刻1 价格10）；
// B 已设置持仓限额并有有效报价 seq1@时刻1 价格 5，无持仓；
// 以净值 1000 开启交易日 1、亏损上限 lossLimit；A 的 seq2@时刻2 价格 8 生效后
// 净值 980、亏损 20，保护尚未触发。
// 再依次接受 A 的 2 股限价 8 买单 bidA、B 的 3 股限价 5 买单 bidB（共占用 31 现金）
// 与 A 的 6 股限价 3 卖单 ask（占用 6 股可卖数量）。
// 返回引擎与三笔订单编号；除显式说明外调用前保护尚未触线。
func riskSellSetup(t *testing.T, lossLimit int64) (e *Engine, bidA, bidB, ask int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 5}, 1)
	first := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: first, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 8}, 1) // 净值 980，亏损 20
	st := e.RiskStatus()
	if st.Baseline != 1000 || st.Equity != 980 || st.Loss != 20 || st.Restricted {
		t.Fatalf("开日与降价后的前置风险状态错误: %+v", st)
	}
	bidA = mustBuy(t, e, "A", 2, 8)
	bidB = mustBuy(t, e, "B", 3, 5)
	ask = mustSell(t, e, "A", 6, 3)
	if e.Cash() != 900 || e.ReservedCash() != 31 || e.AvailableCash() != 869 {
		t.Fatalf("两笔买单应共占用 31 现金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 || e.Sellable("A") != 4 {
		t.Fatalf("卖单应占用 6 股可卖数量: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	return e, bidA, bidB, ask
}

// TestRiskPartialSellFillSettlesThenRiskCancelKeepsSellOrder 覆盖主场景：卖单部分
// 成交先成功入账并使亏损首次达到上限，随后风险保护只撤销两笔买单；卖单及其剩余
// 占用保留，成交结算与风险撤单互不覆盖。
func TestRiskPartialSellFillSettlesThenRiskCancelKeepsSellOrder(t *testing.T) {
	e, bidA, bidB, ask := riskSellSetup(t, 30)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: 3, Qty: 2})
	if err != nil {
		t.Fatalf("卖单部分成交必须成功入账: %v", err)
	}

	// 成交返回结果反映“成交记账完成、风险撤单之前”的卖单：累计 2、剩余 4、
	// 部分成交。该结果随后不得被风险撤单改写。
	if res != (FillResult{TradeID: 2, OrderID: ask, Price: 3, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 2、剩余 4、部分成交: %+v", res)
	}

	// 资金与持仓：卖出所得 3×2=6 已入账，现金 906、A 持仓 8；两笔买单被风险撤销，
	// 31 现金占用全部释放，可用现金等于余额 906；已入账的卖出保留。
	if e.Cash() != 906 || e.Position("A") != 8 {
		t.Fatalf("成交入账与风险撤单后的资金持仓错误: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 906 {
		t.Fatalf("两笔买单的 31 现金占用必须全部释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}
	// 卖单未被风险保护撤销：仍为部分成交，继续占用 4 股，可卖数量为 8-4=4。
	if e.Sellable("A") != 4 {
		t.Fatalf("卖单剩余 4 股占用必须保留，可卖数量应为 4: sellable=%d", e.Sellable("A"))
	}

	// 风险状态：成交价 3 只用于现金结算，剩余持仓按有效报价 8 估值，
	// 净值 906+8×8=970、亏损 30 达到上限，已限制增险。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 30 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 970 || st.Loss != 30 {
		t.Fatalf("成交后应触线且按报价 8 估值: %+v", st)
	}

	// 卖单查询：总量 6、已成交 2、剩余 4 保留，仍为部分成交、无撤销原因；
	// 与两笔买单的已撤销状态并存、互不覆盖。
	o, ok := e.Order(ask)
	if !ok {
		t.Fatal("卖单必须仍可查询")
	}
	if o.Qty != 6 || o.Filled != 2 || o.Status != StatusPartial || o.Remaining() != 4 ||
		o.Limit != 3 || o.Reason != "" {
		t.Fatalf("卖单应保持部分成交并保留剩余 4 股占用，不得被风险保护撤销: %+v", o)
	}

	// 两笔买单均被撤销、剩余量归零，撤销原因按亏损触线解释，不得写成持仓限额不足。
	for _, id := range []int64{bidA, bidB} {
		bo, _ := e.Order(id)
		if bo.Status != StatusCanceled || bo.Remaining() != 0 {
			t.Fatalf("买单 %d 必须被风险保护撤销: %+v", id, bo)
		}
		if !strings.Contains(bo.Reason, "亏损 30") || !strings.Contains(bo.Reason, "上限 30") {
			t.Fatalf("买单 %d 的撤销原因必须按亏损 30、上限 30 解释: %q", id, bo.Reason)
		}
		if strings.Contains(bo.Reason, "持仓限额") {
			t.Fatalf("买单 %d 的撤销原因不得写成持仓限额不足: %q", id, bo.Reason)
		}
	}

	// 新增记录恰好 4 条，顺序为：成交 → 当日首次触线 → 撤销 bidB（编号大）→ 撤销 bidA。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 4 {
		t.Fatalf("应只新增 4 条记录（成交、触线、两笔撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：成交价 3 单独保存；报价快照是当时已生效的估值价 8
	// （seq2、时刻 2），两者不能混用；剩余 4 取成交记账后、撤单前的口径。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 2 || fr.OrderID != ask || fr.Side != Sell {
		t.Fatalf("第一条应为卖出成交记录: %+v", fr)
	}
	if fr.TradePrice != 3 || fr.Qty != 2 || fr.Filled != 2 || fr.Remaining != 4 {
		t.Fatalf("成交记录应保存成交价 3、本次 2、累计 2、当时剩余 4: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 2 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定估值价 8（seq2、时刻 2），不得混入成交价 3: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 970 ||
		fr.RiskLoss != 30 || fr.RiskLimit != 30 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源是该笔卖出成交，关联成交编号 2；固化基准 1000、净值 970、
	// 亏损与上限 30；估值报价定位只含当时持仓的 A（seq2、时刻 2、价格 8），
	// B 只有未成交买单、无持仓，不进入持仓估值报价。
	tr := newRecs[1]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", tr)
	}
	if tr.RiskTrigger != RiskTriggerTrade || tr.RiskRefSeq != 2 || tr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 2（成交编号），不得挂到报价或其他来源: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 1000 || tr.RiskEquity != 970 ||
		tr.RiskLoss != 30 || tr.RiskLimit != 30 || !tr.RiskRestrict {
		t.Fatalf("触线快照必须固化基准 1000、净值 970、亏损与上限 30: %+v", tr)
	}
	if len(tr.RiskQuoteRefs) != 1 ||
		tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 8}) {
		t.Fatalf("触线估值报价定位必须只含 A 的 seq2@时刻2 价格 8（B 无持仓不参与）: %+v", tr.RiskQuoteRefs)
	}
	if !strings.Contains(tr.Reason, "亏损 30") || !strings.Contains(tr.Reason, "上限 30") {
		t.Fatalf("触线原因必须写明亏损 30 与上限 30: %q", tr.Reason)
	}

	// 3) 撤销记录：按订单编号从大到小，先 bidB 后 bidA；均固化亏损 30 与上限 30，
	// 原因说明风险保护撤单，不得写成持仓限额不足。
	cb := newRecs[2]
	if cb.Kind != RecordCanceled || cb.OrderID != bidB || cb.Symbol != "B" || cb.Side != Buy {
		t.Fatalf("第三条应为先撤编号更大的 B 买单: %+v", cb)
	}
	if cb.Qty != 3 || cb.Filled != 0 || cb.Remaining != 3 {
		t.Fatalf("B 买单撤销记录应注明取消剩余 3 股: %+v", cb)
	}
	if !cb.QuoteValid || cb.QuoteSeq != 1 || cb.QuoteMoment != 1 || cb.QuotePrice != 5 {
		t.Fatalf("B 买单撤销记录报价定位应为 B 的 seq1@时刻1 价格 5: %+v", cb)
	}
	ca := newRecs[3]
	if ca.Kind != RecordCanceled || ca.OrderID != bidA || ca.Symbol != "A" || ca.Side != Buy {
		t.Fatalf("第四条应为后撤编号较小的 A 买单: %+v", ca)
	}
	if ca.Qty != 2 || ca.Filled != 0 || ca.Remaining != 2 {
		t.Fatalf("A 买单撤销记录应注明取消剩余 2 股: %+v", ca)
	}
	if !ca.QuoteValid || ca.QuoteSeq != 2 || ca.QuoteMoment != 2 || ca.QuotePrice != 8 {
		t.Fatalf("A 买单撤销记录报价定位应为 A 的 seq2@时刻2 价格 8: %+v", ca)
	}
	for i, cr := range []Record{cb, ca} {
		if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 970 ||
			cr.RiskLoss != 30 || cr.RiskLimit != 30 || !cr.RiskRestrict {
			t.Fatalf("撤单记录 %d 必须固化触线时的亏损 30 与上限 30: %+v", i, cr)
		}
		if !strings.Contains(cr.Reason, "亏损 30") || !strings.Contains(cr.Reason, "上限 30") {
			t.Fatalf("撤单记录 %d 的原因必须以亏损 30、上限 30 解释: %q", i, cr.Reason)
		}
		if strings.Contains(cr.Reason, "持仓限额") {
			t.Fatalf("撤单记录 %d 的原因不得写成持仓限额不足: %q", i, cr.Reason)
		}
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 限制持续：两合约的新买单都被拒绝。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制后 A 的新买单必须拒绝")
	}
	if _, err := e.Buy("B", 1, 5); err == nil {
		t.Fatal("触线限制后 B 的新买单必须拒绝")
	}

	// 卖单剩余 4 股之后仍可按合法成交价（不低于限价 3）结算：按价格 4 成交，
	// 现金结算用成交价 4（906+16=922），剩余持仓估值仍用报价 8。
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: ask, Symbol: "A", Side: Sell, Price: 4, Qty: 4})
	if err != nil {
		t.Fatalf("卖单保留的剩余 4 股必须仍可成交: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 6 || res2.Remaining != 0 {
		t.Fatalf("剩余 4 股成交后卖单应全部成交: %+v", res2)
	}
	if e.Cash() != 922 || e.Position("A") != 4 || e.Sellable("A") != 4 {
		t.Fatalf("剩余成交应按价格 4 结算现金: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if st := e.RiskStatus(); !st.Restricted || st.Equity != 954 || st.Loss != 46 {
		t.Fatalf("剩余持仓仍按报价 8 估值且限制不解除: %+v", st)
	}
	if o2, _ := e.Order(ask); o2.Status != StatusFilled || o2.Filled != 6 {
		t.Fatalf("卖单应全部成交: %+v", o2)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("已限制后卖单剩余成交不得重复产生触线记录")
	}
}

// TestRiskPartialSellFillBelowLimitKeepsOrdersAndReserve 覆盖未触线路径：同样的
// 部分卖出在亏损上限 31 时只完成结算（亏损 30 < 31），卖单剩余占用正确保留，
// 两笔买单与 31 现金占用保持有效，不增加触线或风险撤销记录。
func TestRiskPartialSellFillBelowLimitKeepsOrdersAndReserve(t *testing.T) {
	e, bidA, bidB, ask := riskSellSetup(t, 31)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: 3, Qty: 2})
	if err != nil {
		t.Fatalf("卖单部分成交必须成功: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: ask, Price: 3, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("成交结果应为累计 2、剩余 4、部分成交: %+v", res)
	}

	// 净值 970、亏损 30 低于上限 31，不触线。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 970 || st.Loss != 30 || st.LossLimit != 31 {
		t.Fatalf("亏损 30 未达上限 31，不应触线: %+v", st)
	}

	// 卖单剩余 4 股占用保留；两笔买单与 31 现金占用继续有效。
	if e.Cash() != 906 || e.ReservedCash() != 31 || e.AvailableCash() != 875 {
		t.Fatalf("两笔买单的 31 现金占用必须继续保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 8 || e.Sellable("A") != 4 {
		t.Fatalf("卖单剩余 4 股占用必须保留: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(ask); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("卖单应保持部分成交、剩余 4: %+v", o)
	}
	for _, id := range []int64{bidA, bidB} {
		if bo, _ := e.Order(id); bo.Status != StatusPending || bo.Remaining() != bo.Qty {
			t.Fatalf("未触线时买单 %d 必须继续有效: %+v", id, bo)
		}
	}

	// 只新增一条成交记录，不得有触线或撤销记录。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", newRecs)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("未达上限不得产生触线记录")
	}

	// 未触线时新买单仍可接受（额度与现金充足）。
	if _, err := e.Buy("B", 1, 5); err != nil {
		t.Fatalf("未触线时新买单必须仍可接受: %v", err)
	}
}
