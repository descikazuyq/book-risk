package book

import (
	"errors"
	"math"
	"testing"
)

// setupHolding 准备：现金 1000，A 合约 10 股已在 price 成交入账，报价停留在 price。
// 返回引擎（成交后现金 1000-10*price，持仓 10）。
func setupHolding(t *testing.T, price int64) *Engine {
	t.Helper()
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: price}, 1)
	id := mustBuy(t, e, "A", 10, price)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: price, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRiskClosedKeepsBaselineBehavior(t *testing.T) {
	e := setupHolding(t, 10)

	st := e.RiskStatus()
	if st.Open {
		t.Fatalf("未开日时 Open 必须为 false: %+v", st)
	}
	if err := e.SetLossLimit(10); err == nil {
		t.Fatal("未开日时调整亏损上限必须报错")
	}

	// 未开日：暴跌报价不触发任何限制，买单仍可接受。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	mustBuy(t, e, "A", 1, 1)

	for _, r := range e.Records() {
		if r.Kind == RecordRiskTriggered {
			t.Fatal("未开日不得产生触线记录")
		}
		if r.RiskDay != 0 {
			t.Fatalf("未开日记录不得携带风险快照: %+v", r)
		}
	}
}

func TestRiskStartDayValidation(t *testing.T) {
	e, _ := NewEngine(100)

	if err := e.StartTradingDay(0, 10); err == nil {
		t.Fatal("非正日号必须报错")
	}
	if err := e.StartTradingDay(-1, 10); err == nil {
		t.Fatal("负日号必须报错")
	}
	if err := e.StartTradingDay(1, -1); err == nil {
		t.Fatal("负亏损上限必须报错")
	}
	if e.RiskStatus().Open {
		t.Fatal("非法开日不得改变状态")
	}

	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}
	recordsBefore := len(e.Records())

	// 同日同上限：幂等无操作、无记录。
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatalf("同日同上限重复开日应无操作: %v", err)
	}
	if len(e.Records()) != recordsBefore {
		t.Fatal("同日同上限重复开日不得产生记录")
	}
	// 同日不同上限：报错且不改上限。
	if err := e.StartTradingDay(1, 50); err == nil {
		t.Fatal("同日不同上限必须报错")
	}
	if got := e.RiskStatus().LossLimit; got != 100 {
		t.Fatalf("同日不同上限失败后上限应保持 100，实际 %d", got)
	}
	// 更大日号后倒退：报错且日号不变。
	if err := e.StartTradingDay(2, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 100); err == nil {
		t.Fatal("倒退日号必须报错")
	}
	if got := e.RiskStatus().Day; got != 2 {
		t.Fatalf("倒退失败后日号应保持 2，实际 %d", got)
	}
}

func TestRiskBaselineIncludesReservedCashOnce(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustBuy(t, e, "A", 10, 10) // 占用 100 现金，未成交
	if e.ReservedCash() != 100 {
		t.Fatalf("前置占用错误: %d", e.ReservedCash())
	}

	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.LossLimit != 100 || st.Restricted {
		t.Fatalf("基准净值应为现金全额 1000（占用不重复扣除）: %+v", st)
	}
}

func TestRiskZeroLimitTriggersImmediately(t *testing.T) {
	e, _ := NewEngine(100)
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if !st.Restricted || st.Loss != 0 {
		t.Fatalf("零上限应在开日立即触线: %+v", st)
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerStartDay {
		t.Fatalf("触线来源应为开日，实际 %s", rec.RiskTrigger)
	}
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线后买单必须拒绝")
	}

	// 更大日号清除限制并重新确定基准。
	if err := e.StartTradingDay(2, 100); err != nil {
		t.Fatal(err)
	}
	st = e.RiskStatus()
	if st.Restricted || st.Day != 2 || st.Baseline != 100 {
		t.Fatalf("跨日后应清除限制并重定基准: %+v", st)
	}
	mustBuy(t, e, "A", 1, 1) // 新日可以买入
}

func TestRiskQuoteTriggerCancelsBuysDescending(t *testing.T) {
	e := setupHolding(t, 10) // cash 900, pos A 10, quote A seq1@10
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	bID := mustBuy(t, e, "B", 5, 20) // id=2，占用 100，未成交

	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); st.Baseline != 1000 {
		t.Fatalf("基准应为 900 现金 + 100 市值 = 1000: %+v", st)
	}

	// 先盈后亏均不触线。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 11}, 1)
	if st := e.RiskStatus(); st.Loss != 0 {
		t.Fatalf("浮盈时亏损应记零: %+v", st)
	}
	aID := mustBuy(t, e, "A", 3, 9) // id=3，占用 27

	// 跌到 2：净值 920，亏损 80，未触线。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 2}, 1)
	if e.RiskStatus().Restricted {
		t.Fatal("亏损 80 未达上限 90，不应触线")
	}

	// 跌到 1：净值 910，亏损 90，达到上限，触线。
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: 1}, 1)
	st2 := e.RiskStatus()
	if !st2.Restricted || st2.Equity != 910 || st2.Loss != 90 {
		t.Fatalf("亏损达到上限应触线: %+v", st2)
	}

	// 触线记录内容。
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerQuote || rec.RiskRefSeq != 4 || rec.RiskRefMoment != 4 {
		t.Fatalf("触线记录来源/关联报价错误: trigger=%s ref=%d moment=%d",
			rec.RiskTrigger, rec.RiskRefSeq, rec.RiskRefMoment)
	}
	if rec.RiskDay != 1 || rec.RiskBaseline != 1000 || rec.RiskEquity != 910 ||
		rec.RiskLoss != 90 || rec.RiskLimit != 90 || !rec.RiskRestrict {
		t.Fatalf("触线记录风险快照错误: %+v", rec)
	}
	if len(rec.RiskQuoteRefs) != 1 {
		t.Fatalf("应保存 A 一个持仓合约的报价，实际 %+v", rec.RiskQuoteRefs)
	}
	ref := rec.RiskQuoteRefs[0]
	if ref.Symbol != "A" || ref.Seq != 4 || ref.Moment != 4 || ref.Price != 1 {
		t.Fatalf("持仓报价定位错误: %+v", ref)
	}

	// 撤销顺序：编号从大到小（id=3 的 A 买单先，id=2 的 B 买单后），全部跨合约撤销。
	var canceledIDs []int64
	var sawTrigger bool
	for _, r := range e.Records() {
		if r.Kind == RecordRiskTriggered {
			sawTrigger = true
			continue
		}
		if sawTrigger && r.Kind == RecordCanceled {
			canceledIDs = append(canceledIDs, r.OrderID)
			if r.RiskDay != 1 || r.RiskLoss != 90 || r.RiskLimit != 90 || !r.RiskRestrict {
				t.Fatalf("触线撤单记录必须携带当日亏损与上限: %+v", r)
			}
		}
	}
	if len(canceledIDs) != 2 || canceledIDs[0] != aID || canceledIDs[1] != bID {
		t.Fatalf("应按编号降序撤销 %d,%d，实际 %v", aID, bID, canceledIDs)
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("撤单应释放全部买单占用: reserved=%d", e.ReservedCash())
	}
	if e.Cash() != 900 || e.Position("A") != 10 {
		t.Fatalf("已入账成交、现金与持仓必须保留: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	for _, o := range e.Orders() {
		if o.Status == StatusCanceled {
			continue
		}
		if o.Side == Buy && o.Remaining() > 0 {
			t.Fatalf("不得残留有效买单: %+v", o)
		}
	}

	// 所有合约新买单被拒，拒绝记录携带当日亏损与上限。
	recsBefore := len(e.Records())
	if _, err := e.Buy("B", 1, 1); err == nil {
		t.Fatal("触线后 B 合约买单也必须拒绝")
	}
	rj := e.Records()[recsBefore]
	if rj.Kind != RecordRejected || rj.RiskLoss != 90 || rj.RiskLimit != 90 ||
		!rj.RiskRestrict || rj.RiskDay != 1 {
		t.Fatalf("触线拒单记录风险快照错误: %+v", rj)
	}

	// 卖单、卖单成交、主动撤卖单仍按原规则处理。
	sid := mustSell(t, e, "A", 5, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 5}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 905 || e.Position("A") != 5 {
		t.Fatalf("触线后卖出成交应正常入账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	sid2 := mustSell(t, e, "A", 2, 1)
	if err := e.Cancel(sid2); err != nil {
		t.Fatalf("触线后主动撤卖单应正常: %v", err)
	}

	// 报价回升不解除限制。
	mustQuote(t, e, "A", Quote{Seq: 5, Moment: 5, Price: 100}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("报价回升不得解除限制")
	}
	// 调高上限也不解除。
	if err := e.SetLossLimit(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if !e.RiskStatus().Restricted {
		t.Fatal("调高上限不得解除限制")
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}
}

func TestRiskAdjustLimitTriggersImmediately(t *testing.T) {
	e := setupHolding(t, 10) // cash 900, pos 10, quote 10，净值 1000
	if err := e.StartTradingDay(1, 200); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 净值 990，亏损 10
	before := len(e.Records())

	// 下调到当前亏损以下：立刻触线；同值再调不产生变化。
	if err := e.SetLossLimit(10); err != nil {
		t.Fatal(err)
	}
	if !e.RiskStatus().Restricted {
		t.Fatal("下调上限使当前亏损达限必须立即触线")
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerAdjustLimit || rec.RiskRefSeq != 0 {
		t.Fatalf("触线来源应为调限额: %s ref=%d", rec.RiskTrigger, rec.RiskRefSeq)
	}
	if err := e.SetLossLimit(10); err != nil {
		t.Fatal(err)
	}
	if len(e.Records()) != before+1 {
		t.Fatal("同值调整不得产生记录，触线记录只应有一条")
	}
}

func TestRiskTradeTriggerSettlesFirstThenCancels(t *testing.T) {
	e := setupHolding(t, 10) // cash 900, pos 10, quote seq1@10
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 5}, 1) // 净值 950，亏损 50
	if e.RiskStatus().Restricted {
		t.Fatal("前置不应触线")
	}

	// 高限价挂买单并以 20 成交：现金 700、持仓 20，但按最新报价 5 估值仅 100，
	// 净值 800、亏损 200，由成交触发触线。
	id := mustBuy(t, e, "A", 10, 20)
	res, err := e.Fill(Trade{TradeID: 2, OrderID: id, Symbol: "A", Side: Buy, Price: 20, Qty: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFilled {
		t.Fatalf("成交应先完整记账: %+v", res)
	}
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 800 || st.Loss != 200 {
		t.Fatalf("成交后应触线: %+v", st)
	}
	if e.Cash() != 700 || e.Position("A") != 20 {
		t.Fatalf("成交必须先入账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerTrade || rec.RiskRefSeq != 2 {
		t.Fatalf("触线应关联成交编号 2: trigger=%s ref=%d", rec.RiskTrigger, rec.RiskRefSeq)
	}
	if len(rec.RiskQuoteRefs) != 1 || rec.RiskQuoteRefs[0].Seq != 2 || rec.RiskQuoteRefs[0].Price != 5 {
		t.Fatalf("应保存成交记账时所用报价 seq2@5: %+v", rec.RiskQuoteRefs)
	}
	// 成交记录在触线记录之前。
	kinds := e.Records()
	if kinds[len(kinds)-2].Kind != RecordFilled || kinds[len(kinds)-1].Kind != RecordRiskTriggered {
		t.Fatal("必须先记成交、后记触线")
	}
}

func TestRiskGapAppliesOneByOneAndRestrictionSticks(t *testing.T) {
	// 现金 1000 买入 100 股 @10 后：cash 0，持仓 100。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 200)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}

	// seq3 暴跌报价跨号等待：不参与估值，净值仍是 1000。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 2}, 0)
	if !e.HasGap("A") || e.RiskStatus().Equity != 1000 {
		t.Fatalf("跨号等待报价不得参与估值: %+v", e.RiskStatus())
	}

	// 补齐 seq2：seq2@10 不触线，seq3@2 使净值 200、亏损 800，触线并保留。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 10}, 2)
	if !e.RiskStatus().Restricted {
		t.Fatal("链中暴跌价格必须触线")
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskRefSeq != 3 || rec.RiskRefMoment != 3 || rec.RiskEquity != 200 || rec.RiskLoss != 800 {
		t.Fatalf("应以中间暴跌价格固化触线: %+v", rec)
	}

	// 最后价格恢复：限制不解除，也不产生新触线记录。
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: 10}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("即使链后价格恢复，限制也必须保留")
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("限制保留期间不得重复记录触线")
	}
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("限制保留期间买单必须拒绝")
	}
}

func TestRiskNextDayResetsBaselineAndKeepsOldOrdersCanceled(t *testing.T) {
	e := setupHolding(t, 10)
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	pending := mustBuy(t, e, "A", 2, 9) // 占用 18
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("前置应触线")
	}
	if o, _ := e.Order(pending); o.Status != StatusCanceled {
		t.Fatalf("触线应撤销该买单: %+v", o)
	}
	day1Records := len(e.Records())

	// 更大日号：重新确定基准（现金 900 + 持仓 10@1 = 910），清除限制，不恢复旧订单。
	if err := e.StartTradingDay(2, 500); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if st.Restricted || st.Day != 2 || st.Baseline != 910 || st.Equity != 910 {
		t.Fatalf("跨日应重定基准并清除限制: %+v", st)
	}
	if o, _ := e.Order(pending); o.Status != StatusCanceled {
		t.Fatal("跨日不得恢复旧订单")
	}
	if err := e.Cancel(pending); err == nil {
		t.Fatal("旧订单仍处撤销态，重复撤销必须报错")
	}
	// 跨日不产生风险记录，旧触线记录保持日号 1。
	if len(e.Records()) != day1Records {
		t.Fatal("跨日本身不得新增事件记录")
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskDay != 1 {
		t.Fatalf("跨日不得改写旧触线记录: %+v", rec)
	}

	// 新日可正常交易，且可再次触线（新日一条新触线记录）。
	mustBuy(t, e, "A", 1, 1)
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 1}, 1) // 净值不变
	if countRiskRecords(e) != 1 {
		t.Fatal("新日尚未再次触线")
	}
}

func TestRiskMultiSymbolQuoteRefs(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	bA := mustBuy(t, e, "A", 10, 10)
	bB := mustBuy(t, e, "B", 5, 20)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bA, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bB, Symbol: "B", Side: Buy, Price: 20, Qty: 5}); err != nil {
		t.Fatal(err)
	}
	// cash 800；A 市值 100，B 市值 100，基准 1000。
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 9, Price: 1}, 1) // 净值 910，亏损 90
	rec := lastRiskRecord(t, e)
	if len(rec.RiskQuoteRefs) != 2 {
		t.Fatalf("触线记录必须保存各持仓合约报价: %+v", rec.RiskQuoteRefs)
	}
	// 按合约排序、序号/时刻/价格俱全。
	if rec.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 9, Price: 1}) {
		t.Fatalf("A 报价定位错误: %+v", rec.RiskQuoteRefs[0])
	}
	if rec.RiskQuoteRefs[1] != (RiskQuoteRef{Symbol: "B", Seq: 1, Moment: 1, Price: 20}) {
		t.Fatalf("B 报价定位错误: %+v", rec.RiskQuoteRefs[1])
	}
}

func TestRiskFillIdempotencyCreatesNoRiskRecord(t *testing.T) {
	e := setupHolding(t, 10)
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 2, 10)
	tr := Trade{TradeID: 9, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 2}
	res1, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	before := len(e.Records())
	cashBefore := e.Cash()

	res2, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res1 != res2 || len(e.Records()) != before || e.Cash() != cashBefore {
		t.Fatal("幂等成交不得重复记账或产生风险记录")
	}

	// 触线后重放旧成交仍返回原结果，不新增记录。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("前置应触线")
	}
	before = len(e.Records())
	res3, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res3 != res1 || len(e.Records()) != before {
		t.Fatal("触线后重放旧成交必须幂等且无新记录")
	}
}

func TestRiskOverflowStartDayRejected(t *testing.T) {
	e, _ := NewEngine(math.MaxInt64)
	mustSetMax(t, e, "A", 2)
	big := int64(math.MaxInt64 - 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: big}, 1)
	id := mustBuy(t, e, "A", 1, big)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: big, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 100 || e.Position("A") != 1 {
		t.Fatalf("前置状态错误: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	// 未开日时报价可使估值溢出（不参与风险检查）。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: math.MaxInt64}, 1)

	err := e.StartTradingDay(1, 100)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("开日净值溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if e.RiskStatus().Open {
		t.Fatal("溢出开日必须整体失败，不得进入开日状态")
	}
	recs := e.Records()
	last := recs[len(recs)-1]
	if last.Kind != RecordRejected || last.RiskDay != 0 {
		t.Fatalf("溢出开日只留无风险快照的拒绝记录: %+v", last)
	}
	if err := e.SetLossLimit(10); err == nil {
		t.Fatal("溢出开日后仍应处于未开日状态")
	}
}

func TestRiskOverflowQuoteBatchRollsBack(t *testing.T) {
	e, _ := NewEngine(10)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 5}, 1)
	id := mustBuy(t, e, "A", 2, 5)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 5, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}

	// seq3 溢出报价先等待，补齐 seq2@6 时：链中 seq3 市值溢出，整批回滚。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: math.MaxInt64}, 0)
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 6})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("链中溢出必须整体报错且 0 条生效: n=%d err=%v", n, err)
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 5 {
		t.Fatalf("整批回滚后最新报价应停留在 seq1@5: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("整批回滚后 seq3 应仍在等待")
	}
	if st := e.RiskStatus(); st.Equity != 10 || st.Loss != 0 {
		t.Fatalf("整批回滚不得改变净值: %+v", st)
	}
}

func TestRiskOverflowFillRejectedAndTradeIDReusable(t *testing.T) {
	e, _ := NewEngine(math.MaxInt64 - 5)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	id := mustBuy(t, e, "A", 1, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	cash := e.Cash() // MaxInt64-6
	if err := e.StartTradingDay(1, 100); err != nil {
		t.Fatal(err)
	}

	sid := mustSell(t, e, "A", 1, 1)
	// 成交价 10：现金 +10 溢出 int64，整笔拒绝。
	_, err := e.Fill(Trade{TradeID: 7, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("成交致现金溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if e.Cash() != cash || e.Position("A") != 1 {
		t.Fatalf("溢出成交不得记账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("溢出成交后卖单应保持有效: %+v", o)
	}
	// 编号未被占用：修正价格后同编号可成功。
	if _, err := e.Fill(Trade{TradeID: 7, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 1}); err != nil {
		t.Fatalf("被溢出拒绝的成交编号应可复用: %v", err)
	}
	if e.Position("A") != 0 {
		t.Fatalf("修正后成交应正常入账: pos=%d", e.Position("A"))
	}
}

func TestRiskRecordsAreImmutableAcrossChanges(t *testing.T) {
	e := setupHolding(t, 10) // cash 900, pos A 10
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, e, "A", 2, 9) // 触线时应被撤销
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	rec := lastRiskRecord(t, e)
	if rec.RiskEquity != 910 || rec.RiskLoss != 90 {
		t.Fatalf("前置触线快照错误: %+v", rec)
	}

	// Records() 返回深拷贝：篡改副本不得影响内部记录。
	recs := e.Records()
	for i := range recs {
		if recs[i].Kind == RecordRiskTriggered {
			recs[i].RiskQuoteRefs[0].Price = 999
			recs[i].RiskLoss = 1
		}
	}
	again := lastRiskRecord(t, e)
	if again.RiskQuoteRefs[0].Price != 1 || again.RiskLoss != 90 {
		t.Fatalf("内部触线记录不得被外部副本改写: %+v", again)
	}

	// 后续行情、跨日不得改写旧记录。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 100}, 1)
	if err := e.StartTradingDay(2, 100); err != nil {
		t.Fatal(err)
	}
	again = lastRiskRecord(t, e)
	if again.RiskDay != 1 || again.RiskLoss != 90 || again.RiskLimit != 90 ||
		again.RiskQuoteRefs[0].Price != 1 {
		t.Fatalf("跨日与行情不得改写旧触线记录: %+v", again)
	}
}

func TestRiskSellTradeRealizesLossAndTriggers(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 100}, 1)
	bid := mustBuy(t, e, "A", 10, 100)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 100, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 50); err != nil {
		t.Fatal(err)
	}
	// 浮亏 40 尚未触线。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 96}, 1)
	if e.RiskStatus().Loss != 40 || e.RiskStatus().Restricted {
		t.Fatalf("浮亏 40 不应触线: %+v", e.RiskStatus())
	}
	// 以 95 卖出全部持仓：现金 950、空仓，实现亏损 50，卖单成交触发触线。
	sid := mustSell(t, e, "A", 10, 90)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 95, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 950 || st.Loss != 50 {
		t.Fatalf("卖单实现亏损应触发触线: %+v", st)
	}
	rec := lastRiskRecord(t, e)
	if rec.RiskTrigger != RiskTriggerTrade || rec.RiskRefSeq != 2 || len(rec.RiskQuoteRefs) != 0 {
		t.Fatalf("空仓后触线记录应关联成交且无持仓报价: %+v", rec)
	}
}

func lastRiskRecord(t *testing.T, e *Engine) Record {
	t.Helper()
	for i := len(e.records) - 1; i >= 0; i-- {
		if e.records[i].Kind == RecordRiskTriggered {
			return e.records[i]
		}
	}
	t.Fatal("不存在触线记录")
	return Record{}
}

func countRiskRecords(e *Engine) int {
	n := 0
	for _, r := range e.Records() {
		if r.Kind == RecordRiskTriggered {
			n++
		}
	}
	return n
}
