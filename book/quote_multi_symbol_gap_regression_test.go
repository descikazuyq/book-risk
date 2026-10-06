package book

import (
	"strings"
	"testing"
)

// 本文件为“多个合约同时出现行情缺口时的报价补齐”补充回归保障：调用方交错
// 提交 A、B 两个合约的模拟报价，序号分别从 1 开始，连续性、等待与缺口判断
// 都只在各自合约内进行；两边使用相同序号、不同价格与模拟时刻互不影响。
// 补齐一个合约只推进该合约的行情，另一合约的最新报价、缺口与等待中的报价
// 保持原样；模拟时刻不代替报价序号判断是否补齐。接受与拒绝记录各自保存
// 所属合约当时已生效报价的序号、时刻与价格，不混用另一合约的报价，也不
// 记录尚在等待的报价。全程不开启日内亏损保护、不设置账户总持仓金额上限，
// 报价等待或补齐生效本身不结算成交：现金余额、买单现金占用、持仓数量与
// 既有订单状态均保持原值，新买单被接受后才按原有限价规则增加占用。
//
// 共用前置状态（multiGapSetup）：
// 初始现金 10000；A、B 持仓限额均充足；A seq1（时刻100,价格10）、
// B seq1（时刻200,价格20）已生效。A 买单 5 股限价 10 已成交 2 股 @10
// （现金 9980、持仓 2、剩余 3 股占用 30），B 买单 4 股限价 20 未成交
// （占用 80）；买单现金占用合计 110。
func multiGapSetup(t *testing.T) (e *Engine, a1, b1 int64, recsBase int) {
	t.Helper()
	e, _ = NewEngine(10000)
	mustSetMax(t, e, "A", 1000)
	mustSetMax(t, e, "B", 1000)

	// 相同序号 1、不同模拟时刻与价格，两个合约各自正常生效。
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 100, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 200, Price: 20}, 1)

	// 此前已接受的未成交订单与已入账持仓：后续报价等待/补齐不得改变它们。
	a1 = mustBuy(t, e, "A", 5, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: a1, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	b1 = mustBuy(t, e, "B", 4, 20)

	if e.Cash() != 9980 || e.ReservedCash() != 110 || e.AvailableCash() != 9870 ||
		e.Position("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("前置资金/持仓错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(a1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("前置 A 买单应为部分成交、剩余 3 股: %+v", o)
	}
	if o, _ := e.Order(b1); o.Status != StatusPending || o.Remaining() != 4 {
		t.Fatalf("前置 B 买单应为待成交、剩余 4 股: %+v", o)
	}
	return e, a1, b1, len(e.Records())
}

// assertMultiGapBooks 校验现金、占用、持仓与既有订单状态保持前置值；
// 报价等待或补齐生效本身不结算成交，这些账实不得变化。reserved 为当时
// 应有的买单现金占用（新买单被接受后才会按限价规则增加）。
func assertMultiGapBooks(t *testing.T, e *Engine, a1, b1 int64, reserved int64, msg string) {
	t.Helper()
	if e.Cash() != 9980 || e.ReservedCash() != reserved || e.AvailableCash() != 9980-reserved ||
		e.Position("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("%s不得改变资金与持仓: cash=%d reserved=%d posA=%d posB=%d",
			msg, e.Cash(), e.ReservedCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(a1); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 3 {
		t.Fatalf("%s不得改变 A 既有部分成交订单: %+v", msg, o)
	}
	if o, _ := e.Order(b1); o.Status != StatusPending || o.Remaining() != 4 {
		t.Fatalf("%s不得改变 B 既有待成交订单: %+v", msg, o)
	}
}

// assertQuoteSnap 校验事件记录保存的报价快照确为所属合约当时已生效的报价。
func assertQuoteSnap(t *testing.T, r Record, seq, moment, price int64, msg string) {
	t.Helper()
	if !r.QuoteValid || r.QuoteSeq != seq || r.QuoteMoment != moment || r.QuotePrice != price {
		t.Fatalf("%s的报价快照应为序号 %d（时刻 %d，价格 %d）: %+v", msg, seq, moment, price, r)
	}
}

// TestMultiSymbolGapBackfillPerSymbolChain 覆盖主场景：A、B 各有序号 1 的
// 有效报价后交错收到跨号报价（A 收序号 3、5，B 收序号 3），都只进入等待；
// 补交 A 序号 2 只推进 A 到序号 3（序号 5 仍等待、仍有缺口、新买单仍被拒）；
// 再补 A 序号 4 使序号 4、5 一起生效并解除缺口，A 新买单接受、B 新买单仍因
// 本合约缺口被拒；最后补交 B 序号 2 使 B 的序号 2、3 一起生效，B 解除缺口，
// A 的行情不受影响。
func TestMultiSymbolGapBackfillPerSymbolChain(t *testing.T) {
	e, a1, b1, recsBase := multiGapSetup(t)

	// 第一阶段：交错提交跨号报价。两边都缺序号 2；相同序号 3 在不同合约
	// 使用不同价格与模拟时刻，各自进入等待，提交都返回新生效条数 0。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 120, Price: 12}, 0)
	mustQuote(t, e, "B", Quote{Seq: 3, Moment: 220, Price: 2}, 0)
	mustQuote(t, e, "A", Quote{Seq: 5, Moment: 140, Price: 14}, 0)

	// 最新报价仍停在各自的序号 1；两边都报告缺口。等待报价的模拟时刻
	// （如 B 序号 3 的时刻 220 晚于 A 全部时刻）不代替序号判断补齐。
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 1, Moment: 100, Price: 10}) {
		t.Fatalf("A 最新报价必须停在 seq1（时刻100,价格10）: %+v ok=%v", q, ok)
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 1, Moment: 200, Price: 20}) {
		t.Fatalf("B 最新报价必须停在 seq1（时刻200,价格20）: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") || !e.HasGap("B") {
		t.Fatalf("两边缺少序号 2 都必须报告缺口: gapA=%v gapB=%v", e.HasGap("A"), e.HasGap("B"))
	}
	// 报价进入等待不产生任何事件记录，也不结算成交。
	if len(e.Records()) != recsBase {
		t.Fatalf("报价进入等待不得新增事件记录: 基础 %d，现有 %d", recsBase, len(e.Records()))
	}
	assertMultiGapBooks(t, e, a1, b1, 110, "报价进入等待")

	// 缺口期间 A 新买单被拒绝；拒绝记录保存 A 当时已生效的 seq1，
	// 不得记录尚在等待的 seq3/seq5，也不得混用 B 的报价。
	if _, err := e.Buy("A", 1, 10); err == nil ||
		!strings.Contains(err.Error(), "报价存在缺口") || !strings.Contains(err.Error(), "合约 A") {
		t.Fatalf("A 缺口期间新买单必须被拒绝并说明本合约缺口: %v", err)
	}
	recs := e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("拒绝新买单只新增一条拒绝记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	rj := recs[recsBase]
	if rj.Kind != RecordRejected || rj.Symbol != "A" || rj.Side != Buy ||
		!strings.Contains(rj.Reason, "报价存在缺口") {
		t.Fatalf("新增记录必须是 A 买单的缺口拒绝记录: %+v", rj)
	}
	assertQuoteSnap(t, rj, 1, 100, 10, "A 缺口拒绝记录")
	recsBase = len(recs)

	// 第二阶段：补交 A 的序号 2（时刻110,价格11）。序号 2、3 依次生效，
	// 返回新生效条数 2；A 最新报价推进到序号 3（时刻120,价格12）。
	// 序号 5 仍在等待（缺序号 4），A 仍报告缺口——等待中的序号 5 时刻 140
	// 更大也不顶替补齐判断。B 的行情、缺口与等待中的序号 3 保持原样。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 110, Price: 11})
	if err != nil || n != 2 {
		t.Fatalf("补交 A seq2 应成功并生效 2 条（seq2、seq3）: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 3, Moment: 120, Price: 12}) {
		t.Fatalf("A 最新报价应推进到 seq3（时刻120,价格12）: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("A 序号 5 仍在等待，缺口必须保持")
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 1, Moment: 200, Price: 20}) {
		t.Fatalf("补齐 A 不得推进 B 的行情: %+v ok=%v", q, ok)
	}
	if !e.HasGap("B") {
		t.Fatal("补齐 A 不得消除 B 的缺口")
	}
	// 补齐生效本身不结算成交、不新增事件记录。
	if len(e.Records()) != recsBase {
		t.Fatalf("报价补齐生效不得新增事件记录: 基础 %d，现有 %d", recsBase, len(e.Records()))
	}
	assertMultiGapBooks(t, e, a1, b1, 110, "A 补齐 seq2/seq3")

	// A 仍缺序号 4，新买单仍因缺口被拒绝；拒绝记录保存 A 当时已生效的
	// seq3（时刻120,价格12），不得记录等待中的 seq5 或 B 的报价。
	if _, err := e.Buy("A", 1, 12); err == nil ||
		!strings.Contains(err.Error(), "报价存在缺口") || !strings.Contains(err.Error(), "等待序号 4") {
		t.Fatalf("A 仍缺序号 4，新买单必须仍被缺口拒绝: %v", err)
	}
	recs = e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("缺口拒绝只新增一条记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	rj = recs[recsBase]
	if rj.Kind != RecordRejected || rj.Symbol != "A" || rj.Side != Buy {
		t.Fatalf("新增记录必须是 A 买单的拒绝记录: %+v", rj)
	}
	assertQuoteSnap(t, rj, 3, 120, 12, "A 二次缺口拒绝记录")
	recsBase = len(recs)

	// 第三阶段：补交 A 的序号 4（时刻130,价格13）。序号 4 与原先等待的
	// 序号 5 一起生效，返回 2；A 最新报价成为序号 5（时刻140,价格14），
	// 缺口解除。B 保持原样。
	n, err = e.UpdateQuote("A", Quote{Seq: 4, Moment: 130, Price: 13})
	if err != nil || n != 2 {
		t.Fatalf("补交 A seq4 应成功并生效 2 条（seq4、seq5）: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 5, Moment: 140, Price: 14}) {
		t.Fatalf("A 最新报价应推进到 seq5（时刻140,价格14）: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("A 序号 4、5 生效后缺口必须解除")
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 1, Moment: 200, Price: 20}) {
		t.Fatalf("补齐 A 不得推进 B 的行情: %+v ok=%v", q, ok)
	}
	if !e.HasGap("B") {
		t.Fatal("B 的缺口必须保持")
	}
	if len(e.Records()) != recsBase {
		t.Fatalf("报价补齐生效不得新增事件记录: 基础 %d，现有 %d", recsBase, len(e.Records()))
	}
	assertMultiGapBooks(t, e, a1, b1, 110, "A 补齐 seq4/seq5")

	// 现金与合约持仓限额充足，A 的新买单可以接受；接受记录保存 A 当时
	// 已生效的 seq5（时刻140,价格14）。接受后才按限价规则增加占用 2×14=28。
	a2 := mustBuy(t, e, "A", 2, 14)
	recs = e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("接受新买单只新增一条接受记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	ar := recs[recsBase]
	if ar.Kind != RecordAccepted || ar.Symbol != "A" || ar.Side != Buy ||
		ar.OrderID != a2 || ar.Qty != 2 || ar.Limit != 14 {
		t.Fatalf("新增记录必须是 A 新买单的接受记录: %+v", ar)
	}
	assertQuoteSnap(t, ar, 5, 140, 14, "A 接受记录")
	recsBase = len(recs)
	assertMultiGapBooks(t, e, a1, b1, 138, "A 新买单接受")

	// B 的新买单仍因本合约缺口被拒绝；拒绝记录保存 B 自己已生效的
	// seq1（时刻200,价格20），不得混用 A 的 seq5，也不得记录 B 等待中的
	// seq3（时刻220,价格2）。
	if _, err := e.Buy("B", 1, 20); err == nil ||
		!strings.Contains(err.Error(), "报价存在缺口") || !strings.Contains(err.Error(), "合约 B") {
		t.Fatalf("B 缺口期间新买单必须被拒绝并说明本合约缺口: %v", err)
	}
	recs = e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("B 缺口拒绝只新增一条记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	rj = recs[recsBase]
	if rj.Kind != RecordRejected || rj.Symbol != "B" || rj.Side != Buy {
		t.Fatalf("新增记录必须是 B 买单的拒绝记录: %+v", rj)
	}
	assertQuoteSnap(t, rj, 1, 200, 20, "B 缺口拒绝记录")
	recsBase = len(recs)
	assertMultiGapBooks(t, e, a1, b1, 138, "B 新买单被拒")

	// 第四阶段：向 B 补交序号 2（时刻210,价格20）。B 的序号 2 与最初等待的
	// 序号 3 一起生效，一次生效 2 条；B 最新报价成为最初等待的序号 3
	// （时刻220,价格2），缺口解除。A 的报价与缺口不受影响。
	n, err = e.UpdateQuote("B", Quote{Seq: 2, Moment: 210, Price: 20})
	if err != nil || n != 2 {
		t.Fatalf("补交 B seq2 应成功并生效 2 条（seq2、seq3）: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 3, Moment: 220, Price: 2}) {
		t.Fatalf("B 最新报价应是最初等待的 seq3（时刻220,价格2）: %+v ok=%v", q, ok)
	}
	if e.HasGap("B") {
		t.Fatal("B 补齐后缺口必须解除")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 5, Moment: 140, Price: 14}) {
		t.Fatalf("补齐 B 不得影响 A 的行情: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐 B 不得改变 A 的缺口状态")
	}
	if len(e.Records()) != recsBase {
		t.Fatalf("B 补齐生效不得新增事件记录: 基础 %d，现有 %d", recsBase, len(e.Records()))
	}
	assertMultiGapBooks(t, e, a1, b1, 138, "B 补齐 seq2/seq3")

	// 全程未启用日内亏损保护与账户总持仓金额上限。
	if risk := e.RiskStatus(); risk.Open || risk != (RiskStatus{}) {
		t.Fatalf("日内亏损保护必须保持未开启: %+v", risk)
	}
	if am := e.PositionAmountStatus(); am.Enabled || am != (PositionAmountStatus{}) {
		t.Fatalf("账户总持仓金额上限必须保持未启用: %+v", am)
	}

	// B 缺口解除后新买单可以接受了；接受记录保存 B 当时已生效的
	// seq3（时刻220,价格2），不混用 A 的报价。
	b2 := mustBuy(t, e, "B", 1, 2)
	recs = e.Records()
	if len(recs) != recsBase+1 {
		t.Fatalf("B 新买单接受只新增一条记录: 基础 %d，现有 %d", recsBase, len(recs))
	}
	ar = recs[recsBase]
	if ar.Kind != RecordAccepted || ar.Symbol != "B" || ar.OrderID != b2 || ar.Qty != 1 || ar.Limit != 2 {
		t.Fatalf("新增记录必须是 B 新买单的接受记录: %+v", ar)
	}
	assertQuoteSnap(t, ar, 3, 220, 2, "B 接受记录")
	if e.ReservedCash() != 140 {
		t.Fatalf("B 新买单接受后按限价规则增加占用 1×2: reserved=%d", e.ReservedCash())
	}
	if e.Cash() != 9980 || e.Position("A") != 2 || e.Position("B") != 0 {
		t.Fatalf("接受只增加占用，现金余额与持仓不变: cash=%d posA=%d posB=%d",
			e.Cash(), e.Position("A"), e.Position("B"))
	}
}

// TestMultiSymbolQuoteSeqIndependentPerSymbol 单独锁定“序号按合约各自从 1
// 开始”：交错提交相同序号、不同价格与模拟时刻的报价，两边都正常接收并各自
// 推进；一个合约的连续提交对另一合约的最新报价与缺口判断毫无影响。
func TestMultiSymbolQuoteSeqIndependentPerSymbol(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)

	// 相同序号、不同价格与模拟时刻，交错提交，各自生效 1 条。
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 100, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 900, Price: 20}, 1)
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 200, Price: 11}, 1)
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 800, Price: 19}, 1)

	// 模拟时刻不按合约间比较：B 的时刻整体更大也不影响各自按序号推进。
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 2, Moment: 200, Price: 11}) {
		t.Fatalf("A 最新报价应为自己的 seq2: %+v ok=%v", q, ok)
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 2, Moment: 800, Price: 19}) {
		t.Fatalf("B 最新报价应为自己的 seq2: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") || e.HasGap("B") {
		t.Fatalf("各自序号连续，不得报告缺口: gapA=%v gapB=%v", e.HasGap("A"), e.HasGap("B"))
	}

	// A 的跨号报价只进入 A 的等待，不影响 B 的连续接收。
	mustQuote(t, e, "A", Quote{Seq: 4, Moment: 400, Price: 13}, 0)
	if !e.HasGap("A") || e.HasGap("B") {
		t.Fatalf("只有 A 应报告缺口: gapA=%v gapB=%v", e.HasGap("A"), e.HasGap("B"))
	}
	mustQuote(t, e, "B", Quote{Seq: 3, Moment: 700, Price: 18}, 1)
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 3, Moment: 700, Price: 18}) {
		t.Fatalf("B 应继续按自己的序号推进到 seq3: %+v ok=%v", q, ok)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 2, Moment: 200, Price: 11}) {
		t.Fatalf("B 的报价不得推进 A 的行情: %+v ok=%v", q, ok)
	}

	// 补交 A 的序号 3：A 的序号 3、4 一起生效，B 保持原样。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 300, Price: 12}, 2)
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 4, Moment: 400, Price: 13}) {
		t.Fatalf("A 补齐后应推进到 seq4: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("A 补齐后缺口必须解除")
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 3, Moment: 700, Price: 18}) {
		t.Fatalf("补齐 A 不得影响 B 的行情: %+v ok=%v", q, ok)
	}
}
