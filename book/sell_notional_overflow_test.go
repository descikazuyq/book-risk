package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// hugeLimit 是单笔自身合法、但乘以委托总量 2 后超出 int64 的限价
// （5e18 < MaxInt64 ≈ 9.22e18，而 5e18×2 = 1e19 > MaxInt64）。
const hugeLimit int64 = 5000000000000000000

// setupSoldAgainstHugeLimit 复现题述前置：初始现金 100，报价与持仓限额就绪，
// 以价格 1 买入并成交 2 个单位后现金 98、持仓 2；随后直接提交总量 2、
// 限价 hugeLimit 的卖单。卖单必须被接受（整笔限价金额 1e19 无法用 int64
// 表示也不拒绝），返回正常订单编号，且不预先改变现金与持仓。
func setupSoldAgainstHugeLimit(t *testing.T) (e *Engine, bid, sid int64) {
	t.Helper()
	e, _ = NewEngine(100)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	bid = mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 98 || e.Position("A") != 2 {
		t.Fatalf("前置状态错误: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}

	before := len(e.Records())
	sid, err := e.Sell("A", 2, hugeLimit)
	if err != nil {
		t.Fatalf("整笔限价金额超出 int64 的卖单必须按可卖持仓接受，实际被拒: %v", err)
	}
	if sid <= 0 {
		t.Fatalf("接受卖单必须返回正常订单编号，实际 %d", sid)
	}
	// 接受后仍是普通待成交卖单：持仓不减持、现金不增加、买单现金占用不变，
	// 只占用相应可卖数量。
	if e.Position("A") != 2 || e.Sellable("A") != 0 {
		t.Fatalf("接受卖单不得扣减持仓，只占用可卖数量: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if e.Cash() != 98 || e.ReservedCash() != 0 || e.AvailableCash() != 98 {
		t.Fatalf("接受卖单不得预先改变现金: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	o, _ := e.Order(sid)
	if o.Status != StatusPending || o.Qty != 2 || o.Limit != hugeLimit ||
		o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("卖单应原样保留总量与限价: %+v", o)
	}
	recs := e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordAccepted {
		t.Fatalf("接受卖单只应新增一条接受记录，实际新增 %d 条", len(recs)-before)
	}
	if r := recs[len(recs)-1]; r.OrderID != sid || r.Qty != 2 || r.Limit != hugeLimit {
		t.Fatalf("接受记录要素错误: %+v", r)
	}
	return e, bid, sid
}

// TestSellNotionalOverflowAcceptedSettlesPerFill 复现题述完整链路：
// 整笔限价金额溢出的卖单接受后，实际成交按每笔回报结算——首笔正常入账并成为
// 部分成交，剩余继续占用可卖；第二笔同价回报使现金加总溢出时拒绝并返回
// ErrInt64Overflow，前一笔成交与剩余卖单全部保留，成交编号不被占用。
func TestSellNotionalOverflowAcceptedSettlesPerFill(t *testing.T) {
	e, _, sid := setupSoldAgainstHugeLimit(t)

	// 以该限价成交 1 个单位：单笔金额与入账后现金均在 int64 范围内，
	// 正常增加现金、减少持仓，订单成为部分成交，剩余 1 继续占用可卖数量。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: hugeLimit, Qty: 1})
	if err != nil {
		t.Fatalf("首笔成交金额与入账现金均可表示，必须正常成交: %v", err)
	}
	wantCash := int64(98 + hugeLimit) // 5000000000000000098
	if e.Cash() != wantCash {
		t.Fatalf("首笔成交应按单笔金额增加现金: got=%d want=%d", e.Cash(), wantCash)
	}
	if e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("首笔成交后持仓应减少、剩余量继续占用可卖: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if res.Status != StatusPartial || res.Filled != 1 || res.Remaining != 1 || res.Qty != 1 {
		t.Fatalf("首笔成交结果错误: %+v", res)
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 1 ||
		o.Qty != 2 || o.Limit != hugeLimit || o.Remaining() != 1 {
		t.Fatalf("订单应成为部分成交并保留原总量与限价: %+v", o)
	}

	// 再提交另一个成交编号、同价同量的回报：现金加总 98 + 5e18 + 5e18 超出 int64，
	// 拒绝这一笔并返回可识别的 ErrInt64Overflow。
	before := len(e.Records())
	res2, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: hugeLimit, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("现金加总溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if res2 != (FillResult{}) {
		t.Fatalf("溢出拒绝必须返回零值成交结果，实际 %+v", res2)
	}

	// 前一笔成交与剩余卖单都保留：现金、持仓、订单累计成交量与占用均不改变。
	if e.Cash() != wantCash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("溢出拒绝不得改变现金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
		t.Fatalf("溢出拒绝不得改变订单累计成交量与剩余量: %+v", o)
	}
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("溢出只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected || rj.TradeID != 3 || rj.OrderID != sid ||
		rj.TradePrice != hugeLimit || rj.Qty != 1 || !strings.Contains(rj.Reason, "超出") {
		t.Fatalf("拒绝记录要素或原因错误: %+v", rj)
	}

	// 被拒成交不占用编号：以编号 3 提交不同内容时应按新内容重新校验
	// （此处因低于限价被普通拒绝），而不是报“编号已用于不同内容”。
	_, err = e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 1})
	if err == nil {
		t.Fatal("低于限价的成交必须拒绝")
	}
	if strings.Contains(err.Error(), "已用于不同内容") {
		t.Fatalf("被溢出拒绝的成交编号不得被占用: %v", err)
	}
	expectErr(t, err, "低于限价")

	// 剩余 1 个单位继续占用可卖数量；撤销剩余卖单后占用释放，持仓保留。
	if err := e.Cancel(sid); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != 1 || e.Sellable("A") != 1 || e.Cash() != wantCash {
		t.Fatalf("撤销剩余卖单应只释放可卖占用: pos=%d sellable=%d cash=%d",
			e.Position("A"), e.Sellable("A"), e.Cash())
	}
}

// TestSellFillUnitAmountOverflowStillRejected 保证实际成交仍按每笔回报结算：
// 单笔成交金额（成交价 × 本次数量）本身超出 int64 时仍报普通错误并说明金额越界，
// 只留拒绝记录，不改变现金、持仓、订单累计成交量或占用，也不占用成交编号。
func TestSellFillUnitAmountOverflowStillRejected(t *testing.T) {
	e, _, sid := setupSoldAgainstHugeLimit(t) // 卖单 2 股，待成交，可卖 0

	before := len(e.Records())
	// 成交价 MaxInt64 不低于限价，但 × 数量 2 单笔金额即溢出 int64。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell,
		Price: math.MaxInt64, Qty: 2})
	if err == nil {
		t.Fatal("单笔成交金额超出 int64 必须拒绝")
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("单笔金额本身越界应走普通金额拒绝，不得包装 ErrInt64Overflow: %v", err)
	}
	expectErr(t, err, "超出整数范围")
	if res != (FillResult{}) {
		t.Fatalf("拒绝必须返回零值成交结果，实际 %+v", res)
	}

	// 只留原有拒绝记录：现金、持仓、订单累计成交量与可卖占用全部不变。
	if e.Cash() != 98 || e.Position("A") != 2 || e.Sellable("A") != 0 {
		t.Fatalf("金额越界成交不得改变现金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("金额越界成交不得改变订单: %+v", o)
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("金额越界只能追加一条拒绝记录")
	}

	// 成交编号未被占用：同编号改报合法回报（2 股 @1，不低于限价不现实——
	// 限价为 hugeLimit，故改报限价价 1 股两笔），这里以同编号提交合法的 1 股成交。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell,
		Price: hugeLimit, Qty: 1}); err != nil {
		t.Fatalf("被拒成交编号应可复用提交合法回报: %v", err)
	}
	if e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("复用编号的合法成交应正常入账: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(sid); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
		t.Fatalf("订单应为部分成交: %+v", o)
	}
}

// TestSellDirectSubmitParityWithModifyOnOverflow 校验直接提交与把有效卖单
// 修改到相同参数在整笔金额溢出规则上保持一致：两者都只按可卖持仓接受；
// 可卖数量不足仍按原有规则拒绝。
func TestSellDirectSubmitParityWithModifyOnOverflow(t *testing.T) {
	// 路径一：直接提交总量 2、限价 hugeLimit 的卖单被接受。
	e1, _, sid1 := setupSoldAgainstHugeLimit(t)
	if o, _ := e1.Order(sid1); o.Status != StatusPending || o.Limit != hugeLimit || o.Qty != 2 {
		t.Fatalf("直接提交的卖单状态错误: %+v", o)
	}
	// 可卖数量不足时，整笔金额能否表示都不能接受：仍按原有规则普通拒绝。
	before := len(e1.Records())
	id, err := e1.Sell("A", 3, hugeLimit)
	if err == nil || errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("数量超过可卖持仓必须按原有普通规则拒绝，实际 id=%d err=%v", id, err)
	}
	if id != 0 {
		t.Fatalf("被拒委托必须返回零订单编号，实际 %d", id)
	}
	expectErr(t, err, "可卖数量不足")
	if len(e1.Records()) != before+1 || e1.Records()[before].Kind != RecordRejected {
		t.Fatal("可卖不足只能追加一条拒绝记录")
	}
	if e1.Sellable("A") != 0 {
		t.Fatalf("拒绝不得改变可卖占用: %d", e1.Sellable("A"))
	}

	// 路径二：先接受小限价卖单，再修改到与直接提交完全相同的参数，同样成功。
	e2, _ := NewEngine(100)
	mustSetMax(t, e2, "A", 100)
	mustQuote(t, e2, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e2, "A", 2, 1)
	if _, err := e2.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	sid2 := mustSell(t, e2, "A", 2, 1)
	before = len(e2.Records())
	if err := e2.Modify(sid2, 2, hugeLimit); err != nil {
		t.Fatalf("把有效卖单修改到整笔金额溢出的参数必须成功，与直接提交一致: %v", err)
	}
	if o, _ := e2.Order(sid2); o.Status != StatusPending || o.Limit != hugeLimit ||
		o.Qty != 2 || o.Filled != 0 || o.Remaining() != 2 {
		t.Fatalf("修改后卖单状态错误: %+v", o)
	}
	if e2.Cash() != 98 || e2.Position("A") != 2 || e2.Sellable("A") != 0 {
		t.Fatalf("修改卖单不得预先改变现金与持仓: cash=%d pos=%d sellable=%d",
			e2.Cash(), e2.Position("A"), e2.Sellable("A"))
	}
	if len(e2.Records()) != before+1 || e2.Records()[before].Kind != RecordModified {
		t.Fatalf("真实修改应只追加一条修改记录，实际新增 %d 条", len(e2.Records())-before)
	}

	// 两条路径接受的订单在后续成交结算上行为一致：首笔成交入账、次笔现金溢出拒绝。
	for i, e := range []*Engine{e1, e2} {
		// e1 的卖单仍挂 2 股；e2 的卖单刚改好也是 2 股。
		id := sid1
		if i == 1 {
			id = sid2
		}
		if _, err := e.Fill(Trade{TradeID: int64(10 + i), OrderID: id, Symbol: "A",
			Side: Sell, Price: hugeLimit, Qty: 1}); err != nil {
			t.Fatalf("路径 %d 首笔成交应正常入账: %v", i, err)
		}
		if _, err := e.Fill(Trade{TradeID: int64(20 + i), OrderID: id, Symbol: "A",
			Side: Sell, Price: hugeLimit, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
			t.Fatalf("路径 %d 第二笔现金加总溢出必须返回 ErrInt64Overflow，实际 %v", i, err)
		}
		if o, _ := e.Order(id); o.Status != StatusPartial || o.Filled != 1 || o.Remaining() != 1 {
			t.Fatalf("路径 %d 拒绝后前一笔成交与剩余卖单必须保留: %+v", i, o)
		}
	}
}
