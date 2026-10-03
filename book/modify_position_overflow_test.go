package book

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// setupModifyNearInt64Max 准备题述场景（报价连续、现金足够、未开日内亏损保护、
// 未设置账户总持仓金额上限）：记 M 为 int64 最大值，初始现金 M，合约 A 最大持仓量 M，
// 报价 seq1@1。建仓并部分卖出后：当前持仓 M-3；另一笔有效买单 other 剩余 1；
// 被修改订单 target 总量 2、已成交 1、剩余 1、限价 1。
// 此时现金 7、占用现金 2（other 1 + target 1）、可用现金 5，合约合计占用 M-1。
func setupModifyNearInt64Max(t *testing.T) (e *Engine, other, target int64) {
	t.Helper()
	M := int64(math.MaxInt64)
	e, _ = NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 4, 2)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	other = mustBuy(t, e, "A", 1, 1)
	target = mustBuy(t, e, "A", 2, 1)
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: target, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}

	if e.Cash() != 7 || e.ReservedCash() != 2 || e.AvailableCash() != 5 || e.Position("A") != M-3 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	o, ok := e.Order(target)
	if !ok || o.Qty != 2 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("被修改订单前置状态错误: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("另一笔买单前置状态错误: %+v", o)
	}
	return e, other, target
}

// TestModifyBuyPositionSumOverflowNearInt64Max 复现题述场景：把 target 总量改为 4、
// 限价保持 1 后，新剩余量为 3，合约合计占用 (M-3)+1+3 = M+1 无法用 int64 表示。
// 虽然每个输入数量（4、1）和本笔现金占用（3×1）都能表示，修改仍须整体拒绝并返回
// 可识别为 ErrInt64Overflow 的错误：不能因数值回绕而接受，也不能仅返回普通超限错误。
// 拒绝后订单、资金、持仓、其他买单与事件记录均保持申请前状态。
func TestModifyBuyPositionSumOverflowNearInt64Max(t *testing.T) {
	M := int64(math.MaxInt64)
	e, other, target := setupModifyNearInt64Max(t)

	before := len(e.Records())
	err := e.Modify(target, 4, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("合计 M+1 超出 int64 必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "int64") {
		t.Fatalf("错误须明确指出 int64 溢出而非普通超限: %v", err)
	}
	if strings.Contains(err.Error(), "超过最大持仓量") {
		t.Fatalf("不得仅返回普通超限错误: %v", err)
	}

	// 只追加一条修改拒绝记录：保留被修改订单编号、申请总量与限价、明确的溢出原因，
	// 以及当时的报价和持仓限额快照；不得产生修改成功或撤销记录。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	if rj.OrderID != target || rj.Qty != 4 || rj.Limit != 1 {
		t.Fatalf("拒绝记录须保留订单编号与申请总量/限价: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝原因须明确为 int64 溢出: %q", rj.Reason)
	}
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 1 || rj.QuotePrice != 1 {
		t.Fatalf("拒绝记录缺少报价快照: %+v", rj)
	}
	if !rj.MaxPositionValid || rj.MaxPosition != M {
		t.Fatalf("拒绝记录缺少最大持仓量快照: %+v", rj)
	}
	for _, r := range recs[before:] {
		if r.Kind == RecordModified || r.Kind == RecordCanceled {
			t.Fatalf("不得产生修改成功或撤销记录: %+v", r)
		}
	}

	// 被修改订单的总量、限价、已成交量和状态保持原值；另一笔买单仍然有效，
	// 不能为了腾出额度撤销其他订单。
	o, _ := e.Order(target)
	if o.Qty != 2 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 1 {
		t.Fatalf("溢出拒绝不得改变被修改订单: %+v", o)
	}
	if o, _ := e.Order(other); o.Status != StatusPending || o.Qty != 1 || o.Remaining() != 1 {
		t.Fatalf("溢出拒绝不得影响另一笔有效买单: %+v", o)
	}

	// 现金余额、占用现金、可用现金及合约持仓均与申请前一致：
	// 不能先释放旧占用再留下不完整的结果。
	if e.Cash() != 7 || e.ReservedCash() != 2 || e.AvailableCash() != 5 || e.Position("A") != M-3 {
		t.Fatalf("溢出拒绝不得改变资金与持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
}

// TestModifyBuyPositionSumExactlyInt64Max 在相同持仓和订单条件下，把 target 总量
// 改为 3：新剩余量为 2，合约合计占用 (M-3)+1+2 恰好为 M，应成功替换占用并留下
// 修改记录；已成交的 1 份只计入持仓，不能再次计入未成交占用，历史成交金额保持不变。
func TestModifyBuyPositionSumExactlyInt64Max(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _, target := setupModifyNearInt64Max(t)

	before := len(e.Records())
	mustModify(t, e, target, 3, 1)

	// 恰好用满额度：成功替换占用，只追加一条修改记录。
	recs := e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordModified {
		t.Fatalf("恰好用满额度的修改必须成功且只留一条修改记录，新增 %d 条", len(recs)-before)
	}
	r := recs[len(recs)-1]
	if r.OrderID != target || r.OldQty != 2 || r.OldLimit != 1 || r.OldFilled != 1 ||
		r.Qty != 3 || r.Limit != 1 || r.Filled != 1 || r.Remaining != 2 {
		t.Fatalf("修改记录前后参数错误: %+v", r)
	}
	if !r.QuoteValid || r.QuoteSeq != 1 || r.QuotePrice != 1 ||
		!r.MaxPositionValid || r.MaxPosition != M {
		t.Fatalf("修改记录必须固化当时报价与限额: %+v", r)
	}

	o, _ := e.Order(target)
	if o.Qty != 3 || o.Limit != 1 || o.Filled != 1 || o.Status != StatusPartial || o.Remaining() != 2 {
		t.Fatalf("修改后订单状态错误: %+v", o)
	}

	// 已成交的 1 份只计入持仓：占用现金只按未成交剩余量计（other 1 + target 2 = 3），
	// 历史成交金额保持不变（现金 7 不因修改重新计价）。
	if e.Cash() != 7 || e.ReservedCash() != 3 || e.AvailableCash() != 4 || e.Position("A") != M-3 {
		t.Fatalf("修改后资金与持仓错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}

	// 合计恰好为 M：再申请 1 份即 M+1，必须按溢出拒绝（证明占用恰好用满且已成交
	// 部分未被重复计入未成交占用，否则修改时的合计判断早已溢出）。
	if _, err := e.Buy("A", 1, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("合计恰好为 M 后再买 1 份应溢出拒绝，实际 %v", err)
	}

	// 剩余部分仍可按新参数成交：成交 2 份 @1 后持仓 M-1、现金 5，历史成交不重新计价。
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: target, Symbol: "A", Side: Buy, Price: 1, Qty: 2}); err != nil {
		t.Fatalf("修改后剩余部分应能按新参数成交: %v", err)
	}
	if e.Cash() != 5 || e.ReservedCash() != 1 || e.Position("A") != M-1 {
		t.Fatalf("按新参数成交后账目错误: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(target); o.Status != StatusFilled || o.Filled != 3 {
		t.Fatalf("target 应全部成交: %+v", o)
	}
}

// TestModifyPendingBuyPositionSumOverflow 覆盖待成交买单（无成交）的同类数量溢出：
// 把待成交的 other（总量 1、剩余 1）改为总量 4 后，合约合计占用
// (M-3)+1(target 剩余)+4 = M+2 超出 int64，必须整体拒绝且保持全部状态。
func TestModifyPendingBuyPositionSumOverflow(t *testing.T) {
	M := int64(math.MaxInt64)
	e, other, target := setupModifyNearInt64Max(t)

	before := len(e.Records())
	err := e.Modify(other, 4, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("待成交买单合计 M+2 超出 int64 必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}

	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected || rj.OrderID != other || rj.Qty != 4 || rj.Limit != 1 ||
		!strings.Contains(rj.Reason, "int64") {
		t.Fatalf("拒绝记录要素错误: %+v", rj)
	}
	if !rj.QuoteValid || rj.QuotePrice != 1 || !rj.MaxPositionValid || rj.MaxPosition != M {
		t.Fatalf("拒绝记录缺少报价与限额快照: %+v", rj)
	}

	if o, _ := e.Order(other); o.Qty != 1 || o.Limit != 1 || o.Filled != 0 ||
		o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("溢出拒绝不得改变待成交订单: %+v", o)
	}
	if o, _ := e.Order(target); o.Qty != 2 || o.Filled != 1 || o.Status != StatusPartial {
		t.Fatalf("溢出拒绝不得影响部分成交订单: %+v", o)
	}
	if e.Cash() != 7 || e.ReservedCash() != 2 || e.AvailableCash() != 5 || e.Position("A") != M-3 {
		t.Fatalf("溢出拒绝不得改变资金与持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
}

// TestModifyPositionLimitRepresentableExcessKeepsOrdinaryRejection 保留合计可表示但
// 超过较小持仓限额时的普通拒绝：合计 6 可用 int64 表示但超过限额 5，须按原有
// “超过最大持仓量”原因拒绝，不得返回 ErrInt64Overflow；恰好等于限额仍允许。
func TestModifyPositionLimitRepresentableExcessKeepsOrdinaryRejection(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "B", 5)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	b1 := mustBuy(t, e, "B", 2, 10)
	b2 := mustBuy(t, e, "B", 1, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: b1, Symbol: "B", Side: Buy, Price: 10, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 持仓 1，b1 剩余 1，b2 剩余 1，合计占用 3。

	before := len(e.Records())
	err := e.Modify(b1, 5, 10) // 新剩余 4：1+1+4=6 可表示但超过限额 5
	if err == nil {
		t.Fatal("可表示但超限的修改必须拒绝")
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("合计可表示的普通超限不得返回 ErrInt64Overflow: %v", err)
	}
	expectErr(t, err, "超过最大持仓量")

	recs := e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordRejected {
		t.Fatal("普通超限只能追加一条拒绝记录")
	}
	if rj := recs[len(recs)-1]; !strings.Contains(rj.Reason, "超过最大持仓量") ||
		rj.OrderID != b1 || rj.Qty != 5 || rj.Limit != 10 {
		t.Fatalf("普通超限拒绝记录要素错误: %+v", rj)
	}
	if o, _ := e.Order(b1); o.Qty != 2 || o.Limit != 10 || o.Filled != 1 || o.Status != StatusPartial {
		t.Fatalf("普通超限拒绝不得改变订单: %+v", o)
	}
	if o, _ := e.Order(b2); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("普通超限拒绝不得影响其他订单: %+v", o)
	}

	// 恰好用满较小限额仍允许：新剩余 3，1+1+3=5。
	mustModify(t, e, b1, 4, 10)
	if o, _ := e.Order(b1); o.Qty != 4 || o.Remaining() != 3 {
		t.Fatalf("恰好用满限额应成功: %+v", o)
	}
}
