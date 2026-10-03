package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func mustQuote(t *testing.T, e *Engine, symbol string, q Quote, wantApplied int) {
	t.Helper()
	n, err := e.UpdateQuote(symbol, q)
	if err != nil {
		t.Fatalf("UpdateQuote(%+v) 意外错误: %v", q, err)
	}
	if n != wantApplied {
		t.Fatalf("UpdateQuote(seq=%d) 生效条数=%d, 期望 %d", q.Seq, n, wantApplied)
	}
}

func mustSetMax(t *testing.T, e *Engine, symbol string, max int64) []int64 {
	t.Helper()
	ids, err := e.SetMaxPosition(symbol, max)
	if err != nil {
		t.Fatalf("SetMaxPosition(%d) 意外错误: %v", max, err)
	}
	return ids
}

func mustBuy(t *testing.T, e *Engine, symbol string, qty, limit int64) int64 {
	t.Helper()
	id, err := e.Buy(symbol, qty, limit)
	if err != nil {
		t.Fatalf("Buy(%s,%d,%d) 意外错误: %v", symbol, qty, limit, err)
	}
	return id
}

func mustSell(t *testing.T, e *Engine, symbol string, qty, limit int64) int64 {
	t.Helper()
	id, err := e.Sell(symbol, qty, limit)
	if err != nil {
		t.Fatalf("Sell(%s,%d,%d) 意外错误: %v", symbol, qty, limit, err)
	}
	return id
}

func expectErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望包含 %q 的错误，实际为 nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("错误 %q 不包含 %q", err.Error(), substr)
	}
}

func TestNewEngineRejectsNegativeCash(t *testing.T) {
	if _, err := NewEngine(-1); err == nil {
		t.Fatal("负初始现金必须报错")
	}
	e, err := NewEngine(0)
	if err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 0 || e.AvailableCash() != 0 || e.ReservedCash() != 0 {
		t.Fatal("零现金初始查询不正确")
	}
}

func TestQuoteSequenceAndGap(t *testing.T) {
	e, _ := NewEngine(1000)

	if _, err := e.UpdateQuote("A", Quote{Seq: 0, Moment: 1, Price: 10}); err == nil {
		t.Fatal("序号 0 必须报错")
	}
	if _, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 1, Price: 0}); err == nil {
		t.Fatal("非正价格必须报错")
	}
	if _, err := e.UpdateQuote("", Quote{Seq: 1, Moment: 1, Price: 10}); err == nil {
		t.Fatal("空合约必须报错")
	}

	// 尚无有效报价：拒绝下单（即使已设置限额）。
	mustSetMax(t, e, "A", 100)
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("无报价时下单必须拒绝")
	}

	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	if e.HasGap("A") {
		t.Fatal("首个连续报价后不应有缺口")
	}

	// 跨号报价先等待。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 12}, 0)
	if !e.HasGap("A") {
		t.Fatal("跨号后应有缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 {
		t.Fatalf("缺口期间最新报价应停留在 1，实际 %+v ok=%v", q, ok)
	}

	// 缺口时拒绝新买单；卖单在有持仓时仍可提交（此处无持仓，用现金限额验证买单路径）。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("报价缺口时新买单必须拒绝")
	}

	// 补齐 seq2 后，2、3 依次生效。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 11}, 2)
	if e.HasGap("A") {
		t.Fatal("补齐后不应再有缺口")
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 3 || q.Moment != 30 || q.Price != 12 {
		t.Fatalf("补齐后最新报价应为 seq3，实际 %+v ok=%v", q, ok)
	}

	// 重复序号内容一致：不产生新事件。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 11}, 0)

	// 重复序号内容不同：报错且保留原值。
	if _, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 99, Price: 11}); err == nil {
		t.Fatal("重复序号不同时刻必须报错")
	}
	if _, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 99}); err == nil {
		t.Fatal("重复序号不同价格必须报错")
	}
	q, _ = e.CurrentQuote("A")
	if q.Seq != 3 {
		t.Fatal("冲突报价不得改变最新报价")
	}
}

func TestBuyReservesCashAndMaxPosition(t *testing.T) {
	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 5)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)

	// 未设置限额的合约拒绝下单。
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	if _, err := e.Buy("B", 1, 1); err == nil {
		t.Fatal("未设置最大持仓量必须拒绝")
	}

	// 错误输入不改变状态。
	if _, err := e.Buy("A", 0, 10); err == nil {
		t.Fatal("数量 0 必须拒绝")
	}
	if _, err := e.Buy("A", 1, -1); err == nil {
		t.Fatal("负限价必须拒绝")
	}
	if got := e.ReservedCash(); got != 0 {
		t.Fatalf("被拒绝委托不得占用现金，reserved=%d", got)
	}

	// 现金不足：20*6=120 > 100。
	if _, err := e.Buy("A", 6, 20); err == nil {
		t.Fatal("现金不足必须拒绝")
	}

	// 超最大持仓量：持仓 0 + 买单剩余 0 + 6 > 5。
	if _, err := e.Buy("A", 6, 10); err == nil {
		t.Fatal("超最大持仓量必须拒绝")
	}

	id := mustBuy(t, e, "A", 4, 20) // 占用 80 现金、4 持仓额度
	if e.Cash() != 100 || e.ReservedCash() != 80 || e.AvailableCash() != 20 {
		t.Fatalf("现金占用错误: cash=%d reserved=%d available=%d", e.Cash(), e.ReservedCash(), e.AvailableCash())
	}

	// 剩余持仓额度只有 1（持仓 0 + 买单 4），2 股超限必须拒绝。
	if _, err := e.Buy("A", 2, 10); err == nil {
		t.Fatal("持仓额度不足必须拒绝")
	}
	// 1 股限价 20：现金 20 与持仓额度 1 均恰好满足，接受；
	// 已成交占用不重复——两单合计占用全部 100 现金。
	mustBuy(t, e, "A", 1, 20)
	if e.AvailableCash() != 0 || e.ReservedCash() != 100 {
		t.Fatalf("两笔买单应占用全部现金: available=%d reserved=%d", e.AvailableCash(), e.ReservedCash())
	}
	// 此后任何新买单都因现金不足被拒，且不重复占用。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("现金全部占用后新买单必须拒绝")
	}
	if e.ReservedCash() != 100 {
		t.Fatalf("被拒绝委托不得改变占用: reserved=%d", e.ReservedCash())
	}

	o, _ := e.Order(id)
	if o.Status != StatusPending || o.Remaining() != 4 {
		t.Fatalf("订单状态错误: %+v", o)
	}
}

func TestSellRequiresPositionAndReserves(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)

	if _, err := e.Sell("A", 1, 10); err == nil {
		t.Fatal("零持仓不能卖空")
	}

	bid := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 10 || e.Sellable("A") != 10 {
		t.Fatalf("成交后持仓错误: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}

	sid := mustSell(t, e, "A", 6, 9) // 占用 6 可卖
	if e.Sellable("A") != 4 {
		t.Fatalf("卖单占用后可卖应为 4，实际 %d", e.Sellable("A"))
	}
	if _, err := e.Sell("A", 5, 9); err == nil {
		t.Fatal("可卖不足必须拒绝")
	}
	mustSell(t, e, "A", 4, 9)
	if e.Sellable("A") != 0 {
		t.Fatalf("全部可卖占用后应为 0，实际 %d", e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPending || o.Remaining() != 6 {
		t.Fatalf("卖单应保持待成交、剩余 6: %+v", o)
	}
}

func TestFillPartialReleasesLimitDifference(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)

	id := mustBuy(t, e, "A", 10, 10) // 占用 100

	// 买入价高于限价：整笔拒绝。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 11, Qty: 1}); err == nil {
		t.Fatal("买入价高于限价必须拒绝")
	}
	// 成交量超过剩余量：整笔拒绝。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 11}); err == nil {
		t.Fatal("超量成交必须拒绝")
	}
	// 不存在的订单：整笔拒绝。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: 999, Symbol: "A", Side: Buy, Price: 10, Qty: 1}); err == nil {
		t.Fatal("订单不存在必须拒绝")
	}
	// 被拒绝成交后状态不变。
	if e.Cash() != 1000 || e.ReservedCash() != 100 || e.Position("A") != 0 {
		t.Fatalf("拒绝成交不得改状态: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 部分成交 4 股，价格 8（低于限价 10）：按实际金额 32 扣现金，价差 8 立即释放。
	res, err := e.Fill(Trade{TradeID: 4, OrderID: id, Symbol: "A", Side: Buy, Price: 8, Qty: 4})
	if err != nil {
		t.Fatal(err)
	}
	if res.Filled != 4 || res.Remaining != 6 || res.Status != StatusPartial {
		t.Fatalf("成交结果错误: %+v", res)
	}
	if e.Cash() != 968 || e.ReservedCash() != 60 || e.AvailableCash() != 908 {
		t.Fatalf("部分买入后资金错误: cash=%d reserved=%d available=%d", e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 {
		t.Fatalf("持仓应为 4，实际 %d", e.Position("A"))
	}

	// 剩余 6 股以限价成交后全部完成。
	res, err = e.Fill(Trade{TradeID: 5, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFilled || e.ReservedCash() != 0 {
		t.Fatalf("全部成交后占用应清零: %+v reserved=%d", res, e.ReservedCash())
	}
	if e.Cash() != 908 {
		t.Fatalf("现金余额应为 968-60=908，实际 %d", e.Cash())
	}

	if err := e.Cancel(id); err == nil {
		t.Fatal("全部成交的订单不能撤销")
	}
}

func TestSellFillSettlementAndCancel(t *testing.T) {
	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 0 {
		t.Fatalf("买入全额后现金应为 0，实际 %d", e.Cash())
	}

	sid := mustSell(t, e, "A", 10, 7)
	// 卖出价低于限价必须拒绝。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 6, Qty: 1}); err == nil {
		t.Fatal("卖出价低于限价必须拒绝")
	}
	// 部分成交 3 股 @8：现金 +24，持仓 -3，释放 3 占用。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 8, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 24 || e.Position("A") != 7 || e.Sellable("A") != 0 {
		t.Fatalf("卖出部分成交后状态错误: cash=%d pos=%d sellable=%d", e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 撤销剩余 7 股只释放未成交部分；已成交 3 股保留。
	if err := e.Cancel(sid); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 7 || e.Sellable("A") != 7 {
		t.Fatalf("撤单应释放 7 股可卖: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(sid)
	if o.Status != StatusCanceled || o.Filled != 3 || o.Remaining() != 0 {
		t.Fatalf("撤单后订单状态错误: %+v", o)
	}
	// 已撤销订单不能再成交。
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: sid, Symbol: "A", Side: Sell, Price: 8, Qty: 1}); err == nil {
		t.Fatal("已撤销订单的成交必须拒绝")
	}
	if err := e.Cancel(sid); err == nil {
		t.Fatal("重复撤销必须报错")
	}
	if err := e.Cancel(12345); err == nil {
		t.Fatal("撤销不存在订单必须报错")
	}
}

func TestFillIdempotency(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 10, 10)

	tr := Trade{TradeID: 77, OrderID: id, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	recordsBefore := len(e.Records())

	// 相同编号相同内容：返回原结果，不新增事件，不再次记账。
	res2, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res1 != res2 {
		t.Fatalf("幂等结果不一致: %+v vs %+v", res1, res2)
	}
	if len(e.Records()) != recordsBefore {
		t.Fatal("幂等成交不得新增事件记录")
	}
	if e.Cash() != 1000-36 || e.Position("A") != 4 || e.ReservedCash() != 60 {
		t.Fatalf("幂等重放不得再次记账: cash=%d pos=%d reserved=%d", e.Cash(), e.Position("A"), e.ReservedCash())
	}

	// 撤销订单后，旧编号重放仍返回原结果，不能再次记账。
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	res3, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res3 != res1 {
		t.Fatalf("撤单后幂等结果应保持: %+v vs %+v", res3, res1)
	}
	if e.Cash() != 1000-36 || e.Position("A") != 4 {
		t.Fatalf("撤单后重放旧成交不得记账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	// 同一编号不同内容：报错且不改状态。记录条数会增加一条拒绝。
	before := len(e.Records())
	if _, err := e.Fill(Trade{TradeID: 77, OrderID: id, Symbol: "A", Side: Buy, Price: 8, Qty: 4}); err == nil {
		t.Fatal("同一编号不同内容必须报错")
	}
	if len(e.Records()) != before+1 {
		t.Fatal("编号冲突拒绝应记录一条拒绝")
	}

	// 被拒绝的成交不占用编号：新订单修正后仍可用新编号提交。
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 5}, 1)
	mustSetMax(t, e, "B", 100)
	b2 := mustBuy(t, e, "B", 10, 5)
	// 先用编号 88 提交一笔会被拒绝的成交（超量）。
	if _, err := e.Fill(Trade{TradeID: 88, OrderID: b2, Symbol: "B", Side: Buy, Price: 5, Qty: 99}); err == nil {
		t.Fatal("超量成交应拒绝")
	}
	// 编号 88 未被占用，修正数量后提交成功。
	if _, err := e.Fill(Trade{TradeID: 88, OrderID: b2, Symbol: "B", Side: Buy, Price: 5, Qty: 2}); err != nil {
		t.Fatalf("被拒绝编号应可复用: %v", err)
	}
}

func TestLowerMaxPositionCancelsLastBuys(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)

	b1 := mustBuy(t, e, "A", 3, 10) // 最早接受，占用 30
	b2 := mustBuy(t, e, "A", 3, 10)
	b3 := mustBuy(t, e, "A", 3, 10) // 最后接受，持仓占用共 9

	// b1 部分成交 2 股 @9：现金 -18，持仓 2，买单剩余额度 7。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b1, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	// 此时 持仓 2 + 买单剩余 7 = 9。下调限额到 5：
	// 先取消 b3（剩余 3）→ 2+4=6 仍超；再取消 b2（剩余 3）→ 2+1 满足。
	canceled := mustSetMax(t, e, "A", 5)
	if len(canceled) != 2 || canceled[0] != b3 || canceled[1] != b2 {
		t.Fatalf("应按最后接受顺序撤销 b3,b2，实际 %v", canceled)
	}
	if e.ReservedCash() != 10 { // b1 剩余 1 股 × 10
		t.Fatalf("撤单释放现金错误: reserved=%d", e.ReservedCash())
	}
	if e.Cash() != 1000-18 {
		t.Fatalf("撤单不得触动已成交金额: cash=%d", e.Cash())
	}
	o1, _ := e.Order(b1)
	if o1.Status != StatusPartial {
		t.Fatal("b1 应保持部分成交")
	}

	// 再下调到 1：持仓 2 已超限，撤掉全部未成交买单（b1 剩余 1）。
	canceled = mustSetMax(t, e, "A", 1)
	if len(canceled) != 1 || canceled[0] != b1 {
		t.Fatalf("持仓超限时应撤销 b1，实际 %v", canceled)
	}
	if e.ReservedCash() != 0 || e.Position("A") != 2 {
		t.Fatalf("撤销全部买单后 reserved=0、持仓保留: reserved=%d pos=%d", e.ReservedCash(), e.Position("A"))
	}
	// 持仓超限：后续买单拒绝。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("持仓超限时新买单必须拒绝")
	}
	// 仍允许卖出，不自动平仓。
	sid := mustSell(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 0 || e.Cash() != 1000-18+20 {
		t.Fatalf("卖出后状态错误: pos=%d cash=%d", e.Position("A"), e.Cash())
	}

	// 负限额报错。
	if _, err := e.SetMaxPosition("A", -1); err == nil {
		t.Fatal("负最大持仓量必须报错")
	}
}

func TestRecordsSnapshotsAndImmutability(t *testing.T) {
	e, _ := NewEngine(1000)

	// 无报价、无限额时的拒绝记录：快照明确为空。
	_, err := e.Buy("X", 1, 10)
	expectErr(t, err, "尚未设置最大持仓量")
	recs := e.Records()
	if len(recs) != 1 || recs[0].Kind != RecordRejected || recs[0].QuoteValid || recs[0].MaxPositionValid {
		t.Fatalf("拒绝记录快照应为空: %+v", recs)
	}
	if recs[0].Reason == "" {
		t.Fatal("拒绝记录必须包含具体原因")
	}

	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 100, Price: 10}, 1)
	id := mustBuy(t, e, "A", 4, 10)

	// 接受记录快照：报价 seq1 与限额 10。
	rec := e.Records()[1]
	if rec.Kind != RecordAccepted || !rec.QuoteValid || rec.QuoteSeq != 1 || rec.QuoteMoment != 100 ||
		rec.QuotePrice != 10 || !rec.MaxPositionValid || rec.MaxPosition != 10 {
		t.Fatalf("接受记录快照错误: %+v", rec)
	}

	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 200, Price: 20}, 1)
	mustSetMax(t, e, "A", 99)

	// 后续报价与限额变化不能改写历史记录，且成交金额不重算。
	rec = e.Records()[2]
	if rec.Kind != RecordFilled || rec.QuoteSeq != 1 || rec.QuotePrice != 10 || rec.MaxPosition != 10 {
		t.Fatalf("成交记录被后续变化改写: %+v", rec)
	}
	if e.Cash() != 1000-18 {
		t.Fatalf("报价变化不得重算已成交金额: cash=%d", e.Cash())
	}

	// 限额下调触发的撤销记录带原因与当时快照（seq2, 限额99）。
	canceled := mustSetMax(t, e, "A", 1) // 持仓 2 超限，撤销剩余买单 2
	if len(canceled) != 1 {
		t.Fatalf("应撤销 1 单，实际 %v", canceled)
	}
	rec = e.Records()[len(e.Records())-1]
	if rec.Kind != RecordCanceled || rec.Reason == "" || !rec.QuoteValid ||
		rec.QuoteSeq != 2 || rec.QuotePrice != 20 || rec.MaxPosition != 1 {
		t.Fatalf("限额撤销记录快照/原因错误: %+v", rec)
	}
	if rec.Remaining != 2 || rec.Filled != 2 {
		t.Fatalf("撤销记录应区分已成交 2 与剩余 2: %+v", rec)
	}

	// 状态枚举中文可辨识。
	if StatusPending.String() != "待成交" || StatusPartial.String() != "部分成交" ||
		StatusFilled.String() != "全部成交" || StatusCanceled.String() != "已撤销" {
		t.Fatal("订单状态文案错误")
	}
}

func TestCancelUnfilledBuyReleasesCash(t *testing.T) {
	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 5, 10)
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 100 || e.ReservedCash() != 0 || e.AvailableCash() != 100 {
		t.Fatalf("撤销未成交买单应全额释放现金: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
	o, _ := e.Order(id)
	if o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("订单应标记撤销: %+v", o)
	}
}

func TestGapAllowsSellCancelAndFill(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 5, 10)

	// 制造缺口：seq3 先到。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 10}, 0)

	// 缺口期间：买单拒绝，但卖单、撤单、已有订单成交均可处理。
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("缺口时买单必须拒绝")
	}
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 2}); err != nil {
		t.Fatalf("缺口时已有卖单应能成交: %v", err)
	}
	extra := mustSell(t, e, "A", 2, 10)
	if err := e.Cancel(extra); err != nil {
		t.Fatalf("缺口时撤单应可处理: %v", err)
	}
	if e.Position("A") != 8 {
		t.Fatalf("缺口期间卖出成交后持仓应为 8，实际 %d", e.Position("A"))
	}
}

func TestSellFillCashOverflowRejectedWithoutRiskOpen(t *testing.T) {
	// 未开启交易日、未设置金额上限：卖出所得加回现金后溢出 int64 也必须整笔拒绝。
	e, _ := NewEngine(math.MaxInt64 - 5)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e, "A", 1, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	cash := e.Cash() // MaxInt64-6
	sid := mustSell(t, e, "A", 1, 1)
	before := len(e.Records())

	// 成交金额 10 本身未溢出，但加回现金后无法表示：整笔拒绝。
	res, err := e.Fill(Trade{TradeID: 7, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("卖出现金溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if res != (FillResult{}) {
		t.Fatalf("溢出拒绝必须返回零值成交结果，实际 %+v", res)
	}
	if e.Cash() != cash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("溢出成交不得改变资金与持仓: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(sid)
	if o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("卖单占用不得提前释放: %+v", o)
	}

	// 只追加一条拒绝记录，且固化成交要素与当时的报价、持仓限额快照。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rec := recs[len(recs)-1]
	if rec.Kind != RecordRejected || rec.TradeID != 7 || rec.OrderID != sid ||
		rec.Symbol != "A" || rec.Side != Sell || rec.TradePrice != 10 || rec.Qty != 1 {
		t.Fatalf("拒绝记录要素错误: %+v", rec)
	}
	if !strings.Contains(rec.Reason, "超出") {
		t.Fatalf("拒绝原因应说明加回现金后超出范围: %q", rec.Reason)
	}
	if !rec.QuoteValid || rec.QuoteSeq != 1 || rec.QuotePrice != 1 {
		t.Fatalf("拒绝记录缺少报价快照: %+v", rec)
	}
	if !rec.MaxPositionValid || rec.MaxPosition != 10 {
		t.Fatalf("拒绝记录缺少持仓限额快照: %+v", rec)
	}

	// 编号未被占用：修正价格后同编号可成交；现金恰好到达 MaxInt64 允许。
	res, err = e.Fill(Trade{TradeID: 7, OrderID: sid, Symbol: "A", Side: Sell, Price: 6, Qty: 1})
	if err != nil {
		t.Fatalf("被溢出拒绝的成交编号应可复用: %v", err)
	}
	if e.Cash() != math.MaxInt64 || e.Position("A") != 0 {
		t.Fatalf("修正后成交应正常入账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	if res.Status != StatusFilled || res.Filled != 1 || res.Remaining != 0 {
		t.Fatalf("修正后成交结果错误: %+v", res)
	}
	// 已入账成交幂等重放：返回原结果，不因余额变化重新结算。
	again, err := e.Fill(Trade{TradeID: 7, OrderID: sid, Symbol: "A", Side: Sell, Price: 6, Qty: 1})
	if err != nil || again != res {
		t.Fatalf("幂等重放应返回原结果: %+v, %v", again, err)
	}
	if e.Cash() != math.MaxInt64 {
		t.Fatalf("幂等重放不得再次记账: cash=%d", e.Cash())
	}
}

func TestSellFillCashOverflowPartialFillKept(t *testing.T) {
	// 部分成交后的卖单遇到现金溢出：只拒绝本次成交，已入账部分保留。
	e, _ := NewEngine(math.MaxInt64 - 12)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 5, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	cash := e.Cash() // MaxInt64-9

	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("第二笔卖出现金溢出必须拒绝，实际 %v", err)
	}
	if e.Cash() != cash || e.Position("A") != 1 {
		t.Fatalf("之前入账的现金与持仓必须保留: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	o, _ := e.Order(sid)
	if o.Filled != 1 || o.Remaining() != 1 || o.Status != StatusPartial {
		t.Fatalf("卖单累计成交量与剩余量必须保留: %+v", o)
	}
	// 修正后同编号成交：按实际成交金额入账。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 9, Qty: 1}); err != nil {
		t.Fatalf("修正后同编号成交应成功: %v", err)
	}
	if e.Cash() != math.MaxInt64 || e.Position("A") != 0 {
		t.Fatalf("修正后成交应正常入账: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
}
