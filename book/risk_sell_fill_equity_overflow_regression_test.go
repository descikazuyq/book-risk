package book

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“日内亏损保护已经触线、买入已被限制之后，卖出成交所得能够合法加入现金，
// 但成交后账户净值（现金余额 + 剩余持仓按最新已生效报价计算的市值）超出 int64
// 范围”补充回归保障：该笔成交必须整笔拒绝（返回零值 FillResult 与可 errors.Is
// 识别的 ErrInt64Overflow），现金、持仓、卖单累计成交量/状态、未成交部分占用的
// 持仓与可卖数量全部维持提交前的值，已有成交不能被撤回；只追加一条保存本次成交
// 编号、订单编号、价格、数量与当时报价定位的拒绝记录，不新增成交、触线或撤单记录。
//
// 限制买入以后卖单仍可成交，净值检查也继续生效；被拒成交不占用编号。同一编号把
// 价格修正为仍满足限价的值、使成交后净值恰好等于 int64 最大值时必须正常结算：
// 现金按成交价增加、持仓按本次数量减少，只释放对应数量的卖单占用；风险查询准确
// 显示最大净值，浮盈时亏损为零但买入限制保持，且这笔合法卖出不能再次产生当日
// 首次触线记录。卖出后的净值始终按现金余额加剩余持仓的最新已生效报价市值计算，
// 成交价只用于本笔现金结算。
//
// 数值构造（各分量均可表示，唯独现金加剩余市值溢出）：
//
//	M = MaxInt64 = 2^63-1；p = 2^61；q = 2^62 = 2p，故 4p = M+1、2q = M+1。
//
// 初始现金 6：以价格 2 买入 A 3 股并一次性全部成交，现金 0、持仓 3；A 报价
// seq1@时刻1 价格 p。随后以零亏损上限开启交易日 1：基准净值 3p，零上限开日
// 立即触线，账户进入限制（买单已全部成交，无剩余买单可撤），新买单从此被拒绝。
// 再接受 A 的总量 3、限价 p 卖单，先成交 1 股 @p：现金 p、持仓 2，卖单剩余 2
// 股继续占用可卖数量（可卖 0），提交前净值 p+2p = 3p 合法。
//
// 提交 1 股 @q（q=2p，不低于限价 p）：
//   - 成交金额 q、成交后现金 p+q = 3p、剩余持仓市值 1×p = p 各自都在 int64 范围内；
//   - 净值 = (p+q) + p = 4p = M+1，超出 int64 上界；
//   - 现金加总本身不溢出（3p < M），本场景必须走“成交后净值越界”而非
//     “卖出所得加回现金余额溢出”或“成交价低于限价”的拒绝路径。
//
// 同一编号改报 1 股 @q-1（仍不低于限价 p）：成交后现金 3p-1、剩余市值 p，
// 净值 4p-1 = M 恰好达到上界，正常入账，只释放本次 1 股的卖单占用，剩余 1 股
// 继续有效；该剩余部分随后按限价 p 成交，现金恰好增加到 M。对照测试中账户只
// 持有 1 股，同样以 q 卖出后剩余持仓市值为 0，净值 q < M，必须正常成交——
// 决定主场景越界的正是那 1 股剩余持仓按报价 p 计入的市值。

// equityOverflowSellSetup 构造主场景的提交前状态：
// 现金 p、A 持仓 2（报价 seq1@时刻1 价格 p）；A 的卖单总量 3、限价 p，已成交
// 1 股 @p，仍有 2 股有效剩余并占用 2 股可卖数量（可卖 0）；交易日 1 已开启
// （基准 3p、亏损上限 0），零上限开日即触线，账户已处于限制状态；账户总持仓
// 金额上限未启用。
func equityOverflowSellSetup(t *testing.T) (e *Engine, ask int64) {
	t.Helper()
	const M = math.MaxInt64
	p := int64(M/4 + 1) // 2^61

	e, _ = NewEngine(6)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: p}, 1)

	// 以价格 2 买入 3 股并一次性全部成交：现金 0、持仓 3，买单不再留有占用。
	bid := mustBuy(t, e, "A", 3, 2)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 2, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 0 || e.ReservedCash() != 0 || e.Position("A") != 3 {
		t.Fatalf("前置买入成交后状态错误: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 零亏损上限开启交易日：基准 3p，开日立即触线，账户进入限制状态。
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatal(err)
	}
	baseline := 3 * p
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != baseline || st.Equity != baseline ||
		st.Loss != 0 || st.LossLimit != 0 || !st.Restricted {
		t.Fatalf("零上限开日后的前置风险状态错误: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatalf("零上限开日应产生一条触线记录，实际 %d 条", countRiskRecords(e))
	}

	// 限制买入以后卖单仍可接受：总量 3、限价 p，占用全部 3 股可卖数量。
	ask = mustSell(t, e, "A", 3, p)
	// 先成交 1 股 @p：现金 p、持仓 2，剩余 2 股继续占用可卖数量。
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: p, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != p || e.ReservedCash() != 0 || e.AvailableCash() != p ||
		e.Position("A") != 2 || e.Sellable("A") != 0 {
		t.Fatalf("前置部分卖出后状态错误: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(ask)
	if o.Qty != 3 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 2 ||
		o.Limit != p || o.Reason != "" {
		t.Fatalf("前置卖单状态错误: %+v", o)
	}
	// 已限制状态下的卖出成交不解除限制，也不再次触线。
	st = e.RiskStatus()
	if !st.Restricted || st.Equity != baseline || st.Loss != 0 {
		t.Fatalf("前置部分卖出后风险状态错误: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatalf("已限制后的卖出成交不得新增触线记录，实际 %d 条", countRiskRecords(e))
	}
	if e.PositionAmountStatus().Enabled {
		t.Fatal("本场景要求账户总持仓金额上限未启用")
	}
	if len(e.Records()) != 5 { // 买单接受/成交、开日触线、卖单接受/成交
		t.Fatalf("前置记录数应为 5，实际 %d", len(e.Records()))
	}
	return e, ask
}

// TestRiskRestrictedSellFillEquityOverflowRejectsWholeFill 覆盖主场景：账户已开启
// 交易日且处于买入限制状态时，1 股 @q 的卖出成交所得与成交后现金都在 int64
// 范围内，但现金加剩余持仓报价市值达到 M+1，必须整笔拒绝并返回 ErrInt64Overflow；
// 资金、持仓、卖单累计成交量/剩余量/状态、交易日基准/上限/限制状态全部维持提交
// 前的值，已有成交保留，只新增一条要素与快照完整的拒绝记录。
func TestRiskRestrictedSellFillEquityOverflowRejectsWholeFill(t *testing.T) {
	const M = math.MaxInt64
	p := int64(M/4 + 1) // 2^61
	q := int64(1 << 62) // 2^62 = 2p；4p = M+1
	e, ask := equityOverflowSellSetup(t)

	// 构造自检：成交价 q 不低于卖单限价 p；成交金额 q、成交后现金 p+q=3p、
	// 剩余持仓市值 p 各自合法，唯独现金与剩余市值之和 4p = M+1 越界。
	if q < p {
		t.Fatalf("成交价 q=%d 必须不低于限价 p=%d", q, p)
	}
	postCash := p + q // 3p
	if q > M || postCash > M || p > M {
		t.Fatalf("成交金额、成交后现金与剩余市值必须各自合法: q=%d postCash=%d p=%d", q, postCash, p)
	}
	if _, ok := addInt64(postCash, p); ok {
		t.Fatalf("构造必须使成交后现金 %d 加剩余市值 %d 超出 int64", postCash, p)
	}

	// 提交前快照：资金、持仓、可卖、订单、风险状态与既有全部记录。
	cash, reserved, available := e.Cash(), e.ReservedCash(), e.AvailableCash()
	posA, sellA := e.Position("A"), e.Sellable("A")
	riskBefore := e.RiskStatus()
	orderBefore, _ := e.Order(ask)
	recordsBefore := append([]Record(nil), e.Records()...)

	res, err := e.Fill(Trade{TradeID: 3, OrderID: ask, Symbol: "A", Side: Sell, Price: q, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("净值越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if res != (FillResult{}) {
		t.Fatalf("整笔拒绝必须返回零值成交结果，实际 %+v", res)
	}

	// 资金三要素维持提交前的值：卖出所得不得加入现金。
	if e.Cash() != cash || e.ReservedCash() != reserved || e.AvailableCash() != available {
		t.Fatalf("拒绝不得改变现金余额/占用/可用: cash=%d(before %d) reserved=%d(before %d) available=%d(before %d)",
			e.Cash(), cash, e.ReservedCash(), reserved, e.AvailableCash(), available)
	}
	// 持仓、卖单未成交部分占用的持仓与可卖数量维持提交前的值。
	if e.Position("A") != posA || e.Sellable("A") != sellA {
		t.Fatalf("拒绝不得改变持仓与可卖数量: pos=%d(before %d) sellable=%d(before %d)",
			e.Position("A"), posA, e.Sellable("A"), sellA)
	}

	// 卖单：累计成交量仍为 1，仍为部分成交、有效剩余 2，拒绝不能代替成交或撤单。
	o, _ := e.Order(ask)
	if o.Qty != 3 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 2 ||
		o.Limit != p || o.Reason != "" {
		t.Fatalf("拒绝不得改变卖单累计成交量、限价、剩余量与状态: %+v", o)
	}
	if o != orderBefore {
		t.Fatalf("拒绝前后卖单快照必须完全一致: before=%+v after=%+v", orderBefore, o)
	}

	// 交易日基准、亏损上限与已限制状态不变；提交前净值 3p 合法，不新增触线记录。
	riskAfter := e.RiskStatus()
	if riskAfter != riskBefore {
		t.Fatalf("拒绝不得改变风险状态: before=%+v after=%+v", riskBefore, riskAfter)
	}
	if countRiskRecords(e) != 1 {
		t.Fatalf("拒绝不得新增触线记录，实际 %d 条", countRiskRecords(e))
	}

	// 只新增一条拒绝记录，先前的接受、成交与开日触线记录保留原样。
	recs := e.Records()
	if len(recs) != len(recordsBefore)+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-len(recordsBefore))
	}
	if !reflect.DeepEqual(recordsBefore, recs[:len(recordsBefore)]) {
		t.Fatal("拒绝不得改写先前的接受、成交与触线记录")
	}
	newRecs := recs[len(recordsBefore):]
	for _, r := range newRecs {
		if r.Kind == RecordFilled || r.Kind == RecordCanceled || r.Kind == RecordRiskTriggered {
			t.Fatalf("拒绝不得新增成交、撤单或触线记录，实际 %s", r.Kind)
		}
	}
	rj := newRecs[0]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝，实际 %s", rj.Kind)
	}
	// 保存本次回报的成交编号、订单编号、合约、方向、价格和数量。
	if rj.OrderID != ask || rj.TradeID != 3 || rj.Symbol != "A" || rj.Side != Sell ||
		rj.TradePrice != q || rj.Qty != 1 {
		t.Fatalf("拒绝记录的回报要素错误: %+v", rj)
	}
	// 原因明确说明成交入账会使净值越界（不能写成低于限价或现金加总溢出）。
	if !strings.Contains(rj.Reason, "成交 3") || !strings.Contains(rj.Reason, "净值") ||
		!strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须说明成交 3 入账将使净值超出 int64 范围: %q", rj.Reason)
	}
	if strings.Contains(rj.Reason, "低于限价") || strings.Contains(rj.Reason, "加入现金") {
		t.Fatalf("本场景不得走低于限价或现金加总溢出的拒绝口径: %q", rj.Reason)
	}
	// 当时的报价快照：A 的最新已生效报价 seq1、时刻 1、价格 p（成交价 q 不得混入）。
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 1 || rj.QuotePrice != p {
		t.Fatalf("拒绝记录必须保存当时的 A 报价快照: %+v", rj)
	}
	if rj.QuotePrice == q {
		t.Fatalf("报价定位必须是估值价 p，不得混入成交价 q: %+v", rj)
	}
	// 当时的持仓限额快照。
	if !rj.MaxPositionValid || rj.MaxPosition != 100 {
		t.Fatalf("拒绝记录必须保存当时的持仓限额快照: %+v", rj)
	}
	// 日内风险快照：日号、基准 3p、提交前合法净值 3p 与零亏损、上限 0、已限制。
	if rj.RiskDay != 1 || rj.RiskBaseline != riskBefore.Baseline ||
		rj.RiskEquity != riskBefore.Equity || rj.RiskLoss != 0 ||
		rj.RiskLimit != 0 || !rj.RiskRestrict {
		t.Fatalf("拒绝记录必须保存提交时点的日内风险快照: %+v", rj)
	}
	// 账户总持仓金额上限未启用，拒绝记录不得携带金额上限快照。
	if rj.AmtEnabled || rj.AmtQuoteRefs != nil {
		t.Fatalf("金额上限未启用时拒绝记录不得携带金额快照: %+v", rj)
	}

	// 已有成交不能被撤回：成交 2 同内容重放仍返回原结果，不再次记账、不新增记录。
	replay, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: p, Qty: 1})
	if err != nil {
		t.Fatalf("已有成交 2 必须保留且可幂等重放: %v", err)
	}
	if replay != (FillResult{TradeID: 2, OrderID: ask, Price: p, Qty: 1,
		Filled: 1, Remaining: 2, Status: StatusPartial}) {
		t.Fatalf("成交 2 重放结果错误: %+v", replay)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("已有成交重放不得新增记录")
	}
	if e.Cash() != cash || e.Position("A") != posA || e.Sellable("A") != sellA {
		t.Fatalf("重放不得改变资金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
}

// TestRiskRestrictedSellFillEquityOverflowTradeIDReusableAtExactMax 覆盖上界边界与
// 编号复用：被净值越界拒绝的成交 3 不占用编号；同一编号把价格修正为 q-1（仍满足
// 限价 p）后，成交后净值恰好等于 MaxInt64，必须正常结算——现金按成交价增加、
// 持仓减少、只释放本次 1 股的卖单占用，未成交的 1 股继续有效；风险净值准确显示
// 为 M，浮盈亏损为零而买入限制保持，不新增触线记录。原拒绝记录保留。
func TestRiskRestrictedSellFillEquityOverflowTradeIDReusableAtExactMax(t *testing.T) {
	const M = math.MaxInt64
	p := int64(M/4 + 1)
	q := int64(1 << 62)
	e, ask := equityOverflowSellSetup(t)

	// 先以编号 3 报 1 股 @q：净值 M+1 被整笔拒绝（编号不被占用）。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: ask, Symbol: "A", Side: Sell, Price: q, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("前置越界成交必须被 ErrInt64Overflow 拒绝，实际 %v", err)
	}
	rejectIdx := len(e.Records()) - 1

	// 同一编号修正为 1 股 @q-1（仍不低于限价 p）：成交后现金 3p-1、剩余市值 p，
	// 净值 4p-1 = M 恰好达到上界，必须正常入账，不能报“编号已用于不同内容”。
	wantCash := 3*p - 1
	res, err := e.Fill(Trade{TradeID: 3, OrderID: ask, Symbol: "A", Side: Sell, Price: q - 1, Qty: 1})
	if err != nil {
		t.Fatalf("被拒编号修正回报后应按新内容重新校验并成功，实际: %v", err)
	}
	if res != (FillResult{TradeID: 3, OrderID: ask, Price: q - 1, Qty: 1,
		Filled: 2, Remaining: 1, Status: StatusPartial}) {
		t.Fatalf("边界成交结果应为累计 2、剩余 1、部分成交: %+v", res)
	}

	// 现金按实际成交价 q-1 增加（p + q-1 = 3p-1）；买单占用始终为 0。
	if e.Cash() != wantCash || e.ReservedCash() != 0 || e.AvailableCash() != wantCash {
		t.Fatalf("边界成交后资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓按本次成交数量减少到 1；只释放本次 1 股的卖单占用，剩余 1 股继续占用。
	if e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("边界成交后持仓与卖单占用错误: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(ask)
	if o.Qty != 3 || o.Filled != 2 || o.Status != StatusPartial || o.Remaining() != 1 ||
		o.Limit != p || o.Reason != "" {
		t.Fatalf("边界成交后卖单应保留 1 股有效剩余: %+v", o)
	}

	// 净值准确显示为 int64 最大值；估值口径是现金余额加剩余 1 股的最新报价 p 市值，
	// 成交价 q-1 只用于本笔现金结算；浮盈（净值高于基准 3p）对应亏损记零，
	// 买入限制仍保持。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 3*p || st.Equity != M ||
		st.Loss != 0 || st.LossLimit != 0 || !st.Restricted {
		t.Fatalf("边界成交后净值应恰为 MaxInt64、浮盈亏损为零且限制保持: %+v", st)
	}
	if st.Equity != e.Cash()+p*e.Position("A") {
		t.Fatalf("净值必须按现金加剩余持仓的最新报价市值计算: equity=%d cash=%d pos=%d",
			st.Equity, e.Cash(), e.Position("A"))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("合法卖出不得再次产生当日首次触线记录")
	}

	// 原拒绝记录保留；其后只新增一条成交记录，固化记账后净值 M、零亏损与已限制。
	recs := e.Records()
	if len(recs) != rejectIdx+2 {
		t.Fatalf("边界成交只能再新增一条记录，实际新增 %d 条", len(recs)-(rejectIdx+1))
	}
	rj := recs[rejectIdx]
	if rj.Kind != RecordRejected || rj.TradeID != 3 || rj.TradePrice != q {
		t.Fatalf("原拒绝记录必须保留: %+v", rj)
	}
	fr := recs[len(recs)-1]
	if fr.Kind != RecordFilled || fr.TradeID != 3 || fr.OrderID != ask || fr.Side != Sell ||
		fr.TradePrice != q-1 || fr.Qty != 1 || fr.Filled != 2 || fr.Remaining != 1 {
		t.Fatalf("成交记录要素错误: %+v", fr)
	}
	if !fr.QuoteValid || fr.QuoteSeq != 1 || fr.QuoteMoment != 1 || fr.QuotePrice != p {
		t.Fatalf("成交记录报价定位必须锚定估值价 p（seq1、时刻 1）: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 3*p || fr.RiskEquity != M ||
		fr.RiskLoss != 0 || fr.RiskLimit != 0 || !fr.RiskRestrict {
		t.Fatalf("成交记录风险快照应固化净值 MaxInt64、零亏损、已限制: %+v", fr)
	}

	// 同编号同内容重放：原样返回首次结果，不再次记账、不新增记录。
	replay, err := e.Fill(Trade{TradeID: 3, OrderID: ask, Symbol: "A", Side: Sell, Price: q - 1, Qty: 1})
	if err != nil {
		t.Fatalf("成功成交的同编号重放必须成功: %v", err)
	}
	if replay != res {
		t.Fatalf("重放必须原样返回边界成交结果: first=%+v replay=%+v", res, replay)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("重放不得新增记录")
	}
	if e.Cash() != wantCash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("重放不得改变资金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 剩余 1 股仍是有效剩余：按不低于限价 p 的价格成交，卖单全部成交，
	// 现金按成交价 p 增加后恰好达到 MaxInt64（现金恰好达到上界允许）。
	res2, err := e.Fill(Trade{TradeID: 4, OrderID: ask, Symbol: "A", Side: Sell, Price: p, Qty: 1})
	if err != nil {
		t.Fatalf("卖单保留的剩余 1 股必须仍可成交: %v", err)
	}
	if res2.Status != StatusFilled || res2.Filled != 3 || res2.Remaining != 0 {
		t.Fatalf("剩余 1 股成交后卖单应全部成交: %+v", res2)
	}
	if e.Cash() != M || e.Position("A") != 0 || e.Sellable("A") != 0 {
		t.Fatalf("剩余成交后现金应恰为 MaxInt64、持仓与占用归零: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if o2, _ := e.Order(ask); o2.Status != StatusFilled || o2.Filled != 3 || o2.Remaining() != 0 {
		t.Fatalf("卖单形态错误: %+v", o2)
	}
	// 剩余持仓为零后净值等于现金 M；限制不解除，也不重复触线。
	if st := e.RiskStatus(); !st.Restricted || st.Equity != M || st.Loss != 0 {
		t.Fatalf("全部卖出后净值应恰为 MaxInt64 且限制保持: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("已限制后卖单剩余成交不得重复产生触线记录")
	}

	// 买入限制仍保持：新买单继续被拒绝（卖单全程可成交已由成交 2/3/4 证明）。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("已限制状态下新买单必须拒绝")
	}
}

// TestRiskRestrictedSellFillEquityExactMaxWithoutRemainingPosition 是决定性对照：
// 账户只持有 1 股时，以与主场景完全相同的成交价 q 卖出，成交后无剩余持仓、
// 剩余市值为 0，净值就是成交后现金 q（< M），必须正常成交。这验证主场景中
// 决定越界的是现金之外那 1 股剩余持仓按报价 p 计入的市值，而成交金额与成交后
// 现金本身始终合法；成交价只用于本笔现金结算。
func TestRiskRestrictedSellFillEquityExactMaxWithoutRemainingPosition(t *testing.T) {
	const M = math.MaxInt64
	p := int64(M/4 + 1)
	q := int64(1 << 62)

	// 初始现金只需 2：买入 1 股 @2 并全部成交，现金 0、持仓 1。
	e, _ := NewEngine(2)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: p}, 1)
	bid := mustBuy(t, e, "A", 1, 2)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 2, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 零上限开日：基准 p，立即触线进入限制。
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); !st.Restricted || st.Baseline != p || st.Equity != p || st.Loss != 0 {
		t.Fatalf("前置风险状态错误: %+v", st)
	}

	// 卖单 1 股限价 p，以主场景相同的成交价 q（q=2p ≥ p）成交：
	// 成交金额 q、成交后现金 q 均合法；剩余持仓 0、剩余市值 0，净值 q < M。
	ask := mustSell(t, e, "A", 1, p)
	if q >= M {
		t.Fatalf("成交价 q=%d 本身必须合法且小于 MaxInt64", q)
	}
	res, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: q, Qty: 1})
	if err != nil {
		t.Fatalf("无剩余持仓时净值等于成交后现金 q，必须正常成交: %v", err)
	}
	if res != (FillResult{TradeID: 2, OrderID: ask, Price: q, Qty: 1,
		Filled: 1, Remaining: 0, Status: StatusFilled}) {
		t.Fatalf("对照成交结果错误: %+v", res)
	}
	if e.Cash() != q || e.Position("A") != 0 || e.Sellable("A") != 0 {
		t.Fatalf("对照成交后资金持仓错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	// 净值等于现金 q（成交价只用于现金结算）；相对基准 p 是浮盈，亏损记零，
	// 但零上限造成的买入限制不解除。
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != q || st.Loss != 0 || st.Baseline != p {
		t.Fatalf("对照成交后净值应为现金 q、浮盈亏损为零且限制保持: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("合法卖出不得再次产生触线记录")
	}

	// 同编号同内容重放原样返回，资金、持仓、订单与记录均不再变化。
	before := len(e.Records())
	replay, err := e.Fill(Trade{TradeID: 2, OrderID: ask, Symbol: "A", Side: Sell, Price: q, Qty: 1})
	if err != nil {
		t.Fatalf("重放必须成功: %v", err)
	}
	if replay != res || len(e.Records()) != before {
		t.Fatalf("重放必须原样返回且不新增记录: replay=%+v res=%+v records=%d->%d",
			replay, res, before, len(e.Records()))
	}
	if e.Cash() != q || e.Position("A") != 0 {
		t.Fatalf("重放不得改变状态: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
}
