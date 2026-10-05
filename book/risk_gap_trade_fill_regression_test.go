package book

import (
	"strings"
	"testing"
)

// 本文件为“行情缺口与日内亏损保护同时存在”时，已接受买单的部分成交补充
// 回归保障。
//
// 关键构造：初始现金 1000，A 合约已设足够的持仓限额，最新已生效报价为
// seq1@时刻10@价格8；接受总量 10、限价 10 的买单（占用现金 100），再以
// 基准净值 1000 开启亏损上限为 lossLimit 的交易日。随后 seq3@时刻30@价格20
// 因缺少 seq2 只进入等待：最新报价与风险状态都不能改变，缺口期间申请新买单
// 仍按既有规则拒绝；但为已接受买单提交价格 10、数量 5 的合法成交必须成功
// 入账，不能因缺口被拒绝。
//
// 成交后的净值只能使用最新已生效报价 seq1@8：现金 950 + 持仓 5×8 = 990，
// 日内亏损 10。若错误地提前使用等待中的价格 20，净值将是 1050（浮盈、亏损
// 记零），本文件的触线断言正是对这条口径的固定。成交本身不能消耗等待中的
// seq3：成交处理结束后最新报价仍为 seq1，缺口仍在。
//
// 边界：亏损上限为 10 时亏损恰好达限，立即触发保护并撤销剩余 5 股，成交
// 返回仍定格在部分成交（累计 5、剩余 5），订单查询则为已撤销、有效剩余 0；
// 亏损上限放宽到 11 时同一笔成交只造成亏损 10，订单保持部分成交、剩余 5
// 继续占用 50 现金，没有触线或撤销记录。两种情况下缺口期间的新买单都被拒绝。

// gapRiskTradeSetup 构造共用前置状态：
// 初始现金 1000；A 合约限额 100；seq1@时刻10@价格8 已生效；
// 买单总量 10、限价 10 已接受（占用现金 100，余额仍为 1000）；
// 已开启交易日 1、亏损上限 lossLimit（无持仓，基准净值 1000）；
// seq3@时刻30@价格20 跨号等待中，缺口存在。
// 返回引擎与该买单编号。
func gapRiskTradeSetup(t *testing.T, lossLimit int64) (e *Engine, bid int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 8}, 1)

	// 先接受买单：按限价 10 × 总量 10 冻结 100 现金，余额不扣减。
	bid = mustBuy(t, e, "A", 10, 10)
	if e.Cash() != 1000 || e.ReservedCash() != 100 || e.AvailableCash() != 900 {
		t.Fatalf("买单接受后的前置资金状态错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}

	// 账户尚无持仓，买单冻结的现金仍是余额的一部分：基准与当前净值都是 1000。
	if err := e.StartTradingDay(1, lossLimit); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.LossLimit != lossLimit || st.Restricted {
		t.Fatalf("开日后前置风险状态错误: %+v", st)
	}

	// seq3 跨号：只进入等待，最新报价、风险状态与占用都不得变化。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)
	if !e.HasGap("A") {
		t.Fatal("缺少 seq2 时 seq3 必须进入等待，报告缺口")
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 1 || q.Moment != 10 || q.Price != 8 {
		t.Fatalf("等待中的 seq3 不得推进最新报价: %+v ok=%v", q, ok)
	}
	if st = e.RiskStatus(); st.Equity != 1000 || st.Loss != 0 || st.Restricted {
		t.Fatalf("等待中的价格 20 不得提前参与估值或改变风险状态: %+v", st)
	}
	if e.Cash() != 1000 || e.ReservedCash() != 100 || e.Position("A") != 0 {
		t.Fatalf("等待报价不得改动资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	return e, bid
}

// TestGapPartialBuyFillTriggersRiskOnAppliedQuoteOnly 覆盖上限为 10 的主场景：
// 缺口期间已接受买单的合法部分成交必须入账；成交后按最新已生效报价 seq1@8
// 估值，亏损恰好 10 达限，同一调用内再撤销剩余 5 股。成交返回定格在撤单前，
// 订单查询反映撤单后；等待的 seq3 既不能阻止成交、不能充当估值依据，也不能
// 被成交消耗。
func TestGapPartialBuyFillTriggersRiskOnAppliedQuoteOnly(t *testing.T) {
	e, bid := gapRiskTradeSetup(t, 10)

	// 缺口期间新买单按既有规则拒绝（此时尚未触线，拒绝原因必须是行情缺口）。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("行情缺口期间新买单必须拒绝")
	} else if !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("触线前的缺口拒单必须给出缺口原因: %v", err)
	}

	recsBefore := len(e.Records())
	tr := Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 5}
	res, err := e.Fill(tr)
	if err != nil {
		t.Fatalf("缺口不得阻止已接受买单的合法成交: %v", err)
	}

	// 成交返回定格在“记账完成、风险撤单之前”：本次 5、累计 5、剩余 5、部分成交。
	if res != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 5,
		Filled: 5, Remaining: 5, Status: StatusPartial}) {
		t.Fatalf("成交返回应反映记账完成时的部分成交、累计 5、剩余 5: %+v", res)
	}

	// 成交按成交价 10 结算：现金 950、持仓 5；剩余 5 股被风险撤销，占用归零。
	if e.Cash() != 950 || e.Position("A") != 5 || e.Sellable("A") != 5 {
		t.Fatalf("成交入账与剩余撤单后的资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 950 {
		t.Fatalf("剩余 5 股的占用 50 必须在保护撤单时释放: reserved=%d available=%d",
			e.ReservedCash(), e.AvailableCash())
	}

	// 净值只能按最新已生效报价 seq1@8：950 + 5×8 = 990，亏损恰好 10。
	// 等待中的 seq3@20 若提前参与，净值将为 1050、亏损为 0，就不会触线。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 10 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 990 || st.Loss != 10 {
		t.Fatalf("必须按 seq1@8 估值得到净值 990、亏损 10 并触线，不能按等待价 20 记为盈利: %+v", st)
	}

	// 订单查询反映整次操作结束后：已成交 5 保留，已撤销、有效剩余 0，
	// 并携带日内亏损保护的撤销原因。
	o, ok := e.Order(bid)
	if !ok {
		t.Fatal("订单必须仍可查询")
	}
	if o.Qty != 10 || o.Limit != 10 || o.Filled != 5 ||
		o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应保留总量 10、已成交 5，已撤销且有效剩余量为 0: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 10") || !strings.Contains(o.Reason, "上限 10") {
		t.Fatalf("撤销原因必须是日内亏损保护并写明亏损 10、上限 10: %q", o.Reason)
	}

	// 成交处理结束后：最新报价仍为 seq1，缺口仍在，等待的 seq3 没有被成交消耗。
	q, qok := e.CurrentQuote("A")
	if !qok || q.Seq != 1 || q.Moment != 10 || q.Price != 8 {
		t.Fatalf("成交不得推进最新报价: %+v ok=%v", q, qok)
	}
	if !e.HasGap("A") {
		t.Fatal("成交后行情缺口必须仍在，等待中的 seq3 不能被成交消耗")
	}

	// 新增记录恰好 3 条，顺序为：成交 → 当日首次触线 → 剩余量撤销。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("应只新增 3 条记录（成交、触线、撤单），实际 %d 条", len(newRecs))
	}

	// 1) 成交记录：成交价 10 与估值报价 seq1@8 分别留存；风险快照为记账后、
	// 触线前（净值 990、亏损 10、尚未限制），剩余 5 取成交时点口径。
	fr := newRecs[0]
	if fr.Kind != RecordFilled || fr.TradeID != 1 || fr.OrderID != bid || fr.Side != Buy {
		t.Fatalf("第一条应为成交记录: %+v", fr)
	}
	if fr.TradePrice != 10 || fr.Qty != 5 || fr.Filled != 5 || fr.Remaining != 5 {
		t.Fatalf("成交记录应保存成交价 10、本次 5、累计 5、成交时剩余 5: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 1 || fr.QuoteMoment != 10 || fr.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定最新已生效报价 seq1@时刻10 价格8: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 1000 || fr.RiskEquity != 990 ||
		fr.RiskLoss != 10 || fr.RiskLimit != 10 || fr.RiskRestrict {
		t.Fatalf("成交记录应携带记账后、触线前的风险快照: %+v", fr)
	}

	// 2) 触线记录：来源是这笔成交（关联成交编号 1），估值依据只能是合约 A
	// seq1@时刻10@价格8；绝不能把等待中的 seq3@20 写成估值依据。
	trr := newRecs[1]
	if trr.Kind != RecordRiskTriggered {
		t.Fatalf("第二条应为当日首次触线记录: %+v", trr)
	}
	if trr.RiskTrigger != RiskTriggerTrade || trr.RiskRefSeq != 1 || trr.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是成交 1：trigger=%s ref=%d moment=%d",
			trr.RiskTrigger, trr.RiskRefSeq, trr.RiskRefMoment)
	}
	if trr.RiskDay != 1 || trr.RiskBaseline != 1000 || trr.RiskEquity != 990 ||
		trr.RiskLoss != 10 || trr.RiskLimit != 10 || !trr.RiskRestrict {
		t.Fatalf("触线快照必须固化净值 990、亏损 10、上限 10: %+v", trr)
	}
	if len(trr.RiskQuoteRefs) != 1 ||
		trr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 8}) {
		t.Fatalf("触线估值报价定位必须锚定 seq1@时刻10 价格8，不得使用等待中的 seq3@20: %+v",
			trr.RiskQuoteRefs)
	}
	if !strings.Contains(trr.Reason, "成交") ||
		!strings.Contains(trr.Reason, "亏损 10") || !strings.Contains(trr.Reason, "上限 10") {
		t.Fatalf("触线原因必须写明由成交引发、亏损 10、上限 10: %q", trr.Reason)
	}

	// 3) 撤销记录：取消剩余 5 股，已成交 5 保留；报价定位仍是 seq1@8。
	cr := newRecs[2]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy {
		t.Fatalf("第三条应为该买单的剩余量撤销记录: %+v", cr)
	}
	if cr.Qty != 10 || cr.Filled != 5 || cr.Remaining != 5 {
		t.Fatalf("撤销记录应保留总量 10、已成交 5，并注明本次取消 5 股: %+v", cr)
	}
	if cr.TradePrice != 0 {
		t.Fatalf("撤销记录不得混入成交价: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 1 || cr.QuoteMoment != 10 || cr.QuotePrice != 8 {
		t.Fatalf("撤销记录报价定位必须仍是 seq1@时刻10 价格8: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskEquity != 990 || cr.RiskLoss != 10 ||
		cr.RiskLimit != 10 || !cr.RiskRestrict {
		t.Fatalf("撤销记录必须固化触线后的风险快照: %+v", cr)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 触线后缺口仍在：新买单仍被拒绝（限制与缺口两条规则并存）。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("触线且缺口期间新买单必须拒绝")
	}

	// 等待中的 seq3 确实保留：补齐 seq2 后 seq2、seq3 一次生效，最新报价推进
	// 到 seq3@20；限制整日保留，且不新增第二条触线记录。
	recsAfter := len(e.Records())
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 8}, 2)
	if q2, _ := e.CurrentQuote("A"); q2.Seq != 3 || q2.Moment != 30 || q2.Price != 20 {
		t.Fatalf("补齐后等待的 seq3 必须仍可生效，最新报价推进到 seq3@时刻30 价格20: %+v", q2)
	}
	if e.HasGap("A") {
		t.Fatal("补齐 seq2 后缺口应消失")
	}
	if !e.RiskStatus().Restricted {
		t.Fatal("即使补来的报价使净值回升，日内限制也必须整日保留")
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("报价补齐不得再产生第二条触线记录")
	}
	if len(e.Records()) != recsAfter {
		t.Fatalf("已限制期间报价补齐只推进报价，不应新增事件记录: %d -> %d",
			recsAfter, len(e.Records()))
	}
}

// TestGapPartialBuyFillBelowLimitKeepsRemainderAndPendingQuote 覆盖上限为 11
// 的同类成交：亏损 10 仍低于上限，成交只完成部分成交记账：剩余 5 股继续有效
// 并占用 50 现金，没有触线或保护撤销记录；最新报价仍为 seq1，缺口与等待的
// seq3 保留，缺口期间新买单仍按缺口规则拒绝。
func TestGapPartialBuyFillBelowLimitKeepsRemainderAndPendingQuote(t *testing.T) {
	e, bid := gapRiskTradeSetup(t, 11)

	recsBefore := len(e.Records())
	res, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 5})
	if err != nil {
		t.Fatalf("缺口不得阻止已接受买单的合法成交: %v", err)
	}
	if res != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 5,
		Filled: 5, Remaining: 5, Status: StatusPartial}) {
		t.Fatalf("成交返回应为部分成交、累计 5、剩余 5: %+v", res)
	}

	// 现金 950、持仓 5；剩余 5 股继续按限价 10 占用 50 现金。
	if e.Cash() != 950 || e.ReservedCash() != 50 || e.AvailableCash() != 900 ||
		e.Position("A") != 5 || e.Sellable("A") != 5 {
		t.Fatalf("未触线时剩余占用必须保留: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}

	// 按 seq1@8 估值：净值 990、亏损 10 < 上限 11，不触线。
	st := e.RiskStatus()
	if st.Restricted || st.Baseline != 1000 || st.Equity != 990 ||
		st.Loss != 10 || st.LossLimit != 11 {
		t.Fatalf("亏损 10 低于上限 11，不应触线，且估值不得使用等待价 20: %+v", st)
	}

	// 订单保持部分成交、有效剩余 5、无撤销原因。
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Filled != 5 || o.Qty != 10 ||
		o.Remaining() != 5 || o.Reason != "" {
		t.Fatalf("未触线时订单必须保持部分成交、剩余 5 股继续有效: %+v", o)
	}

	// 只新增一条成交记录；成交记录的估值报价仍是 seq1@8。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 1 || newRecs[0].Kind != RecordFilled {
		t.Fatalf("未触线时只能新增一条成交记录，实际 %+v", newRecs)
	}
	f0 := newRecs[0]
	if !f0.QuoteValid || f0.QuoteSeq != 1 || f0.QuoteMoment != 10 || f0.QuotePrice != 8 {
		t.Fatalf("成交记录的报价定位必须锚定 seq1@时刻10 价格8: %+v", f0)
	}
	if f0.RiskEquity != 990 || f0.RiskLoss != 10 || f0.RiskLimit != 11 || f0.RiskRestrict {
		t.Fatalf("成交记录应携带未触线的风险快照: %+v", f0)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("亏损未达上限不得产生触线记录")
	}

	// 成交不消耗等待报价：最新报价仍是 seq1，缺口与等待的 seq3 保留。
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Moment != 10 || q.Price != 8 {
		t.Fatalf("成交后最新报价必须仍为 seq1@时刻10 价格8: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("成交后缺口必须仍在，等待的 seq3 不能被消耗")
	}

	// 缺口期间新买单仍按既有缺口规则拒绝（保护尚未触发）。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("行情缺口期间新买单必须拒绝")
	} else if !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("未触线时拒单原因必须是行情缺口: %v", err)
	}
	if e.Cash() != 950 || e.ReservedCash() != 50 || e.Position("A") != 5 {
		t.Fatalf("缺口拒单不得改动资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 剩余 5 股仍是有效剩余量、占用 50 现金：本用例只固定这笔部分成交本身，
	// 不再提交后续成交（按 @10 成交、@8 估值的口径，再成交 1 股亏损即达 12，
	// 会转入上限 11 的触线场景，不属于本用例范围）。
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 5 {
		t.Fatalf("部分成交买单必须保持有效剩余 5 股: %+v", o)
	}
	if e.ReservedCash() != 50 {
		t.Fatalf("剩余 5 股必须继续占用 50 现金: %d", e.ReservedCash())
	}
}
