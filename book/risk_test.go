package book

import (
	"strings"
	"testing"
)

// riskRec 返回最后一条风险触线记录。
func lastRiskRec(t *testing.T, e *Engine) Record {
	t.Helper()
	recs := e.Records()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind == RecordRiskTriggered {
			return recs[i]
		}
	}
	t.Fatal("缺少风险触线记录")
	return Record{}
}

// countRecKind 统计指定类型的记录条数。
func countRecKind(e *Engine, kind RecordKind) int {
	n := 0
	for _, r := range e.Records() {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func TestRiskNotStartedByDefault(t *testing.T) {
	e, _ := NewEngine(1000)
	rs := e.RiskStatus()
	if rs.Started {
		t.Fatal("尚未开始交易日时 Started 应为 false")
	}
	if rs.Day != 0 || rs.Baseline != 0 || rs.NetValue != 0 || rs.Loss != 0 || rs.Limit != 0 || rs.Restricted {
		t.Fatalf("未开日时风险状态应为零值: %+v", rs)
	}
	if err := e.SetLimit(100); err == nil {
		t.Fatal("未开日时调整上限必须报错")
	}
	// 未开日时现有交易行为不受影响。
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if id, err := e.Buy("A", 1, 10); err != nil || id == 0 {
		t.Fatalf("未开日时买单应正常接受: id=%d err=%v", id, err)
	}
}

func TestStartDayValidation(t *testing.T) {
	e, _ := NewEngine(1000)
	for _, day := range []int64{0, -1} {
		if err := e.StartDay(day, 100); err == nil {
			t.Fatalf("非正日号 %d 必须报错", day)
		}
	}
	if err := e.StartDay(1, -1); err == nil {
		t.Fatal("负上限必须报错")
	}
	// 报错后状态不变。
	if rs := e.RiskStatus(); rs.Started {
		t.Fatal("报错后不应进入已开日状态")
	}

	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if !rs.Started || rs.Day != 1 || rs.Limit != 100 || rs.Restricted {
		t.Fatalf("开日状态错误: %+v", rs)
	}

	// 同日号同上限：不产生变化（无新记录）。
	before := len(e.Records())
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	if len(e.Records()) != before {
		t.Fatal("同日号同上限不应产生新记录")
	}

	// 同日号不同上限：报错且不改状态。
	if err := e.StartDay(1, 200); err == nil {
		t.Fatal("同日号不同上限必须报错")
	}
	if rs := e.RiskStatus(); rs.Limit != 100 {
		t.Fatalf("报错后上限应保持 100，实际 %d", rs.Limit)
	}

	// 倒退日号：报错。
	if err := e.StartDay(0, 100); err == nil {
		t.Fatal("非正日号必须报错")
	}
}

func TestStartDayZeroLimitTriggersImmediately(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 10, 10) // 占用 100 现金

	if err := e.StartDay(1, 0); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if !rs.Restricted || rs.Loss != 0 || rs.Limit != 0 {
		t.Fatalf("零上限开日应立即触线: %+v", rs)
	}
	// 买单被撤销，占用释放。
	if e.ReservedCash() != 0 {
		t.Fatalf("触线后买单应被撤销、占用释放: reserved=%d", e.ReservedCash())
	}
	o, _ := e.Order(id)
	if o.Status != StatusCanceled {
		t.Fatalf("买单应被撤销: %+v", o)
	}
	// 新买单被拒绝。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("触线后新买单必须拒绝")
	}
	// 卖单仍可提交（无持仓，用零持仓验证拒绝路径不影响卖单规则）。
	if _, err := e.Sell("A", 1, 10); err == nil {
		t.Fatal("零持仓卖单仍应按原规则拒绝")
	}

	rec := lastRiskRec(t, e)
	if rec.RiskTrigger != RiskTriggerDay || rec.RiskDay != 1 || rec.RiskLimit != 0 {
		t.Fatalf("触线记录应注明开日引发: %+v", rec)
	}
	if rec.RiskBaseline != 1000 || rec.RiskNetValue != 1000 || rec.RiskLoss != 0 {
		t.Fatalf("触线记录净值快照错误: %+v", rec)
	}
}

func TestQuoteTriggerRestrictsAndCancelsBuys(t *testing.T) {
	// 现金 2000，买 100 @ 10 成交 90 → 现金 1100，持仓 90，买单剩余 10。
	// 再挂 5 股买单 → 剩余共 15。上限 100：报价 8 → 净值 1820，亏损 180，触线。
	e, _ := NewEngine(2000)
	mustSetMax(t, e, "A", 200)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 90}); err != nil {
		t.Fatal(err)
	}
	// 触线前再挂一笔买单（不同价格），验证触线时一并撤销。
	bid2 := mustBuy(t, e, "A", 5, 10)
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 8}, 1)
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatalf("应触线: %+v", rs)
	}
	// 两笔买单剩余均被撤销，占用全部释放。
	if e.ReservedCash() != 0 {
		t.Fatalf("触线后买单应全部撤销: reserved=%d", e.ReservedCash())
	}
	o1, _ := e.Order(id)
	o2, _ := e.Order(bid2)
	if o1.Status != StatusCanceled || o1.Filled != 90 {
		t.Fatalf("第一笔买单应部分成交后撤销: %+v", o1)
	}
	if o2.Status != StatusCanceled || o2.Filled != 0 {
		t.Fatalf("第二笔买单应全部撤销: %+v", o2)
	}
	// 新买单拒绝。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("触线后新买单必须拒绝")
	}
	// 卖单仍可提交（持仓 90，可卖）。
	if _, err := e.Sell("A", 10, 8); err != nil {
		t.Fatalf("触线后卖单应仍可提交: %v", err)
	}
}

func TestQuoteTriggerDirect(t *testing.T) {
	// 现金 1000，买 100 @ 10 成交 90 → 现金 100，持仓 90，买单剩余 10。
	// 上限 100：报价 8 → 净值 100 + 720 = 820，亏损 180，触线。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 90}); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); rs.Loss != 0 {
		t.Fatalf("成交后应无亏损: %+v", rs)
	}

	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 8}, 1)
	rs := e.RiskStatus()
	if !rs.Restricted || rs.Loss != 180 || rs.NetValue != 820 || rs.Baseline != 1000 {
		t.Fatalf("报价 8 应触线: %+v", rs)
	}
	// 剩余买单被撤销，占用释放。
	if e.ReservedCash() != 0 {
		t.Fatalf("触线后买单应撤销: reserved=%d", e.ReservedCash())
	}
	o, _ := e.Order(id)
	if o.Status != StatusCanceled || o.Filled != 90 {
		t.Fatalf("买单应部分成交后剩余被撤销: %+v", o)
	}
	// 新买单拒绝，且拒绝记录带亏损与上限。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("触线后新买单必须拒绝")
	}
	recs := e.Records()
	last := recs[len(recs)-1]
	if last.Kind != RecordRejected || last.RiskLoss != 180 || last.RiskLimit != 100 || last.RiskDay != 1 {
		t.Fatalf("拒绝记录应带当日亏损与上限: %+v", last)
	}
	// 撤销记录带亏损与上限。
	cancelRec := recs[len(recs)-2]
	if cancelRec.Kind != RecordCanceled || cancelRec.RiskLoss != 180 || cancelRec.RiskLimit != 100 {
		t.Fatalf("撤销记录应带当日亏损与上限: %+v", cancelRec)
	}

	rec := lastRiskRec(t, e)
	if rec.RiskTrigger != RiskTriggerQuote || rec.RiskQuoteSeq != 2 {
		t.Fatalf("触线记录应注明报价引发及序号: %+v", rec)
	}
	if rec.RiskNetValue != 820 || rec.RiskLoss != 180 || rec.RiskLimit != 100 {
		t.Fatalf("触线记录净值快照错误: %+v", rec)
	}
	if len(rec.Valuations) != 1 || rec.Valuations[0].Symbol != "A" || rec.Valuations[0].Qty != 90 ||
		!rec.Valuations[0].QuoteValid || rec.Valuations[0].QuoteSeq != 2 ||
		rec.Valuations[0].QuoteMoment != 2 || rec.Valuations[0].QuotePrice != 8 {
		t.Fatalf("触线记录持仓估值快照错误: %+v", rec.Valuations)
	}
}

func TestTriggerPersistsOnRecovery(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 触线
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("应触线")
	}
	// 报价回升，限制不解除。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 20}, 1)
	if rs := e.RiskStatus(); !rs.Restricted || rs.Loss != 0 {
		t.Fatalf("报价回升不应解除限制: %+v", rs)
	}
	// 调高上限也不解除。
	if err := e.SetLimit(1000); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("调高上限不应解除限制")
	}
	// 卖出盈利也不解除（卖单仍可提交成交）。
	sid := mustSell(t, e, "A", 10, 20)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 20, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("卖出盈利不应解除限制")
	}
}

func TestGapFillTriggerPersists(t *testing.T) {
	// 缺口补齐后逐条生效：中间价格触线、最后价格恢复，限制保留。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	// seq3 先到，跨号等待，不参与估值。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 20}, 0)
	if rs := e.RiskStatus(); rs.Restricted {
		t.Fatal("跨号报价不应参与估值、不应触线")
	}
	// seq2 到达 → 2、3 依次生效：seq2 @9 触线，seq3 @20 恢复。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 2)
	rs := e.RiskStatus()
	if !rs.Restricted {
		t.Fatal("seq2 @9 应触线")
	}
	rec := lastRiskRec(t, e)
	if rec.RiskTrigger != RiskTriggerQuote || rec.RiskQuoteSeq != 2 {
		t.Fatalf("触线应发生在 seq2: %+v", rec)
	}
	// 最新报价为 seq3，但限制保留。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 3 || q.Price != 20 {
		t.Fatalf("最新报价应为 seq3 @20: %+v", q)
	}
	if !rs.Restricted || rs.Loss != 0 {
		t.Fatalf("seq3 恢复后限制应保留、亏损为 0: %+v", rs)
	}
}

func TestFillTrigger(t *testing.T) {
	// 成交本身先记账，再处理触线撤单。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	// 报价先跌到 9（无持仓，不触线）。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1)
	if rs := e.RiskStatus(); rs.Restricted {
		t.Fatal("无持仓时报价下跌不应触线")
	}
	// 买单 100 @ 10 成交：现金 0，持仓 100，报价 9 → 净值 900，亏损 100，触线。
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if !rs.Restricted || rs.Loss != 100 || rs.NetValue != 900 {
		t.Fatalf("成交后应触线: %+v", rs)
	}
	rec := lastRiskRec(t, e)
	if rec.RiskTrigger != RiskTriggerFill || rec.RiskTradeID != 1 {
		t.Fatalf("触线记录应注明成交引发及编号: %+v", rec)
	}
	// 成交记录已入账（持仓保留）。
	if e.Position("A") != 100 || e.Cash() != 0 {
		t.Fatalf("成交应完整记账: pos=%d cash=%d", e.Position("A"), e.Cash())
	}
	// 幂等重放：不重复计算、不产生新风险记录。
	before := len(e.Records())
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	if len(e.Records()) != before {
		t.Fatal("幂等重放不应产生新记录")
	}
	if e.Position("A") != 100 {
		t.Fatal("幂等重放不应再次记账")
	}
}

func TestFillTriggerCancelsOwnOrder(t *testing.T) {
	// 部分成交后触线：该订单剩余部分也应被撤销。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1)
	id := mustBuy(t, e, "A", 100, 10)
	// 只成交 50 股：现金 500，持仓 50，净值 500 + 450 = 950，亏损 50，不触线。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 50}); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); rs.Restricted {
		t.Fatalf("部分成交后不应触线: %+v", rs)
	}
	// 剩余 50 股继续成交：现金 0，持仓 100，净值 900，亏损 100，触线。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 50}); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("全部成交后应触线")
	}
	o, _ := e.Order(id)
	if o.Status != StatusFilled {
		t.Fatalf("订单应全部成交（触线不撤销已成交部分）: %+v", o)
	}
}

func TestTriggerCancelsBuyOrdersDescending(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	// 三笔买单，均不成交。
	b1 := mustBuy(t, e, "A", 10, 10)
	b2 := mustBuy(t, e, "A", 10, 10)
	b3 := mustBuy(t, e, "A", 10, 10)
	// 报价跌到 1：净值 = 1000（无持仓），不触线。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	// 用零上限... 不行，已开日。用下调上限触线：当前净值 1000，亏损 0。
	// 直接构造：持仓为零，报价不影响净值。改为先成交再触线。
	// 成交 b1 全部：现金 900，持仓 10，净值 900 + 10×1 = 910，亏损 90。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b1, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	// 报价 1：净值 910，亏损 90 < 100。
	if rs := e.RiskStatus(); rs.Restricted {
		t.Fatalf("亏损 90 不应触线: %+v", rs)
	}
	// 报价... 900 + 10P ≤ 900 → P ≤ 0，不可能。需要更小上限。
	if err := e.SetLimit(90); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted || rs.Loss != 90 {
		t.Fatalf("上限 90 应触线: %+v", rs)
	}
	// b2、b3 剩余买单被撤销（b1 已成交，无剩余）。
	if e.ReservedCash() != 0 {
		t.Fatalf("触线后买单应全部撤销: reserved=%d", e.ReservedCash())
	}
	o2, _ := e.Order(b2)
	o3, _ := e.Order(b3)
	if o2.Status != StatusCanceled || o3.Status != StatusCanceled {
		t.Fatalf("b2、b3 应被撤销: b2=%+v b3=%+v", o2, o3)
	}
	// 撤销顺序：b3 先于 b2（按编号从大到小）。
	var canceledIDs []int64
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && r.RiskLoss == 90 {
			canceledIDs = append(canceledIDs, r.OrderID)
		}
	}
	if len(canceledIDs) != 2 || canceledIDs[0] != b3 || canceledIDs[1] != b2 {
		t.Fatalf("应按编号从大到小撤销 b3,b2: %v", canceledIDs)
	}
}

func TestNextDayClearsRestrictionButNotOrders(t *testing.T) {
	// 现金 2000，买 100 @ 10 成交 → 现金 1000，持仓 100，基准 2000。
	// 上限 100：报价 9 → 净值 1900，亏损 100，触线。
	e, _ := NewEngine(2000)
	mustSetMax(t, e, "A", 200)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 触线
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("应触线")
	}
	// 再挂一笔买单被拒绝。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("触线后新买单必须拒绝")
	}

	// 开始更大日号：重新确定基准（1000 + 100×9 = 1900），清除限制。
	if err := e.StartDay(2, 100); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if !rs.Started || rs.Day != 2 || rs.Restricted || rs.Baseline != 1900 || rs.Loss != 0 {
		t.Fatalf("新日状态错误: %+v", rs)
	}
	// 限制清除，新买单可接受（现金 1000，买 1 @ 10）。
	if _, err := e.Buy("A", 1, 10); err != nil {
		t.Fatalf("新日限制清除后买单应可接受: %v", err)
	}
	// 但上一日已成交的订单保持成交，不恢复为待成交。
	o, _ := e.Order(id)
	if o.Status != StatusFilled {
		t.Fatalf("上一日已成交订单保持成交: %+v", o)
	}
}

func TestSetLimitLowerTriggersImmediately(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 1000); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 净值 900，亏损 100
	if rs := e.RiskStatus(); rs.Restricted {
		t.Fatalf("上限 1000 时不应触线: %+v", rs)
	}
	// 下调上限到 100：亏损 100 ≥ 100，触线。
	if err := e.SetLimit(100); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted || rs.Loss != 100 || rs.Limit != 100 {
		t.Fatalf("下调上限应立即触线: %+v", rs)
	}
	rec := lastRiskRec(t, e)
	if rec.RiskTrigger != RiskTriggerLimit {
		t.Fatalf("触线记录应注明调限额引发: %+v", rec)
	}
	// 再下调不产生新触线记录。
	before := countRecKind(e, RecordRiskTriggered)
	if err := e.SetLimit(50); err != nil {
		t.Fatal(err)
	}
	if countRecKind(e, RecordRiskTriggered) != before {
		t.Fatal("重复触线不应产生新记录")
	}
}

func TestRecordsNotRewrittenByLaterChanges(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 触线

	// 后续行情、跨日、调限额不改写历史触线记录。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 1}, 1)
	if err := e.StartDay(2, 100); err != nil {
		t.Fatal(err)
	}
	if err := e.SetLimit(1); err != nil {
		t.Fatal(err)
	}
	recs := e.Records()
	var old Record
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].RiskQuoteSeq == 2 && recs[i].RiskDay == 1 {
			old = recs[i]
			break
		}
	}
	if old.Kind != RecordRiskTriggered || old.RiskNetValue != 900 || old.RiskLoss != 100 ||
		old.RiskQuoteSeq != 2 || old.RiskTrigger != RiskTriggerQuote {
		t.Fatalf("历史触线记录被改写或丢失: %+v", old)
	}
}

func TestQuoteOverflowErrorsWithoutStateChange(t *testing.T) {
	// 持仓 1e9，报价 1e9 → 市值 1e18（不溢出）；报价 1e10 → 1e19 溢出。
	e, _ := NewEngine(1_000_000_000_000_000_000) // 1e18
	mustSetMax(t, e, "A", 1_000_000_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1_000_000_000}, 1)
	if err := e.StartDay(1, 1_000_000_000); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 1_000_000_000, 1_000_000_000) // 占用 1e18
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 1_000_000_000, Qty: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); rs.NetValue != 1_000_000_000_000_000_000 {
		t.Fatalf("成交后净值应为 1e18: %+v", rs)
	}
	// 报价 1e10 → 市值 1e19，超出 int64 范围。
	if _, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 10_000_000_000}); err == nil {
		t.Fatal("报价导致净值溢出必须报错")
	}
	// 报价未生效，状态不变。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 1_000_000_000 {
		t.Fatalf("溢出报价不得生效: %+v", q)
	}
	if rs := e.RiskStatus(); rs.NetValue != 1_000_000_000_000_000_000 || rs.Restricted {
		t.Fatalf("溢出不得改变风险状态: %+v", rs)
	}
	// 跨号等待的报价也不受影响：seq3 先到等待，seq2 溢出报错，seq3 仍等待。
	if _, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 3, Price: 1}); err != nil {
		t.Fatal(err)
	}
	if !e.HasGap("A") {
		t.Fatal("seq2 未生效，seq3 应仍在等待")
	}
}

func TestFillOverflowRejectedWithoutAccounting(t *testing.T) {
	// 现金接近 MaxInt64，卖出成交导致现金 + 金额溢出。
	const maxInt64 = 9_223_372_036_854_775_807
	e, _ := NewEngine(maxInt64)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 1, 1) // 现金 MaxInt64-1，持仓 1
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 卖 1 @ 2：现金 MaxInt64-1+2 = MaxInt64+1，溢出。
	sid := mustSell(t, e, "A", 1, 1)
	before := len(e.Records())
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 1}); err == nil {
		t.Fatal("成交导致净值溢出必须报错")
	}
	// 除拒绝记录外不改变业务状态。
	if len(e.Records()) != before+1 {
		t.Fatal("溢出应只增加一条拒绝记录")
	}
	if e.Cash() != maxInt64-1 || e.Position("A") != 1 {
		t.Fatalf("溢出成交不得记账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	// 成交编号不被占用，修正后仍可用。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 1}); err != nil {
		t.Fatalf("被拒绝编号应可复用: %v", err)
	}
}

func TestStartDayOverflowErrorsWithoutStateChange(t *testing.T) {
	// 白盒构造：现金 MaxInt64 + 持仓 1×1 = MaxInt64+1，超出 int64 范围。
	const maxInt64 = 9_223_372_036_854_775_807
	e, _ := NewEngine(0)
	e.cash = maxInt64
	st := e.state("A")
	st.position = 1
	st.hasQuote = true
	st.latest = Quote{Seq: 1, Moment: 1, Price: 1}

	if err := e.StartDay(1, 100); err == nil {
		t.Fatal("开日净值溢出必须报错")
	}
	// 状态不变：未开日。
	if rs := e.RiskStatus(); rs.Started {
		t.Fatal("溢出报错后不应进入已开日状态")
	}
	// 修正后可正常开日。
	e.cash = maxInt64 - 1
	if err := e.StartDay(1, 100); err != nil {
		t.Fatalf("净值修正后开日应成功: %v", err)
	}
	rs := e.RiskStatus()
	if !rs.Started || rs.Baseline != maxInt64 {
		t.Fatalf("开日状态错误: %+v", rs)
	}
}

func TestSetLimitOverflowErrorsWithoutChange(t *testing.T) {
	// 白盒构造：开日后净值溢出，调限额必须报错且上限不变。
	const maxInt64 = 9_223_372_036_854_775_807
	e, _ := NewEngine(0)
	e.cash = maxInt64 - 1
	st := e.state("A")
	st.position = 1
	st.hasQuote = true
	st.latest = Quote{Seq: 1, Moment: 1, Price: 1}
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	// 净值 = (MaxInt64-1) + 1 = MaxInt64，不溢出。
	if rs := e.RiskStatus(); rs.NetValue != maxInt64 {
		t.Fatalf("净值应为 MaxInt64: %+v", rs)
	}
	// 构造溢出：现金 + 持仓市值 > MaxInt64。
	e.cash = maxInt64
	if err := e.SetLimit(50); err == nil {
		t.Fatal("调限额时净值溢出必须报错")
	}
	// 上限不变。
	if rs := e.RiskStatus(); rs.Limit != 100 {
		t.Fatalf("溢出报错后上限应保持 100: %+v", rs)
	}
}

func TestMultiSymbolValuation(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	if err := e.StartDay(1, 1000); err != nil {
		t.Fatal(err)
	}
	// 买 10 A @ 10（成交）：现金 900，持仓 A 10。
	idA := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: idA, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	// 买 5 B @ 20（成交）：现金 800，持仓 B 5。
	idB := mustBuy(t, e, "B", 5, 20)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: idB, Symbol: "B", Side: Buy, Price: 20, Qty: 5}); err != nil {
		t.Fatal(err)
	}
	// 净值 = 800 + 10×10 + 5×20 = 800 + 100 + 100 = 1000，亏损 0。
	if rs := e.RiskStatus(); rs.NetValue != 1000 || rs.Loss != 0 {
		t.Fatalf("多合约净值计算错误: %+v", rs)
	}
	// A 报价跌到 1：净值 = 800 + 10 + 100 = 910，亏损 90。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if rs := e.RiskStatus(); rs.Loss != 90 {
		t.Fatalf("A 下跌后亏损应为 90: %+v", rs)
	}
	// B 报价跌到 1：净值 = 800 + 10 + 5 = 815，亏损 185 ≥ 1000？不，185 < 1000。
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 3, Price: 1}, 1)
	if rs := e.RiskStatus(); rs.Loss != 185 {
		t.Fatalf("B 下跌后亏损应为 185: %+v", rs)
	}
	// 下调上限到 185：触线。
	if err := e.SetLimit(185); err != nil {
		t.Fatal(err)
	}
	if rs := e.RiskStatus(); !rs.Restricted {
		t.Fatal("上限 185 应触线")
	}
	rec := lastRiskRec(t, e)
	if len(rec.Valuations) != 2 {
		t.Fatalf("触线记录应含两个持仓估值: %+v", rec.Valuations)
	}
	// 估值按合约代码排序。
	if rec.Valuations[0].Symbol != "A" || rec.Valuations[1].Symbol != "B" {
		t.Fatalf("估值应按合约代码排序: %+v", rec.Valuations)
	}
	if rec.Valuations[0].QuoteSeq != 2 || rec.Valuations[0].QuotePrice != 1 {
		t.Fatalf("A 估值快照错误: %+v", rec.Valuations[0])
	}
	if rec.Valuations[1].QuoteSeq != 2 || rec.Valuations[1].QuotePrice != 1 {
		t.Fatalf("B 估值快照错误: %+v", rec.Valuations[1])
	}
}

func TestProfitRecordedAsZeroLoss(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	// 报价涨到 20：净值 = 100×20 = 2000，盈利，亏损记零。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 1)
	rs := e.RiskStatus()
	if rs.Loss != 0 || rs.NetValue != 2000 || rs.Restricted {
		t.Fatalf("盈利时亏损应记零: %+v", rs)
	}
}

func TestRiskTriggerKindString(t *testing.T) {
	cases := map[RiskTriggerKind]string{
		RiskTriggerDay:   "开日",
		RiskTriggerLimit: "调限额",
		RiskTriggerQuote: "报价",
		RiskTriggerFill:  "成交",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Fatalf("RiskTriggerKind(%d).String()=%q，期望 %q", k, got, want)
		}
	}
	if got := RecordRiskTriggered.String(); got != "风险触线" {
		t.Fatalf("RecordRiskTriggered.String()=%q", got)
	}
}

func TestStartDayWithExistingPositions(t *testing.T) {
	// 上一日持仓过夜，新日基准包含持仓市值。
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 50, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 50}); err != nil {
		t.Fatal(err)
	}
	// 现金 500，持仓 50，报价 10 → 净值 1000。
	if rs := e.RiskStatus(); rs.NetValue != 1000 {
		t.Fatalf("净值应为 1000: %+v", rs)
	}
	// 新日：基准 = 500 + 50×10 = 1000。
	if err := e.StartDay(2, 100); err != nil {
		t.Fatal(err)
	}
	rs := e.RiskStatus()
	if rs.Baseline != 1000 || rs.Day != 2 || rs.Restricted {
		t.Fatalf("新日基准应包含持仓市值: %+v", rs)
	}
}

func TestSameDaySameLimitNoChange(t *testing.T) {
	e, _ := NewEngine(1000)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	recsBefore := len(e.Records())
	// 同日号同上限重复开日：不产生变化。
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	if len(e.Records()) != recsBefore {
		t.Fatal("同日号同上限不应产生新记录")
	}
	if rs := e.RiskStatus(); rs.Day != 1 || rs.Baseline != 1000 {
		t.Fatalf("重复开日不应改变状态: %+v", rs)
	}
}

func TestBackwardDayRejected(t *testing.T) {
	e, _ := NewEngine(1000)
	if err := e.StartDay(5, 100); err != nil {
		t.Fatal(err)
	}
	for _, day := range []int64{4, 0, -1} {
		if err := e.StartDay(day, 100); err == nil {
			t.Fatalf("倒退日号 %d 必须报错", day)
		}
	}
	if rs := e.RiskStatus(); rs.Day != 5 {
		t.Fatalf("报错后日号应保持 5: %+v", rs)
	}
}

func TestSetLimitWhenNotStartedErrors(t *testing.T) {
	e, _ := NewEngine(1000)
	if err := e.SetLimit(100); err == nil {
		t.Fatal("未开日时调整上限必须报错")
	}
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	// 同日号不同上限报错。
	if err := e.StartDay(1, 200); err == nil {
		t.Fatal("同日号不同上限必须报错")
	}
	// 报错后上限不变。
	if rs := e.RiskStatus(); rs.Limit != 100 {
		t.Fatalf("上限应保持 100: %+v", rs)
	}
}

func TestRejectRecordContainsReason(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	if err := e.StartDay(1, 100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 100, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 100}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 9}, 1) // 触线
	_, err := e.Buy("A", 1, 10)
	if err == nil {
		t.Fatal("触线后买单必须拒绝")
	}
	if !strings.Contains(err.Error(), "日内亏损已触线") {
		t.Fatalf("拒绝原因应说明触线: %v", err)
	}
}
