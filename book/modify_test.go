package book

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func mustModify(t *testing.T, e *Engine, id, qty, limit int64) {
	t.Helper()
	if err := e.Modify(id, qty, limit); err != nil {
		t.Fatalf("Modify(%d, %d, %d) 意外错误: %v", id, qty, limit, err)
	}
}

// setupModify 准备：现金 1000，A 已设持仓限额 100，报价 seq1@10。
func setupModify(t *testing.T) *Engine {
	t.Helper()
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	return e
}

func lastModifyRecord(t *testing.T, e *Engine) Record {
	t.Helper()
	for i := len(e.records) - 1; i >= 0; i-- {
		if e.records[i].Kind == RecordModified {
			return e.records[i]
		}
	}
	t.Fatal("不存在修改记录")
	return Record{}
}

func TestModifyValidationAndRejections(t *testing.T) {
	e := setupModify(t)

	recordsBefore := len(e.Records())

	// 订单不存在。
	if err := e.Modify(999, 10, 10); err == nil {
		t.Fatal("修改不存在的订单必须报错")
	}
	bid := mustBuy(t, e, "A", 10, 10) // 占用 100

	// 非法参数。
	for _, c := range []struct{ qty, limit int64 }{
		{0, 10}, {-1, 10}, {10, 0}, {10, -5}, {0, 0},
	} {
		if err := e.Modify(bid, c.qty, c.limit); err == nil {
			t.Fatalf("Modify(qty=%d,limit=%d) 必须报错", c.qty, c.limit)
		}
	}

	// 已撤销订单不能修改。
	canceled := mustBuy(t, e, "A", 1, 10)
	if err := e.Cancel(canceled); err != nil {
		t.Fatal(err)
	}
	if err := e.Modify(canceled, 1, 10); err == nil {
		t.Fatal("已撤销订单不能修改")
	}

	// 新总量必须大于已成交量；全部成交订单不能修改。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if err := e.Modify(bid, 4, 10); err == nil {
		t.Fatal("新总量等于已成交量必须拒绝")
	}
	if err := e.Modify(bid, 2, 10); err == nil {
		t.Fatal("新总量小于已成交量必须拒绝")
	}
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 6}); err != nil {
		t.Fatal(err)
	}
	if err := e.Modify(bid, 12, 10); err == nil {
		t.Fatal("全部成交订单不能修改")
	}

	// 每次失败恰好一条拒绝记录，原因明确；资金、持仓与订单参数不变。
	var rejectCount int
	for _, r := range e.Records()[recordsBefore:] {
		if r.Kind == RecordRejected {
			rejectCount++
			if r.Reason == "" {
				t.Fatalf("修改失败必须留明确原因: %+v", r)
			}
		}
	}
	wantRejects := 1 + 5 + 1 + 2 + 1 // 不存在 + 非法参数 + 已撤销 + 总量不足 + 全部成交
	if rejectCount != wantRejects {
		t.Fatalf("拒绝记录条数错误: %d，期望 %d", rejectCount, wantRejects)
	}
	if e.Cash() != 904 || e.ReservedCash() != 0 || e.Position("A") != 10 {
		t.Fatalf("拒绝修改不得改变资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(bid); o.Limit != 10 || o.Qty != 10 || o.Filled != 10 || o.Status != StatusFilled {
		t.Fatalf("拒绝修改不得改变订单: %+v", o)
	}
	if o, _ := e.Order(canceled); o.Status != StatusCanceled || o.Reason == "" {
		t.Fatalf("已撤销订单原因必须保留: %+v", o)
	}
}

func TestModifyPendingBuyChangesReservationAndRecord(t *testing.T) {
	e := setupModify(t)
	id := mustBuy(t, e, "A", 10, 10) // 占用 100
	before := len(e.Records())

	mustModify(t, e, id, 8, 12) // 剩余 8 × 12 = 96
	if e.Cash() != 1000 || e.ReservedCash() != 96 || e.AvailableCash() != 904 {
		t.Fatalf("修改后现金占用错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	o, _ := e.Order(id)
	if o.Qty != 8 || o.Limit != 12 || o.Filled != 0 || o.Status != StatusPending || o.Remaining() != 8 {
		t.Fatalf("订单查询应显示新参数: %+v", o)
	}
	if len(e.Records()) != before+1 {
		t.Fatal("真实修改必须恰好新增一条记录")
	}
	r := lastModifyRecord(t, e)
	if r.Kind != RecordModified || r.OrderID != id ||
		r.OldLimit != 10 || r.OldQty != 10 || r.OldFilled != 0 ||
		r.Limit != 12 || r.Qty != 8 || r.Filled != 0 || r.Remaining != 8 {
		t.Fatalf("修改记录前后参数错误: %+v", r)
	}
	if !r.QuoteValid || r.QuoteSeq != 1 || r.QuotePrice != 10 ||
		!r.MaxPositionValid || r.MaxPosition != 100 {
		t.Fatalf("修改记录必须固化当时报价与限额: %+v", r)
	}

	// 重复提交当前参数：成功返回、不新增记录、不动占用。
	if err := e.Modify(id, 8, 12); err != nil {
		t.Fatalf("重复提交当前参数应成功: %v", err)
	}
	if len(e.Records()) != before+1 || e.ReservedCash() != 96 {
		t.Fatal("重复提交当前参数不得新增记录或改变占用")
	}
}

func TestModifyPartialBuySpecExample(t *testing.T) {
	e := setupModify(t)
	id := mustBuy(t, e, "A", 10, 10)
	// 买入总量 10、已成交 4（成交价 9）。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 964 || e.ReservedCash() != 60 || e.Position("A") != 4 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 改为总量 8、限价 12：剩余 4 只占用 48；现金余额与已入账持仓不变。
	mustModify(t, e, id, 8, 12)
	if e.Cash() != 964 || e.ReservedCash() != 48 || e.AvailableCash() != 916 || e.Position("A") != 4 {
		t.Fatalf("修改只替换未成交部分占用: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(id)
	if o.Qty != 8 || o.Limit != 12 || o.Filled != 4 || o.Status != StatusPartial || o.Remaining() != 4 {
		t.Fatalf("部分成交订单修改后状态错误: %+v", o)
	}
	r := lastModifyRecord(t, e)
	if r.OldLimit != 10 || r.OldQty != 10 || r.OldFilled != 4 ||
		r.Limit != 12 || r.Qty != 8 || r.Filled != 4 || r.Remaining != 4 {
		t.Fatalf("修改记录必须保存前后参数与已成交量: %+v", r)
	}

	// 历史成交不重新计价：此后成交按新限价判断。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: id, Symbol: "A", Side: Buy, Price: 13, Qty: 1}); err == nil {
		t.Fatal("高于新限价 12 的成交必须拒绝")
	}
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: id, Symbol: "A", Side: Buy, Price: 12, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 916 || e.ReservedCash() != 0 || e.Position("A") != 8 {
		t.Fatalf("剩余部分按新限价成交后账目错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(id); o.Status != StatusFilled {
		t.Fatalf("应全部成交: %+v", o)
	}
}

func TestModifySellAdjustsOnlyUnfilledReservation(t *testing.T) {
	e := setupModify(t)
	bid := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 6, 9) // 占用 6 可卖
	if e.Sellable("A") != 4 {
		t.Fatalf("前置可卖错误: %d", e.Sellable("A"))
	}

	mustModify(t, e, sid, 8, 9) // 未成交部分占用改为 8
	if e.Sellable("A") != 2 || e.Position("A") != 10 {
		t.Fatalf("卖单改大只增加未成交部分占用: sellable=%d pos=%d", e.Sellable("A"), e.Position("A"))
	}
	mustModify(t, e, sid, 8, 8) // 改限价不影响可卖占用
	if e.Sellable("A") != 2 {
		t.Fatalf("卖单改限价不得改变可卖数量: %d", e.Sellable("A"))
	}
	before := len(e.Records())
	if err := e.Modify(sid, 11, 9); err == nil {
		t.Fatal("占用超过持仓必须拒绝")
	}
	if e.Sellable("A") != 2 || len(e.Records()) != before+1 {
		t.Fatalf("拒绝后可卖与记录条数错误: sellable=%d records=%d", e.Sellable("A"), len(e.Records())-before)
	}

	// 其他卖单占用保留：本单 8 + 另一单 2 恰好全部可卖。
	other := mustSell(t, e, "A", 2, 9)
	if e.Sellable("A") != 0 {
		t.Fatalf("两单应占用全部可卖: %d", e.Sellable("A"))
	}
	if err := e.Modify(sid, 9, 9); err == nil {
		t.Fatal("其他卖单占用必须保留，不得超额")
	}
	mustModify(t, e, sid, 8, 9) // 恰好 8+2=10 允许
	if o, _ := e.Order(other); o.Status != StatusPending || o.Qty != 2 {
		t.Fatalf("其他卖单不得受影响: %+v", o)
	}

	// 部分成交卖单修改：成交 3 股后持仓 7，本单剩余 5。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 9, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 7 || e.Sellable("A") != 0 {
		t.Fatalf("卖出成交后状态错误（另一卖单仍占 2）: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	// 其他卖单仍占 2：本单剩余改为 6（总占用 2+6=8>7）必须拒绝。
	if err := e.Modify(sid, 9, 9); err == nil {
		t.Fatal("超过当前持仓（含其他卖单占用）必须拒绝")
	}
	// 改为总量 6、限价 8：新剩余 3（2+3=5 ≤ 7），允许。
	mustModify(t, e, sid, 6, 8)
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 7, Qty: 1}); err == nil {
		t.Fatal("低于新限价 8 的成交必须拒绝")
	}
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: sid, Symbol: "A", Side: Sell, Price: 8, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("按新参数成交后持仓/可卖错误（另一卖单仍占 2）: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusFilled {
		t.Fatalf("本单应全部成交: %+v", o)
	}
}

func TestModifyCashReplacementKeepsOtherOrders(t *testing.T) {
	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 4, 20) // 80
	b2 := mustBuy(t, e, "A", 1, 20) // 20，合计 100 恰满
	if e.AvailableCash() != 0 {
		t.Fatalf("前置应占满现金: %d", e.AvailableCash())
	}

	// 改小释放占用，其他订单占用保留。
	mustModify(t, e, b1, 3, 20)
	if e.ReservedCash() != 80 || e.AvailableCash() != 20 {
		t.Fatalf("改小后占用应为 60+20=80: reserved=%d", e.ReservedCash())
	}
	// 改回恰好满额允许。
	mustModify(t, e, b1, 4, 20)
	if e.ReservedCash() != 100 {
		t.Fatalf("恰好用满现金额度应允许: reserved=%d", e.ReservedCash())
	}
	// 改大致现金不足：拒绝且不撤销其他订单。
	before := len(e.Records())
	if err := e.Modify(b1, 5, 20); err == nil {
		t.Fatal("现金不足必须拒绝修改")
	}
	if len(e.Records()) != before+1 {
		t.Fatal("修改失败只能增加一条拒绝记录")
	}
	if e.ReservedCash() != 100 || e.AvailableCash() != 0 {
		t.Fatalf("拒绝修改不得改变占用: reserved=%d", e.ReservedCash())
	}
	if o, _ := e.Order(b1); o.Qty != 4 || o.Limit != 20 {
		t.Fatalf("被拒订单保持原参数: %+v", o)
	}
	if o, _ := e.Order(b2); o.Status != StatusPending || o.Qty != 1 {
		t.Fatalf("绝不能撤销其他订单腾额度: %+v", o)
	}
}

func TestModifyMaxPositionReplacement(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 5)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 4, 10)

	mustModify(t, e, b1, 5, 10) // 持仓 0 + 剩余 5 = 5 恰好允许
	if err := e.Modify(b1, 6, 10); err == nil {
		t.Fatal("超过合约持仓限额必须拒绝")
	}
	mustModify(t, e, b1, 4, 10)
	b2 := mustBuy(t, e, "A", 1, 10) // 4+1=5 满
	if err := e.Modify(b1, 5, 10); err == nil {
		t.Fatal("替换后 5+1=6 超限必须拒绝")
	}
	if o, _ := e.Order(b2); o.Status != StatusPending {
		t.Fatalf("限额不足不得影响其他订单: %+v", o)
	}
	mustModify(t, e, b1, 3, 10) // 3+1=4，释放 1 股额度
}

func TestModifyAmountLimitReplacementAndRejectSnapshot(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 5, 12) // 5×max(12,10)=60
	if st := e.PositionAmountStatus(); st.BuyReserved != 60 || st.Total != 60 {
		t.Fatalf("前置占用错误: %+v", st)
	}

	// 降低限价：金额占用降为 50。
	mustModify(t, e, id, 5, 10)
	if st := e.PositionAmountStatus(); st.BuyReserved != 50 || st.Total != 50 {
		t.Fatalf("修改后应按新限价重算占用: %+v", st)
	}
	// 提高限价到 20：5×20=100 恰好用满，允许。
	mustModify(t, e, id, 5, 20)
	if st := e.PositionAmountStatus(); st.BuyReserved != 100 || st.Total != 100 {
		t.Fatalf("恰好用满金额上限应允许: %+v", st)
	}
	// 再增 1 股：申请后 120 > 100，拒绝。
	before := len(e.Records())
	if err := e.Modify(id, 6, 20); err == nil {
		t.Fatal("超过金额上限必须拒绝修改")
	}
	rj := e.Records()[before]
	if rj.Kind != RecordRejected || !rj.AmtEnabled || rj.AmtLimit != 100 ||
		rj.AmtTotal != 100 || rj.AmtApplyTotal != 120 {
		t.Fatalf("金额上限拒绝快照错误: %+v", rj)
	}
	if len(rj.AmtQuoteRefs) != 1 || rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 1, Price: 10}) {
		t.Fatalf("拒绝必须保存参与计算的报价定位: %+v", rj.AmtQuoteRefs)
	}
	if o, _ := e.Order(id); o.Qty != 5 || o.Limit != 20 {
		t.Fatalf("拒绝后订单保持原参数: %+v", o)
	}
	if st := e.PositionAmountStatus(); st.Total != 100 {
		t.Fatalf("拒绝后合计不变: %+v", st)
	}

	// 跨合约：B 单占用保留，申请后合计按两合约计。
	mustModify(t, e, id, 5, 10) // A 占用 50
	bID := mustBuy(t, e, "B", 2, 20)
	if st := e.PositionAmountStatus(); st.BuyReserved != 90 || st.Total != 90 {
		t.Fatalf("前置合计错误: %+v", st)
	}
	mustModify(t, e, id, 6, 10) // A 60 + B 40 = 100 恰好
	if err := e.Modify(id, 7, 10); err == nil {
		t.Fatal("A 70 + B 40 = 110 超限必须拒绝")
	}
	rj = e.Records()[len(e.Records())-1]
	if rj.AmtTotal != 100 || rj.AmtApplyTotal != 110 || len(rj.AmtQuoteRefs) != 2 {
		t.Fatalf("跨合约拒绝快照错误: total=%d apply=%d refs=%+v", rj.AmtTotal, rj.AmtApplyTotal, rj.AmtQuoteRefs)
	}
	if o, _ := e.Order(bID); o.Status != StatusPending {
		t.Fatalf("额度不足不得撤销 B 单: %+v", o)
	}
}

func TestModifyAmountLimitOverflow(t *testing.T) {
	// 报价天价：新占用 2×MaxInt64 溢出，即使限价 1。
	e, _ := NewEngine(math.MaxInt64)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: math.MaxInt64}, 1)
	id := mustBuy(t, e, "A", 1, 1) // 现金占用 1；金额口径 1×MaxInt64
	if _, err := e.SetPositionAmountLimit(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	before := len(e.Records())
	err := e.Modify(id, 2, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("金额占用溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("溢出只能追加一条拒绝记录")
	}
	if o, _ := e.Order(id); o.Qty != 1 || o.Limit != 1 || o.Status != StatusPending {
		t.Fatalf("溢出拒绝不得改变订单: %+v", o)
	}
	if e.ReservedCash() != 1 {
		t.Fatalf("溢出拒绝不得改变占用: %d", e.ReservedCash())
	}

	// 现金占用乘法本身溢出（不依赖金额上限）。
	e2 := setupModify(t)
	id2 := mustBuy(t, e2, "A", 2, 10)
	before = len(e2.Records())
	if err := e2.Modify(id2, 2, math.MaxInt64); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("限价×剩余量溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if len(e2.Records()) != before+1 || e2.ReservedCash() != 20 {
		t.Fatalf("溢出后只加一条拒绝且占用不变: records=%d reserved=%d",
			len(e2.Records())-before, e2.ReservedCash())
	}
}

func TestModifyBuyGapRestrictions(t *testing.T) {
	e := setupModify(t)
	id := mustBuy(t, e, "A", 5, 10) // 剩余 5、现金占用 50、金额占用 5×10=50

	// seq3 跨号等待，制造缺口；等待报价不参与判断。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 99}, 0)
	if st := e.PositionAmountStatus(); st.BuyReserved != 0 {
		t.Fatalf("未启用金额上限时查询应为零值: %+v", st)
	}

	// 剩余量增加 → 拒绝。
	if err := e.Modify(id, 6, 10); err == nil {
		t.Fatal("缺口时增加剩余量必须拒绝")
	}
	// 现金占用增加（5×11=55>50）→ 拒绝。
	if err := e.Modify(id, 5, 11); err == nil {
		t.Fatal("缺口时增加现金占用必须拒绝")
	}
	if e.ReservedCash() != 50 {
		t.Fatalf("拒绝后占用不变: %d", e.ReservedCash())
	}
	// 剩余量不增、现金不增：降量允许（金额口径 4×10=40 不增）。
	recs := len(e.Records())
	mustModify(t, e, id, 4, 10)
	if e.ReservedCash() != 40 {
		t.Fatalf("缺口时降量应允许: reserved=%d", e.ReservedCash())
	}
	// 缺口中重复提交当前参数也成功，且不新增记录。
	if err := e.Modify(id, 4, 10); err != nil {
		t.Fatalf("缺口时重复提交当前参数应成功: %v", err)
	}
	if len(e.Records()) != recs+1 {
		t.Fatal("重复提交不得新增记录")
	}
	// 降价但提量：剩余量增加，拒绝（不能借降价绕开）。
	if err := e.Modify(id, 5, 1); err == nil {
		t.Fatal("缺口时不得靠降价增加剩余量")
	}
	// 量价都不增：4@8 → 现金 32、金额 4×max(8,10)=40，均不增，允许。
	mustModify(t, e, id, 4, 8)
	if e.ReservedCash() != 32 {
		t.Fatalf("量价不增的修改应允许: reserved=%d", e.ReservedCash())
	}

	// 报价高于限价时，金额口径按报价计：提量会同时抬高金额占用，仍被剩余量规则拦截。
	// 这里直接验证等待中的 seq3@99 从未参与（补齐前占用不随其变化）。
	if o, _ := e.Order(id); o.Qty != 4 || o.Limit != 8 {
		t.Fatalf("订单参数错误: %+v", o)
	}

	// 补齐 seq2、seq3 后，剩余部分按新限价与新剩余量成交（@8 四股）。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 10}, 2)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 8, Qty: 4}); err != nil {
		t.Fatalf("补齐后应按新参数成交: %v", err)
	}
	if e.Position("A") != 4 || e.ReservedCash() != 0 {
		t.Fatalf("按新参数成交后账目错误: pos=%d reserved=%d", e.Position("A"), e.ReservedCash())
	}

	// 缺口期间卖单修改仍可办理（另有持仓）。
	e2 := setupModify(t)
	bid := mustBuy(t, e2, "A", 10, 10)
	if _, err := e2.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e2, "A", 5, 9)
	mustQuote(t, e2, "A", Quote{Seq: 3, Moment: 3, Price: 10}, 0)
	mustModify(t, e2, sid, 8, 9) // 缺口不阻止卖单修改
	if e2.Sellable("A") != 2 {
		t.Fatalf("缺口时卖单修改应正常调整占用: %d", e2.Sellable("A"))
	}
	if err := e2.Modify(sid, 11, 9); err == nil {
		t.Fatal("卖单修改仍不得占用超过持仓的数量")
	}
}

func TestModifyAmountGapUsesAppliedQuoteOnly(t *testing.T) {
	// 启用金额上限且报价高于限价：缺口期间降低限价不改变金额占用（按报价计），
	// 提量则金额增加——由缺口规则拒绝；等待中的天价报价不参与计算。
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(1000); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 5, 10)                              // 报价 10：金额 50
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 1) // 金额变 60
	// seq4 报价跨号等待，绝不参与；最新已生效报价停在 seq2@12，缺口存在。
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: 99}, 0)
	if !e.HasGap("A") {
		t.Fatal("前置应存在缺口")
	}
	if st := e.PositionAmountStatus(); st.BuyReserved != 60 {
		t.Fatalf("等待报价不得参与金额计算: %+v", st)
	}
	// 5@1：现金占用 5 大降，但金额口径仍 5×max(1,12)=60，不增；允许。
	mustModify(t, e, id, 5, 1)
	if st := e.PositionAmountStatus(); st.BuyReserved != 60 {
		t.Fatalf("缺口期间金额口径必须按最新已生效报价 12 计: %+v", st)
	}
	if e.ReservedCash() != 5 {
		t.Fatalf("现金占用按新限价计: %d", e.ReservedCash())
	}
}

func TestModifyRiskProtection(t *testing.T) {
	e := setupHolding(t, 10) // cash 900, pos 10, quote seq1@10
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	bid := mustBuy(t, e, "A", 2, 9) // 占用 18
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("前置应触线")
	}
	if o, _ := e.Order(bid); o.Status != StatusCanceled {
		t.Fatalf("保护撤单前置: %+v", o)
	}

	// 被保护撤销的订单不能借修改恢复。
	recs := len(e.Records())
	err := e.Modify(bid, 2, 9)
	if err == nil {
		t.Fatal("亏损保护撤销的订单不能修改")
	}
	if o, _ := e.Order(bid); o.Status != StatusCanceled || !strings.Contains(o.Reason, "亏损") {
		t.Fatalf("订单必须保持撤销态且原因保留: %+v", o)
	}
	if len(e.Records()) != recs+1 || e.Records()[recs].Kind != RecordRejected {
		t.Fatal("拒绝修改必须留一条拒绝记录")
	}

	// 触线后卖单及其修改不受影响。
	sid := mustSell(t, e, "A", 5, 1)
	mustModify(t, e, sid, 3, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 3}); err != nil {
		t.Fatalf("触线后卖单按新参数成交应正常: %v", err)
	}
	if e.Position("A") != 7 || e.Cash() != 903 {
		t.Fatalf("触线后卖出账目错误: pos=%d cash=%d", e.Position("A"), e.Cash())
	}
}

func TestModifyKeepsAcceptanceOrder(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 3, 10)
	b2 := mustBuy(t, e, "A", 3, 10)
	b3 := mustBuy(t, e, "A", 3, 10) // 接受顺序 b1 < b2 < b3
	// b1 部分成交 2：持仓 2，剩余买单合计 1+3+3=7，总额度占用 9。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b1, Symbol: "A", Side: Buy, Price: 9, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	// 修改中间的 b2（2 股），不改变其接受次序。
	mustModify(t, e, b2, 2, 10) // 持仓 2 + 1+2+3 = 8

	// 下调限额到 5：若 b2 被排到队尾会先撤 b2；正确次序只撤最后的 b3（2+1+2=5 恰好）。
	canceled := mustSetMax(t, e, "A", 5)
	if len(canceled) != 1 || canceled[0] != b3 {
		t.Fatalf("修改不得改变接受顺序，应只撤 %d，实际 %v", b3, canceled)
	}
	if o, _ := e.Order(b2); o.Status != StatusPending || o.Qty != 2 {
		t.Fatalf("b2 必须按原次序保留: %+v", o)
	}
	// 再下调到 4：接着撤 b2（2+1=3 ≤ 4），b1 保留。
	canceled = mustSetMax(t, e, "A", 4)
	if len(canceled) != 1 || canceled[0] != b2 {
		t.Fatalf("下一轮应按原次序撤 b2，实际 %v", canceled)
	}
	if o, _ := e.Order(b1); o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("最早的 b1 应保留: %+v", o)
	}
}

func TestModifyKeepsOrderOnQuoteAmountCancel(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}
	bA := mustBuy(t, e, "A", 5, 10) // 50
	bB := mustBuy(t, e, "B", 5, 20) // 100，合计 150
	mustModify(t, e, bA, 6, 10)     // 60 + 100 = 160

	// A 报价涨到 20：A 单 6×20=120、B 单 100，合计 220 > 200。
	// 按编号从大到小先撤 bB，120 ≤ 200，bA（已修改）保留。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 1)
	if o, _ := e.Order(bB); o.Status != StatusCanceled {
		t.Fatalf("应收敛撤销编号更大的 B 单: %+v", o)
	}
	if o, _ := e.Order(bA); o.Status != StatusPending || o.Qty != 6 || o.Limit != 10 {
		t.Fatalf("已修改的 A 单应保留新参数: %+v", o)
	}
	if st := e.PositionAmountStatus(); st.BuyReserved != 120 || st.Total != 120 {
		t.Fatalf("撤单后金额错误: %+v", st)
	}
}

func TestModifyThenCancelReleasesNewRemaining(t *testing.T) {
	e := setupModify(t)
	bid := mustBuy(t, e, "A", 10, 10)
	mustModify(t, e, bid, 6, 12) // 占用 72
	if err := e.Cancel(bid); err != nil {
		t.Fatal(err)
	}
	if e.ReservedCash() != 0 || e.AvailableCash() != 1000 {
		t.Fatalf("撤单必须释放修改后的新剩余量占用: reserved=%d", e.ReservedCash())
	}
	if err := e.Modify(bid, 6, 12); err == nil {
		t.Fatal("撤销后不能再修改")
	}

	// 卖单版。
	e2 := setupModify(t)
	b := mustBuy(t, e2, "A", 10, 10)
	if _, err := e2.Fill(Trade{TradeID: 1, OrderID: b, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e2, "A", 6, 9)
	mustModify(t, e2, sid, 4, 9)
	if err := e2.Cancel(sid); err != nil {
		t.Fatal(err)
	}
	if e2.Sellable("A") != 10 {
		t.Fatalf("撤卖单应释放修改后的占用: %d", e2.Sellable("A"))
	}
}

func TestModifyFillIdempotencyAcrossLifecycle(t *testing.T) {
	e := setupModify(t)
	id := mustBuy(t, e, "A", 10, 10)
	tr := Trade{TradeID: 7, OrderID: id, Symbol: "A", Side: Buy, Price: 9, Qty: 4}
	res1, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	// 修改：总量 8、限价 12。
	mustModify(t, e, id, 8, 12)

	// 已入账成交重复提交仍返回第一次结果，即使订单后来修改。
	res2, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res1 != res2 {
		t.Fatalf("修改后幂等重放必须返回第一次结果: %+v vs %+v", res1, res2)
	}
	if e.Cash() != 964 || e.ReservedCash() != 48 || e.Position("A") != 4 {
		t.Fatalf("幂等重放不得按新参数重新记账: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	// 撤销后重放仍旧结果。
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	res3, err := e.Fill(tr)
	if err != nil {
		t.Fatal(err)
	}
	if res3 != res1 {
		t.Fatalf("撤销后重放仍须返回第一次结果: %+v vs %+v", res3, res1)
	}

	// 尚未入账的成交按处理时有效参数判断：新订单新限价。
	id2 := mustBuy(t, e, "A", 5, 10)
	mustModify(t, e, id2, 5, 7)
	if _, err := e.Fill(Trade{TradeID: 8, OrderID: id2, Symbol: "A", Side: Buy, Price: 8, Qty: 1}); err == nil {
		t.Fatal("成交必须按修改后的新限价 7 判断")
	}
	if _, err := e.Fill(Trade{TradeID: 9, OrderID: id2, Symbol: "A", Side: Buy, Price: 7, Qty: 2}); err != nil {
		t.Fatalf("符合新限价的成交应入账: %v", err)
	}
}

func TestModifyRecordsImmutable(t *testing.T) {
	e := setupModify(t)
	id := mustBuy(t, e, "A", 10, 10)
	mustModify(t, e, id, 8, 12)

	recs := e.Records()
	for i := range recs {
		if recs[i].Kind == RecordModified {
			recs[i].OldLimit = 1
			recs[i].OldQty = 1
			recs[i].Limit = 1
			recs[i].Qty = 1
		}
	}
	r := lastModifyRecord(t, e)
	if r.OldLimit != 10 || r.OldQty != 10 || r.Limit != 12 || r.Qty != 8 {
		t.Fatalf("查询副本篡改不得改写内部修改记录: %+v", r)
	}

	// 再次修改与后续报价不得改写历史记录。
	mustModify(t, e, id, 6, 11)
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 1)
	var firstMod Record
	for _, r := range e.Records() {
		if r.Kind == RecordModified && r.OldQty == 10 {
			firstMod = r
			break
		}
	}
	if firstMod.OldLimit != 10 || firstMod.Limit != 12 || firstMod.Qty != 8 ||
		firstMod.QuotePrice != 10 {
		t.Fatalf("历史修改记录不得被后续操作改写: %+v", firstMod)
	}
}

func TestModifyConcurrentWithFillsAndCancels(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	const n = 40
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, mustBuy(t, e, "A", 100, 1)) // 各占用 100
	}

	var tradeID atomic.Int64
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(3)
		go func(id int64) { // 修改：参数可能被额度/剩余量拒绝，忽略错误
			defer wg.Done()
			_ = e.Modify(id, 50+id%120, 1)
		}(id)
		go func(id int64) { // 成交：超量或撤销后被拒，忽略错误
			defer wg.Done()
			for k := 0; k < 3; k++ {
				_, _ = e.Fill(Trade{
					TradeID: tradeID.Add(1), OrderID: id, Symbol: "A", Side: Buy, Price: 1, Qty: 30,
				})
			}
		}(id)
		go func(id int64) { // 撤单
			defer wg.Done()
			_ = e.Cancel(id)
		}(id)
	}
	wg.Wait()

	// 不变量：占用现金 = Σ 有效买单 限价 × 剩余量；现金与持仓和已入账成交一致。
	var reserved int64
	for _, o := range e.Orders() {
		if o.Side != Buy || o.Status == StatusCanceled || o.Status == StatusFilled {
			continue
		}
		reserved += o.Limit * o.Remaining()
	}
	if reserved != e.ReservedCash() {
		t.Fatalf("并发后现金占用不变量被破坏: 计算 %d，实际 %d", reserved, e.ReservedCash())
	}
	var tradedCash, tradedQty int64
	for _, f := range e.fills {
		tradedCash += f.price * f.qty
		tradedQty += f.qty
	}
	if e.Cash() != 1_000_000-tradedCash {
		t.Fatalf("并发后现金余额与成交不一致: cash=%d 成交金额=%d", e.Cash(), tradedCash)
	}
	if e.Position("A") != tradedQty {
		t.Fatalf("并发后持仓与成交不一致: pos=%d 成交量=%d", e.Position("A"), tradedQty)
	}
	if e.AvailableCash() < 0 {
		t.Fatal("可用现金不能为负")
	}
}

func TestModifyRecordKindString(t *testing.T) {
	if RecordModified.String() != "修改" {
		t.Fatalf("修改记录类型文案错误: %s", RecordModified)
	}
}
