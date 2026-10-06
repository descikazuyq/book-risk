package book

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// 本文件为“日内亏损保护已经触线（账户处于限制状态）后，一笔卖出成交的所得能够
// 合法加入现金，但成交后的账户净值（现金 + 剩余持仓按最新已生效报价计算的市值）
// 超出 int64 范围”补充回归保障。限制买入以后卖单仍可接受、仍可成交，净值的
// int64 可表示性检查也继续生效：该笔成交必须整笔拒绝（零值 FillResult 与可
// errors.Is 识别的 ErrInt64Overflow），只拒绝这一笔；随后同一成交编号把价格
// 修正为仍满足限价、使成交后净值恰好等于 int64 最大值时，必须正常结算。
//
// 数值构造（单合约 A；各分量各自合法，唯独现金加剩余市值越界）：
//
//	M = MaxInt64 = 2^63-1；half = M/2 = 2^63 的整数半值；
//	P = half-1，故 2P = M-3；
//	被拒价 p1 = half+2（远高于卖单限价 1，合法）；
//	修正价 p2 = half+1（同样不低于限价 1）。
//
// 前置全部由引擎公开操作形成真实账实：初始现金 3，报价 seq1@1，买入 3 股 @1
// 并整笔成交后现金 0、持仓 3；以零亏损上限开启交易日 1，基准净值 3 且立即触线
// （买单已全部成交，无单可撤，账户进入限制状态）。限制状态下接受 3 股限价 1 的
// 真实卖单并先成交 1 股 @1：现金 1、持仓 2、卖单累计成交 1、剩余 2 继续占用
// 2 股可卖数量。随后 A 报价 seq2 升到 P：提交前净值 1+2P = M-2 合法。
//
// 再提交 1 股 @p1 的卖出成交：
//   - 成交价 p1 ≥ 限价 1，成交金额 p1 本身在 int64 范围内；
//   - 成交后现金 1+p1 = half+3 仍在 int64 范围内（不是现金余额本身溢出）；
//   - 剩余持仓 1 股按最新已生效报价 P 计市值 P，也在 int64 范围内；
//   - 净值 = half+3 + P = M+1，唯一越界发生在“现金 + 剩余市值”相加。
//
// 同一成交编号改报 1 股 @p2：成交后现金 half+2、剩余市值 P，相加
// half+2+P = M 恰好达到上界，必须正常入账：现金按 p2 增加、持仓减 1，只释放
// 本次这 1 股的卖单占用（剩余 1 股继续有效）；风险净值准确显示为 M，浮盈亏损
// 记零，但限制状态不解除，也不产生新的当日首次触线记录。成交价只用于本笔现金
// 结算，剩余持仓始终按报价 P 估值，两个价格不能混用。

// riskSellEquityOverflowSetup 用公开操作构造共用前置状态：
// 现金 1、A 持仓 2（最新报价 seq2@时刻2 价格 P）；A 的卖单总量 3、限价 1，
// 已成交 1 股 @1，累计成交 1、剩余 2 继续占用 2 股可卖数量（可卖为 0）；
// 交易日 1 已开启（基准净值 3、亏损上限 0），账户已处于限制状态，当日首条
// 触线记录已经存在，账户总持仓金额上限未启用。
func riskSellEquityOverflowSetup(t *testing.T) (e *Engine, sid int64) {
	t.Helper()
	const M = math.MaxInt64
	P := int64(M/2 - 1) // 2P = M-3

	e, _ = NewEngine(3)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	// 真实买入并整笔成交 3 股 @1：现金 0、持仓 3，买单无剩余量。
	bid := mustBuy(t, e, "A", 3, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 0 || e.ReservedCash() != 0 || e.AvailableCash() != 0 || e.Position("A") != 3 {
		t.Fatalf("前置买入成交后状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 零亏损上限开日立即触线：基准净值 3；买单已全部成交，触线无单可撤，
	// 账户进入持续到下一交易日的限制状态。
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatal(err)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 3 || st.Equity != 3 ||
		st.Loss != 0 || st.LossLimit != 0 || !st.Restricted {
		t.Fatalf("零上限开日后的前置风险状态错误: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatalf("开日应产生当日唯一一条触线记录，实际 %d 条", countRiskRecords(e))
	}

	// 限制买入以后，卖单仍可接受：3 股限价 1 的卖单占用全部 3 股可卖数量，
	// 不预先增加现金、不扣减持仓。
	sid = mustSell(t, e, "A", 3, 1)
	if e.Position("A") != 3 || e.Sellable("A") != 0 {
		t.Fatalf("限制后接受的卖单应占用 3 股可卖数量: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if e.Cash() != 0 || e.ReservedCash() != 0 {
		t.Fatalf("接受卖单不得预先改变现金: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}

	// 卖单先真实成交 1 股 @1：现金 1、持仓 2，卖单累计成交 1、剩余 2 继续占用。
	res, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res != (FillResult{TradeID: 2, OrderID: sid, Price: 1, Qty: 1,
		Filled: 1, Remaining: 2, Status: StatusPartial}) {
		t.Fatalf("前置卖出成交结果错误: %+v", res)
	}
	if e.Cash() != 1 || e.ReservedCash() != 0 || e.AvailableCash() != 1 ||
		e.Position("A") != 2 || e.Sellable("A") != 0 {
		t.Fatalf("前置卖出成交后账实错误: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}

	// 报价升到 P：提交前净值 1+2P = M-2，合法；已限制状态不因报价重复触线。
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 2, Price: P}, 1)
	st = e.RiskStatus()
	if !st.Restricted || st.Equity != M-2 || st.Loss != 0 {
		t.Fatalf("升价后提交前风险状态错误（净值应为合法的 M-2）: %+v", st)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("已限制状态下报价生效不得新增触线记录")
	}
	if e.PositionAmountStatus().Enabled {
		t.Fatal("本场景要求账户总持仓金额上限未启用")
	}
	return e, sid
}

// TestRiskRestrictedSellFillEquityOverflowRejectsWholeFill 覆盖主场景：限制状态
// 下 1 股 @p1 的卖出成交，金额、成交后现金、剩余持仓市值各自合法，唯独现金加
// 剩余市值达到 M+1。必须整笔拒绝并返回 ErrInt64Overflow；现金、持仓、卖单
// 累计成交量/状态、未成交部分占用与可卖数量、交易日基准/亏损上限/已限制状态
// 全部维持提交前的值，已有成交不被撤回；只追加一条要素与快照完整、原因明确
// 指向净值越界的拒绝记录，不得新增成交、触线或撤单记录。
func TestRiskRestrictedSellFillEquityOverflowRejectsWholeFill(t *testing.T) {
	const M = math.MaxInt64
	const half = M / 2
	P := int64(half - 1)  // 剩余持仓估值价；2P = M-3
	p1 := int64(half + 2) // 被拒成交价：≥ 卖单限价 1
	e, sid := riskSellEquityOverflowSetup(t)

	// 先证明这是“相加才越界”的场景，而不是价格非法、单笔金额或现金余额溢出：
	// 成交价符合卖单限价，成交金额、成交后现金余额、剩余持仓市值分别都在界内。
	postCash := int64(1 + p1) // half+3
	if p1 < 1 {
		t.Fatalf("成交价必须不低于卖单限价 1，实际 %d", p1)
	}
	if p1 <= 0 || p1 >= M || postCash <= 0 || postCash >= M || P <= 0 || P >= M {
		t.Fatalf("成交金额、成交后现金与剩余市值必须各自合法: amount=%d postCash=%d market=%d M=%d",
			p1, postCash, P, M)
	}
	if uint64(postCash)+uint64(P) != uint64(M)+1 {
		t.Fatalf("构造错误：现金 %d 加剩余市值 %d 应为 M+1，实际 uint64 和 %d",
			postCash, P, uint64(postCash)+uint64(P))
	}

	// 提交前快照：资金、持仓、订单、风险状态与既有全部记录。
	cash, reserved, available := e.Cash(), e.ReservedCash(), e.AvailableCash()
	pos, sellable := e.Position("A"), e.Sellable("A")
	riskBefore := e.RiskStatus()
	orderBefore, _ := e.Order(sid)
	recordsBefore := append([]Record(nil), e.Records()...)
	// 买单接受/成交、开日触线、卖单接受、前置卖出成交：共 5 条。
	if len(recordsBefore) != 5 {
		t.Fatalf("前置记录数应为 5，实际 %d", len(recordsBefore))
	}

	res, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: p1, Qty: 1})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("成交后净值越界必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if res != (FillResult{}) {
		t.Fatalf("整笔拒绝必须返回零值成交结果，实际 %+v", res)
	}

	// 资金三要素与持仓、可卖数量全部维持提交前的值；已有成交不被撤回。
	if e.Cash() != cash || e.ReservedCash() != reserved || e.AvailableCash() != available {
		t.Fatalf("拒绝不得改变现金余额/占用/可用: cash=%d(before %d) reserved=%d(before %d) available=%d(before %d)",
			e.Cash(), cash, e.ReservedCash(), reserved, e.AvailableCash(), available)
	}
	if e.Position("A") != pos || e.Sellable("A") != sellable {
		t.Fatalf("拒绝不得改变持仓与可卖数量: pos=%d(before %d) sellable=%d(before %d)",
			e.Position("A"), pos, e.Sellable("A"), sellable)
	}

	// 卖单：累计成交仍为 1，仍为部分成交、有效剩余 2 并占用 2 股，拒绝不能代替
	// 成交，也不能代替撤单。
	o, _ := e.Order(sid)
	if o.Qty != 3 || o.Limit != 1 || o.Filled != 1 ||
		o.Status != StatusPartial || o.Remaining() != 2 || o.Reason != "" {
		t.Fatalf("拒绝不得改变卖单累计成交量、剩余量与状态: %+v", o)
	}
	if o != orderBefore {
		t.Fatalf("拒绝前后卖单快照必须完全一致: before=%+v after=%+v", orderBefore, o)
	}

	// 交易日基准、亏损上限与已限制状态不变；净值仍是提交前合法的 M-2，不新增触线。
	riskAfter := e.RiskStatus()
	if riskAfter != riskBefore {
		t.Fatalf("拒绝不得改变风险状态: before=%+v after=%+v", riskBefore, riskAfter)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("净值越界拒绝不得新增风险触线记录")
	}

	// 只新增一条拒绝记录；先前的接受、成交与触线记录原样保留。
	recs := e.Records()
	if len(recs) != len(recordsBefore)+1 {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-len(recordsBefore))
	}
	if !reflect.DeepEqual(recordsBefore, recs[:len(recordsBefore)]) {
		t.Fatal("拒绝不得改写先前的接受、成交与触线记录")
	}
	rj := recs[len(recordsBefore)]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝，实际 %s", rj.Kind)
	}
	// 保存本次成交编号、订单编号、方向、价格与数量。
	if rj.OrderID != sid || rj.TradeID != 3 || rj.Symbol != "A" || rj.Side != Sell ||
		rj.TradePrice != p1 || rj.Qty != 1 {
		t.Fatalf("拒绝记录的回报要素错误: %+v", rj)
	}
	// 原因明确说明成交入账会使净值越界；不能写成价格不合法，也不能写成现金余额
	// 本身溢出（那是另一条始终生效的现金结算拒绝路径）。
	if !strings.Contains(rj.Reason, "成交 3") || !strings.Contains(rj.Reason, "净值") ||
		!strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须说明成交 3 入账将使净值超出 int64 范围: %q", rj.Reason)
	}
	if strings.Contains(rj.Reason, "加入现金") {
		t.Fatalf("本场景现金余额本身不溢出，不得走现金加回拒绝口径: %q", rj.Reason)
	}
	if strings.Contains(rj.Reason, "低于限价") {
		t.Fatalf("成交价 p1 不低于限价，拒绝原因不得指向价格非法: %q", rj.Reason)
	}
	// 当时报价定位：A 最新已生效报价 seq2、时刻 2、价格 P（成交价 p1 不得混入）。
	if !rj.QuoteValid || rj.QuoteSeq != 2 || rj.QuoteMoment != 2 || rj.QuotePrice != P {
		t.Fatalf("拒绝记录必须保存当时的 A 报价定位（seq2/P），不得混入成交价: %+v", rj)
	}
	if !rj.MaxPositionValid || rj.MaxPosition != 100 {
		t.Fatalf("拒绝记录必须保存当时的持仓限额快照: %+v", rj)
	}
	// 风险快照反映提交前的合法状态：日号 1、基准 3、提交前净值 M-2、浮盈亏损
	// 记零、上限 0、已限制。
	if rj.RiskDay != 1 || rj.RiskBaseline != 3 || rj.RiskEquity != M-2 ||
		rj.RiskLoss != 0 || rj.RiskLimit != 0 || !rj.RiskRestrict {
		t.Fatalf("拒绝记录必须保存提交前的合法风险快照: %+v", rj)
	}
	// 账户总持仓金额上限未启用，拒绝记录不得携带金额上限快照。
	if rj.AmtEnabled || rj.AmtQuoteRefs != nil {
		t.Fatalf("金额上限未启用时拒绝记录不得携带金额快照: %+v", rj)
	}

	// 被拒成交不占用编号：剩余 2 股仍可继续提交合法成交（下方边界测试复用编号 3）。
	if o2, _ := e.Order(sid); o2.Status != StatusPartial || o2.Remaining() != 2 {
		t.Fatalf("被拒后卖单必须仍有 2 股有效剩余: %+v", o2)
	}
}

// TestRiskRestrictedSellFillEquityOverflowTradeIDReusableAtExactMax 覆盖边界与
// 编号复用：被净值越界拒绝的成交 3 不占用编号；同一编号把价格修正为仍满足限价
// 的 p2，使成交后净值恰好等于 MaxInt64 时必须成功结算。现金按实际成交价 p2
// 增加、持仓按本次 1 股减少，只释放本次这 1 股的卖单占用（剩余 1 股继续有效）；
// 风险查询准确显示最大净值、浮盈亏损为零，买入限制仍保持，且这笔合法卖出不
// 产生新的当日首次触线记录；原拒绝记录保留。
func TestRiskRestrictedSellFillEquityOverflowTradeIDReusableAtExactMax(t *testing.T) {
	const M = math.MaxInt64
	const half = M / 2
	P := int64(half - 1)  // 估值价
	p1 := int64(half + 2) // 先被拒的价格
	p2 := int64(half + 1) // 修正价：成交后净值恰为 M
	e, sid := riskSellEquityOverflowSetup(t)

	// 先以编号 3 报 1 股 @p1：净值 M+1 被整笔拒绝（编号不被占用）。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: p1, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("前置越界成交必须被 ErrInt64Overflow 拒绝，实际 %v", err)
	}
	recordsAfterReject := len(e.Records())
	rejectRec := e.Records()[recordsAfterReject-1]
	if rejectRec.Kind != RecordRejected || rejectRec.TradeID != 3 || rejectRec.TradePrice != p1 {
		t.Fatalf("前置拒绝记录定位错误: %+v", rejectRec)
	}

	// 同一编号修正为 1 股 @p2（仍不低于限价 1）：成交后现金 half+2、剩余市值 P，
	// 相加恰为 M，必须正常入账，不能报“编号已用于不同内容”。
	res, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: p2, Qty: 1})
	if err != nil {
		t.Fatalf("被拒编号修正回报后应按新内容重新校验并成功，实际: %v", err)
	}
	if res != (FillResult{TradeID: 3, OrderID: sid, Price: p2, Qty: 1,
		Filled: 2, Remaining: 1, Status: StatusPartial}) {
		t.Fatalf("边界成交结果应为累计 2、剩余 1、部分成交: %+v", res)
	}

	// 现金按实际成交价 p2 增加 1+p2 = half+2；卖单不涉及买单现金占用。
	wantCash := int64(1 + p2)
	if e.Cash() != wantCash || e.ReservedCash() != 0 || e.AvailableCash() != wantCash {
		t.Fatalf("边界成交后资金错误: cash=%d want=%d reserved=%d available=%d",
			e.Cash(), wantCash, e.ReservedCash(), e.AvailableCash())
	}
	// 持仓按本次成交数量减少到 1；只释放本次 1 股的卖单占用，剩余 1 股继续占用，
	// 故可卖数量仍为 0（持仓 1 − 剩余卖单占用 1）。
	if e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("边界成交后只应释放本次 1 股占用: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	o, _ := e.Order(sid)
	if o.Qty != 3 || o.Limit != 1 || o.Filled != 2 ||
		o.Status != StatusPartial || o.Remaining() != 1 || o.Reason != "" {
		t.Fatalf("边界成交后卖单应保留 1 股有效剩余: %+v", o)
	}

	// 净值准确显示为 int64 最大值（现金 half+2 加剩余 1 股按报价 P 的市值）；
	// 成交价 p2 只用于本笔现金结算，不参与剩余持仓估值。浮盈（净值远高于基准 3）
	// 对应亏损记零；限制状态不解除。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 3 || st.LossLimit != 0 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if st.Equity != M || st.Loss != 0 || !st.Restricted {
		t.Fatalf("边界成交后净值应恰为 MaxInt64、浮盈亏损为零且仍处限制: %+v", st)
	}
	if uint64(e.Cash())+uint64(P) != uint64(M) {
		t.Fatalf("构造错误：现金 %d 加剩余市值 %d 应为 M", e.Cash(), P)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("合法卖出使净值恰好达到上界不得再次产生当日首次触线记录")
	}

	// 记录：原拒绝记录保留不改写，只在其后追加一条成交记录。
	recs := e.Records()
	if len(recs) != recordsAfterReject+1 {
		t.Fatalf("边界成交只能再新增一条记录，实际 %d 条", len(recs)-recordsAfterReject)
	}
	if !reflect.DeepEqual(rejectRec, recs[recordsAfterReject-1]) {
		t.Fatal("原拒绝记录必须保留且不得改写")
	}
	fr := recs[len(recs)-1]
	if fr.Kind != RecordFilled || fr.TradeID != 3 || fr.OrderID != sid || fr.Side != Sell ||
		fr.TradePrice != p2 || fr.Qty != 1 || fr.Filled != 2 || fr.Remaining != 1 {
		t.Fatalf("成交记录要素错误: %+v", fr)
	}
	// 成交记录的报价定位锚定估值价 P（seq2、时刻 2），风险快照固化净值 M、零亏损、
	// 已限制。
	if !fr.QuoteValid || fr.QuoteSeq != 2 || fr.QuoteMoment != 2 || fr.QuotePrice != P {
		t.Fatalf("成交记录报价定位必须为估值价 P（seq2、时刻 2）: %+v", fr)
	}
	if fr.RiskDay != 1 || fr.RiskBaseline != 3 || fr.RiskEquity != M ||
		fr.RiskLoss != 0 || fr.RiskLimit != 0 || !fr.RiskRestrict {
		t.Fatalf("成交记录风险快照应固化净值 MaxInt64、零亏损、已限制: %+v", fr)
	}

	// 买入限制仍保持：新买单继续被拒绝（卖单合法成交不能解除限制）。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("限制状态下新买单必须继续拒绝")
	} else if !strings.Contains(err.Error(), "日内亏损保护") {
		t.Fatalf("新买单必须因日内亏损保护被拒，实际: %v", err)
	}

	// 同编号同内容重放：原样返回首次结果，不再次记账、不新增记录。
	replay, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: p2, Qty: 1})
	if err != nil {
		t.Fatalf("成功成交的同编号重放必须成功: %v", err)
	}
	if replay != res {
		t.Fatalf("重放必须原样返回边界成交结果: first=%+v replay=%+v", res, replay)
	}
	if len(e.Records()) != recordsAfterReject+1+1 { // 含上面新买单的一条拒绝
		t.Fatal("重放不得新增记录")
	}
	if e.Cash() != wantCash || e.Position("A") != 1 || e.Sellable("A") != 0 {
		t.Fatalf("重放不得改变资金、持仓与占用: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 剩余 1 股仍是有效卖单剩余：撤销只释放这 1 股的占用（可卖 0→1），
	// 已入账持仓 1 股与现金保留。
	if err := e.Cancel(sid); err != nil {
		t.Fatalf("剩余 1 股必须仍可撤销（证明卖单剩余部分仍有效）: %v", err)
	}
	if e.Sellable("A") != 1 || e.Position("A") != 1 || e.Cash() != wantCash {
		t.Fatalf("撤销剩余卖单应只释放 1 股可卖占用: sellable=%d pos=%d cash=%d",
			e.Sellable("A"), e.Position("A"), e.Cash())
	}
	if o2, _ := e.Order(sid); o2.Status != StatusCanceled || o2.Filled != 2 || o2.Remaining() != 0 {
		t.Fatalf("撤销后卖单形态错误: %+v", o2)
	}
}

// TestRiskRestrictedSellFillInvalidPriceStillOrdinaryRejected 补充口径边界：
// 限制状态下卖单成交的常规合法性校验在净值溢出检查之前仍然有效——非法价格
// （此处限价为 1，不存在低于 1 的正数，故用价格 0）按普通规则拒绝，不包装
// ErrInt64Overflow，与本文件主场景的净值越界拒绝明确区分，且不占用成交编号。
func TestRiskRestrictedSellFillInvalidPriceStillOrdinaryRejected(t *testing.T) {
	e, sid := riskSellEquityOverflowSetup(t)
	before := len(e.Records())

	res, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: 0, Qty: 1})
	if err == nil {
		t.Fatal("价格非法的成交必须拒绝")
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("价格非法必须走普通拒绝，不得包装 ErrInt64Overflow: %v", err)
	}
	expectErr(t, err, "成交价格必须为正")
	if res != (FillResult{}) {
		t.Fatalf("普通拒绝必须返回零值成交结果，实际 %+v", res)
	}
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("价格非法只能追加一条普通拒绝记录")
	}

	// 编号未被占用：同编号随后可提交主场景的越界回报。
	const M = math.MaxInt64
	p1 := int64(M/2 + 2)
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sid, Symbol: "A", Side: Sell, Price: p1, Qty: 1}); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("被普通拒绝的编号应可复用，且越界回报仍须被 ErrInt64Overflow 拒绝: %v", err)
	}
}
