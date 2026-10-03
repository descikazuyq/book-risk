package book

import (
	"errors"
	"math"
	"testing"
)

// 本组测试回归保护“一次补齐多条报价”时的订单状态承接语义：
// 较早生效的报价为收敛账户总持仓金额上限撤销买单后，较后（原先行等待的）
// 高序号报价做溢出/超限判断时，必须以前一条处理后的订单状态为准——已撤销
// 买单的剩余量不得再计入占用；被撤订单释放的现金、撤单原因与报价定位都
// 固定在实际引发超限的那条报价上，不能被后面的高价改写。
//
// 共同形态：seq3 高价先到并等待，调用方再提交缺失的 seq2；两条报价连续
// 生效，seq2 使金额超限并按订单编号从大到小撤单。若后一条报价还把已撤
// 买单的剩余量按高价计一次，乘积就会超过 int64，补齐会被误判为整批溢出。

// assertNoRejectAfter 断言基准时刻之后未新增任何拒绝记录。
func assertNoRejectAfter(t *testing.T, e *Engine, before int) {
	t.Helper()
	recs := e.Records()
	for i := before; i < len(recs); i++ {
		if recs[i].Kind == RecordRejected {
			t.Fatalf("补齐成功后不得新增拒绝记录，却出现: %+v", recs[i])
		}
	}
}

// findCancelRecord 取回 bid 的撤销记录；不存在则失败。
func findCancelRecord(t *testing.T, e *Engine, bid int64) Record {
	t.Helper()
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled && r.OrderID == bid {
			return r
		}
	}
	t.Fatalf("未找到订单 %d 的撤销记录", bid)
	return Record{}
}

// TestAmountGapFillCanceledBuySkippedByLaterHugeQuote 题述核心场景：
// 已启用金额上限、同一合约有两笔有效买单；seq3 天价先到并等待，再提交
// 缺失的 seq2。seq2 使金额超限并撤掉编号最大的买单；seq3 的高价不得再把
// 这笔已撤买单计入（否则 Q×P 溢出），而真实保留买单金额可表示，补齐成功。
func TestAmountGapFillCanceledBuySkippedByLaterHugeQuote(t *testing.T) {
	M := int64(math.MaxInt64)
	// 数值设计（均在 int64 内）：
	//   P = M/3+1：3×P = M+2 已超出 int64；2×P 可表示。
	//   Q = M/16：被撤买单数量；Q×P 必然溢出（Q 远大于 2）。
	//   L = 2P：金额上限，使链末保留买单恰好等于上限而存活。
	P := M/3 + 1
	Q := M / 16
	L := 2 * P

	e, _ := NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 2, 1) // 保留单：2 股限价 1
	b2 := mustBuy(t, e, "A", Q, 1) // 将被 seq2 撤销的大单
	// seq1@10 口径：b1 20 + b2 10Q；10Q ≈ 5.76e18 < L ≈ 6.15e18，设置时不撤单。
	canceledAtSet, err := e.SetPositionAmountLimit(L)
	if err != nil {
		t.Fatal(err)
	}
	if len(canceledAtSet) != 0 {
		t.Fatalf("设置上限时合计未超限，不应撤单: %v", canceledAtSet)
	}

	// seq3 天价跨号等待，不参与任何计算。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: P}, 0)
	if !e.HasGap("A") {
		t.Fatal("seq3 必须进入等待")
	}

	// 补齐 seq2@12：b1 24 + b2 12Q = 12Q+24 > L，撤 b2（编号最大）→ 24 ≤ L。
	// 随后 seq3@P：b2 已撤必须跳过（Q×P 溢出不得发生）；b1 2P = L 恰好等于上限。
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if err != nil {
		t.Fatalf("已撤买单不应被 seq3 高价计入，补齐必须成功，实际 %v", err)
	}
	if n != 2 {
		t.Fatalf("本次真正生效的报价应为 2 条，实际 %d", n)
	}
	assertNoRejectAfter(t, e, before)

	o2, _ := e.Order(b2)
	if o2.Status != StatusCanceled || o2.Remaining() != 0 || o2.Filled != 0 {
		t.Fatalf("b2 应已撤销且无剩余量: %+v", o2)
	}
	o1, _ := e.Order(b1)
	if o1.Status != StatusPending || o1.Remaining() != 2 {
		t.Fatalf("链末 b1 占用恰为上限，应仍有效: %+v", o1)
	}

	// 最新报价推进到链末 seq3，缺口消失。
	if q, _ := e.CurrentQuote("A"); q != (Quote{Seq: 3, Moment: 3, Price: P}) {
		t.Fatalf("最新报价应为链末 seq3@P: %+v", q)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}

	// 链末口径：无持仓；保留买单 2×max(限价 1, 高价 P)=2P=L；无已撤单残留占用。
	st := e.PositionAmountStatus()
	if st.Holding != 0 || st.BuyReserved != L || st.Total != L {
		t.Fatalf("链末真实金额错误（不能残留已撤单占用）: %+v", st)
	}
	// b2 的限价占用 Q 随撤销释放，只剩 b1 的 2。
	if e.ReservedCash() != 2 {
		t.Fatalf("被撤 b2 的现金占用必须释放，只保留 b1 的 2: %d", e.ReservedCash())
	}

	// 撤单原因与报价定位来自实际引发超限的 seq2@12，seq3 的高价不能改写。
	rec := findCancelRecord(t, e, b2)
	if len(rec.AmtQuoteRefs) != 1 ||
		rec.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("撤单定位必须来自引发超限的 seq2@12: %+v", rec.AmtQuoteRefs)
	}
	if rec.AmtHolding != 0 || rec.AmtBuyReserved != 12*Q+24 ||
		rec.AmtTotal != 12*Q+24 || rec.AmtLimit != L {
		t.Fatalf("撤单记录应固化 seq2 判断时撤销前的金额与上限: %+v", rec)
	}
	if rec.Remaining != Q {
		t.Fatalf("撤销记录应注明被取消的剩余量 %d: %+v", Q, rec)
	}
}

// TestAmountGapFillPartialFillCanceledBuySkipped 覆盖部分成交买单：
// seq2 引发的撤销只取消未成交数量；已成交的 1 股继续参与 seq3 高价下的
// 金额计算（不能为避免溢出漏算已有持仓），现金余额与已入账成交保持原值。
func TestAmountGapFillPartialFillCanceledBuySkipped(t *testing.T) {
	M := int64(math.MaxInt64)
	P := M / 2 // 2×P = M-1 可表示（持仓 1 + 保留剩余 1）；3×P 溢出（若误计已撤单）

	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b1, Symbol: "A", Side: Buy, Price: 9, Qty: 1}); err != nil {
		t.Fatal(err)
	} // 持仓 1、b1 剩余 1
	b2 := mustBuy(t, e, "A", 3, 10)
	// seq1 口径：持仓 10 + b1 剩余 10 + b2 30 = 50。
	if _, err := e.SetPositionAmountLimit(50); err != nil {
		t.Fatal(err)
	}

	// seq3 天价先等待。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: P}, 0)

	// 补齐 seq2@12：持仓 12、b1 12、b2 36 合计 60 > 50，撤 b2 → 24 ≤ 50。
	// 接着 seq3@P：b2 已撤须跳过（3×P 会溢出）；持仓 P + b1 P = M-1 可表示，
	// 但仅持仓金额 P 已远超上限 50，按既有规则再撤 b1 的剩余 1 股（不自动卖出）。
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if err != nil {
		t.Fatalf("已撤剩余量不应被 seq3 计入，补齐必须成功，实际 %v", err)
	}
	if n != 2 {
		t.Fatalf("应返回本次真正生效的 2 条报价，实际 %d", n)
	}
	assertNoRejectAfter(t, e, before)

	o2, _ := e.Order(b2)
	if o2.Status != StatusCanceled || o2.Filled != 0 || o2.Remaining() != 0 {
		t.Fatalf("b2 未成交的 3 股必须全部撤销: %+v", o2)
	}
	o1, _ := e.Order(b1)
	if o1.Status != StatusCanceled || o1.Filled != 1 || o1.Remaining() != 0 {
		t.Fatalf("b1 已成交 1 股保留、剩余 1 股被 seq3 收敛撤销: %+v", o1)
	}

	if q, _ := e.CurrentQuote("A"); q != (Quote{Seq: 3, Moment: 3, Price: P}) {
		t.Fatalf("最新报价必须推进到链末: %+v", q)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}

	// 链末口径：已成交 1 股仍按 seq3 高价计市值（持仓不漏算）；买单全部撤销，
	// 不残留任何已撤单占用。
	st := e.PositionAmountStatus()
	if st.Holding != P || st.BuyReserved != 0 || st.Total != P {
		t.Fatalf("链末应只剩按高价计值的真实持仓: %+v", st)
	}
	if e.Position("A") != 1 {
		t.Fatal("撤销未成交部分不得改变已成交持仓")
	}
	if e.Cash() != 1_000_000-9 {
		t.Fatalf("现金余额与已入账成交必须保持原值: %d", e.Cash())
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("两单剩余现金占用都应释放: %d", e.ReservedCash())
	}

	// b2 的撤单原因与定位固定在 seq2@12；b1 的撤销则由链末 seq3@P 引发。
	rec2 := findCancelRecord(t, e, b2)
	if len(rec2.AmtQuoteRefs) != 1 ||
		rec2.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 2, Moment: 2, Price: 12}) {
		t.Fatalf("b2 撤单定位必须来自 seq2@12: %+v", rec2.AmtQuoteRefs)
	}
	if rec2.Remaining != 3 {
		t.Fatalf("b2 被取消的应是全部剩余 3 股: %+v", rec2)
	}
	rec1 := findCancelRecord(t, e, b1)
	if len(rec1.AmtQuoteRefs) != 1 ||
		rec1.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 3, Price: P}) {
		t.Fatalf("b1 撤单定位必须来自链末 seq3@P: %+v", rec1.AmtQuoteRefs)
	}
	if rec1.Filled != 1 || rec1.Remaining != 1 {
		t.Fatalf("b1 只取消未成交的 1 股、已成交 1 股保留: %+v", rec1)
	}
}

// TestAmountGapFillHoldingOverflowAfterCancelRollsBack 保留真正溢出的失败：
// seq2 本会触发撤单，但 seq3 高价使仍存在的持仓市值（2×MaxInt64）溢出，
// 与已撤买单无关。整次补齐必须回滚：0 条生效、不留撤单/占用变化/成功记录，
// 最新报价、订单、资金保持提交前值，原等待报价继续等待，只新增一条拒绝记录。
func TestAmountGapFillHoldingOverflowAfterCancelRollsBack(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	hb := mustBuy(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: hb, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	b1 := mustBuy(t, e, "A", 5, 10)
	b2 := mustBuy(t, e, "A", 3, 10)
	// seq1 口径：持仓 20 + 买单 80 = 100，上限取 100（设置时不撤单）。
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}

	// seq3 价格 M：持仓 2×M 必然溢出（撤多少买单都改变不了持仓）。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: M}, 0)
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("链末持仓市值溢出必须整批失败且 0 条生效: n=%d err=%v", n, err)
	}

	// 即使 seq2@12（合计 124 > 100）本来会撤掉 b2，也不得留下那次撤销。
	for _, id := range []int64{b1, b2} {
		if o, _ := e.Order(id); o.Status != StatusPending {
			t.Fatalf("回滚不得撤销订单 %d: %+v", id, o)
		}
	}
	// 最新已生效报价、资金、占用、持仓保持提交前值。
	if q, _ := e.CurrentQuote("A"); q != (Quote{Seq: 1, Moment: 1, Price: 10}) {
		t.Fatalf("最新报价应停留在 seq1@10: %+v", q)
	}
	if e.Cash() != 1_000_000-20 || e.ReservedCash() != 80 || e.Position("A") != 2 {
		t.Fatalf("现金/占用/持仓保持提交前值: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 20 || st.BuyReserved != 80 || st.Total != 100 {
		t.Fatalf("金额状态应保持 seq1 口径: %+v", st)
	}
	// 原先等待的 seq3 原样继续等待，只新增一条说明原因的拒绝记录。
	pending := pendingQuotesOf(e, "A")
	if len(pending) != 1 || pending[3] != (Quote{Seq: 3, Moment: 3, Price: M}) {
		t.Fatalf("等待中的 seq3 必须原样保留: %+v", pending)
	}
	recs := e.Records()
	if len(recs) != before+1 || recs[before].Kind != RecordRejected {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	if !recs[before].AmtEnabled || recs[before].AmtLimit != 100 {
		t.Fatalf("拒绝记录应固化金额上限设置: %+v", recs[before])
	}
}

// TestAmountGapFillActiveBuyReserveOverflowAfterCancelRollsBack 保留真正溢出的
// 失败（有效买单来源）：seq2 撤掉编号最大的 b2 后，链中 seq3 天价仍使仍有效
// 买单 b1 的占用 6×MaxInt64 溢出——恰因已撤 b2 未被计入，才定位到真实占用者。
// 整批同样回滚，且等待报价原样保留。
func TestAmountGapFillActiveBuyReserveOverflowAfterCancelRollsBack(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 6, 10) // seq3 下的真实占用者：6×M 溢出
	b2 := mustBuy(t, e, "A", 3, 10) // 预检中会被 seq2 模拟撤销
	// seq1 口径：60+30=90，上限取 90（设置时不撤单）。
	if _, err := e.SetPositionAmountLimit(90); err != nil {
		t.Fatal(err)
	}

	// seq2@12 本会撤 b2（108 > 90 → 撤后 72）；seq3@M 下 b1 的 6×M 才是真溢出。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: M}, 0)
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if !errors.Is(err, ErrInt64Overflow) || n != 0 {
		t.Fatalf("链末有效买单占用溢出必须整批失败且 0 条生效: n=%d err=%v", n, err)
	}

	for _, id := range []int64{b1, b2} {
		if o, _ := e.Order(id); o.Status != StatusPending {
			t.Fatalf("回滚不得撤销订单 %d: %+v", id, o)
		}
	}
	if q, _ := e.CurrentQuote("A"); q != (Quote{Seq: 1, Moment: 1, Price: 10}) {
		t.Fatalf("最新报价应停留在 seq1@10: %+v", q)
	}
	if e.ReservedCash() != 90 {
		t.Fatalf("占用保持提交前值（60+30）: %d", e.ReservedCash())
	}
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 90 || st.Total != 90 {
		t.Fatalf("金额状态应保持 seq1 口径: %+v", st)
	}
	pending := pendingQuotesOf(e, "A")
	if len(pending) != 1 || pending[3] != (Quote{Seq: 3, Moment: 3, Price: M}) {
		t.Fatalf("等待中的 seq3 必须原样保留: %+v", pending)
	}
	recs := e.Records()
	if len(recs) != before+1 || recs[before].Kind != RecordRejected {
		t.Fatalf("只能新增一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
}

// TestAmountGapFillOverLimitButRepresentableStillCancels 金额超过上限但仍可
// 表示时，继续按已有规则撤单，不得误当成整数溢出：seq2 撤 b2 后，seq3 的
// 高价（乘积与合计都在 int64 内）使 b1 占用远超上限，继续收敛撤掉 b1。
func TestAmountGapFillOverLimitButRepresentableStillCancels(t *testing.T) {
	e, _ := NewEngine(1_000_000)
	mustSetMax(t, e, "A", 1_000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "A", 5, 10)
	b2 := mustBuy(t, e, "A", 3, 10)
	if _, err := e.SetPositionAmountLimit(80); err != nil {
		t.Fatal(err)
	}

	// seq3 高价 1_000_000：5×1e6 可表示但远超限额 80。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 3, Price: 1_000_000}, 0)
	before := len(e.Records())
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 2, Price: 12})
	if err != nil {
		t.Fatalf("可表示的超限必须按撤单收敛，不得报溢出: %v", err)
	}
	if n != 2 {
		t.Fatalf("两条报价都应生效，实际 %d", n)
	}

	for _, id := range []int64{b2, b1} {
		if o, _ := e.Order(id); o.Status != StatusCanceled {
			t.Fatalf("seq2 撤 b2、seq3 继续撤 b1，订单 %d 应已撤销: %+v", id, o)
		}
	}
	if st := e.PositionAmountStatus(); st.Holding != 0 || st.BuyReserved != 0 || st.Total != 0 {
		t.Fatalf("全部买单撤光后金额应为零: %+v", st)
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("两单现金占用都应释放: %d", e.ReservedCash())
	}
	// 只有撤销记录、没有拒绝记录；b1 的撤单定位来自链末 seq3。
	var sawReject bool
	var b1Rec *Record
	for _, r := range e.Records()[before:] {
		if r.Kind == RecordRejected {
			sawReject = true
		}
		if r.Kind == RecordCanceled && r.OrderID == b1 {
			rr := r
			b1Rec = &rr
		}
	}
	if sawReject {
		t.Fatal("可表示的超限不得产生拒绝记录")
	}
	if b1Rec == nil || len(b1Rec.AmtQuoteRefs) != 1 ||
		b1Rec.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 3, Price: 1_000_000}) {
		t.Fatalf("b1 应由链末 seq3@1e6 引发撤销并固化其定位: %+v", b1Rec)
	}
}

// pendingQuotesOf 读取等待中的报价副本（测试辅助，直接访问包内状态）。
func pendingQuotesOf(e *Engine, symbol string) map[int64]Quote {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.symbols[symbol]
	out := make(map[int64]Quote, len(st.pending))
	for k, v := range st.pending {
		out[k] = v
	}
	return out
}
