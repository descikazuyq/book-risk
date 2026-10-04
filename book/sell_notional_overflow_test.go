package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestSellAcceptanceIgnoresNotionalOverflow 复现题述场景：卖单接受与否只取决于可卖
// 持仓，整笔“限价 × 委托总量”无法用 int64 表示时也必须接受（可逐笔分次成交）。
// 接受后保留总量与限价、占用可卖数量，不预先增加现金、扣减持仓或改变买单现金占用；
// 这与把有效卖单修改到相同参数的结果一致。
func TestSellAcceptanceIgnoresNotionalOverflow(t *testing.T) {
	const huge int64 = 5000000000000000000 // 2×huge = 1e19 > math.MaxInt64

	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	// 价格 1 买入并成交 2：现金 98、持仓 2。
	bid := mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 98 || e.Position("A") != 2 {
		t.Fatalf("前置状态错误: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	// 直接提交总量 2、限价 huge 的卖单：整笔金额 1e19 超出 int64，仍应成功，
	// 返回正常订单编号。
	before := len(e.Records())
	sid, err := e.Sell("A", 2, huge)
	if err != nil {
		t.Fatalf("整笔限价金额超出 int64 不得拒绝卖单，实际 %v", err)
	}
	if sid <= 0 {
		t.Fatalf("必须返回正常订单编号，实际 %d", sid)
	}

	// 持仓仍为 2，可卖数量变为 0，现金仍为 98，买单现金占用不受影响。
	if e.Position("A") != 2 || e.Sellable("A") != 0 {
		t.Fatalf("接受卖单不得提前扣减持仓: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if e.Cash() != 98 || e.ReservedCash() != 0 || e.AvailableCash() != 98 {
		t.Fatalf("接受卖单不得改变现金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	o, _ := e.Order(sid)
	if o.Status != StatusPending || o.Qty != 2 || o.Limit != huge || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("卖单应原样保留总量与限价: %+v", o)
	}

	// 接受记录固化提交的总量与限价。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条接受记录，实际新增 %d 条", len(recs)-before)
	}
	ar := recs[len(recs)-1]
	if ar.Kind != RecordAccepted || ar.OrderID != sid || ar.Qty != 2 || ar.Limit != huge ||
		ar.Remaining != 2 || ar.Side != Sell {
		t.Fatalf("接受记录要素错误: %+v", ar)
	}

	// 与修改口径一致：对该卖单先把限价改到 1，再改回 huge，卖单修改从不按
	// “限价 × 剩余量”试算，均成功；改回后不新增任何额外占用。
	if err := e.Modify(sid, 2, 1); err != nil {
		t.Fatalf("卖单应可改低限价: %v", err)
	}
	if err := e.Modify(sid, 2, huge); err != nil {
		t.Fatalf("把有效卖单修改到与直接提交相同的超限金额参数也必须成功: %v", err)
	}
	if o, _ := e.Order(sid); o.Limit != huge || o.Qty != 2 || o.Status != StatusPending {
		t.Fatalf("修改后卖单参数错误: %+v", o)
	}
	if e.Cash() != 98 || e.Position("A") != 2 || e.Sellable("A") != 0 || e.ReservedCash() != 0 {
		t.Fatalf("修改不得改变现金、持仓与占用: cash=%d pos=%d sellable=%d reserved=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"), e.ReservedCash())
	}

	// 成交 1 个单位 @huge：单笔金额与入账后现金 98+5e18 均在 int64 范围内，
	// 正常增加现金、减少持仓，订单成为部分成交，剩余 1 继续占用可卖数量。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: huge, Qty: 1})
	if err != nil {
		t.Fatalf("首笔成交应正常入账: %v", err)
	}
	wantCash := int64(98 + huge)
	if e.Cash() != wantCash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("首笔成交后账目错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if res.Status != StatusPartial || res.Filled != 1 || res.Remaining != 1 {
		t.Fatalf("首笔成交结果错误: %+v", res)
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
		t.Fatalf("卖单应为部分成交且剩余量继续占用: %+v", o)
	}

	// 再提交另一个成交编号、同价同量：现金加总 98+1e19 越界，必须拒绝并返回
	// 可识别的 ErrInt64Overflow；前一笔成交与剩余卖单都保留。
	before = len(e.Records())
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: huge, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("现金加总越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if e.Cash() != wantCash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("越界成交不得改变现金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
		t.Fatalf("越界成交不得改变订单累计成交量: %+v", o)
	}
	recs = e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordRejected {
		t.Fatalf("越界成交只能追加一条拒绝记录，新增 %d 条", len(recs)-before)
	}
	if rj := recs[len(recs)-1]; rj.TradeID != 3 || !strings.Contains(rj.Reason, "超出") {
		t.Fatalf("拒绝记录须固化成交编号并说明越界: %+v", rj)
	}
	// 被拒绝的编号不被占用：以相同内容再次提交仍走重新校验并再次越界拒绝，
	// 而不是返回任何旧结果或“编号已用于不同内容”。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: huge, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("被拒绝的成交编号不应被占用，实际 %v", err)
	}
	if e.Cash() != wantCash {
		t.Fatalf("重复越界提交不得改变现金: %d", e.Cash())
	}
}

// TestSellNotionalOverflowKeepsOrdinaryRules 校验放宽整笔金额限制后，卖单的其他
// 既有规则不变：数量超过可卖持仓仍拒绝；成交价低于限价、成交量超过剩余量仍拒绝；
// 买单按整笔限价金额占用现金的规则保持不变。
func TestSellNotionalOverflowKeepsOrdinaryRules(t *testing.T) {
	const huge int64 = 5000000000000000000

	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}

	// 数量超过可卖持仓：即使限价 × 数量也越界，仍按可卖不足拒绝，不生成订单。
	before := len(e.Records())
	if _, err := e.Sell("A", 3, huge); err == nil {
		t.Fatal("卖单数量超过可卖持仓必须拒绝")
	} else if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("可卖数量不足是普通拒绝，不得返回 ErrInt64Overflow: %v", err)
	} else if !strings.Contains(err.Error(), "可卖数量不足") {
		t.Fatalf("拒绝原因必须说明可卖不足: %v", err)
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("可卖不足只能追加一条拒绝记录")
	}
	if len(e.Orders()) != 1 || e.Sellable("A") != 2 {
		t.Fatalf("拒绝不得生成卖单或占用可卖数量: orders=%d sellable=%d", len(e.Orders()), e.Sellable("A"))
	}

	sid := mustSell(t, e, "A", 2, huge)

	// 成交价低于限价：普通拒绝。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: huge - 1, Qty: 1}); err == nil {
		t.Fatal("成交价低于限价必须拒绝")
	} else if errors.Is(err, ErrInt64Overflow) || !strings.Contains(err.Error(), "低于限价") {
		t.Fatalf("低于限价必须走原有普通拒绝，实际 %v", err)
	}

	// 成交量超过剩余量：普通拒绝。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: huge, Qty: 3}); err == nil {
		t.Fatal("成交量超过剩余量必须拒绝")
	} else if errors.Is(err, ErrInt64Overflow) || !strings.Contains(err.Error(), "超过订单剩余量") {
		t.Fatalf("超过剩余量必须走原有普通拒绝，实际 %v", err)
	}

	// 单笔成交金额本身超出 int64（价格 MaxInt64 × 数量 2）：报错并说明金额越界，
	// 现金、持仓、订单累计成交量与占用都不变。
	cash, pos := e.Cash(), e.Position("A")
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: sid, Symbol: "A", Side: Sell, Price: math.MaxInt64, Qty: 2}); err == nil {
		t.Fatal("单笔成交金额超出 int64 必须拒绝")
	} else if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("单笔金额本身越界属于成交内容非法的普通拒绝，实际 %v", err)
	} else if !strings.Contains(err.Error(), "超出整数范围") {
		t.Fatalf("拒绝原因必须说明成交金额越界，实际 %v", err)
	}
	if e.Cash() != cash || e.Position("A") != pos || e.Sellable("A") != 0 {
		t.Fatalf("越界成交不得改变现金、持仓与可卖占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Filled != 0 || o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("越界成交不得改变订单累计成交量: %+v", o)
	}
	// 该编号未被占用：修正为合法内容后同编号可以成交。
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: sid, Symbol: "A", Side: Sell, Price: huge, Qty: 1}); err != nil {
		t.Fatalf("被拒绝的成交编号应可复用: %v", err)
	}
	if o, _ := e.Order(sid); o.Filled != 1 || o.Status != StatusPartial {
		t.Fatalf("同编号修正后应正常成交: %+v", o)
	}

	// 买单规则不变：限价 × 数量超出 int64 时整笔拒绝（普通错误，不生成订单）。
	before = len(e.Records())
	if _, err := e.Buy("A", 2, math.MaxInt64); err == nil {
		t.Fatal("买单限价×数量溢出必须拒绝")
	} else if errors.Is(err, ErrInt64Overflow) || !strings.Contains(err.Error(), "超出整数范围") {
		t.Fatalf("买单整笔金额越界应保持原有普通拒绝，实际 %v", err)
	}
	if len(e.Records()) != before+1 || e.ReservedCash() != 0 {
		t.Fatalf("买单拒绝不得改变现金占用: records=%d reserved=%d",
			len(e.Records())-before, e.ReservedCash())
	}
}

// TestSellNotionalOverflowDoesNotTouchBuyReservation 校验接受超限金额卖单时，
// 在挂买单的现金占用与其他卖单占用都原样保留。
func TestSellNotionalOverflowDoesNotTouchBuyReservation(t *testing.T) {
	const huge int64 = 5000000000000000000

	e, _ := NewEngine(100)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid1 := mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid1, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	// 持仓 2、现金 98；再挂一笔买单占用现金 3，并挂 1 份小额卖单。
	bid2 := mustBuy(t, e, "A", 1, 3)
	other := mustSell(t, e, "A", 1, 1)
	if e.ReservedCash() != 3 || e.Sellable("A") != 1 {
		t.Fatalf("前置状态错误: reserved=%d sellable=%d", e.ReservedCash(), e.Sellable("A"))
	}

	sid := mustSell(t, e, "A", 1, huge) // 2×huge 越界，但只占用最后 1 份可卖
	if e.Sellable("A") != 0 {
		t.Fatalf("新卖单应占用剩余 1 份可卖: %d", e.Sellable("A"))
	}
	if e.Cash() != 98 || e.ReservedCash() != 3 || e.AvailableCash() != 95 || e.Position("A") != 2 {
		t.Fatalf("接受卖单不得改变现金与买单占用: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(bid2); o.Status != StatusPending {
		t.Fatalf("在挂买单不受影响: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("其他卖单不受影响: %+v", o)
	}
	if o, _ := e.Order(sid); o.Status != StatusPending || o.Limit != huge {
		t.Fatalf("新卖单应原样保留限价: %+v", o)
	}
}
