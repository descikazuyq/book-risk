package book

import (
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方在报价缺口尚未补齐时首次开启交易日。
//
// 既有缺口链回归均是“先开日、后出现缺口”；本文件覆盖相反次序——账户已带有持仓、
// 部分成交买单与有效卖单，且一条意味着大幅亏损的报价正跨号等待时，调用方才首次
// 开日。必须保证：
//
//   - 开日基准与当前净值都以“现金余额 + 持仓按最新已生效报价的市值”确定；
//     未成交买单占用的现金仍是余额的一部分，不能重复扣除；等待补齐的报价既不
//     参与估值，也不能提前触发保护。
//   - 开日是纯状态开启：订单状态、累计成交量、现金与可卖数量两类占用、最新报价
//     与等待缺口全部原样保留，不产生触线或撤单记录。
//   - 随后补齐缺口时，缺失报价与原先等待的报价在同一次提交中依次生效；亏损恰好
//     达到上限的那一刻在刚开启的交易日触线，触线锚定真正致损的那条报价及其时刻，
//     只撤销买单剩余量并释放其现金占用；现金余额、已入账持仓与有效卖单不变，
//     历史成交记录的报价快照不被开日或补齐改写。
//   - 非正日号或负亏损上限的开日请求报错且不留痕迹：风险仍关闭，订单、两类占用
//     与等待报价保留；之后合法开日与补齐仍按上述行为处理。

// riskStartDayGapSetup 构造共用前置状态（全程尚未开日）：
// 初始现金 1000，A 合约持仓限额充足；seq1（时刻 10、价格 10）为最新已生效报价。
// 接受总量 15、限价 10 的买单 buyID，其中 10 份按价格 10 成交（成交编号 1），
// 再接受数量 3、限价 1 的卖单 sellID；随后 seq3（时刻 30、价格 1）因缺少 seq2
// 进入等待。
//
// 此时 cash=900、pos=10，买单剩余 5 份占用现金 50，卖单占用 3 份持仓，可卖 7。
func riskStartDayGapSetup(t *testing.T) (e *Engine, buyID, sellID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	buyID = mustBuy(t, e, "A", 15, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyID, Symbol: "A", Side: Buy, Price: 10, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	sellID = mustSell(t, e, "A", 3, 1)

	// 跨号报价只等待：seq3 缺 seq2，不生效、不参与估值。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 1}, 0)

	if e.Cash() != 900 || e.ReservedCash() != 50 || e.AvailableCash() != 850 {
		t.Fatalf("前置资金状态错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("前置持仓状态错误: pos=%d sellable=%d", e.Position("A"), e.Sellable("A"))
	}
	if o, _ := e.Order(buyID); o.Status != StatusPartial || o.Filled != 10 || o.Remaining() != 5 {
		t.Fatalf("买单应为部分成交、累计 10、剩余 5: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("卖单应保持待成交、剩余 3: %+v", o)
	}
	if !e.HasGap("A") {
		t.Fatal("seq3 跨号必须形成等待缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 1, Moment: 10, Price: 10}) {
		t.Fatalf("最新已生效报价必须停留在 seq1: %+v ok=%v", q, ok)
	}
	return e, buyID, sellID
}

// TestRiskStartDayWithPendingGapBasesOnAppliedQuoteThenFillTriggers 覆盖主场景：
// 缺口等待中首次开日，基准与净值均为 1000、亏损 0、不触线；补齐 seq2 后
// seq2 与原等待的 seq3 一次依次生效，seq3@1 使净值 910、亏损恰好 90 达限，
// 在刚开启的交易日触线并只撤销买单剩余 5 份。
func TestRiskStartDayWithPendingGapBasesOnAppliedQuoteThenFillTriggers(t *testing.T) {
	e, buyID, sellID := riskStartDayGapSetup(t)

	// 首次开日（日号 1，亏损上限 90）：等待中的 seq3@1 不能提前参与估值。
	recsBefore := len(e.Records())
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatalf("缺口等待中合法开日必须成功: %v", err)
	}

	// 基准 = 现金余额 900 + 持仓 10×已生效价 10 = 1000；占用的 50 现金属于余额，
	// 不重复扣除；当前净值同为 1000，亏损 0，保护尚未触发。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.LossLimit != 90 || st.Restricted {
		t.Fatalf("缺口等待中开日应按 seq1@10 定基准且不触线: %+v", st)
	}

	// 开日不产生任何触线或撤单记录。
	if len(e.Records()) != recsBefore {
		t.Fatalf("无触线开日不得新增记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	for _, r := range e.Records()[recsBefore:] {
		if r.Kind == RecordRiskTriggered || r.Kind == RecordCanceled {
			t.Fatalf("开日不得留下触线/撤单记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("开日时亏损为 0，不得存在触线记录")
	}

	// 开日保留订单状态、累计成交量与两类占用。
	if o, _ := e.Order(buyID); o.Status != StatusPartial || o.Qty != 15 || o.Limit != 10 ||
		o.Filled != 10 || o.Remaining() != 5 {
		t.Fatalf("开日后买单必须原样保留: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("开日后卖单必须仍有效: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 50 || e.AvailableCash() != 850 {
		t.Fatalf("开日后现金与买单占用必须保留: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("开日后持仓与卖单占用必须保留: pos=%d sellable=%d",
			e.Position("A"), e.Sellable("A"))
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Price != 10 {
		t.Fatalf("开日后最新报价仍应停在 seq1: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("开日不得补齐或清除等待缺口")
	}

	// 提交缺失的 seq2（时刻 20、价格 10）：seq2 与原等待的 seq3 一次依次生效。
	recsBefore = len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil {
		t.Fatalf("补齐缺口必须成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("本次应使 seq2 与等待中的 seq3 共 2 条生效，实际 %d", n)
	}

	// seq2@10 下净值仍为 1000；seq3@1 下净值 900+10×1=910、亏损恰好 90 达限，
	// 在刚开启的交易日触发保护。
	st = e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1000 || st.LossLimit != 90 {
		t.Fatalf("开日基准与上限应保持: %+v", st)
	}
	if !st.Restricted || st.Equity != 910 || st.Loss != 90 {
		t.Fatalf("seq3@1 生效后应按亏损恰好 90 触线: %+v", st)
	}

	// 只撤销买单剩余 5 份并释放 50 现金占用；现金余额与已入账的 10 份持仓保留。
	o, _ := e.Order(buyID)
	if o.Status != StatusCanceled || o.Qty != 15 || o.Filled != 10 || o.Remaining() != 0 {
		t.Fatalf("买单应保留总量 15、已成交 10，仅撤销剩余 5: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 90") || !strings.Contains(o.Reason, "上限 90") {
		t.Fatalf("撤单原因必须固化触线时亏损 90 与上限 90: %q", o.Reason)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.AvailableCash() != 900 {
		t.Fatalf("撤单只释放占用、不得扣减余额: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 10 {
		t.Fatalf("已入账持仓必须保留: pos=%d", e.Position("A"))
	}

	// 卖单仍有效，卖单占用保留，可卖数量仍为 7。
	if oSell, _ := e.Order(sellID); oSell.Status != StatusPending ||
		oSell.Qty != 3 || oSell.Filled != 0 || oSell.Remaining() != 3 {
		t.Fatalf("卖单必须保持有效且未被风险撤单波及: %+v", oSell)
	}
	if e.Sellable("A") != 7 {
		t.Fatalf("卖单占用的 3 份持仓必须保留，可卖仍为 7: %d", e.Sellable("A"))
	}

	// 最新报价推进到 seq3，缺口消失。
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 3, Moment: 30, Price: 1}) {
		t.Fatalf("最新报价应推进到 seq3@时刻30 价格1: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("链全部生效后不得再报告缺口")
	}

	// 本批只新增“当日首次触线 + 买单撤销”共 2 条记录。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 2 {
		t.Fatalf("应只新增 2 条记录（1 触线 + 1 撤单），实际 %d 条", len(newRecs))
	}
	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerQuote || trig.RiskRefSeq != 3 || trig.RiskRefMoment != 30 {
		t.Fatalf("触线必须关联真正致损的 seq3 及其时刻 30: trigger=%s ref=%d moment=%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1000 || trig.RiskEquity != 910 ||
		trig.RiskLoss != 90 || trig.RiskLimit != 90 || !trig.RiskRestrict {
		t.Fatalf("触线快照必须固化净值 910、亏损恰好 90: %+v", trig)
	}
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 30, Price: 1}) {
		t.Fatalf("估值报价定位必须锚定 seq3@时刻30 价格1: %+v", trig.RiskQuoteRefs)
	}

	cr := newRecs[1]
	if cr.Kind != RecordCanceled || cr.OrderID != buyID || cr.Side != Buy ||
		cr.Qty != 15 || cr.Filled != 10 || cr.Remaining != 5 {
		t.Fatalf("第二条记录必须是买单剩余 5 份的撤销: %+v", cr)
	}
	if !cr.QuoteValid || cr.QuoteSeq != 3 || cr.QuoteMoment != 30 || cr.QuotePrice != 1 {
		t.Fatalf("撤单报价快照必须锚定触线时的 seq3@时刻30 价格1: %+v", cr)
	}
	if cr.RiskDay != 1 || cr.RiskBaseline != 1000 || cr.RiskEquity != 910 ||
		cr.RiskLoss != 90 || cr.RiskLimit != 90 || !cr.RiskRestrict {
		t.Fatalf("撤单记录必须携带触线时风险快照: %+v", cr)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 限制持续：新买单被拒，并追加一条拒绝记录。
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("触线限制持续期间新买单必须拒绝")
	}
	if rj := e.Records()[len(e.Records())-1]; rj.Kind != RecordRejected ||
		rj.RiskDay != 1 || rj.RiskLoss != 90 || !rj.RiskRestrict {
		t.Fatalf("限制期新买单应追加携带当日风险快照的拒绝记录: %+v", rj)
	}

	// 原成交记录保留成交时的报价 seq1（时刻 10、价格 10），不随开日或补齐改写。
	var fills []Record
	for _, r := range e.Records() {
		if r.Kind == RecordFilled && r.TradeID == 1 {
			fills = append(fills, r)
		}
	}
	if len(fills) != 1 {
		t.Fatalf("成交编号 1 应恰有一条成交记录，实际 %d", len(fills))
	}
	fr := fills[0]
	if !fr.QuoteValid || fr.QuoteSeq != 1 || fr.QuoteMoment != 10 || fr.QuotePrice != 10 {
		t.Fatalf("历史成交记录的报价快照必须停留在成交时的 seq1@时刻10 价格10: %+v", fr)
	}
	if fr.TradePrice != 10 || fr.Qty != 10 || fr.Filled != 10 || fr.RiskDay != 0 {
		t.Fatalf("历史成交记录不应被开日或补齐改写: %+v", fr)
	}
}

// TestRiskStartDayInvalidParamsWithPendingGapPreserveEverythingAndRetry 覆盖开日
// 参数失败边界：非正日号或负亏损上限一律报错，风险保持关闭，订单、两类占用与
// 等待报价全部保留、不留记录；随后合法开日与补齐仍按主场景行为处理。
func TestRiskStartDayInvalidParamsWithPendingGapPreserveEverythingAndRetry(t *testing.T) {
	e, buyID, sellID := riskStartDayGapSetup(t)

	recsBefore := len(e.Records())
	if err := e.StartTradingDay(0, 90); err == nil {
		t.Fatal("非正日号 0 必须报错")
	}
	if err := e.StartTradingDay(-1, 90); err == nil {
		t.Fatal("负日号必须报错")
	}
	if err := e.StartTradingDay(1, -1); err == nil {
		t.Fatal("负亏损上限必须报错")
	}

	// 风险状态保持“未开日”，失败调用不追加任何记录。
	if st := e.RiskStatus(); st.Open {
		t.Fatalf("非法开日不得开启风险状态: %+v", st)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("非法开日不得新增记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("非法开日不得产生触线记录")
	}

	// 订单、累计成交量、两类占用、最新报价与等待缺口全部保留。
	if o, _ := e.Order(buyID); o.Status != StatusPartial || o.Filled != 10 || o.Remaining() != 5 {
		t.Fatalf("非法开日后买单必须原样保留: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("非法开日后卖单必须仍有效: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 50 || e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("非法开日后资金与占用必须保留: cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Price != 10 {
		t.Fatalf("非法开日后最新报价仍应停在 seq1: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("非法开日不得清除等待缺口，seq3 必须继续等待")
	}

	// 后续合法开日照常按已生效报价定基准。
	if err := e.StartTradingDay(1, 90); err != nil {
		t.Fatalf("失败调用后合法开日必须成功: %v", err)
	}
	if st := e.RiskStatus(); !st.Open || st.Baseline != 1000 || st.Equity != 1000 ||
		st.Loss != 0 || st.Restricted {
		t.Fatalf("合法开日应按 seq1@10 定基准 1000 且不触线: %+v", st)
	}

	// 补齐后仍是 seq3 致损：2 条依次生效、亏损恰好 90 触线、撤销买单剩余 5 份。
	recsBefore = len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil || n != 2 {
		t.Fatalf("补齐应使 2 条报价依次生效: n=%d err=%v", n, err)
	}
	st := e.RiskStatus()
	if !st.Restricted || st.Equity != 910 || st.Loss != 90 {
		t.Fatalf("补齐后应按 seq3@1 估值 910、亏损 90 触线: %+v", st)
	}
	if o, _ := e.Order(buyID); o.Status != StatusCanceled || o.Filled != 10 || o.Remaining() != 0 {
		t.Fatalf("买单剩余 5 份必须被撤销、已成交 10 保留: %+v", o)
	}
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("卖单必须仍有效: %+v", o)
	}
	if e.Cash() != 900 || e.ReservedCash() != 0 || e.Position("A") != 10 || e.Sellable("A") != 7 {
		t.Fatalf("触线后资金持仓应为 cash=900 reserved=0 pos=10 sellable=7: "+
			"cash=%d reserved=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 2 || newRecs[0].Kind != RecordRiskTriggered ||
		newRecs[0].RiskTrigger != RiskTriggerQuote ||
		newRecs[0].RiskRefSeq != 3 || newRecs[0].RiskRefMoment != 30 ||
		newRecs[1].Kind != RecordCanceled || newRecs[1].OrderID != buyID ||
		newRecs[1].Remaining != 5 {
		t.Fatalf("补齐后新增记录必须是锚定 seq3@时刻30 的触线加买单剩余 5 份撤销: %+v", newRecs)
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("合法开日后的交易日只能有一条触线记录")
	}
}
