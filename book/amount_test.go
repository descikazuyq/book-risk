package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// setupAmt 准备：现金 1_000_000；A、B 已设置持仓限额并各有一条报价。
func setupAmt(t *testing.T) *Engine {
	t.Helper()
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustSetMax(t, e, "B", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	return e
}

func TestAmountLimitDisabledByDefault(t *testing.T) {
	e := setupAmt(t)
	id := mustBuy(t, e, "A", 5, 10)

	st := e.PositionAmountStatus()
	if st.Enabled || st.Limit != 0 || st.Holding != 0 || st.BuyReserved != 0 || st.Total != 0 {
		t.Fatalf("未设置上限时查询除 Enabled 外必须为零: %+v", st)
	}

	// 未设置时不改变基线行为：报价涨到天价也不撤单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1_000_000}, 1)
	if o, _ := e.Order(id); o.Status != StatusPending {
		t.Fatalf("未设置金额上限时报价不得撤单: %+v", o)
	}
	for _, r := range e.Records() {
		if r.AmtEnabled {
			t.Fatalf("未设置时记录不得携带金额快照: %+v", r)
		}
	}
}

func TestAmountLimitSetValidation(t *testing.T) {
	e := setupAmt(t)

	if _, err := e.SetPositionAmountLimit(-1); err == nil {
		t.Fatal("负上限必须报错")
	}
	if e.PositionAmountStatus().Enabled {
		t.Fatal("负上限不得启用限额")
	}

	// 首次设置为 0：允许且立即生效；同值重复设置无操作、无记录。
	before := len(e.Records())
	if _, err := e.SetPositionAmountLimit(0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(0); err != nil {
		t.Fatal(err)
	}
	if len(e.Records()) != before {
		t.Fatal("同值重复设置不得产生记录")
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 0 {
		t.Fatalf("零上限应已启用: %+v", st)
	}
}

func TestAmountZeroLimitRejectsAndCancelsAllBuys(t *testing.T) {
	e := setupAmt(t)
	b1 := mustBuy(t, e, "A", 2, 10) // 占用 20
	b2 := mustBuy(t, e, "B", 1, 30) // 占用 30（限价高于报价）

	canceled, err := e.SetPositionAmountLimit(0)
	if err != nil {
		t.Fatal(err)
	}
	// 持仓为 0（0 不超限），合计 50 > 0：按编号降序撤 b2、b1。
	if len(canceled) != 2 || canceled[0] != b2 || canceled[1] != b1 {
		t.Fatalf("应按编号降序撤销 %d,%d，实际 %v", b2, b1, canceled)
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("撤单应释放全部现金占用: %d", e.ReservedCash())
	}

	// 零上限禁止任何买单。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("零上限必须禁止新买单")
	}
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Total != 0 || st.Holding != 0 || st.BuyReserved != 0 {
		t.Fatalf("撤光后查询应全零: %+v", st)
	}

	// 卖单与卖出成交不受影响（无持仓可卖时卖出会被基线拒绝，这里先建仓验证）。
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 5, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(0); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 5, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 5}); err != nil {
		t.Fatal("持仓本身超限时卖出成交仍应允许")
	}
}

func TestAmountNewBuyUsesMaxOfLimitAndQuote(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}

	// A 报价 10：买单限价 12 → 每股按 12 占用。5 股 = 60。
	mustBuy(t, e, "A", 5, 12)
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 60 || st.Total != 60 {
		t.Fatalf("买单应按限价 12 占用: %+v", st)
	}

	// B 报价 20：买单限价 5 → 每股按报价 20 占用。2 股 = 40；合计恰 100，允许。
	mustBuy(t, e, "B", 2, 5)
	if st := e.PositionAmountStatus(); st.BuyReserved != 100 || st.Total != 100 {
		t.Fatalf("合计恰好等于上限应接受: %+v", st)
	}

	// 再买任意 1 股都超限（A 最少占用 10，B 最少占用 20）。
	recsBefore := len(e.Records())
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("合计超过上限必须拒绝")
	}
	rj := e.Records()[recsBefore]
	if rj.Kind != RecordRejected || rj.AmtLimit != 100 || rj.AmtHolding != 0 ||
		rj.AmtBuyReserved != 100 || rj.AmtTotal != 100 || rj.AmtApplyTotal != 110 || !rj.AmtEnabled {
		t.Fatalf("拒绝记录金额快照错误: %+v", rj)
	}
	if len(rj.AmtQuoteRefs) != 2 {
		t.Fatalf("应保存 A、B 两个参与计算合约的报价: %+v", rj.AmtQuoteRefs)
	}
}

func TestAmountQuoteRiseCancelsDescendingAcrossSymbols(t *testing.T) {
	e := setupAmt(t)
	// A 持仓 4 股（@10 成交），现金扣 40。
	id := mustBuy(t, e, "A", 4, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	bA := mustBuy(t, e, "A", 2, 10) // 限价 10，报价 10 → 占用 20
	bB := mustBuy(t, e, "B", 1, 20) // 限价 20，报价 20 → 占用 20
	// 合计 = 持仓 40 + 买单 40 = 80。
	if _, err := e.SetPositionAmountLimit(80); err != nil {
		t.Fatal(err)
	}

	// A 报价涨到 12：持仓 48、A 买单 24、B 买单 20，合计 92 > 80。
	// 编号最大的 bB 先撤（释放 20）→ 72 ≤ 80；bA 保留。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 1)
	if o, _ := e.Order(bB); o.Status != StatusCanceled {
		t.Fatalf("报价上涨应先撤销编号最大的 B 买单: %+v", o)
	}
	if o, _ := e.Order(bA); o.Status != StatusPending {
		t.Fatalf("撤销一单后合计已不超限，A 买单应保留: %+v", o)
	}
	st := e.PositionAmountStatus()
	if st.Holding != 48 || st.BuyReserved != 24 || st.Total != 72 {
		t.Fatalf("撤单后金额错误: %+v", st)
	}

	// 撤单记录携带判断时金额与各合约报价定位。
	rec := lastAmountRecord(t, e, RecordCanceled)
	if rec.AmtHolding != 48 || rec.AmtBuyReserved != 44 || rec.AmtTotal != 92 || rec.AmtLimit != 80 {
		t.Fatalf("撤单记录应固化撤销前金额: %+v", rec)
	}
	if len(rec.AmtQuoteRefs) != 2 {
		t.Fatalf("应保存 A(seq2@12)、B(seq1@20) 两个报价: %+v", rec.AmtQuoteRefs)
	}
	if rec.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("A 报价定位错误: %+v", rec.AmtQuoteRefs[0])
	}
	if rec.AmtQuoteRefs[1] != (RiskQuoteRef{Symbol: "B", Seq: 1, Moment: 1, Price: 20}) {
		t.Fatalf("B 报价定位错误: %+v", rec.AmtQuoteRefs[1])
	}
}

func TestAmountHoldingOnlyOverLimitCancelsAllButNoSell(t *testing.T) {
	e := setupAmt(t)
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	b1 := mustBuy(t, e, "A", 1, 10)
	b2 := mustBuy(t, e, "B", 1, 20)

	// 上限 99：持仓市值 100 仅持仓就超限 → 撤光全部买单，不自动卖出。
	canceled, err := e.SetPositionAmountLimit(99)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceled) != 2 || canceled[0] != b2 || canceled[1] != b1 {
		t.Fatalf("仅持仓超限时应撤光买单（降序）: %v", canceled)
	}
	if e.Position("A") != 10 {
		t.Fatal("不得自动卖出持仓")
	}
	if st := e.PositionAmountStatus(); st.Holding != 100 || st.BuyReserved != 0 || st.Total != 100 {
		t.Fatalf("撤光买单后只剩持仓金额: %+v", st)
	}
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("仍超限期间新买单必须拒绝")
	}

	// 正常卖单及其成交仍允许；卖出后持仓金额下降、额度恢复便可再次买入。
	sid := mustSell(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if st := e.PositionAmountStatus(); st.Holding != 80 || st.Total != 80 {
		t.Fatalf("卖出成交应减少持仓金额: %+v", st)
	}
	mustBuy(t, e, "A", 1, 9) // 80 + 1×max(9,报价10)=10 → 90 ≤ 99，恢复买入
}

func TestAmountSellOrderHoldingStillCountsUntilFill(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	// 持仓 100 已达上限：新买单被拒。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("持仓已达上限必须拒绝新买单")
	}

	// 挂卖单不提前抵减额度：卖单占用的 6 股仍计入持仓金额。
	sid := mustSell(t, e, "A", 6, 10)
	if st := e.PositionAmountStatus(); st.Holding != 100 || st.Total != 100 {
		t.Fatalf("未成交卖单不得提前抵减额度: %+v", st)
	}
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("卖单未成交前额度不得释放")
	}

	// 卖出成交后才减少持仓金额（10-6=4 股 × 10 = 40），释放 60 额度。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 10, Qty: 6}); err != nil {
		t.Fatal(err)
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 0 || st.Total != 40 {
		t.Fatalf("卖出成交后应按实际持仓计金额: %+v", st)
	}
}

func TestAmountPartialBuyFillRecalculatesWithActualCash(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	// 10 股限价 10：占用 100，恰满。
	id := mustBuy(t, e, "A", 10, 10)
	if st := e.PositionAmountStatus(); st.BuyReserved != 100 || st.Holding != 0 {
		t.Fatalf("前置占用错误: %+v", st)
	}

	// 部分成交 4 股 @8：现金按实际价 32 结算；持仓 4×10(报价)=40，
	// 剩余买单 6×max(10,10)=60，合计仍 100；同一数量不计两处。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 8, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 1_000_000-32 {
		t.Fatalf("现金必须按实际成交价结算: %d", e.Cash())
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 60 || st.Total != 100 {
		t.Fatalf("部分成交后应按实际持仓与剩余买单重算: %+v", st)
	}

	// 报价涨到 12：持仓 48 + 剩余买单 72 = 120 > 100，撤销剩余买单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 1)
	if o, _ := e.Order(id); o.Status != StatusCanceled || o.Filled != 4 {
		t.Fatalf("应收敛撤销剩余 6 股且保留已成交 4 股: %+v", o)
	}
	if st := e.PositionAmountStatus(); st.Holding != 48 || st.BuyReserved != 0 || st.Total != 48 {
		t.Fatalf("撤单后只剩 4 股持仓市值: %+v", st)
	}
	if e.Cash() != 1_000_000-32 {
		t.Fatal("收敛撤单不得重算已成交现金")
	}
}

func TestAmountCancelBuyReleasesOnlyRemaining(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	id := mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	// 持仓 40 + 剩余买单 60 = 100。
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 0 || st.Total != 40 {
		t.Fatalf("撤销只释放剩余量对应额度: %+v", st)
	}
}

func TestAmountQuoteRecoveryAllowsBuyAgainNoOrderRestore(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	b := mustBuy(t, e, "A", 10, 10) // 占用 100
	// 报价涨到 20：占用 200 超限，撤单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 1)
	if o, _ := e.Order(b); o.Status != StatusCanceled {
		t.Fatalf("应收敛撤单: %+v", o)
	}
	cancelRecs := len(e.Records())

	// 报价回落：不恢复已撤订单，也不补记录，但可再次买入。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 10}, 1)
	if o, _ := e.Order(b); o.Status != StatusCanceled {
		t.Fatal("报价回落不得恢复已撤订单")
	}
	if len(e.Records()) != cancelRecs {
		t.Fatal("报价回落到额度内不得新增记录")
	}
	id := mustBuy(t, e, "A", 10, 10)
	if id == b {
		t.Fatal("新买单必须使用新订单编号")
	}

	// 上调上限同样立即可买。
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, e, "A", 15, 10) // 占用 150 ≤ 200
}

func TestAmountGapAppliesOneByOneAndIntermediateCancelsStick(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	b1 := mustBuy(t, e, "A", 5, 10) // 占用 50
	b2 := mustBuy(t, e, "A", 5, 10) // 占用 50，合计 100

	// seq3 报价 20 跨号等待：不参与计算，合计仍 100。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 20}, 0)
	if st := e.PositionAmountStatus(); st.BuyReserved != 100 || st.Total != 100 {
		t.Fatalf("等待补齐的报价不得参与计算: %+v", st)
	}

	// 补齐 seq2@12：两条买单各变 60，第一条生效即超限，撤 b2 后仍 60 ≤ 100；
	// 接着 seq3@20：b1 变 100 恰好等于上限，保留。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 2)
	if o, _ := e.Order(b2); o.Status != StatusCanceled {
		t.Fatalf("seq2@12 应撤销编号最大的 b2: %+v", o)
	}
	if o, _ := e.Order(b1); o.Status != StatusPending {
		t.Fatalf("seq3@20 下 b1 占用恰 100，应保留: %+v", o)
	}

	// 中间报价造成的撤单即使最后价格回落也保留。
	e2 := setupAmt(t)
	if _, err := e2.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	c1 := mustBuy(t, e2, "A", 10, 10)
	mustQuote(t, e2, "A", Quote{Seq: 3, Moment: 3, Price: 5}, 0)
	mustQuote(t, e2, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 2)
	if o, _ := e2.Order(c1); o.Status != StatusCanceled {
		t.Fatal("链中 seq2@20 造成的撤单必须保留")
	}
	mustQuote(t, e2, "A", Quote{Seq: 4, Moment: 4, Price: 10}, 1)
	if o, _ := e2.Order(c1); o.Status != StatusCanceled {
		t.Fatal("最后价格回落不得恢复已撤订单")
	}
}

func TestAmountCoexistsWithLossProtection(t *testing.T) {
	// 现金 1000，A 持仓 10@10（成本 100）。
	e := setupHolding(t, 10)
	// 金额上限 200：买单 id=2，10 股限价 10，占用 100。
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}
	bid := mustBuy(t, e, "A", 10, 10)

	// 开启亏损保护；报价跌到 1：亏损 90 达限（上限 90），亏损保护先撤光买单。
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("亏损保护应触发")
	}
	if o, _ := e.Order(bid); o.Status != StatusCanceled {
		t.Fatalf("亏损保护应撤销买单: %+v", o)
	}
	// 撤单原因必须保留亏损保护的原因，不得被金额上限改写。
	var cancelRec Record
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && r.OrderID == bid {
			cancelRec = r
		}
	}
	if cancelRec.Reason == "" || !strings.Contains(cancelRec.Reason, "亏损") {
		t.Fatalf("应保留亏损保护撤单原因: %q", cancelRec.Reason)
	}

	// 金额上限跨交易日保留；新一日亏损限制解除，但金额上限仍在。
	// 报价 1 下持仓市值 10，上限 200 本可买入。
	if err := e.StartTradingDay(2, 1000); err != nil {
		t.Fatal(err)
	}
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 200 {
		t.Fatalf("金额上限必须跨交易日保留: %+v", st)
	}
	mustBuy(t, e, "A", 10, 1) // 占用 max(1,1)×10=10，合计 20，允许
}

func TestAmountLossRestrictionStillBlocksEvenIfAmountAllows(t *testing.T) {
	e := setupHolding(t, 10)
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 1}, 1)
	if !e.RiskStatus().Restricted {
		t.Fatal("前置应触发亏损保护")
	}
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("亏损整日锁定期间，即使金额上限充裕也必须拒绝买单")
	}
}

func TestAmountOverflowOnSetKeepsOldSetting(t *testing.T) {
	// 未开日、未设金额上限时报价可使持仓市值溢出：低位建仓后报价涨到天价。
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 3, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: math.MaxInt64}, 1) // 3×MaxInt64 溢出

	// 当前合计已溢出：任何设置都整体报错，只增加一条拒绝记录且不进入启用态。
	before := len(e.Records())
	_, err := e.SetPositionAmountLimit(math.MaxInt64)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("设置时金额溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if st := e.PositionAmountStatus(); st.Enabled {
		t.Fatal("溢出后设置不得生效")
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("溢出设置只能增加一条拒绝记录")
	}

	// 负值报错且保留原设置（此时仍是未启用）。
	if _, err := e.SetPositionAmountLimit(-1); err == nil {
		t.Fatal("负上限必须报错")
	}

	// 合法设置后再下单致申请后合计溢出：下单整体报错并留一条拒绝，上限保持不变。
	e2, _ := NewEngine(math.MaxInt64)
	mustSetMax(t, e2, "A", 10)
	mustQuote(t, e2, "A", Quote{Seq: 1, Moment: 1, Price: 5}, 1)
	bid := mustBuy(t, e2, "A", 1, 5)
	if _, err := e2.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 5, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e2, "A", Quote{Seq: 2, Moment: 2, Price: math.MaxInt64}, 1) // 持仓 1×MaxInt64
	if _, err := e2.SetPositionAmountLimit(math.MaxInt64); err != nil {
		t.Fatalf("合计恰为 MaxInt64 时设置应成功: %v", err)
	}
	before = len(e2.Records())
	// 限价 5 通过现金检查；占用按 max(限价 5, 报价 MaxInt64) 计，与持仓合计溢出。
	if _, err := e2.Buy("A", 1, 5); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("申请后合计溢出必须返回 ErrInt64Overflow，实际 %v", err)
	}
	if len(e2.Records()) != before+1 || e2.Records()[before].Kind != RecordRejected {
		t.Fatal("溢出下单只能增加一条拒绝记录")
	}
	if st := e2.PositionAmountStatus(); !st.Enabled || st.Limit != math.MaxInt64 {
		t.Fatalf("溢出下单不得改写上限: %+v", st)
	}
}

func TestAmountOverflowQuoteBatchRollsBack(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	id := mustBuy(t, e, "A", 5, 10) // 占用 50
	if _, err := e.SetPositionAmountLimit(1_000_000); err != nil {
		t.Fatal(err)
	}

	// seq3 溢出报价先等待；补齐 seq2 时链中 seq3 使 5×MaxInt64 溢出，整批回滚。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: math.MaxInt64}, 0)
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 11})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("链中金额溢出必须整体报错且 0 条生效: n=%d err=%v", n, err)
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 {
		t.Fatalf("回滚后最新报价应停留在 seq1@10: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("回滚后 seq3 必须仍在等待")
	}
	if o, _ := e.Order(id); o.Status != StatusPending {
		t.Fatalf("回滚不得撤单: %+v", o)
	}
	if e.ReservedCash() != 50 {
		t.Fatalf("回滚不得改变占用: %d", e.ReservedCash())
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("整批报错只能增加一条拒绝记录")
	}
}

func TestAmountRecordsImmutable(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(80); err != nil {
		t.Fatal(err)
	}
	mustBuy(t, e, "A", 4, 10) // 占用 40
	mustBuy(t, e, "B", 2, 20) // 占用 40，合计 80
	// A 报价涨到 12：A 买单变 48、B 仍 40，合计 88 超限，撤销编号更大的 B 买单。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 12}, 1)
	rec := lastAmountRecord(t, e, RecordCanceled)
	if len(rec.AmtQuoteRefs) != 2 || rec.AmtTotal != 88 {
		t.Fatalf("前置撤单快照错误: %+v", rec)
	}

	// 篡改查询副本不得影响内部记录。
	recs := e.Records()
	for i := range recs {
		if recs[i].AmtQuoteRefs != nil {
			recs[i].AmtQuoteRefs[0].Price = 999
			recs[i].AmtTotal = 1
		}
	}
	again := lastAmountRecord(t, e, RecordCanceled)
	if again.AmtQuoteRefs[0].Price == 999 || again.AmtTotal == 1 {
		t.Fatalf("内部记录不得被查询副本改写: %+v", again)
	}

	// 后续报价、上限调整不得改写历史记录。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 1}, 1)
	if _, err := e.SetPositionAmountLimit(999); err != nil {
		t.Fatal(err)
	}
	again = lastAmountRecord(t, e, RecordCanceled)
	if again.AmtLimit != 80 || again.AmtQuoteRefs[0].Price != 12 {
		t.Fatalf("历史撤单快照不得被后续变化改写: %+v", again)
	}
}

func TestAmountReplayQuoteAndTradeHasNoNewEffect(t *testing.T) {
	e := setupAmt(t)
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}
	bid := mustBuy(t, e, "A", 10, 10)
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 1)
	if o, _ := e.Order(bid); o.Status != StatusCanceled {
		t.Fatal("前置应撤单")
	}
	recs := len(e.Records())

	// 相同报价重放：不新增业务效果或记录。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: 20}, 0)
	if len(e.Records()) != recs {
		t.Fatal("相同报价重放不得新增记录")
	}

	// 成交幂等重放不改变金额状态。
	id := mustBuy(t, e, "A", 5, 10)
	tr := Trade{TradeID: 5, OrderID: id, Symbol: "A", Side: Buy, Price: 10, Qty: 5}
	if _, err := e.Fill(tr); err != nil {
		t.Fatal(err)
	}
	before := e.PositionAmountStatus()
	recs = len(e.Records())
	if _, err := e.Fill(tr); err != nil {
		t.Fatal(err)
	}
	after := e.PositionAmountStatus()
	if before != after || len(e.Records()) != recs {
		t.Fatalf("成交重放不得改变金额状态或新增记录: before=%+v after=%+v", before, after)
	}
}

func TestAmountRejectQuoteRefsIncludeApplyingSymbol(t *testing.T) {
	// 只有 B 持仓占用额度；A 本身无持仓无买单，但其报价决定本次买单占用单价。
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustSetMax(t, e, "B", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 70, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)
	bid := mustBuy(t, e, "B", 5, 20) // 占用 100，达成持仓前置
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "B", Side: Buy, Price: 20, Qty: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(150); err != nil {
		t.Fatal(err)
	}
	// B 持仓 100；A 买单 6 股限价 5，按报价 10 占用 60，申请后 160 > 150。
	if _, err := e.Buy("A", 6, 5); err == nil {
		t.Fatal("必须拒绝")
	}
	rec := lastAmountRecord(t, e, RecordRejected)
	if rec.AmtApplyTotal != 160 || rec.AmtTotal != 100 {
		t.Fatalf("拒绝快照金额错误: %+v", rec)
	}
	var hasA, hasB bool
	for _, rf := range rec.AmtQuoteRefs {
		if rf.Symbol == "A" && rf.Seq == 1 && rf.Moment == 70 && rf.Price == 10 {
			hasA = true
		}
		if rf.Symbol == "B" && rf.Seq == 1 && rf.Price == 20 {
			hasB = true
		}
	}
	if !hasA || !hasB || len(rec.AmtQuoteRefs) != 2 {
		t.Fatalf("报价定位必须包含申请合约 A 与持仓合约 B: %+v", rec.AmtQuoteRefs)
	}
}

func lastAmountRecord(t *testing.T, e *Engine, kind RecordKind) Record {
	t.Helper()
	for i := len(e.records) - 1; i >= 0; i-- {
		if e.records[i].Kind == kind && e.records[i].AmtEnabled {
			return e.records[i]
		}
	}
	t.Fatal("不存在携带金额快照的记录")
	return Record{}
}
