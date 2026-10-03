package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方补齐行情缺口、一次使多条报价连续生效时，
// 链中报价必须逐条生效，后一条的判断承接前一条实际产生的净值、限制与订单状态；
// 等待中的报价在补齐前不得提前改变任何业务状态。
//
// 覆盖两类结局：
//   - 成功补齐：同批先出现触线价格、批末价格恢复。即使批末当前亏损已回到上限以下，
//     中间报价一旦触线，限制整日保留；所有仍有效买单按编号从大到小撤销（部分成交
//     买单只释放未成交部分），卖单及其可卖数量占用不受影响；触线记录（来源、报价
//     序号、时刻、亏损与参与估值的报价定位）固化在真正触线的时点，查询净值使用批末
//     报价而不停在触线估值。
//   - 补齐失败：同批后续报价使持仓市值（进而现金加市值的净值）超出 int64 范围。
//     整批提交必须返回包装 book.ErrInt64Overflow 的错误、零条生效；此前最新报价、
//     当日基准、亏损上限与限制状态全部保留，前一条报价“拟触发”的撤单不得改变任何
//     订单、现金占用、持仓或卖单占用，原先等待的报价继续等待；只新增一条说明估值
//     溢出的拒绝记录，不得留下触线或撤单记录。

// TestRiskGapFillMidTriggerSticksThoughBatchEndRecovers 复现核心情形：
// seq3@51、seq4@100 先到并等待（seq4 已使当前亏损回到零），再补齐 seq2@50，
// 同一批中 seq2 使亏损 660 达到上限 500 触线。补齐必须成功（3 条生效），
// 限制不因批末恢复而解除；触线记录锚定 seq2，撤单按编号从大到小（含跨合约买单
// 与部分成交买单），卖单与已入账成交保持不变。
func TestRiskGapFillMidTriggerSticksThoughBatchEndRecovers(t *testing.T) {
	e, _ := NewEngine(10_000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 100}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 20}, 1)

	aFilled := mustBuy(t, e, "A", 10, 100) // id1，随后全部成交
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aFilled, Symbol: "A", Side: Buy, Price: 100, Qty: 10}); err != nil {
		t.Fatal(err)
	}
	// 开日：现金 9000 + 持仓 10×100 = 10000 作为当日基准。
	if err := e.StartTradingDay(1, 500); err != nil {
		t.Fatal(err)
	}
	if st := e.RiskStatus(); !st.Open || st.Day != 1 || st.Baseline != 10000 ||
		st.Equity != 10000 || st.Loss != 0 || st.LossLimit != 500 || st.Restricted {
		t.Fatalf("开日状态错误: %+v", st)
	}

	bPending := mustBuy(t, e, "A", 4, 90) // id2：待成交，占用 4×90=360
	bPartial := mustBuy(t, e, "A", 8, 90) // id3：随后部分成交 4 股
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bPartial, Symbol: "A", Side: Buy, Price: 90, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	// 此时现金 8640、A 持仓 14；id2 剩余 4、id3 剩余 4，买单现金占用 720。
	sellID := mustSell(t, e, "A", 6, 80) // id4：有效卖单，占用可卖数量 6
	bBid := mustBuy(t, e, "B", 3, 20)    // id5：跨合约待成交买单，占用 60

	if e.Cash() != 8640 || e.ReservedCash() != 780 || e.Position("A") != 14 ||
		e.Sellable("A") != 8 {
		t.Fatalf("前置账面错误: cash=%d reserved=%d posA=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Sellable("A"))
	}

	// 链中、链末报价先到并等待：seq3@51 仍大幅亏损，seq4@100 已完全恢复。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 51}, 0)
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 4, Price: 100}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3/seq4 跨号必须进入等待")
	}
	// 等待中的报价不得提前改变净值、风险状态或订单。
	if st := e.RiskStatus(); st.Equity != 10040 || st.Loss != 0 || st.Restricted {
		t.Fatalf("等待报价不得参与估值或改变限制状态: %+v", st)
	}
	if e.ReservedCash() != 780 {
		t.Fatalf("等待报价不得提前改变订单现金占用: %d", e.ReservedCash())
	}
	if o, _ := e.Order(bPending); o.Status != StatusPending {
		t.Fatalf("等待报价不得提前撤销订单: %+v", o)
	}

	// 补齐缺失的 seq2：seq2@50 净值 9340、亏损 660 触线；seq3@51 亏损 646；
	// seq4@100 净值 10040、亏损 0。同批 3 条全部生效。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 50})
	if err != nil {
		t.Fatalf("可完成估值的补齐必须成功: %v", err)
	}
	if n != 3 {
		t.Fatalf("本次应新生效 3 条报价（seq2、seq3、seq4），实际 %d", n)
	}

	// 批末价格已恢复、当前亏损为零，但限制必须保留。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 10000 || st.LossLimit != 500 || !st.Restricted {
		t.Fatalf("中间报价触线后限制必须整日保留: %+v", st)
	}
	if st.Equity != 10040 || st.Loss != 0 {
		t.Fatalf("查询净值必须使用批末最后生效报价 seq4@100，不能停在触线估值: %+v", st)
	}

	// 最新报价推进到批末，缺口消失。
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 4 || q.Moment != 4 || q.Price != 100 {
		t.Fatalf("最新报价应推进到链末 seq4@100: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后不应再报告缺口")
	}

	// 买单按编号从大到小全部撤销：id5（B）、id3（A 部分成交）、id2（A 待成交）。
	if o, _ := e.Order(bBid); o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("跨合约买单 bBid 应整单撤销: %+v", o)
	}
	o3, _ := e.Order(bPartial)
	if o3.Status != StatusCanceled || o3.Filled != 4 || o3.Remaining() != 0 {
		t.Fatalf("部分成交买单只应撤销未成交的 4 股、保留已成交 4 股: %+v", o3)
	}
	if !strings.Contains(o3.Reason, "日内亏损 660") || !strings.Contains(o3.Reason, "上限 500") {
		t.Fatalf("撤单原因必须固化真正触线时的亏损 660 与上限 500: %q", o3.Reason)
	}
	o2, _ := e.Order(bPending)
	if o2.Status != StatusCanceled || o2.Filled != 0 || o2.Remaining() != 0 {
		t.Fatalf("待成交买单应整单撤销: %+v", o2)
	}
	// 卖单及其可卖数量占用不受影响。
	if o, _ := e.Order(sellID); o.Status != StatusPending || o.Remaining() != 6 {
		t.Fatalf("有效卖单不得被亏损保护撤销: %+v", o)
	}
	if e.Sellable("A") != 8 {
		t.Fatalf("卖单占用的可卖数量必须保留: %d", e.Sellable("A"))
	}

	// 部分成交只释放未成交部分占用；已入账现金与持仓不变。
	if e.ReservedCash() != 0 {
		t.Fatalf("被撤买单的剩余现金占用必须全部释放: %d", e.ReservedCash())
	}
	if e.Cash() != 8640 {
		t.Fatalf("撤单不得改动已成交入账的现金: %d", e.Cash())
	}
	if e.Position("A") != 14 {
		t.Fatalf("已成交持仓必须保留: %d", e.Position("A"))
	}

	// 事件：恰好新增一条触线记录与三条撤单记录，且触线在撤单之前。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 4 {
		t.Fatalf("应新增 1 条触线 + 3 条撤单，实际新增 %d 条", len(newRecs))
	}
	tr := newRecs[0]
	if tr.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新记录必须是触线记录: %+v", tr)
	}
	// 触线来源、报价序号、模拟时刻、亏损与参与估值的报价定位固化在真正触线的 seq2，
	// 不能被批末恢复价格 seq4@100 替换。
	if tr.RiskTrigger != RiskTriggerQuote || tr.RiskRefSeq != 2 || tr.RiskRefMoment != 2 {
		t.Fatalf("触线记录必须锚定 seq2（时刻 2）: trigger=%s ref=%d moment=%d",
			tr.RiskTrigger, tr.RiskRefSeq, tr.RiskRefMoment)
	}
	if tr.RiskDay != 1 || tr.RiskBaseline != 10000 || tr.RiskEquity != 9340 ||
		tr.RiskLoss != 660 || tr.RiskLimit != 500 || !tr.RiskRestrict {
		t.Fatalf("触线记录风险快照必须固化 seq2 下的估值: %+v", tr)
	}
	if len(tr.RiskQuoteRefs) != 1 ||
		tr.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 50}) {
		t.Fatalf("参与估值的报价定位必须来自 seq2@50，不能被批末价格替换: %+v", tr.RiskQuoteRefs)
	}

	wantCancels := []struct {
		id     int64
		filled int64
		remain int64
		symbol string
		qSeq   int64
		qPrice int64
	}{
		{bBid, 0, 3, "B", 1, 20},    // 编号最大者先撤；B 的报价快照取自 B 自身 seq1@20
		{bPartial, 4, 4, "A", 2, 50},
		{bPending, 0, 4, "A", 2, 50},
	}
	for i, w := range wantCancels {
		c := newRecs[i+1]
		if c.Kind != RecordCanceled || c.OrderID != w.id || c.Symbol != w.symbol {
			t.Fatalf("第 %d 条撤单记录错误: %+v want id=%d", i+1, c, w.id)
		}
		if c.Filled != w.filled || c.Remaining != w.remain {
			t.Fatalf("撤单记录应标明已成交 %d、被取消剩余 %d: %+v", w.filled, w.remain, c)
		}
		if c.QuoteSeq != w.qSeq || c.QuotePrice != w.qPrice {
			t.Fatalf("撤单记录报价快照错误: %+v want seq=%d price=%d", c, w.qSeq, w.qPrice)
		}
		if c.RiskDay != 1 || c.RiskLoss != 660 || c.RiskLimit != 500 || !c.RiskRestrict {
			t.Fatalf("触线撤单记录必须携带触线当日风险快照: %+v", c)
		}
	}
	for _, r := range newRecs {
		if r.Kind == RecordRejected {
			t.Fatalf("成功补齐不得产生拒绝记录: %+v", r)
		}
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 当前亏损已为零，新买单仍必须被拒绝（拒绝记录上的限制位保持置位）。
	rb := len(e.Records())
	if _, err := e.Buy("A", 1, 1); err == nil {
		t.Fatal("批末价格恢复后限制仍须保留，新买单必须拒绝")
	}
	rj := e.Records()[rb]
	if rj.Kind != RecordRejected || !rj.RiskRestrict || rj.RiskDay != 1 || rj.RiskLoss != 0 {
		t.Fatalf("触线后拒单记录应在当前亏损 0 时仍带限制位: %+v", rj)
	}

	// 卖单成交在限制期间正常入账，占用随之释放。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: sellID, Symbol: "A", Side: Sell, Price: 80, Qty: 6}); err != nil {
		t.Fatalf("限制保留期间卖单成交应正常办理: %v", err)
	}
	if e.Cash() != 9120 || e.Position("A") != 8 || e.Sellable("A") != 8 {
		t.Fatalf("卖单成交后账面错误: cash=%d pos=%d sellable=%d",
			e.Cash(), e.Position("A"), e.Sellable("A"))
	}
	if !e.RiskStatus().Restricted {
		t.Fatal("卖出成交后限制仍须保留")
	}
}

// TestRiskGapFillLaterValuationOverflowRollsBackPlannedCancels 覆盖补齐失败：
// seq2@7 本会使亏损 6 达到上限 5 并撤销全部买单，但同批 seq3@MaxInt64 使
// 2 股持仓市值（进而现金加市值净值）无法用 int64 表示。整批必须回滚：
// 0 条生效、返回包装 ErrInt64Overflow 的错误；基准、上限与未限制状态保留，
// 拟触发的撤单不落地，等待中的 seq3 继续等待，只留一条拒绝记录。
func TestRiskGapFillLaterValuationOverflowRollsBackPlannedCancels(t *testing.T) {
	e, _ := NewEngine(10_000)
	mustSetMax(t, e, "A", 10)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	// 现金 9980、A 持仓 2；开日基准 10000，亏损上限 5（seq2@7 下亏损 6 会触线）。
	if err := e.StartTradingDay(1, 5); err != nil {
		t.Fatal(err)
	}
	b1 := mustBuy(t, e, "A", 2, 10) // 待成交买单，占用 20
	b2 := mustBuy(t, e, "A", 2, 10) // 待成交买单，占用 20
	sid := mustSell(t, e, "A", 1, 5) // 有效卖单，占用可卖 1
	if e.ReservedCash() != 40 || e.Sellable("A") != 1 {
		t.Fatalf("前置账面错误: reserved=%d sellable=%d", e.ReservedCash(), e.Sellable("A"))
	}

	// 链末天价报价先到并等待：2×MaxInt64 无法用 int64 表示。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: math.MaxInt64}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3 跨号必须进入等待")
	}
	if st := e.RiskStatus(); st.Equity != 10000 || st.Loss != 0 || st.Restricted {
		t.Fatalf("等待中的天价不得参与估值: %+v", st)
	}

	// 补齐 seq2@7：预检中 seq2 本会触线并模拟撤光 b1、b2；seq3 估值溢出，整批拒绝。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 7})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("链中估值溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if n != 0 {
		t.Fatalf("溢出时生效条数必须为零，实际 %d", n)
	}

	// 拟触发的撤单不能落地：两单继续待成交，占用、现金、持仓与卖单占用全部保留。
	for _, id := range []int64{b1, b2} {
		if o, _ := e.Order(id); o.Status != StatusPending || o.Remaining() != 2 {
			t.Fatalf("回滚不得留下 seq2 拟触发的撤单，订单 %d: %+v", id, o)
		}
	}
	if o, _ := e.Order(sid); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("卖单及其占用必须保持提交前状态: %+v", o)
	}
	if e.ReservedCash() != 40 || e.Cash() != 9980 || e.Position("A") != 2 || e.Sellable("A") != 1 {
		t.Fatalf("回滚后资金、占用与持仓必须保持提交前值: reserved=%d cash=%d pos=%d sellable=%d",
			e.ReservedCash(), e.Cash(), e.Position("A"), e.Sellable("A"))
	}

	// 此前最新报价、当日基准、亏损上限与限制状态全部保留。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 {
		t.Fatalf("回滚后最新报价应停留在 seq1@10: %+v", q)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 10000 || st.LossLimit != 5 ||
		st.Restricted || st.Equity != 10000 || st.Loss != 0 {
		t.Fatalf("回滚后风险状态必须保持提交前值: %+v", st)
	}

	// 原先等待的 seq3 仍然等待，不能被当作已处理报价丢弃。
	if !e.HasGap("A") {
		t.Fatal("回滚后 seq3 必须仍在等待")
	}
	// 等待中的同内容报价重复提交仍是无操作（继续等待，不产生记录）。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: math.MaxInt64}, 0)
	// seq2 从未生效：再次补齐仍会重走整条链并以同样的溢出失败，证明回滚可重复、
	// 等待报价未被丢弃。
	n2, err2 := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 7})
	if !errors.Is(err2, ErrInt64Overflow) || n2 != 0 {
		t.Fatalf("重复补齐必须得到同样的溢出失败与 0 条生效: n=%d err=%v", n2, err2)
	}
	if !e.HasGap("A") {
		t.Fatal("重复失败后 seq3 仍须继续等待")
	}

	// 每次失败只新增一条说明估值溢出的拒绝记录（两次尝试共两条），
	// 任何时候都不得留下触线或撤单记录。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 2 {
		t.Fatalf("两次失败应各新增一条拒绝记录，实际新增 %d 条", len(newRecs))
	}
	for _, r := range newRecs {
		if r.Kind != RecordRejected {
			t.Fatalf("失败只允许新增拒绝记录: %+v", r)
		}
		if !strings.Contains(r.Reason, "序号 3") || !strings.Contains(r.Reason, "int64") {
			t.Fatalf("拒绝原因必须指明链中溢出的 seq3 与 int64 溢出: %q", r.Reason)
		}
		// 拒绝记录固化提交前的风险与报价快照（seq1、未限制、亏损 0）。
		if r.Symbol != "A" || r.QuoteSeq != 1 || r.QuotePrice != 10 {
			t.Fatalf("拒绝记录报价快照应停留在 seq1@10: %+v", r)
		}
		if r.RiskDay != 1 || r.RiskBaseline != 10000 || r.RiskEquity != 10000 ||
			r.RiskLoss != 0 || r.RiskLimit != 5 || r.RiskRestrict {
			t.Fatalf("拒绝记录风险快照应保持提交前状态: %+v", r)
		}
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("整批失败不得留下任何触线记录")
	}
	for _, r := range recs {
		if r.Kind == RecordCanceled {
			t.Fatalf("整批失败不得留下任何撤单记录: %+v", r)
		}
	}

	// 未触线：缺口仍在期间新买单被拒，原因只能来自行情缺口，不能来自亏损保护。
	_, errBuy := e.Buy("A", 1, 10)
	if errBuy == nil {
		t.Fatal("缺口期间新买单应按行情缺口规则拒绝")
	}
	if !strings.Contains(errBuy.Error(), "缺口") {
		t.Fatalf("未触线时买单被拒应只归因于行情缺口: %v", errBuy)
	}
	if strings.Contains(errBuy.Error(), "亏损") {
		t.Fatalf("失败的补齐不得把账户置于亏损限制状态: %v", errBuy)
	}
}
