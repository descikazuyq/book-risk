package book

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestBuyPositionSumOverflowNearInt64Max 复现题述场景：初始现金 M、合约最大持仓量 M、
// 报价 1；买入 M 份全部成交后卖出 1 份（持仓 M-1、现金 2）。再申请买入 2 份时
// 已持仓 + 有效买单剩余 + 本次 = M+1 无法用 int64 表示，必须整笔拒绝而不是回绕接受；
// 申请 1 份则合计恰好为 M，可以接受并成交。
func TestBuyPositionSumOverflowNearInt64Max(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	bid := mustBuy(t, e, "A", M, 1) // id=1
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 0 || e.Position("A") != M {
		t.Fatalf("全额成交后状态错误: cash=%d pos=%d", e.Cash(), e.Position("A"))
	}
	sid := mustSell(t, e, "A", 1, 2) // id=2
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 2 || e.Position("A") != M-1 || e.ReservedCash() != 0 {
		t.Fatalf("卖出 1 份后前置状态错误: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}

	// 现金足够（2），但持仓合计 M+1 超出 int64：整笔拒绝。
	before := len(e.Records())
	id, err := e.Buy("A", 2, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("合计超出 int64 必须返回包装 ErrInt64Overflow 的错误，实际 %v", err)
	}
	if id != 0 {
		t.Fatalf("溢出拒绝必须返回零订单编号，实际 %d", id)
	}
	expectErr(t, err, "委托被拒绝")

	// 只追加一条拒绝记录，业务状态全部保持申请前的值。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected || rj.Symbol != "A" || rj.Side != Buy ||
		rj.Qty != 2 || rj.Limit != 1 || rj.OrderID != 0 {
		t.Fatalf("拒绝记录要素错误: %+v", rj)
	}
	// 原因须说明申请数量、已持仓数量、有效买单剩余量与最大持仓量，且不得出现回绕值。
	reason := rj.Reason
	for _, v := range []int64{2, M - 1, 0, M} {
		if !strings.Contains(reason, strconv.FormatInt(v, 10)) {
			t.Fatalf("拒绝原因 %q 必须包含数值 %d（申请/已持仓/买单剩余/最大持仓）", reason, v)
		}
	}
	if strings.Contains(reason, strconv.FormatInt(-1, 10)) ||
		strings.Contains(reason, strconv.FormatInt(math.MinInt64, 10)) {
		t.Fatalf("拒绝原因不得把回绕后的数值当作真实合计: %q", reason)
	}
	// 保留申请合约、限价及已有报价与限额快照。
	if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuoteMoment != 1 || rj.QuotePrice != 1 {
		t.Fatalf("拒绝记录缺少报价快照: %+v", rj)
	}
	if !rj.MaxPositionValid || rj.MaxPosition != M {
		t.Fatalf("拒绝记录缺少最大持仓量快照: %+v", rj)
	}

	if e.Cash() != 2 || e.ReservedCash() != 0 || e.AvailableCash() != 2 ||
		e.Position("A") != M-1 {
		t.Fatalf("溢出拒绝不得改变现金/占用/持仓: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if len(e.Orders()) != 2 {
		t.Fatalf("被拒绝的申请不得生成订单，实际 %d 笔", len(e.Orders()))
	}

	// 被拒绝的申请不消耗订单编号：下一笔买单编号仍为 3。
	ok := mustBuy(t, e, "A", 1, 1)
	if ok != 3 {
		t.Fatalf("拒绝不得消耗订单编号，新单应为 3，实际 %d", ok)
	}
	// 合计恰好等于限额 M：接受并成交后持仓恰好为 M。
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: ok, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != M || e.Cash() != 1 || e.ReservedCash() != 0 {
		t.Fatalf("边界成交后状态错误: cash=%d pos=%d reserved=%d",
			e.Cash(), e.Position("A"), e.ReservedCash())
	}
}

// TestBuyPositionSumOverflowCountsOnlyActiveBuyRemaining 校验计入额度的口径：
// 部分成交订单只计未成交部分；已撤销或全部成交的订单不再占用；卖单成交前不抵减持仓。
func TestBuyPositionSumOverflowCountsOnlyActiveBuyRemaining(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)

	// 建仓：买入 M 全部成交，再卖出 3 份 @2 → 持仓 M-3、现金 6。
	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 3, 2)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 3}); err != nil {
		t.Fatal(err)
	}

	// 买单 2 股，部分成交 1 股：只剩余 1 股计入额度。
	partial := mustBuy(t, e, "A", 2, 1) // 持仓 M-3 + 剩余 2 = M-1，接受
	if _, err := e.Fill(Trade{TradeID: 3, OrderID: partial, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 此时持仓 M-2、有效买单剩余 1。申请 2 股：M-2+1+2 = M+1 溢出，拒绝。
	if _, err := e.Buy("A", 2, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("部分成交订单应只计剩余量并致合计溢出，实际 %v", err)
	}
	// 申请 1 股：M-2+1+1 = M 恰好允许（不因已成交那 1 股重复计数）。
	edge := mustBuy(t, e, "A", 1, 1)

	// 卖单在真正成交前不能抵减持仓：挂一笔卖单不改变买单额度判断。
	pendingSell := mustSell(t, e, "A", 1, 2)
	if e.Sellable("A") != M-3 { // 持仓 M-2 减去 1 份未成交卖单占用
		t.Fatalf("卖单占用可卖数量: sellable=%d", e.Sellable("A"))
	}
	// 再申请 1 股：仍按 M-2 + 剩余(1+1) + 1 = M+1 溢出拒绝，卖单未提前抵减持仓。
	if _, err := e.Buy("A", 1, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("未成交卖单不得抵减持仓额度，实际 %v", err)
	}
	if err := e.Cancel(pendingSell); err != nil {
		t.Fatal(err)
	}

	// 成交 edge 1 股：持仓 M-1、partial 仍剩 1。
	if _, err := e.Fill(Trade{TradeID: 4, OrderID: edge, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 再成交 partial 的剩余 1 股：全部成交订单不再占用，持仓恰好 M。
	if _, err := e.Fill(Trade{TradeID: 5, OrderID: partial, Symbol: "A", Side: Buy, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Position("A") != M {
		t.Fatalf("全部成交后持仓应为 M，实际 %d", e.Position("A"))
	}
	if o, _ := e.Order(partial); o.Status != StatusFilled {
		t.Fatalf("partial 应全部成交: %+v", o)
	}
	// 持仓已为 M 且限额也是 M：申请 1 股的合计 M+1 本身溢出，按溢出拒绝
	// （可表示但超限的场景在下方小额引擎中覆盖）。
	_, err := e.Buy("A", 1, 1)
	if !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("限额为 M 时 M+1 无法表示，必须按溢出拒绝，实际 %v", err)
	}
	expectErr(t, err, "委托被拒绝")
	if e.Position("A") != M {
		t.Fatalf("拒绝不得改变持仓: %d", e.Position("A"))
	}

	// 已撤销订单不占额度：卖出 1 份腾出 1 股后，先挂再撤的买单不影响后续申请。
	if e.Cash() != 3 { // 建卖 3@2 得 6，此后三次买单成交 @1 共扣 3
		t.Fatalf("现金余额意外: %d", e.Cash())
	}
	rs := mustSell(t, e, "A", 1, 1)
	if _, err := e.Fill(Trade{TradeID: 6, OrderID: rs, Symbol: "A", Side: Sell, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if e.Cash() != 4 {
		t.Fatalf("卖出 1 份后现金应为 4，实际 %d", e.Cash())
	}
	// 持仓 M-1。挂 1 股买单（占满额度）再撤销。
	c := mustBuy(t, e, "A", 1, 1)
	if err := e.Cancel(c); err != nil {
		t.Fatal(err)
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("撤销买单后现金占用应清零: %d", e.ReservedCash())
	}
	// 撤销后额度释放：再申请 1 股合计恰好 M，接受；申请 2 股溢出拒绝。
	last := mustBuy(t, e, "A", 1, 1)
	if _, err := e.Buy("A", 2, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("撤销单不占额度，申请 2 股应因 M-1+2=M+1 溢出拒绝，实际 %v", err)
	}
	// 在挂的 last 不受影响。
	if o, _ := e.Order(last); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("拒绝不得影响其他有效订单: %+v", o)
	}
}

// TestBuyPositionSumOverflowKeepsOrdinaryRejections 校验其他既有规则不受影响：
// 现金不足优先于溢出判断；合计可表示但超限仍走原有原因；恰好等于限额允许。
func TestBuyPositionSumOverflowKeepsOrdinaryRejections(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	// 以 1 卖出 1 份：持仓 M-1、现金仅 1。
	sid := mustSell(t, e, "A", 1, 1)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 1, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	// 申请 2 股：现金不足（需要 2，可用 1）优先，报普通错误，不包装 ErrInt64Overflow。
	before := len(e.Records())
	_, err := e.Buy("A", 2, 1)
	if err == nil {
		t.Fatal("现金不足必须拒绝")
	}
	if errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("现金不足应走原有拒绝，不得返回 ErrInt64Overflow: %v", err)
	}
	expectErr(t, err, "现金不足")
	if len(e.Records()) != before+1 || e.Records()[before].Reason == "" {
		t.Fatal("现金不足只追加一条带原因的拒绝记录")
	}
	if e.Position("A") != M-1 || e.ReservedCash() != 0 {
		t.Fatalf("拒绝不得改变状态: pos=%d reserved=%d", e.Position("A"), e.ReservedCash())
	}

	// 小额场景：合计可表示但超过限额，仍是原有超限原因。
	e2, _ := NewEngine(1000)
	mustSetMax(t, e2, "B", 5)
	mustQuote(t, e2, "B", Quote{Seq: 1, Moment: 1, Price: 10}, 1)
	mustBuy(t, e2, "B", 4, 10)
	_, err = e2.Buy("B", 2, 10)
	if err == nil || errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("可表示的超限必须按普通原因拒绝，实际 %v", err)
	}
	expectErr(t, err, "超过最大持仓量")
	mustBuy(t, e2, "B", 1, 10) // 恰好 5，允许
}

// TestBuyPositionSumOverflowWithAmountLimit 校验金额上限开启时溢出判断依旧生效，
// 且拒绝不改变金额上限与任何占用。
func TestBuyPositionSumOverflowWithAmountLimit(t *testing.T) {
	M := int64(math.MaxInt64)
	e, _ := NewEngine(M)
	mustSetMax(t, e, "A", M)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 1, Price: 1}, 1)
	bid := mustBuy(t, e, "A", M, 1)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 1, Qty: M}); err != nil {
		t.Fatal(err)
	}
	sid := mustSell(t, e, "A", 1, 2)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: sid, Symbol: "A", Side: Sell, Price: 2, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(M); err != nil {
		t.Fatal(err)
	}
	before := len(e.Records())
	if _, err := e.Buy("A", 2, 1); !errors.Is(err, ErrInt64Overflow) {
		t.Fatalf("开启金额上限后持仓合计溢出仍必须拒绝，实际 %v", err)
	}
	if len(e.Records()) != before+1 {
		t.Fatal("溢出只能追加一条拒绝记录")
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != M {
		t.Fatalf("拒绝不得改变金额上限设置: %+v", st)
	}
	if e.Cash() != 2 || e.ReservedCash() != 0 || e.Position("A") != M-1 {
		t.Fatalf("拒绝不得改变资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
}
