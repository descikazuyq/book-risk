package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// setupBuyEquityOverflow 复现题述前置（全部状态由合法操作得到）：
// 初始现金 MaxInt64，交易日已开启且亏损保护尚未触线；账户总持仓金额上限未启用。
//
//	1. 合约 B 报价 3 时买入 1 股 @3（现金 MaxInt64-3，持仓市值 3，净值 MaxInt64）；
//	2. B 报价跌到 2：净值 MaxInt64-1，日内浮亏 1（上限 1000，不触线）；
//	3. 合约 A 报价 4，接受买单 2 股限价 4，首笔按成交价 3 买入 1 股：
//	   现金 MaxInt64-6，A 持仓 1 按报价 4 估值、B 持仓 1 按报价 2 估值，
//	   持仓市值合计 6，净值恰好 MaxInt64，亏损记零，订单部分成交、剩余 1 股有效。
//
// 第二笔回报 1 股 @2（不高于限价 4）时：本次成交金额 2、成交后现金
// MaxInt64-8、持仓市值合计 2*4+2 = 10，各自都能用 int64 表示，但现金加市值
// (MaxInt64-8)+10 = MaxInt64+2 超过 int64 最大值。成交价 2 低于报价 4：本次
// 买入本身没有造成亏损，是估值浮盈推高净值导致越界，不能因为亏损记零就接受。
//
// 同一成交编号改报 1 股 @4（恰为限价）后：现金 MaxInt64-10，持仓市值仍为 10，
// 成交后净值恰好等于 MaxInt64——用于验证被拒编号不被占用且边界成交正常入账。
func setupBuyEquityOverflow(t *testing.T) (e *Engine, oid int64) {
	t.Helper()
	e, _ = NewEngine(math.MaxInt64)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 3}, 1)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 4}, 1)
	if err := e.StartTradingDay(1, 1000); err != nil { // 开日基准净值 MaxInt64
		t.Fatal(err)
	}

	// 已有其他合约持仓：B 1 股，成交价 3；随后报价跌到 2，形成 1 的浮亏，
	// 为后续“成交价低于报价”的买入腾出净值空间（亏损 1，远低于上限 1000）。
	bid := mustBuy(t, e, "B", 1, 3)
	if _, err := e.Fill(Trade{TradeID: 10, OrderID: bid, Symbol: "B", Side: Buy, Price: 3, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 2, Price: 2}, 1)
	if e.RiskStatus().Restricted {
		t.Fatal("前置：B 报价下跌形成的浮亏不应触线")
	}

	// A 的买单已正常接受并部分成交，仍有剩余量。
	oid = mustBuy(t, e, "A", 2, 4) // 限价 4 × 2 = 8 现金占用
	res, err := e.Fill(Trade{TradeID: 1, OrderID: oid, Symbol: "A", Side: Buy, Price: 3, Qty: 1})
	if err != nil {
		t.Fatalf("首笔成交必须正常入账: %v", err)
	}
	if res.Status != StatusPartial || res.Filled != 1 || res.Remaining != 1 {
		t.Fatalf("首笔成交结果错误: %+v", res)
	}
	o, _ := e.Order(oid)
	if o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("订单应部分成交且仍有有效剩余量: %+v", o)
	}
	rs := e.RiskStatus()
	if !rs.Open || rs.Restricted || rs.Loss != 0 || rs.Equity != math.MaxInt64 {
		t.Fatalf("前置必须是已开日、未触线、亏损为零且净值 MaxInt64: %+v", rs)
	}
	return e, oid
}

// TestBuyFillEquityOverflowRejectsWholeFill 覆盖“成交后账户净值越界时整笔拒绝”：
// 本次成交金额、成交后现金与持仓市值各自可表示，只有现金加市值所得净值溢出 int64。
// Fill 必须返回零值结果和可通过 errors.Is 识别的 ErrInt64Overflow，且不能先记账再
// 撤掉剩余量代替拒绝——现金、可用现金、现金占用、各合约持仓、订单累计成交量/有效
// 剩余量/状态、交易日基准、亏损上限与限制状态全部维持提交前的值；事件记录只新增
// 一条拒绝，保留回报的订单编号、成交编号、合约、方向、价格与数量，原因说明成交入账
// 将使净值越界，并固化当时的报价、持仓限额与日内风险快照，先前的接受和成交记录保留。
func TestBuyFillEquityOverflowRejectsWholeFill(t *testing.T) {
	e, oid := setupBuyEquityOverflow(t)

	cashBefore := e.Cash()             // MaxInt64-6
	reservedBefore := e.ReservedCash() // 剩余 1 股 × 限价 4
	availableBefore := e.AvailableCash()
	posABefore := e.Position("A") // 1
	posBBefore := e.Position("B") // 1
	riskBefore := e.RiskStatus()  // 净值 MaxInt64，亏损 0
	oBefore, _ := e.Order(oid)
	recBefore := len(e.Records())

	res, err := e.Fill(Trade{TradeID: 2, OrderID: oid, Symbol: "A", Side: Buy, Price: 2, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("成交后净值越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if res != (FillResult{}) {
		t.Fatalf("越界拒绝必须返回零值成交结果，实际 %+v", res)
	}

	// 资金与占用维持提交前的值：不得先按成交价扣现金再撤单。
	if e.Cash() != cashBefore || e.ReservedCash() != reservedBefore ||
		e.AvailableCash() != availableBefore {
		t.Fatalf("拒绝不得改变现金与占用: cash=%d(=%d) reserved=%d(=%d) available=%d(=%d)",
			e.Cash(), cashBefore, e.ReservedCash(), reservedBefore,
			e.AvailableCash(), availableBefore)
	}
	// 各合约持仓维持提交前的值：本次买入不得入账，B 的已有持仓同样不变。
	if e.Position("A") != posABefore || e.Position("B") != posBBefore {
		t.Fatalf("拒绝不得改变任何合约持仓: A=%d(=%d) B=%d(=%d)",
			e.Position("A"), posABefore, e.Position("B"), posBBefore)
	}
	// 订单累计成交量、有效剩余量与状态维持提交前的值：不能以撤掉剩余量代替拒绝。
	o, _ := e.Order(oid)
	if o.Filled != oBefore.Filled || o.Qty != oBefore.Qty || o.Limit != oBefore.Limit ||
		o.Status != oBefore.Status || o.Remaining() != oBefore.Remaining() || o.Reason != "" {
		t.Fatalf("拒绝不得改变订单: got=%+v remaining=%d, want=%+v remaining=%d",
			o, o.Remaining(), oBefore, oBefore.Remaining())
	}
	if o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("订单必须仍是有剩余量的部分成交: %+v", o)
	}
	// 交易日基准、亏损上限与限制状态不变。
	rs := e.RiskStatus()
	if !rs.Open || rs.Day != riskBefore.Day || rs.Baseline != riskBefore.Baseline ||
		rs.LossLimit != riskBefore.LossLimit || rs.Restricted != riskBefore.Restricted ||
		rs.Equity != riskBefore.Equity || rs.Loss != riskBefore.Loss {
		t.Fatalf("拒绝不得改变风险状态: got=%+v, before=%+v", rs, riskBefore)
	}

	// 事件记录只新增一条拒绝，先前的接受与成交记录保留原样。
	recs := e.Records()
	if len(recs) != recBefore+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-recBefore)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝，实际 %s", rj.Kind)
	}
	if rj.OrderID != oid || rj.TradeID != 2 || rj.Symbol != "A" || rj.Side != Buy ||
		rj.TradePrice != 2 || rj.Qty != 1 {
		t.Fatalf("拒绝记录必须完整保留本次回报要素: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "净值") || !strings.Contains(rj.Reason, "超出 int64") {
		t.Fatalf("拒绝原因必须说明成交入账将使净值越界: %q", rj.Reason)
	}
	// 固化当时的报价与持仓限额快照。
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 1 || rj.QuotePrice != 4 {
		t.Fatalf("拒绝记录必须保存当时的 A 报价快照: %+v", rj)
	}
	if !rj.MaxPositionValid || rj.MaxPosition != 100 {
		t.Fatalf("拒绝记录必须保存当时的持仓限额快照: %+v", rj)
	}
	// 固化日内风险快照（提交前状态：净值 MaxInt64、未触线、亏损为零）。
	if rj.RiskDay != 1 || rj.RiskBaseline != riskBefore.Baseline ||
		rj.RiskEquity != riskBefore.Equity || rj.RiskLoss != 0 ||
		rj.RiskLimit != riskBefore.LossLimit || rj.RiskRestrict {
		t.Fatalf("拒绝记录必须保存提交前的日内风险快照: %+v, before=%+v", rj, riskBefore)
	}
	// 先前记录保留：接受与成交仍在，且没有任何触线或撤销记录混入。
	var acceptKinds, fillKinds int
	for _, r := range recs[:len(recs)-1] {
		switch r.Kind {
		case RecordAccepted:
			acceptKinds++
		case RecordFilled:
			fillKinds++
		case RecordRiskTriggered, RecordCanceled:
			t.Fatalf("越界拒绝不得触发触线或撤单记录: %+v", r)
		}
	}
	if acceptKinds != 2 || fillKinds != 2 { // B、A 两张买单与各自首笔成交
		t.Fatalf("先前的接受与成交记录必须保留原样: accept=%d fill=%d", acceptKinds, fillKinds)
	}
}

// TestBuyFillEquityExactlyMaxInt64Accepted 区分恰好达到上界的合法成交：成交后账户
// 净值正好等于 int64 最大值时必须正常入账——现金按成交金额减少，持仓与累计成交量
// 增加，只释放本次成交量对应的限价现金占用，未成交部分继续有效；成功后的净值准确
// 显示为 MaxInt64，浮盈对应的日内亏损仍为零。
func TestBuyFillEquityExactlyMaxInt64Accepted(t *testing.T) {
	e, _ := NewEngine(math.MaxInt64)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 3}, 1)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 4}, 1)
	if err := e.StartTradingDay(1, 1000); err != nil {
		t.Fatal(err)
	}
	bid := mustBuy(t, e, "B", 1, 3)
	if _, err := e.Fill(Trade{TradeID: 10, OrderID: bid, Symbol: "B", Side: Buy, Price: 3, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 2, Price: 2}, 1)

	// A 买单 3 股限价 4：
	//   首笔 1 股 @3 后现金 MaxInt64-6、持仓市值 4+2=6，净值 MaxInt64；
	//   第二笔 1 股 @4 后现金 MaxInt64-10、持仓市值 2*4+2=10，净值恰为 MaxInt64；
	//   未成交 1 股继续有效，尾笔 1 股 @4 后净值仍为 MaxInt64。
	oid := mustBuy(t, e, "A", 3, 4)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: oid, Symbol: "A", Side: Buy, Price: 3, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	reservedAfterFirst := e.ReservedCash() // 剩余 2 股 × 限价 4 = 8

	res, err := e.Fill(Trade{TradeID: 2, OrderID: oid, Symbol: "A", Side: Buy, Price: 4, Qty: 1})
	if err != nil {
		t.Fatalf("成交后净值恰好等于 MaxInt64 必须正常入账: %v", err)
	}
	// 部分成交：本次 1 股已入账，未成交 1 股继续有效。
	if res.TradeID != 2 || res.Status != StatusPartial || res.Qty != 1 ||
		res.Filled != 2 || res.Remaining != 1 {
		t.Fatalf("边界成交结果错误: %+v", res)
	}
	wantCash := int64(math.MaxInt64 - 10)
	if e.Cash() != wantCash {
		t.Fatalf("现金应按实际成交金额减少: got=%d want=%d", e.Cash(), wantCash)
	}
	// 只释放本次成交量对应的限价占用（4），未成交部分继续占用。
	if e.ReservedCash() != reservedAfterFirst-4 {
		t.Fatalf("只应释放本次成交量对应的限价占用 4: reserved=%d", e.ReservedCash())
	}
	if e.AvailableCash() != wantCash-e.ReservedCash() {
		t.Fatalf("可用现金口径错误: available=%d", e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("本次买入的持仓应增加，其他合约持仓不变: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	o, _ := e.Order(oid)
	if o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 1 {
		t.Fatalf("未成交部分必须继续有效: %+v", o)
	}
	rs := e.RiskStatus()
	if rs.Equity != math.MaxInt64 {
		t.Fatalf("成交后净值应准确显示为 MaxInt64: got=%d", rs.Equity)
	}
	if rs.Loss != 0 || rs.Restricted {
		t.Fatalf("浮盈对应的日内亏损必须为零且不触线: %+v", rs)
	}

	// 未成交部分仍是有效订单：剩余 1 股继续成交（@4）应正常入账。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: oid, Symbol: "A", Side: Buy, Price: 4, Qty: 1}); err != nil {
		t.Fatalf("边界成交后剩余量必须仍可成交: %v", err)
	}
	o, _ = e.Order(oid)
	if o.Status != StatusFilled || o.Filled != 3 || o.Remaining() != 0 || e.ReservedCash() != 0 {
		t.Fatalf("尾笔成交后订单应全部成交、占用清零: %+v reserved=%d", o, e.ReservedCash())
	}
	if s := e.RiskStatus(); s.Equity != math.MaxInt64 || s.Loss != 0 {
		t.Fatalf("尾笔成交后净值仍应恰为 MaxInt64、亏损为零: %+v", s)
	}
}

// TestRejectedBuyFillTradeIDReusableAtExactBoundary 此前因净值越界被拒绝的成交不得
// 占用成交编号：修正回报（成交价 2 改为 4，恰为限价）使成交后净值恰好等于 int64
// 最大值时，同一编号仍可成功提交；现金、持仓、累计成交量正常推进，成功后净值准确
// 显示为 MaxInt64、日内亏损仍为零；同编号同内容重放返回首次结果且不新增记录。
func TestRejectedBuyFillTradeIDReusableAtExactBoundary(t *testing.T) {
	e, oid := setupBuyEquityOverflow(t)
	recBefore := len(e.Records())

	// 原始回报 1 股 @2：成交后净值 MaxInt64+2 越界，被拒绝且不占用编号 2。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: oid, Symbol: "A", Side: Buy, Price: 2, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("前置：净值越界必须拒绝，实际 %v", err)
	}

	// 同一编号修正为 1 股 @4：现金 MaxInt64-10，持仓市值 2*4+2 = 10，
	// 成交后净值恰好等于 MaxInt64。若编号已被占用，会报“已用于不同内容”，
	// 而不是正常入账。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: oid, Symbol: "A", Side: Buy, Price: 4, Qty: 1})
	if err != nil {
		t.Fatalf("被拒编号修正后满足边界时必须可成功提交: %v", err)
	}
	if res.TradeID != 2 || res.Status != StatusFilled || res.Filled != 2 || res.Remaining != 0 {
		t.Fatalf("复用编号的边界成交结果错误: %+v", res)
	}
	wantCash := int64(math.MaxInt64 - 10)
	if e.Cash() != wantCash {
		t.Fatalf("修正成交应按成交价 4 扣减现金: got=%d want=%d", e.Cash(), wantCash)
	}
	if e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("修正成交应正常增加持仓: A=%d B=%d", e.Position("A"), e.Position("B"))
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("全部成交后限价占用应清零: %d", e.ReservedCash())
	}
	o, _ := e.Order(oid)
	if o.Status != StatusFilled || o.Filled != 2 || o.Remaining() != 0 {
		t.Fatalf("订单应全部成交: %+v", o)
	}
	rs := e.RiskStatus()
	if rs.Equity != math.MaxInt64 {
		t.Fatalf("修正后成交净值应恰好为 MaxInt64: got=%d", rs.Equity)
	}
	if rs.Loss != 0 || rs.Restricted {
		t.Fatalf("边界成交的日内亏损必须为零且不触线: %+v", rs)
	}

	// 记录上只比提交前（越界拒绝已含）多一条成交。
	recs := e.Records()
	if len(recs) != recBefore+2 { // 一条拒绝 + 一条成交
		t.Fatalf("只应新增拒绝与成交各一条，实际新增 %d 条", len(recs)-recBefore)
	}
	if recs[len(recs)-1].Kind != RecordFilled || recs[len(recs)-1].TradeID != 2 {
		t.Fatalf("最后一条必须是编号 2 的成交记录: %+v", recs[len(recs)-1])
	}
	// 同编号同内容重放：永远返回首次结果，不再次记账、不新增记录。
	replay, err := e.Fill(Trade{TradeID: 2, OrderID: oid, Symbol: "A", Side: Buy, Price: 4, Qty: 1})
	if err != nil || replay != res {
		t.Fatalf("同编号同内容重放必须返回首次结果: replay=%+v err=%v", replay, err)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("幂等重放不得新增记录")
	}
}
