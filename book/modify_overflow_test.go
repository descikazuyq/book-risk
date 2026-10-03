package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// setupModifyNearInt64Max 构造题述场景（报价连续、现金足够、未开启日内亏损保护、
// 未设置账户总持仓金额上限）：M 为 int64 最大值，合约 A 最大持仓量 M、报价 seq1@1；
// 当前持仓 M-3（含被修改买单已成交的 1 份），另一笔有效买单 other 剩余 1 份，
// 被修改买单 target 总量 2、已成交 1、剩余 1、限价 1。
// 此时现金 7、占用 2（other 1 + target 1）、可用 5。
func setupModifyNearInt64Max(t *testing.T) (e *Engine, other, target int64) {
	t.Helper()
	M := int64(math.MaxInt64)
	e, err := NewEngine(M)
	if err != nil {
		t.Fatal(err)
	}
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	// 建仓：买入 M 全部成交，再卖出 4 份 @2 → 持仓 M-4、现金 8。
	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 4, 2)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 4}); err != nil {
		t.Fatal(err)
	}

	other = mustBuy(t, e, "A", 1, 1)  // 持仓 M-4 + 剩余 1 = M-3，接受
	target = mustBuy(t, e, "A", 2, 1) // 持仓 M-4 + 剩余 1+2 = M-1，接受
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 7 || e.ReservedCash() != 2 || e.AvailableCash() != 5 || e.Position("A") != M-3 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(target)
	if o.Qty != 2 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("被修改订单前置参数错误: %+v", o)
	}
	return e, other, target
}

// TestModifyBuyPositionSumOverflowNearInt64Max 复现题述核心场景：把总量 2（已成交 1）
// 的买单改为总量 4 后，新剩余量 3，合约合计占用 (M-3)+1+3 = M+1 无法用 int64 表示。
// 虽然每个输入数量与本笔现金占用（3×1）都能表示，修改仍必须整体拒绝并返回包装
// ErrInt64Overflow 的错误，不能因数值回绕而接受，也不能仅返回普通超限错误。
func TestModifyBuyPositionSumOverflowNearInt64Max(t *testing.T) {
	M := int64(math.MaxInt64)
	e, other, target := setupModifyNearInt64Max(t)

	before := len(e.Records())
	err := e.Modify(target, 4, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("合计 M+1 超出 int64 必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}

	// 只追加一条修改拒绝记录：保留订单编号、申请总量与限价、明确的溢出原因，
	// 以及当时的报价与持仓限额快照；不得产生修改成功或撤销记录。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	if rj.OrderID != target || rj.Qty != 4 || rj.Limit != 1 || rj.Symbol != "A" || rj.Side != Buy {
		t.Fatalf("拒绝记录必须保留订单编号与申请参数: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "int64") || strings.Contains(rj.Reason, "超过最大持仓量") {
		t.Fatalf("拒绝原因必须是明确的溢出原因而非普通超限: %q", rj.Reason)
	}
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 1 || rj.QuotePrice != 1 {
		t.Fatalf("拒绝记录缺少报价快照: %+v", rj)
	}
	if !rj.MaxPositionValid || rj.MaxPosition != M {
		t.Fatalf("拒绝记录缺少最大持仓量快照: %+v", rj)
	}
	if rj.AmtEnabled {
		t.Fatalf("未设置账户总持仓金额上限，拒绝记录不得携带金额快照: %+v", rj)
	}

	// 业务状态与申请前完全一致：不能先释放旧占用再留下不完整的结果。
	if e.Cash() != 7 || e.ReservedCash() != 2 || e.AvailableCash() != 5 || e.Position("A") != M-3 {
		t.Fatalf("溢出拒绝不得改变现金/占用/持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(target)
	if o.Qty != 2 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("被拒绝的修改不得改变订单参数与状态: %+v", o)
	}
	// 另一笔买单仍然有效：绝不能为腾额度撤销其他订单。
	if oo, _ := e.Order(other); oo.Status != StatusPending || oo.Qty != 1 || oo.Limit != 1 {
		t.Fatalf("其他有效买单不得受影响: %+v", oo)
	}
}

// TestModifyBuyPositionSumExactFitAtInt64Max 相同持仓与订单条件下，把总量改为 3：
// 新剩余量 2，合约合计 (M-3)+1+2 恰好为 M，必须成功替换占用并留下修改记录；
// 已成交的 1 份只计入持仓，不能再次计入未成交占用，历史成交金额保持不变。
func TestModifyBuyPositionSumExactFitAtInt64Max(t *testing.T) {
	M := int64(math.MaxInt64)
	e, other, target := setupModifyNearInt64Max(t)

	before := len(e.Records())
	mustModify(t, e, target, 3, 1)

	// 占用按新剩余量替换：other 1 + target 剩余 2 = 3 份（占用现金 3×1），
	// 已成交的 1 份只在持仓 M-3 中计一次；现金余额与持仓不被修改触碰。
	if e.Cash() != 7 || e.ReservedCash() != 3 || e.AvailableCash() != 4 || e.Position("A") != M-3 {
		t.Fatalf("修改后账目错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, _ := e.Order(target)
	if o.Qty != 3 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 2 {
		t.Fatalf("修改后订单参数错误: %+v", o)
	}
	if oo, _ := e.Order(other); oo.Status != StatusPending || oo.Remaining() != 1 {
		t.Fatalf("其他买单占用必须保留: %+v", oo)
	}

	// 恰好一条修改记录，保存前后参数、已成交量及当时报价与限额快照。
	if len(e.Records()) != before+1 {
		t.Fatal("真实修改必须恰好新增一条记录")
	}
	r := lastModifyRecord(t, e)
	if r.Kind != RecordModified || r.OrderID != target ||
		r.OldQty != 2 || r.OldLimit != 1 || r.OldFilled != 1 ||
		r.Qty != 3 || r.Limit != 1 || r.Filled != 1 || r.Remaining != 2 {
		t.Fatalf("修改记录前后参数错误: %+v", r)
	}
	if !r.QuoteValid || r.QuoteSeq != 1 || r.QuotePrice != 1 ||
		!r.MaxPositionValid || r.MaxPosition != M {
		t.Fatalf("修改记录必须固化当时报价与限额: %+v", r)
	}

	// 合计恰好用满额度 M：再申请 1 份即 M+1，按溢出拒绝，证明边界恰好为 M
	// （若已成交的 1 份被重复计入未成交占用，本次修改就不可能成功）。
	if _, err := e.Buy("A", 1, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("额度恰好用满后再买 1 份应溢出拒绝，实际 %v", err)
	}
	if e.Position("A") != M-3 || e.ReservedCash() != 3 {
		t.Fatalf("探测性拒绝不得改变状态: pos=%d reserved=%d", e.Position("A"), e.ReservedCash())
	}
}

// TestModifyPendingBuyPositionSumOverflow 待成交（尚未有任何成交）买单的同类溢出：
// 持仓 M-3、另一买单剩余 1、被修改买单总量 2 全部未成交；改为总量 3 后合计
// (M-3)+1+3 = M+1 溢出，必须整体拒绝且状态保持原值。
func TestModifyPendingBuyPositionSumOverflow(t *testing.T) {
	M := int64(math.MaxInt64)
	e, err := NewEngine(M)
	if err != nil {
		t.Fatal(err)
	}
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 3, 2)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 3}); err != nil {
		t.Fatal(err)
	}
	// 持仓 M-3、现金 6。
	other := mustBuy(t, e, "A", 1, 1)  // M-3+1 = M-2，接受
	target := mustBuy(t, e, "A", 2, 1) // M-3+1+2 = M 恰好，接受；占用现金合计 3
	if e.Cash() != 6 || e.ReservedCash() != 3 || e.Position("A") != M-3 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}

	// 待成交买单改为总量 3：新剩余量 3，合计 M+1 溢出（现金 3×1 本可支付）。
	before := len(e.Records())
	err = e.Modify(target, 3, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("待成交买单的同类数量溢出必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	recs := e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordRejected {
		t.Fatal("溢出只能追加一条拒绝记录")
	}
	rj := recs[len(recs)-1]
	if rj.OrderID != target || rj.Qty != 3 || rj.Limit != 1 || !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝记录要素错误: %+v", rj)
	}
	if e.Cash() != 6 || e.ReservedCash() != 3 || e.AvailableCash() != 3 || e.Position("A") != M-3 {
		t.Fatalf("溢出拒绝不得改变资金与持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(target); o.Qty != 2 || o.Limit != 1 || o.Filled != 0 || o.Status != StatusPending {
		t.Fatalf("被拒绝的修改不得改变订单: %+v", o)
	}
	if oo, _ := e.Order(other); oo.Status != StatusPending {
		t.Fatalf("不得撤销其他订单腾额度: %+v", oo)
	}
}

// TestModifyBuyPositionSumRepresentableOverLimit 合计可表示但超过较小持仓限额时，
// 仍走原有的普通超限拒绝（不包装 ErrInt64Overflow）；恰好用满额度允许。
func TestModifyBuyPositionSumRepresentableOverLimit(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 5)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	bid := mustBuy(t, e, "A", 1, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	other := mustBuy(t, e, "A", 1, 1)  // 持仓 1 + 剩余 1 = 2
	target := mustBuy(t, e, "A", 2, 1) // 持仓 1 + 剩余 1+2 = 4
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: target, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 持仓 2（含 target 已成交 1）、other 剩余 1、target 剩余 1，合计占用 4。

	// 改为总量 4：新剩余量 3，合计 2+1+3 = 6 > 5，可表示的普通超限。
	before := len(e.Records())
	err := e.Modify(target, 4, 1)
	if err == nil {
		t.Fatal("超过最大持仓量必须拒绝")
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("可表示的超限不得返回 ErrInt64Overflow: %v", err)
	}
	expectErr(t, err, "超过最大持仓量")
	if len(e.Records()) != before+1 || e.Records()[before].Kind != RecordRejected {
		t.Fatal("普通超限只追加一条拒绝记录")
	}
	if e.Cash() != 998 || e.ReservedCash() != 2 || e.Position("A") != 2 {
		t.Fatalf("普通超限拒绝不得改变状态: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(target); o.Qty != 2 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial {
		t.Fatalf("被拒绝的修改不得改变订单: %+v", o)
	}
	if oo, _ := e.Order(other); oo.Status != StatusPending {
		t.Fatalf("其他订单不得受影响: %+v", oo)
	}

	// 改为总量 3：新剩余量 2，合计 2+1+2 = 5 恰好用满额度，允许。
	mustModify(t, e, target, 3, 1)
	if e.ReservedCash() != 3 || e.Position("A") != 2 {
		t.Fatalf("恰好用满额度的修改应成功: reserved=%d pos=%d", e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(target); o.Qty != 3 || o.Remaining() != 2 {
		t.Fatalf("修改后订单参数错误: %+v", o)
	}
}
