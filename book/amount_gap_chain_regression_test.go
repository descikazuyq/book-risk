package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// 本文件为账户总持仓金额上限补充回归保障：一次补齐多条报价时，前一条报价
// 为收敛超限而撤销的买单，不得又被链中后一条报价计入占用；连续报价必须逐条
// 生效，后续判断承接前一条处理后的订单状态。
//
// 关键构造：让较后序号的报价先到并等待，其价格高到“已撤买单数量 × 该价格”
// 会溢出 int64；再提交缺失报价触发整链生效。若实现错误地把前一条已撤买单
// 重新计入后一条的金额计算，整批就会被误判为 ErrInt64Overflow 而回滚。

// TestAmountGapFillCanceledBuyExcludedFromLaterHugeQuote 复现核心情形：
// seq2@20 使买单金额占用超限并撤单；链末 seq3 价格为 MaxInt64，若仍把已撤的
// 2 股计入，2×MaxInt64 必然溢出。真实剩余持仓为 0 且已无有效买单，补齐必须
// 成功：2 条生效、最新报价推进到链末、缺口消失、现金占用释放。
func TestAmountGapFillCanceledBuyExcludedFromLaterHugeQuote(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 2, 10) // 金额占用 2×max(10,10)=20，现金占用 20
	if _, err := e.SetPositionAmountLimit(30); err != nil {
		t.Fatal(err)
	}

	// 较后序号的天价报价先到并等待（2×MaxInt64 无法用 int64 表示）。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: math.MaxInt64}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3 跨号必须进入等待")
	}

	// 调用方补齐缺失的 seq2：seq2@20 下买单占用 40 > 30 必须撤单；
	// seq3 天价只面对“持仓 0、无有效买单”的状态，不得溢出。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 20})
	if err != nil {
		t.Fatalf("已撤买单不得被链末报价计入致整批失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("本次应真正生效 2 条报价（seq2、seq3），实际 %d", n)
	}

	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("买单应被 seq2 撤销且无剩余量: %+v", o)
	}
	// 撤销原因必须来自实际引发超限的 seq2@20，不能被后面的天价改写。
	if !strings.Contains(o.Reason, "报价序号 2") || strings.Contains(o.Reason, "报价序号 3") {
		t.Fatalf("撤单原因必须锚定 seq2，不得被 seq3 改写: %q", o.Reason)
	}

	if e.ReservedCash() != 0 {
		t.Fatalf("被撤订单的剩余现金占用必须释放: %d", e.ReservedCash())
	}
	if e.Cash() != 1_000_000 {
		t.Fatalf("无成交时现金余额不得变化: %d", e.Cash())
	}

	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 3 || q.Price != math.MaxInt64 {
		t.Fatalf("最新报价应推进到链末 seq3@MaxInt64: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}

	// 链末天价下：持仓为 0、无有效买单，合计为 0——既未留下已撤单占用，
	// 也没有为规避溢出而漏算（此处本就无持仓）。
	st := e.PositionAmountStatus()
	if !st.Enabled || st.Limit != 30 || st.Holding != 0 || st.BuyReserved != 0 || st.Total != 0 {
		t.Fatalf("链末金额状态错误: %+v", st)
	}

	// 只新增一条 seq2 引发的撤销记录，不得有任何拒绝记录。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("应只新增一条撤单记录，实际新增 %d 条", len(recs)-recsBefore)
	}
	cr := recs[recsBefore]
	if cr.Kind != RecordCanceled || cr.OrderID != bid {
		t.Fatalf("新增记录必须是该买单的撤销记录: %+v", cr)
	}
	if cr.AmtEnabled != true || cr.AmtLimit != 30 || cr.AmtHolding != 0 ||
		cr.AmtBuyReserved != 40 || cr.AmtTotal != 40 {
		t.Fatalf("撤单记录应固化 seq2 判断时的金额: %+v", cr)
	}
	// 报价定位来自实际引发超限的 seq2@20，而非链末天价。
	if cr.QuoteSeq != 2 || cr.QuotePrice != 20 || len(cr.AmtQuoteRefs) != 1 {
		t.Fatalf("撤单记录的报价快照应锚定 seq2@20: quote=%d/%d refs=%+v",
			cr.QuoteSeq, cr.QuotePrice, cr.AmtQuoteRefs)
	}
	if cr.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 20}) {
		t.Fatalf("撤单报价定位错误: %+v", cr.AmtQuoteRefs[0])
	}
}

// TestAmountGapFillPartialFillKeepsFilledHoldingExcludesCanceledRemaining 覆盖
// 部分成交买单：报价引发的撤销只取消未成交数量；已成交持仓在链末天价下继续
// 参与金额计算（且仍可表示），现金余额与已入账成交保持原值。
func TestAmountGapFillPartialFillKeepsFilledHoldingExcludesCanceledRemaining(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 10, 10)
	// 部分成交 4 股 @8：现金按实际价扣 32；持仓 4、剩余买单 6。
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 8, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	// 持仓 4×10=40 + 剩余买单 6×10=60 = 100，上限恰为 100。
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}

	// P3 取 MaxInt64/5：4×P3 仍可表示（持仓要继续计入），6×P3 则溢出
	// （若被撤的 6 股剩余量仍被计入，整批会误失败）。
	p3 := int64(math.MaxInt64 / 5)
	wantHolding := p3 * 4
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: p3}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if err != nil {
		t.Fatalf("部分成交场景下补齐必须成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("应生效 2 条报价，实际 %d", n)
	}

	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Filled != 4 || o.Remaining() != 0 {
		t.Fatalf("只应撤销未成交的 6 股、保留已成交 4 股: %+v", o)
	}
	if !strings.Contains(o.Reason, "报价序号 2") {
		t.Fatalf("撤销原因必须来自 seq2@12: %q", o.Reason)
	}

	// 现金余额与已入账成交保持原值；剩余买单现金占用 6×10=60 必须释放。
	if e.Cash() != 1_000_000-32 {
		t.Fatalf("收敛撤单不得改动已成交现金结算: %d", e.Cash())
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("未成交部分的现金占用必须释放: %d", e.ReservedCash())
	}
	if e.Position("A") != 4 {
		t.Fatalf("已成交持仓必须保留: %d", e.Position("A"))
	}

	// 链末报价下：真实持仓 4×P3 继续计入（不为规避溢出而漏算），已撤剩余量不计入。
	st := e.PositionAmountStatus()
	if st.Holding != wantHolding || st.BuyReserved != 0 || st.Total != wantHolding {
		t.Fatalf("链末应只剩可表示的真实持仓市值: %+v want=%d", st, wantHolding)
	}

	q, _ := e.CurrentQuote("A")
	if q.Seq != 3 || q.Price != p3 || e.HasGap("A") {
		t.Fatalf("最新报价应推进到链末且缺口消失: %+v gap=%v", q, e.HasGap("A"))
	}

	// 撤单记录固化 seq2 下撤销前金额与“被取消的剩余量 6”。
	recs := e.Records()
	cr := recs[recsBefore]
	if cr.Kind != RecordCanceled || cr.Remaining != 6 || cr.Filled != 4 {
		t.Fatalf("撤单记录应标明取消剩余 6 股、已成交 4 股: %+v", cr)
	}
	if cr.AmtHolding != 48 || cr.AmtBuyReserved != 72 || cr.AmtTotal != 120 || cr.AmtLimit != 100 {
		t.Fatalf("撤单记录金额快照应为 seq2@12 下的值: %+v", cr)
	}
	if cr.QuoteSeq != 2 || cr.QuotePrice != 12 {
		t.Fatalf("撤单报价定位必须来自 seq2: %+v", cr)
	}
	for _, r := range recs[recsBefore:] {
		if r.Kind == RecordRejected {
			t.Fatalf("成功补齐不得新增拒绝记录: %+v", r)
		}
	}
}

// TestAmountGapFillLaterOverflowRollsBackEarlierAmountCancel 保留真正的溢出失败：
// seq2 本会撤掉编号最大的买单，但链末 seq3 使一条仍有效买单的金额占用溢出
// （2×(MaxInt64/2+1) 不可表示）。整批必须回滚——连 seq2 那次撤单、释放的
// 占用都不能留下，生效条数为零，只新增一条拒绝记录，等待中的 seq3 继续等待。
func TestAmountGapFillLaterOverflowRollsBackEarlierAmountCancel(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 2, 10)                         // 占用 20，seq2 后保留
	b2 := mustBuy(t, e, "A", 6, 10)                         // 占用 60，seq2@12 下本应先被撤销
	if _, err := e.SetPositionAmountLimit(80); err != nil { // 20+60 恰满，设置时不撤单
		t.Fatal(err)
	}

	p3 := int64(math.MaxInt64/2 + 1) // 2×p3 溢出；6×p3 同样溢出
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: p3}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("链末仍有效买单金额溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if n != 0 {
		t.Fatalf("溢出时生效条数必须为零，实际 %d", n)
	}

	// seq2 本会触发的撤单不得留下：两单都保持待成交，占用与资金回到提交前。
	for _, id := range []int64{b1, b2} {
		if o, _ := e.Order(id); o.Status != StatusPending {
			t.Fatalf("回滚不得留下 seq2 的撤单，订单 %d 状态: %+v", id, o)
		}
	}
	if o, _ := e.Order(b2); o.Remaining() != 6 {
		t.Fatalf("b2 剩余量应保持 6: %+v", o)
	}
	if e.ReservedCash() != 80 || e.Cash() != 1_000_000 {
		t.Fatalf("资金与占用必须保持提交前值: reserved=%d cash=%d", e.ReservedCash(), e.Cash())
	}

	// 最新已生效报价停留在 seq1；原先等待的 seq3 继续等待。
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || q.Price != 10 {
		t.Fatalf("回滚后最新报价应停留在 seq1@10: %+v", q)
	}
	if !e.HasGap("A") {
		t.Fatal("回滚后 seq3 必须仍在等待")
	}
	// 等待中的同内容报价重复提交仍是无操作（继续等待）。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: p3}, 0)

	// 金额查询反映提交前的可表示状态（seq1 下合计 80）。
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 80 || st.Total != 80 {
		t.Fatalf("回滚后金额状态应恢复提交前值: %+v", st)
	}

	// 只新增一条说明原因的拒绝记录，且不得有任何撤单记录残留。
	recs := e.Records()
	if len(recs) != recsBefore+1 {
		t.Fatalf("整批失败只能新增一条记录，实际 %d 条", len(recs)-recsBefore)
	}
	rj := recs[recsBefore]
	if rj.Kind != RecordRejected || !rj.AmtEnabled || rj.AmtLimit != 80 {
		t.Fatalf("应新增一条携带金额上限快照的拒绝记录: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "序号 3") || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因必须指明链中溢出的 seq3: %q", rj.Reason)
	}
}

// TestAmountGapFillLaterHoldingOverflowRollsBackEarlierCancel 覆盖溢出来自仍存在
// 的持仓市值：seq2 撤光买单剩余量后，链末 seq3 使已成交持仓 4×p3 溢出
// （p3=MaxInt64/4+1）。即使前面没有任何有效买单了，持仓本身不可表示仍要整批
// 回滚，seq2 的撤单同样不能留下。
func TestAmountGapFillLaterHoldingOverflowRollsBackEarlierCancel(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	bid := mustBuy(t, e, "A", 4, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	b2 := mustBuy(t, e, "A", 2, 10) // 剩余买单占用 20
	// 持仓 40 + 买单 20 = 60，上限恰为 60。
	if _, err := e.SetPositionAmountLimit(60); err != nil {
		t.Fatal(err)
	}

	p3 := int64(math.MaxInt64/4 + 1) // 4×p3 溢出
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: p3}, 0)

	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("链末持仓市值溢出必须整批失败且 0 条生效: n=%d err=%v", n, err)
	}

	// seq2 本会撤销 b2（48+24=72>60），回滚后不得留下该撤销。
	if o, _ := e.Order(b2); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("b2 必须保持待成交、剩余 2: %+v", o)
	}
	if e.Position("A") != 4 || e.Cash() != 1_000_000-40 || e.ReservedCash() != 20 {
		t.Fatalf("持仓、现金与占用必须保持提交前值: pos=%d cash=%d reserved=%d",
			e.Position("A"), e.Cash(), e.ReservedCash())
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || !e.HasGap("A") {
		t.Fatalf("最新报价停留 seq1 且 seq3 继续等待: q=%+v gap=%v", q, e.HasGap("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 20 || st.Total != 60 {
		t.Fatalf("金额状态应恢复提交前值: %+v", st)
	}
	recs := e.Records()
	if len(recs) != recsBefore+1 || recs[recsBefore].Kind != RecordRejected {
		t.Fatalf("只能新增一条拒绝记录，实际 %d 条", len(recs)-recsBefore)
	}
}

// TestAmountGapFillRepresentableOverLimitStillCancelsQuoteByQuote 区分“超限”与
// “溢出”：链末高价使仍有效买单金额远超上限但仍可表示时，继续按已有规则撤单，
// 不得误当成整数溢出；且两条报价各自的撤单分别锚定各自的报价定位。
func TestAmountGapFillRepresentableOverLimitStillCancelsQuoteByQuote(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 20)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 2, 10) // 20
	b2 := mustBuy(t, e, "A", 6, 10) // 60，合计恰 80
	if _, err := e.SetPositionAmountLimit(80); err != nil {
		t.Fatal(err)
	}

	// seq3 高价但仍可表示（2×1_000_000=2_000_000）；先等待。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 1_000_000}, 0)

	// seq2@12：24+72=96 > 80，撤 b2 后 24 ≤ 80；
	// seq3@1_000_000：b1 占用 2_000_000 仍超限但可表示，再撤 b1。
	recsBefore := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if err != nil {
		t.Fatalf("可表示的超限必须按规则撤单而非报溢出: %v", err)
	}
	if n != 2 {
		t.Fatalf("应生效 2 条，实际 %d", n)
	}
	if o, _ := e.Order(b2); o.Status != StatusCanceled || !strings.Contains(o.Reason, "报价序号 2") {
		t.Fatalf("b2 应由 seq2 撤销: %+v", o)
	}
	if o, _ := e.Order(b1); o.Status != StatusCanceled || !strings.Contains(o.Reason, "报价序号 3") {
		t.Fatalf("b1 应由链末 seq3 撤销: %+v", o)
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("两单剩余现金占用都应释放: %d", e.ReservedCash())
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 3 || e.HasGap("A") {
		t.Fatalf("链末推进、缺口消失: q=%+v", q)
	}
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 0 || st.Total != 0 {
		t.Fatalf("撤光后合计应为零: %+v", st)
	}

	// 两条撤单记录分别固化各自报价下的判断，不得互相改写。
	var cancels []Record
	for _, r := range e.Records()[recsBefore:] {
		if r.Kind == RecordCanceled {
			cancels = append(cancels, r)
		}
		if r.Kind == RecordRejected {
			t.Fatalf("可表示的超限不得产生拒绝记录: %+v", r)
		}
	}
	if len(cancels) != 2 {
		t.Fatalf("应有两条撤单记录，实际 %d", len(cancels))
	}
	cB2, cB1 := cancels[0], cancels[1]
	if cB2.OrderID != b2 || cB2.QuoteSeq != 2 || cB2.AmtBuyReserved != 96 || cB2.AmtTotal != 96 ||
		len(cB2.AmtQuoteRefs) != 1 || cB2.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("b2 撤单快照必须锚定 seq2@12 与撤销前合计 96: %+v", cB2)
	}
	if cB1.OrderID != b1 || cB1.QuoteSeq != 3 || cB1.AmtBuyReserved != 2_000_000 || cB1.AmtTotal != 2_000_000 ||
		len(cB1.AmtQuoteRefs) != 1 || cB1.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 3, Price: 1_000_000}) {
		t.Fatalf("b1 撤单快照必须锚定 seq3@1_000_000 与撤销前合计 2_000_000: %+v", cB1)
	}
}
