package book

import (
	"strings"
	"testing"
)

// 本文件为“同一张买单先有成交、再修改总量与限价、随后再次部分成交，而这笔成交
// 恰好触发日内亏损保护”补充回归保障。订单修改与买入成交触线分别已有保障，这里
// 保护的是两者组合使用时，历史成交、修改后的新资金占用与风险撤单能否按各自的
// 时点对上。
//
// 关键构造（初始现金 1000、报价 10、无持仓时开启亏损上限 14 的交易日）：
//  1. 买单总量 10、限价 10，先以 9 成交 2 股：现金 982、持仓 2，按原成交价记账；
//  2. 改为总量 8、限价 12：累计成交量仍为 2，只有剩余 6 股按新参数占用 6×12=72；
//  3. 报价降到 8：此时仅持 2 股，净值 998、亏损 2，报价变化本身不触线；
//  4. 再以 12（修改后的有效限价）成交 3 股：现金按本次成交价结算为 946、持仓 5，
//     持仓按最新已生效报价 8 估值，净值 986、亏损 14 恰好等于上限，风险保护撤销
//     按新委托参数计算的剩余 3 股，剩余占用全部释放，可用现金等于现金余额。
//
// 必须同时核对：历史成交不被修改后的限价重新计价（982 不被改写）；持仓估值用报价
// 8 而不是成交价 12（净值 986 而非 1006）；成交返回结果定格在风险撤单之前
// （累计 5、剩余 3、部分成交），订单查询则是整次操作后的已撤销（总量 8、限价 12
// 保留，有效剩余量 0）；新增记录依次为成交、当日首次触线（归因于本次成交，保存
// 报价 8 的序号与时刻）、剩余量撤销（注明实际取消 3 股）；修改记录与第一次成交
// 记录保留原内容。相邻的未触线情形（上限 15）只完成部分成交，剩余 3 股与占用
// 36 继续保留。

// modifiedBuyRiskSetup 构造共用前置状态：
// 现金 1000、seq1@(时刻10,价格10)、无持仓开启亏损上限 lossLimit 的交易日；
// 买单 10 股限价 10 先成交 2 股@9（cash=982, pos=2），再修改为总量 8、限价 12
// （剩余 6 股占用 72）；随后 seq2@(时刻20,价格8) 生效，净值 998、亏损 2，尚未触线。
// 返回引擎与该买单编号。
func modifiedBuyRiskSetup(t *testing.T, lossLimit int64) (e *Engine, bid int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}

	bid = mustBuy(t, e, "A", 10, 10) // 占用 10×10=100
	// 第一次成交以原限价 10 以内的价格 9 成交 2 股：按成交价 9 结算。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatalf("第一次成交必须成功: %v", err)
	}
	if e.Cash() != 982 || e.Position("A") != 2 || e.ReservedCash() != 80 {
		t.Fatalf("第一次成交后账目错误: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}

	// 修改为总量 8、限价 12：历史成交不重新计价，现金余额与持仓不变，
	// 只有剩余 6 股改用新限价占用 6×12=72。
	mustModify(t, e, bid, 8, 12)
	if e.Cash() != 982 || e.Position("A") != 2 ||
		e.ReservedCash() != 72 || e.AvailableCash() != 910 {
		t.Fatalf("修改只应替换未成交部分占用: cash=%d pos=%d reserved=%d available=%d",
			e.Cash(), e.Position("A"), e.ReservedCash(), e.AvailableCash())
	}
	if o, _ := e.Order(bid); o.Qty != 8 || o.Limit != 12 || o.Filled != 2 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("修改后订单应为总量 8、限价 12、累计成交 2、剩余 6: %+v", o)
	}

	// 报价降到 8：持仓 2 股市值 16，净值 998、亏损 2，报价变化本身不触发保护。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 8}, 1)
	st := e.RiskStatus()
	if !st.Open || st.Baseline != 1000 || st.Equity != 998 || st.Loss != 2 ||
		st.LossLimit != lossLimit || st.Restricted {
		t.Fatalf("降价后尚未成交时前置风险状态错误: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("降价只造成亏损 2，尚未触线，不应有触线记录")
	}
	return e, bid
}

// TestModifiedBuySecondPartialFillSettlesThenRiskCancelsRemainder 覆盖主场景：
// 修改后的第二次部分成交（3 股@12）先按实际成交价入账，再在同一次 Fill 内把
// 当日亏损做到恰好等于上限 14，随后撤销按新委托参数计算的剩余 3 股。逐笔核对
// 资金、持仓、成交结果、订单查询与全部新增记录，并确认历史成交与修改记录原样保留。
func TestModifiedBuySecondPartialFillSettlesThenRiskCancelsRemainder(t *testing.T) {
	e, bid := modifiedBuyRiskSetup(t, 14)

	recsBefore := len(e.Records()) // 此前依次为：接受、成交(2@9)、修改
	tr := Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3}
	res, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("第二次部分成交必须成功入账: %v", err)
	}

	// 成交返回结果定格在“本次成交记账完成、风险撤单之前”：累计 5、剩余 3、
	// 部分成交；随后同一调用内的撤单不得回填这个结果。
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 12, Qty: 3,
		Filled: 5, Remaining: 3, Status: StatusPartial}) {
		t.Fatalf("成交返回结果应为累计 5、剩余 3、部分成交: %+v", res)
	}

	// 账目：现金按本次实际成交价 12 结算（982−36=946），绝不能用修改后的限价
	// 重算第一次成交（否则现金会是 944）；持仓 5。
	if e.Cash() != 946 || e.Position("A") != 5 || e.Sellable("A") != 5 {
		t.Fatalf("第二次成交入账后的资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	// 剩余 3 股按新限价 12 计算的占用 36 在风险撤单时全部释放，
	// 可用现金等于现金余额。
	if e.ReservedCash() != 0 || e.AvailableCash() != 946 {
		t.Fatalf("风险撤销后剩余占用必须全部释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}

	// 风险状态：持仓按最新已生效报价 8（而不是成交价 12）估值，
	// 净值 946+5×8=986、亏损 1000−986=14 恰好等于上限，已限制增险。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 14 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 986 || st.Loss != 14 {
		t.Fatalf("亏损 14 恰好等于上限 14 时必须触线，且净值按报价 8 估值: %+v", st)
	}

	// 订单查询反映整次操作结束后：已撤销、有效剩余量 0；已成交 5 股保留，
	// 总量 8 与限价 12（修改后的新委托参数）保留，并携带风险撤销原因。
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应为已撤销且有效剩余量为 0: %+v", o)
	}
	if o.Qty != 8 || o.Limit != 12 || o.Filled != 5 {
		t.Fatalf("订单应保留新总量 8、新限价 12 与累计成交 5: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 14") || !strings.Contains(o.Reason, "上限 14") {
		t.Fatalf("撤销原因必须按成交后亏损 14 与上限 14 解释: %q", o.Reason)
	}

	// 本次操作新增记录恰好 3 条，顺序为：成交 → 当日首次触线 → 剩余量撤销。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（成交、触线、撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：本次成交价 12、本次 3 股、累计 5、成交后（撤单前）剩余 3；
	//    报价快照是当时已生效的估值价 8（seq2、时刻 20），成交价与估值价不能混用；
	//    风险快照取自记账后、触线前（尚未限制）。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 2 || fr.OrderID != bid || fr.Side != Buy {
		t.Fatalf("第一条应为成交记录: %+v", fr)
	}
	if fr.TradePrice != 12 || fr.Qty != 3 || fr.Filled != 5 || fr.Remaining != 3 {
		t.Fatalf("成交记录应保存成交价 12、本次 3、累计 5、当时剩余 3: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 20 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定估值价 8（seq2、时刻 20），不能是成交价 12: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 986 ||
		fr.RiskLoss != 14 || fr.RiskLimit != 14 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照（尚未限制）: %+v", fr)
	}

	// 2) 触线记录：来源必须是这笔成交（关联成交编号 2，时刻字段为 0），
	//    不能归因于此前的修改或 seq2 报价变化；估值报价定位保存报价 8 的序号、
	//    时刻与价格。
	tr2 := newRecs[1]
	if tr2.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", tr2)
	}
	if tr2.RiskTrigger != RiskTriggerTrade || tr2.RiskRefSeq != 2 || tr2.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 2（成交编号），不得归因于修改或报价: trigger=%s ref=%d moment=%d",
			tr2.RiskTrigger, tr2.RiskRefSeq, tr2.RiskRefMoment)
	}
	if tr2.RiskDay != 1 || tr2.RiskBaseline != 1000 || tr2.RiskEquity != 986 ||
		tr2.RiskLoss != 14 || tr2.RiskLimit != 14 || !tr2.RiskRestrict {
		t.Fatalf("触线快照必须固化亏损 14 与上限 14: %+v", tr2)
	}
	if len(tr2.RiskQuoteRefs) != 1 ||
		tr2.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 20, Price: 8}) {
		t.Fatalf("触线估值报价定位必须锚定 seq2@时刻20 价格 8: %+v", tr2.RiskQuoteRefs)
	}
	if !strings.Contains(tr2.Reason, "亏损 14") || !strings.Contains(tr2.Reason, "上限 14") {
		t.Fatalf("触线原因必须写明亏损 14 与上限 14: %q", tr2.Reason)
	}

	// 3) 撤销记录：保留修改后的总量 8、限价 12 与累计成交 5，Remaining 表示
	//    本次实际取消 3 股（按新委托参数计算的剩余量）；不带成交价。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy {
		t.Fatalf("第三条应为该买单的剩余量撤销记录: %+v", cr)
	}
	if cr.Limit != 12 || cr.Qty != 8 || cr.Filled != 5 || cr.Remaining != 3 {
		t.Fatalf("撤销记录应保留新限价 12、总量 8、已成交 5，并注明实际取消 3 股: %+v", cr)
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

	// 历史记录原样保留：第一次成交仍按原成交价 9 记账（当时剩余量为 8），
	// 修改记录保存原总量 10、原限价 10、当时已成交 2 与新参数。
	old := recs[:recsBefore]
	if len(old) != 3 {
		t.Fatalf("前置应只有接受、成交、修改 3 条记录，实际 %d", len(old))
	}
	f1 := old[1]
	if f1.Kind != RecordFilled || f1.TradeID != 1 || f1.TradePrice != 9 ||
		f1.Qty != 2 || f1.Filled != 2 || f1.Remaining != 8 {
		t.Fatalf("第一次成交记录必须保留原成交价 9 与当时剩余 8: %+v", f1)
	}
	if !f1.QuoteValid || f1.QuoteSeq != 1 || f1.QuoteMoment != 10 || f1.QuotePrice != 10 {
		t.Fatalf("第一次成交记录的报价定位必须保留为当时的 seq1@10: %+v", f1)
	}
	mr := old[2]
	if mr.Kind != RecordModified || mr.OldLimit != 10 || mr.OldQty != 10 || mr.OldFilled != 2 ||
		mr.Limit != 12 || mr.Qty != 8 || mr.Filled != 2 || mr.Remaining != 6 {
		t.Fatalf("修改记录必须保留修改前后参数与当时已成交量: %+v", mr)
	}
	if !mr.QuoteValid || mr.QuoteSeq != 1 || mr.QuotePrice != 10 {
		t.Fatalf("修改记录应固化修改发生时（降价前）的报价 seq1@10: %+v", mr)
	}

	// 触线后的持续限制：新买单拒绝；已撤销订单不能借修改恢复；
	// 为它再提交尚未入账的成交被拒绝，账实不变（只多一条拒绝记录）。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制后新买单必须拒绝")
	}
	if err := e.Modify(bid, 8, 12); err == nil {
		t.Fatal("已被风险保护撤销的订单不能借修改恢复")
	}
	accountsBefore := len(e.Records())
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 1}); err == nil {
		t.Fatal("已撤销订单的新成交必须拒绝")
	}
	if e.Cash() != 946 || e.Position("A") != 5 || e.ReservedCash() != 0 {
		t.Fatalf("拒绝新成交不得改变账实: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}
	if len(e.Records()) != accountsBefore+1 || e.Records()[accountsBefore].Kind != RecordRejected {
		t.Fatal("被拒成交只能追加一条拒绝记录")
	}

	// 同编号同内容重放本次成交：永远返回风险撤单前的第一次结果（累计 5、剩余 3），
	// 不再次记账、不新增记录、不改账实。
	replay, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("已入账成交的原样重放必须成功返回原结果: %v", err)
	}
	if replay != res {
		t.Fatalf("重放必须原样返回第一次结果: first=%+v replay=%+v", res, replay)
	}
	if len(e.Records()) != accountsBefore+1 {
		t.Fatal("重放不得新增任何记录")
	}
	if e.Cash() != 946 || e.Position("A") != 5 || e.ReservedCash() != 0 {
		t.Fatalf("重放不得改变账实: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}
}

// TestModifiedBuySecondPartialFillBelowLimitKeepsRemainder 覆盖相邻的未触线情形：
// 其余过程完全相同，仅把亏损上限放宽到 15 时，第二次成交造成的亏损 14 不撤单，
// 订单仍为部分成交、剩余 3 股有效，按新限价计算的占用 36 继续保留。
func TestModifiedBuySecondPartialFillBelowLimitKeepsRemainder(t *testing.T) {
	e, bid := modifiedBuyRiskSetup(t, 15)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3})
	if err != nil {
		t.Fatalf("第二次部分成交必须成功: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: bid, Price: 12, Qty: 3,
		Filled: 5, Remaining: 3, Status: StatusPartial}) {
		t.Fatalf("成交结果应为累计 5、剩余 3、部分成交: %+v", res)
	}

	// 亏损 14 低于上限 15，不触线。
	st := e.RiskStatus()
	if st.Restricted || st.Equity != 986 || st.Loss != 14 || st.LossLimit != 15 {
		t.Fatalf("亏损 14 未达上限 15，不应触线: %+v", st)
	}

	// 订单仍为部分成交、剩余 3 股有效；新总量 8、新限价 12 保留。
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Qty != 8 || o.Limit != 12 ||
		o.Filled != 5 || o.Remaining() != 3 || o.Reason != "" {
		t.Fatalf("未触线时剩余 3 股必须继续有效: %+v", o)
	}
	// 剩余 3 股按新限价 12 的占用 36 继续保留，可用现金 946−36=910。
	if e.Cash() != 946 || e.Position("A") != 5 ||
		e.ReservedCash() != 36 || e.AvailableCash() != 910 {
		t.Fatalf("剩余 3 股的占用 36 必须继续保留: cash=%d pos=%d reserved=%d available=%d",
			e.Cash(), e.Position("A"), e.ReservedCash(), e.AvailableCash())
	}

	// 只新增一条成交记录，不得有触线或撤销记录。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", newRecs)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("亏损未达上限不得产生触线记录")
	}

	// 后续成交按修改后的有效新限价 12 判断：13 被拒，12 允许，拒绝不改账实。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: bid, Symbol: "A", Side: Buy, Price: 13, Qty: 1}); err == nil {
		t.Fatal("高于修改后有效限价 12 的成交必须拒绝")
	}
	if e.Cash() != 946 || e.Position("A") != 5 || e.ReservedCash() != 36 {
		t.Fatalf("超限价成交被拒不得改变账实: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 3 {
		t.Fatalf("超限价成交被拒不得改变订单: %+v", o)
	}

	// 剩余 3 股按新限价 12 全部成交后订单全部成交，占用清零，现金 946−36=910。
	res2, err := e.Fill(Trade{TradeID: 4, OrderID: bid, Symbol: "A", Side: Buy, Price: 12, Qty: 3})
	if err != nil {
		t.Fatalf("保留的剩余 3 股必须仍可按新限价成交: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 8 || res2.Remaining != 0 {
		t.Fatalf("剩余 3 股全部成交后应为全部成交: %+v", res2)
	}
	if o, _ := e.Order(bid); o.Status != StatusFilled || o.Qty != 8 || o.Limit != 12 {
		t.Fatalf("全部成交后订单应保留总量 8、限价 12: %+v", o)
	}
	if e.Cash() != 910 || e.Position("A") != 8 || e.ReservedCash() != 0 || e.AvailableCash() != 910 {
		t.Fatalf("全部成交后账目错误: cash=%d pos=%d reserved=%d available=%d",
			e.Cash(), e.Position("A"), e.ReservedCash(), e.AvailableCash())
	}
}
