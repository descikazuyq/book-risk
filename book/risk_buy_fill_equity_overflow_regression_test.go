package book

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“交易日已开启、亏损保护尚未触线时，买入成交入账将使账户净值（现金 +
// 各合约持仓按最新已生效报价计算的市值）超出 int64 范围”补充回归保障：该笔成交
// 必须整笔拒绝（返回零值 FillResult 与可 errors.Is 识别的 ErrInt64Overflow），
// 不能先记账再撤掉剩余量代替拒绝；被拒成交不占用编号。同时区分成交后净值恰好
// 等于 int64 最大值的合法成交：必须正常入账，净值准确显示为 MaxInt64，浮盈对应
// 的日内亏损记零。
//
// 数值构造（各分量均可表示，唯独现金加市值溢出）：
//
//	M = MaxInt64 = 2^63-1；q = M/2 = 2^62-1；P = q-1，故 2P+1 = M-2。
//
// 主场景先以 M-6 的成交价买入 B 合约 1 股（B 的报价仅为 1，成交价与估值价是两个
// 价格），现金余 6、B 持仓市值 1；A 合约报价 P，接受总量 3、限价 2 的买单（占用
// 现金 6），先成交 1 股 @2：现金 4、A 持仓 1、剩余占用 4。此时开启交易日，基准
// 净值 4+P+1 = q+4，亏损上限给到 M，保护尚未触线。
//
// 再提交 1 股 @1 的买入成交（1 < 限价 2，合法；1 远低于报价 P）：
//   - 成交金额 1、入账后现金 3、持仓市值合计 2P+1 = M-2 各自都能用 int64 表示；
//   - 净值 = 现金 3 + A 市值 2P + B 市值 1 = 3 + M-2 = M+1，超出 int64 上界；
//   - 本次买入按成交价结算根本没有亏损，估值后还是浮盈（净值高于基准），亏损记
//     零也不能接受——越界的是净值本身，与亏损是否触线无关。
//
// B 的 1 单位市值是决定性的：同样构造但不持有 B 时，3 + 2P = M 恰好是上界
// （见对照测试）。同一成交编号改报 1 股 @2 后：现金 2、净值 2+2P+1 = M 恰好
// 达到上界，正常入账，仅释放本次 1 股对应的限价占用 2（剩余占用 4→2），订单
// 仍有 1 股有效剩余；风险净值准确显示为 M，浮盈亏损为零。

// equityOverflowBuySetup 构造主场景的提交前状态：
// 现金 4、A 持仓 1（报价 seq1@P）、B 持仓 1（报价 seq1@1）；A 的买单总量 3、
// 限价 2，已成交 1 股 @2，仍有 2 股有效剩余、占用现金 4；交易日 1 已开启
// （基准 q+4、亏损上限 M），保护尚未触线，账户总持仓金额上限未启用。
func equityOverflowBuySetup(t *testing.T) (e *Engine, aid int64) {
	t.Helper()
	const M = math.MaxInt64
	P := int64(M/2 - 1) // 2P+1 = M-2

	e, _ = NewEngine(M)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: P}, 1)

	// 以 M-6 买入 B 1 股：成交价与报价无关，现金余 6，B 持仓按报价 1 估值。
	bid := mustBuy(t, e, "B", 1, M-6)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "B", Side: Buy, Price: M - 6, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 6 || e.Position("B") != 1 {
		t.Fatalf("B 前置成交后状态错误: cash=%d posB=%d", e.Cash(), e.Position("B"))
	}

	// A 买单总量 3、限价 2：占用 6，恰好用尽可用现金；账户金额上限未启用。
	aid = mustBuy(t, e, "A", 3, 2)
	// 已有部分成交 1 股 @2：现金 4、A 持仓 1，剩余 2 股继续占用 4。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: aid, Symbol: "A", Side: Buy, Price: 2, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 4 || e.ReservedCash() != 4 || e.AvailableCash() != 0 ||
		e.Position("A") != 1 || e.Position("B") != 1 {
		t.Fatalf("A 前置部分成交后状态错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}

	// 开启交易日：基准 4 + P + 1 = q+4；上限给到 M，任何浮盈/小亏都不会触线。
	if err := e.StartTradingDay(1, M); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	wantBaseline := int64(M/2) + 4
	if !st.Open || st.Day != 1 || st.Baseline != wantBaseline || st.Equity != wantBaseline ||
		st.Loss != 0 || st.LossLimit != M || st.Restricted {
		t.Fatalf("开日后的前置风险状态错误: %+v", st)
	}
	if e.PositionAmountStatus().Enabled {
		t.Fatal("本场景要求账户总持仓金额上限未启用")
	}
	return e, aid
}

// TestRiskBuyFillEquityOverflowRejectsWholeFill 覆盖主场景：1 股 @1 的成交入账
// 将使净值达到 M+1，必须整笔拒绝并返回 ErrInt64Overflow；资金、各合约持仓、
// 订单累计成交量/剩余量/状态、交易日基准/上限/限制状态全部维持提交前的值，
// 只新增一条要素与快照完整的拒绝记录。
func TestRiskBuyFillEquityOverflowRejectsWholeFill(t *testing.T) {
	const M = math.MaxInt64
	P := int64(M/2 - 1)
	e, aid := equityOverflowBuySetup(t)

	// 提交前快照：资金、持仓、订单、风险状态与既有全部记录。
	cash, reserved, available := e.Cash(), e.ReservedCash(), e.AvailableCash()
	posA, posB := e.Position("A"), e.Position("B")
	sellA, sellB := e.Sellable("A"), e.Sellable("B")
	riskBefore := e.RiskStatus()
	orderBefore, _ := e.Order(aid)
	recordsBefore := append([]Record(nil), e.Records()...)
	if len(recordsBefore) != 4 { // B 接受/成交、A 接受/成交
		t.Fatalf("前置记录数应为 4，实际 %d", len(recordsBefore))
	}

	res, err := e.Fill(Trade{TradeID: 3, OrderID: aid, Symbol: "A", Side: Buy, Price: 1, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("净值越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if res != (FillResult{}) {
		t.Fatalf("整笔拒绝必须返回零值成交结果，实际 %+v", res)
	}

	// 资金三要素与各合约持仓（含可卖数量）全部维持提交前的值。
	if e.Cash() != cash || e.ReservedCash() != reserved || e.AvailableCash() != available {
		t.Fatalf("拒绝不得改变现金余额/占用/可用: cash=%d(before %d) reserved=%d(before %d) available=%d(before %d)",
			e.Cash(), cash, e.ReservedCash(), reserved, e.AvailableCash(), available)
	}
	if e.Position("A") != posA || e.Position("B") != posB ||
		e.Sellable("A") != sellA || e.Sellable("B") != sellB {
		t.Fatalf("拒绝不得改变任何合约持仓: posA=%d(before %d) posB=%d(before %d) sellA=%d sellB=%d",
			e.Position("A"), posA, e.Position("B"), posB, e.Sellable("A"), e.Sellable("B"))
	}

	// 订单：累计成交量仍为 1，订单仍为部分成交、有效剩余 2，拒绝不能代替撤单。
	o, _ := e.Order(aid)
	if o.Qty != 3 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 2 || o.Reason != "" {
		t.Fatalf("拒绝不得改变订单累计成交量、剩余量与状态: %+v", o)
	}
	if o != orderBefore {
		t.Fatalf("拒绝前后订单快照必须完全一致: before=%+v after=%+v", orderBefore, o)
	}

	// 交易日基准、亏损上限与限制状态不变；净值仍为提交前的 q+4，不得触线。
	riskAfter := e.RiskStatus()
	if riskAfter != riskBefore {
		t.Fatalf("拒绝不得改变风险状态: before=%+v after=%+v", riskBefore, riskAfter)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("净值越界拒绝不得产生风险触线记录")
	}

	// 只新增一条拒绝记录；先前的接受和成交记录保留原样。
	recs := e.Records()
	if len(recs) != len(recordsBefore)+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-len(recordsBefore))
	}
	if !reflect.DeepEqual(recordsBefore, recs[:len(recordsBefore)]) {
		t.Fatal("拒绝不得改写先前的接受和成交记录")
	}
	rj := recs[len(recordsBefore)]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝，实际 %s", rj.Kind)
	}
	// 保留本次回报的订单编号、成交编号、合约、方向、价格和数量。
	if rj.OrderID != aid || rj.TradeID != 3 || rj.Symbol != "A" || rj.Side != Buy ||
		rj.TradePrice != 1 || rj.Qty != 1 {
		t.Fatalf("拒绝记录的回报要素错误: %+v", rj)
	}
	// 原因说明成交入账将使净值越界（不能只提亏损，本笔成交估值后是浮盈、亏损为零）。
	if !strings.Contains(rj.Reason, "成交 3") || !strings.Contains(rj.Reason, "净值") ||
		!strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须说明成交 3 入账将使净值超出 int64 范围: %q", rj.Reason)
	}
	// 当时的报价快照：A 的最新已生效报价 seq1、时刻 10、价格 P（成交价 1 不得混入）。
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 10 || rj.QuotePrice != P {
		t.Fatalf("拒绝记录必须保存当时的 A 报价快照: %+v", rj)
	}
	// 当时的持仓限额快照。
	if !rj.MaxPositionValid || rj.MaxPosition != 100 {
		t.Fatalf("拒绝记录必须保存当时的持仓限额快照: %+v", rj)
	}
	// 日内风险快照：日号、基准 q+4、提交前净值与亏损 0、上限 M、尚未限制。
	if rj.RiskDay != 1 || rj.RiskBaseline != riskBefore.Baseline ||
		rj.RiskEquity != riskBefore.Equity || rj.RiskLoss != 0 ||
		rj.RiskLimit != M || rj.RiskRestrict {
		t.Fatalf("拒绝记录必须保存提交时点的日内风险快照: %+v", rj)
	}
	// 账户总持仓金额上限未启用，拒绝记录不得携带金额上限快照。
	if rj.AmtEnabled || rj.AmtQuoteRefs != nil {
		t.Fatalf("金额上限未启用时拒绝记录不得携带金额快照: %+v", rj)
	}
}

// TestRiskBuyFillEquityOverflowTradeIDReusableAtExactMax 覆盖上界边界与编号复用：
// 被净值越界拒绝的成交 3 不占用编号；同一编号把价格修正为 2 后，成交后净值恰好
// 等于 MaxInt64，必须正常入账——现金按成交金额减少、持仓与累计成交量增加、只
// 释放本次 1 股的限价占用（4→2），未成交的 1 股继续有效；净值准确显示为 M，
// 浮盈对应日内亏损为零，订单不触线、不撤销。
func TestRiskBuyFillEquityOverflowTradeIDReusableAtExactMax(t *testing.T) {
	const M = math.MaxInt64
	e, aid := equityOverflowBuySetup(t)

	// 先以编号 3 报 1 股 @1：净值 M+1 被整笔拒绝（编号不被占用）。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: aid, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("前置越界成交必须被 ErrInt64Overflow 拒绝，实际 %v", err)
	}
	recordsAfterReject := len(e.Records())

	// 同一编号、修正为 1 股 @2（仍不高于限价 2）：现金 4→2，A 持仓 1→2，
	// 净值 = 2 + 2P + 1 = M 恰好达到上界，必须正常入账，不能报“编号已用于不同内容”。
	res, err := e.Fill(Trade{TradeID: 3, OrderID: aid, Symbol: "A", Side: Buy, Price: 2, Qty: 1})
	if err != nil {
		t.Fatalf("被拒编号修正回报后应按新内容重新校验并成功，实际: %v", err)
	}
	if res != (FillResult{TradeID: 3, OrderID: aid, Price: 2, Qty: 1,
		Filled: 2, Remaining: 1, Status: StatusPartial}) {
		t.Fatalf("边界成交结果应为累计 2、剩余 1、部分成交: %+v", res)
	}

	// 现金按成交金额 2 减少；只释放本次 1 股对应的限价占用 2（4→2），
	// 未成交的 1 股继续占用 2；各合约持仓增加/保留。
	if e.Cash() != 2 || e.ReservedCash() != 2 || e.AvailableCash() != 0 {
		t.Fatalf("边界成交后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 || e.Position("B") != 1 || e.Sellable("A") != 2 {
		t.Fatalf("边界成交后持仓错误: posA=%d posB=%d sellA=%d",
			e.Position("A"), e.Position("B"), e.Sellable("A"))
	}
	o, _ := e.Order(aid)
	if o.Qty != 3 || o.Filled != 2 || o.Status != StatusPartial || o.Remaining() != 1 || o.Reason != "" {
		t.Fatalf("边界成交后订单应保留 1 股有效剩余: %+v", o)
	}

	// 净值准确显示为 int64 最大值；基准 q+4 高于…低于净值，浮盈对应亏损记零；
	// 不触线、不限制。
	st := e.RiskStatus()
	wantBaseline := int64(M/2) + 4
	if !st.Open || st.Baseline != wantBaseline || st.Equity != M ||
		st.Loss != 0 || st.LossLimit != M || st.Restricted {
		t.Fatalf("边界成交后净值应恰为 MaxInt64 且浮盈亏损为零: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("恰好达到上界不得产生触线记录")
	}

	// 只在拒绝记录之后新增一条成交记录；成交记录固化记账后净值 M 与零亏损。
	recs := e.Records()
	if len(recs) != recordsAfterReject+1 {
		t.Fatalf("边界成交只能再新增一条记录，实际 %d 条", len(recs)-recordsAfterReject)
	}
	fr := recs[len(recs)-1]
	if fr.Kind != RecordFilled || fr.TradeID != 3 || fr.OrderID != aid ||
		fr.TradePrice != 2 || fr.Qty != 1 || fr.Filled != 2 || fr.Remaining != 1 {
		t.Fatalf("成交记录要素错误: %+v", fr)
	}
	if fr.RiskEquity != M || fr.RiskLoss != 0 || fr.RiskRestrict {
		t.Fatalf("成交记录风险快照应固化净值 MaxInt64、零亏损、未限制: %+v", fr)
	}

	// 同编号同内容重放：原样返回首次结果，不再次记账、不新增记录。
	replay, err := e.Fill(Trade{TradeID: 3, OrderID: aid, Symbol: "A", Side: Buy, Price: 2, Qty: 1})
	if err != nil {
		t.Fatalf("成功成交的同编号重放必须成功: %v", err)
	}
	if replay != res {
		t.Fatalf("重放必须原样返回边界成交结果: first=%+v replay=%+v", res, replay)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("重放不得新增记录")
	}
	if e.Cash() != 2 || e.ReservedCash() != 2 || e.Position("A") != 2 {
		t.Fatalf("重放不得改变资金与持仓: cash=%d reserved=%d posA=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 剩余 1 股仍是有效剩余：撤销只释放这 1 股的限价占用 2，已入账持仓与现金保留。
	if err := e.Cancel(aid); err != nil {
		t.Fatalf("剩余 1 股必须仍可撤销（证明订单仍有效）: %v", err)
	}
	if e.ReservedCash() != 0 || e.Cash() != 2 || e.Position("A") != 2 {
		t.Fatalf("撤销剩余应只释放 2 现金占用: reserved=%d cash=%d posA=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"))
	}
	if o2, _ := e.Order(aid); o2.Status != StatusCanceled || o2.Filled != 2 || o2.Remaining() != 0 {
		t.Fatalf("撤销后订单形态错误: %+v", o2)
	}
}

// TestRiskBuyFillEquityExactMaxWithoutOtherPosition 是决定性对照：与主场景相同的
// 1 股 @1 成交，但账户不持有 B（没有那 1 单位其他合约市值），成交后净值
// 3 + 2P = M 恰好等于上界，必须正常入账。这同时验证：
//   - 已有其他合约持仓计入账户净值（主场景正因 B 的 1 单位市值而越界）；
//   - 成交价 1 低于限价 2 时，也只释放本次成交量对应的“限价”占用 2（4→2），
//     未成交的 1 股继续有效；
//   - 恰好达到上界是合法成交，净值显示为 M，浮盈亏损记零。
func TestRiskBuyFillEquityExactMaxWithoutOtherPosition(t *testing.T) {
	const M = math.MaxInt64
	P := int64(M/2 - 1)

	// 不买入 B：初始现金只需覆盖买单占用 6。
	e, _ := NewEngine(6)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: P}, 1)
	aid := mustBuy(t, e, "A", 3, 2) // 占用 6，可用现金恰为 0
	// 先成交 1 股 @2：现金 4、持仓 1、剩余占用 4。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aid, Symbol: "A", Side: Buy, Price: 2, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 开日基准 = 4 + P = q+3；上限 M，不触线。
	if err := e.StartTradingDay(1, M); err != nil {
		t.Fatal(err)
	}
	wantBaseline := int64(M/2) + 3
	if e.RiskStatus().Baseline != wantBaseline || e.RiskStatus().Loss != 0 || e.RiskStatus().Restricted {
		t.Fatalf("前置风险状态错误: %+v", e.RiskStatus())
	}

	// 1 股 @1：现金 3、持仓 2，市值 2P = M-3；净值 3 + M-3 = M，恰好上界。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: aid, Symbol: "A", Side: Buy, Price: 1, Qty: 1})
	if err != nil {
		t.Fatalf("净值恰好等于 MaxInt64 必须正常入账: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: aid, Price: 1, Qty: 1,
		Filled: 2, Remaining: 1, Status: StatusPartial}) {
		t.Fatalf("边界成交结果错误: %+v", res)
	}
	// 现金按成交价只减 1；占用按限价 2 释放（不是按成交价 1），剩余 1 股继续占用 2。
	if e.Cash() != 3 || e.ReservedCash() != 2 || e.AvailableCash() != 1 {
		t.Fatalf("资金口径错误：现金按成交价扣减、占用按限价释放: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 {
		t.Fatalf("持仓应增加到 2，实际 %d", e.Position("A"))
	}
	if o, _ := e.Order(aid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 1 {
		t.Fatalf("未成交的 1 股必须继续有效: %+v", o)
	}
	st := e.RiskStatus()
	if st.Equity != M || st.Loss != 0 || st.Restricted {
		t.Fatalf("净值应恰为 MaxInt64、浮盈亏损为零且不触线: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("恰好达到上界不得产生触线记录")
	}

	// 同编号同内容重放原样返回，资金、持仓、订单与记录均不再变化。
	before := len(e.Records())
	replay, err := e.Fill(Trade{TradeID: 2, OrderID: aid, Symbol: "A", Side: Buy, Price: 1, Qty: 1})
	if err != nil {
		t.Fatalf("重放必须成功: %v", err)
	}
	if replay != res || len(e.Records()) != before {
		t.Fatalf("重放必须原样返回且不新增记录: replay=%+v res=%+v records=%d->%d",
			replay, res, before, len(e.Records()))
	}
	if e.Cash() != 3 || e.ReservedCash() != 2 || e.Position("A") != 2 {
		t.Fatalf("重放不得改变状态: cash=%d reserved=%d posA=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
}
