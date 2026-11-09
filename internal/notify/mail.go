package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/RedHiwiK/HiwiKInsight/internal/appstore"
	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/mailer"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
	"github.com/RedHiwiK/HiwiKInsight/internal/tz"
)

var t = i18n.T

type row struct{ K, V string }

type mailView struct {
	Subject    string
	AppName    string
	Label      string
	Sandbox    bool
	Accent     string
	Amount     string // "+$18.00"; empty without a transaction
	AmountNote string // conversion to the base currency and estimated proceeds
	Details    []row
	Source     []row // where the purchase started (appAccountToken attribution)
	Renewal    []row
	StatsToday string
	StatsMonth string
	Meta       []row
	L          labels
	P          any // mailer.Palette
}

// labels are the fixed strings of the HTML template, in the selected language.
type labels struct {
	Sandbox, Today, Month, Source, Details, Renewal, Notification, Footer string
}

type mailInput struct {
	AppKey       string
	AppName      string
	ProductName  func(id string) string
	ContextLabel func(context string) string
	Commission   float64
	Currency     string // base currency of Amount and Stats
	Payload      *appstore.NotificationPayload
	Tx           *appstore.TransactionInfo
	Renewal      *appstore.RenewalInfo
	Amount       *float64 // this transaction in the base currency
	Stats        *store.Stats
	Attribution  *store.Attribution // in-app context found through appAccountToken; nil when unattributed
	// TrialConversion: the first charge after a free trial, which Apple reports as a renewal.
	TrialConversion bool
}

func buildView(in mailInput) mailView {
	p, tx := in.Payload, in.Tx
	fullType := joinNonEmpty("/", p.NotificationType, p.Subtype)
	subtype := p.Subtype
	if in.TrialConversion {
		subtype = store.TrialConversionSubtype
	}
	pal := mailer.Palette
	v := mailView{
		AppName: in.AppName,
		Label:   TypeLabel(p.NotificationType, subtype),
		Sandbox: p.Data.Environment != "Production",
		Accent:  pal.Neutral,
		P:       pal,
		L: labels{
			Sandbox: t("Sandbox", "沙盒"), Today: t("Today", "今日"), Month: t("This month", "本月"),
			Source: t("Purchase source", "购买来源"), Details: t("Transaction", "交易详情"),
			Renewal: t("Subscription", "订阅状态"), Notification: t("Notification", "通知信息"),
			Footer: t("Sent in real time by HiwiKInsight from App Store Server Notifications",
				"由 HiwiKInsight 根据 App Store Server Notifications 实时发送"),
		},
	}
	sign := RevenueSign[p.NotificationType]
	// A zero-price start is not income: no income color, no "+$0", not counted as a sale
	free := sign == 1 && tx != nil && tx.Price == 0
	if free {
		sign = 0
	}
	switch sign {
	case 1:
		v.Accent = pal.Income
	case -1:
		v.Accent = pal.Refund
	}

	var subj []string
	if v.Sandbox {
		subj = append(subj, "["+v.L.Sandbox+"]")
	}
	subj = append(subj, in.AppName+" · "+v.Label)

	if tx != nil {
		product := in.ProductName(tx.ProductID)
		prefix := map[int]string{1: "+", -1: "-"}[sign]
		v.Amount = prefix + formatMilli(tx.Price, tx.Currency)
		if free {
			v.Amount = t("Free", "免费")
			if store.IsFreeTrial(p.NotificationType, tx.Price) {
				v.Amount = t("Free trial", "免费试用")
			}
		}

		var notes []string
		if tx.Currency != in.Currency && in.Amount != nil {
			notes = append(notes, "≈ "+i18n.Money(*in.Amount, in.Currency))
		}
		if sign == 1 && tx.Price > 0 {
			if in.Amount != nil {
				notes = append(notes, t("est. proceeds ≈ ", "预计到手 ≈ ")+i18n.Money(*in.Amount*(1-in.Commission), in.Currency))
			} else {
				notes = append(notes, t("est. proceeds ≈ ", "预计到手 ≈ ")+formatMilli(int64(float64(tx.Price)*(1-in.Commission)), tx.Currency))
			}
		}
		if len(notes) > 0 {
			v.AmountNote = strings.Join(notes, " · ")
			if sign == 1 && tx.Price > 0 {
				v.AmountNote += fmt.Sprintf(t(" (after %.0f%% commission, before tax)", "（扣 %.0f%% 佣金，未扣税）"), in.Commission*100)
			}
		}

		subj = append(subj, v.Amount, product)
		if tx.Storefront != "" {
			subj = append(subj, "("+StorefrontName(tx.Storefront)+")")
		}

		v.Details = append(v.Details,
			row{t("Product", "商品"), product},
			row{t("Product type", "商品类型"), i18n.Lookup(productTypeLabels, tx.Type)},
			row{t("Storefront", "地区"), StorefrontName(tx.Storefront)},
		)
		switch tx.TransactionReason {
		case "PURCHASE":
			v.Details = append(v.Details, row{t("Reason", "购买原因"), t("Purchased by the customer", "用户购买")})
		case "RENEWAL":
			v.Details = append(v.Details, row{t("Reason", "购买原因"), t("Auto-renewal", "自动续订")})
		}
		if tx.OfferType != 0 {
			offer := intLabel(offerTypeLabels, tx.OfferType)
			if _, ok := offerDiscountLabels[tx.OfferDiscountType]; ok {
				offer += " · " + i18n.Lookup(offerDiscountLabels, tx.OfferDiscountType)
			}
			if tx.OfferIdentifier != "" {
				offer += " · " + tx.OfferIdentifier
			}
			v.Details = append(v.Details, row{t("Offer", "优惠"), offer})
		}
		if tx.InAppOwnershipType == "FAMILY_SHARED" {
			v.Details = append(v.Details, row{t("Ownership", "来源"), t("Family Sharing", "家庭共享")})
		}
		v.Details = append(v.Details, row{t("Purchased at", "购买时间"), formatMillis(tx.PurchaseDate)})
		if tx.OriginalPurchaseDate > 0 && tx.OriginalPurchaseDate != tx.PurchaseDate {
			v.Details = append(v.Details, row{t("First purchased at", "首次购买"), formatMillis(tx.OriginalPurchaseDate)})
		}
		if tx.ExpiresDate > 0 {
			v.Details = append(v.Details, row{t("Expires at", "到期时间"), formatMillis(tx.ExpiresDate)})
		}
		if tx.RevocationDate > 0 {
			v.Details = append(v.Details, row{t("Refunded at", "退款时间"), formatMillis(tx.RevocationDate)})
		}
		if tx.RevocationReason != nil {
			reason := t("Other", "其他原因")
			if *tx.RevocationReason == 1 {
				reason = t("Issue with the app", "App 问题")
			}
			v.Details = append(v.Details, row{t("Refund reason", "退款原因"), reason})
		}
		v.Details = append(v.Details, row{t("Transaction ID", "交易号"), tx.TransactionID})
		if tx.OriginalTransactionID != tx.TransactionID {
			v.Details = append(v.Details, row{t("Original transaction ID", "原始交易号"), tx.OriginalTransactionID})
		}
	}
	if p.Data.ConsumptionRequestReason != "" {
		v.Details = append(v.Details, row{t("Refund request reason", "申请退款原因"), p.Data.ConsumptionRequestReason})
	}

	if tx != nil {
		v.Source = sourceRows(tx, in.Attribution, in.ContextLabel)
	}

	if r := in.Renewal; r != nil {
		status := t("On", "开启")
		if r.AutoRenewStatus == 0 {
			status = t("Off", "已关闭")
		}
		v.Renewal = append(v.Renewal, row{t("Auto-renew", "自动续订"), status})
		if tx != nil && r.AutoRenewProductID != "" && r.AutoRenewProductID != tx.ProductID {
			v.Renewal = append(v.Renewal, row{t("Next plan", "下期方案"), in.ProductName(r.AutoRenewProductID)})
		}
		if r.AutoRenewStatus == 1 && r.RenewalPrice > 0 {
			v.Renewal = append(v.Renewal, row{t("Next price", "下期价格"), formatMilli(r.RenewalPrice, r.Currency)})
		}
		if r.ExpirationIntent != 0 {
			v.Renewal = append(v.Renewal, row{t("Expiration reason", "过期原因"), intLabel(expirationIntentLabels, r.ExpirationIntent)})
		}
		if r.IsInBillingRetryPeriod {
			v.Renewal = append(v.Renewal, row{t("Billing retry", "扣款重试"), t("Apple is retrying the payment", "Apple 正在重试扣款")})
		}
		if r.GracePeriodExpiresDate > 0 {
			v.Renewal = append(v.Renewal, row{t("Grace period until", "宽限期至"), formatMillis(r.GracePeriodExpiresDate)})
		}
	}

	if s := in.Stats; s != nil {
		v.StatsToday = formatPeriod(s.Today, in.Currency)
		v.StatsMonth = formatPeriod(s.Month, in.Currency)
		if sign == 1 && s.Today.Sales > 0 {
			subj = append(subj, fmt.Sprintf(t("· sale #%d today", "· 今日第 %d 笔"), s.Today.Sales))
		}
	}

	v.Meta = []row{
		{"Bundle ID", p.Data.BundleID},
		{t("Type", "通知类型"), fullType},
		{t("Signed at", "通知时间"), formatMillis(p.SignedDate)},
		{t("Notification ID", "通知 ID"), p.NotificationUUID},
	}
	v.Subject = strings.Join(subj, " ")
	return v
}

// sourceRows describes where the purchase started: paywall context, install age,
// paywall views, app version and device.
func sourceRows(tx *appstore.TransactionInfo, a *store.Attribution, contextLabel func(string) string) []row {
	entry := t("Entry point", "入口")
	if a == nil {
		if tx.InAppOwnershipType == "FAMILY_SHARED" {
			return nil
		}
		note := t("Unattributed", "未归因")
		if tx.AppAccountToken == "" {
			note += t(" (older app version, restore, or purchased outside the app)", "（旧版本 App、恢复购买或在 App 外购买）")
		} else {
			note += t(" (no matching purchase.started event)", "（没有找到对应的 purchase.started）")
		}
		return []row{{entry, note}}
	}
	rows := []row{{entry, contextLabel(a.Context)}}
	if a.InstallDay > 0 {
		rows = append(rows, row{t("Install age", "安装天数"), fmt.Sprintf(t("day %d", "第 %d 天"), a.InstallDay)})
	}
	if a.PaywallViews > 0 {
		rows = append(rows, row{t("Paywall views", "付费墙曝光"), fmt.Sprintf(t("paywall view #%d", "第 %d 次看到付费墙"), a.PaywallViews)})
	}
	if a.AppVersion != "" {
		rows = append(rows, row{t("App version", "App 版本"), a.AppVersion})
	}
	if device := joinNonEmpty(" / ", a.Device, prefixed("iOS ", a.OSVersion)); device != "" {
		rows = append(rows, row{t("Device", "设备"), device})
	}
	rows = append(rows, row{t("User ID", "用户 ID"), a.InstallID})
	return rows
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func formatPeriod(p store.Period, currency string) string {
	s := fmt.Sprintf(t("%d sales", "%d 笔收入"), p.Sales)
	if p.Trials > 0 {
		s += fmt.Sprintf(t(", %d trial starts", "，%d 笔试用开通"), p.Trials)
	}
	if p.Refunds > 0 {
		s += fmt.Sprintf(t(", %d refunds", "，%d 笔退款"), p.Refunds)
	}
	s += t(", net ", "，净额 ") + i18n.Money(p.Net, currency)
	if p.Unconverted > 0 {
		s += fmt.Sprintf(t(" (%d without an exchange rate)", "（%d 笔外币未换算）"), p.Unconverted)
	}
	return s
}

// sendsToChat limits chat messages to charges, refunds and Apple's test notification,
// which confirms the setup. Everything else stays in email.
func sendsToChat(notificationType string) bool {
	return RevenueSign[notificationType] != 0 || notificationType == "TEST"
}

// renderChat is the body of a chat message; the subject already names the app, event,
// amount and product.
func renderChat(v mailView) string {
	var lines []string
	if v.AmountNote != "" {
		lines = append(lines, v.AmountNote)
	}
	if v.StatsToday != "" {
		lines = append(lines, v.L.Today+": "+v.StatsToday, v.L.Month+": "+v.StatsMonth)
	}
	return strings.Join(lines, "\n")
}

func renderText(v mailView) string {
	var b strings.Builder
	b.WriteString(v.AppName + " · " + v.Label)
	if v.Sandbox {
		b.WriteString(" (" + v.L.Sandbox + ")")
	}
	b.WriteString("\n")
	if v.Amount != "" {
		b.WriteString("\n" + v.Amount + "\n")
	}
	if v.AmountNote != "" {
		b.WriteString(v.AmountNote + "\n")
	}
	section := func(title string, rows []row) {
		if len(rows) == 0 {
			return
		}
		b.WriteString("\n[" + title + "]\n")
		for _, r := range rows {
			b.WriteString(r.K + ": " + r.V + "\n")
		}
	}
	section(v.L.Details, v.Details)
	section(v.L.Source, v.Source)
	section(v.L.Renewal, v.Renewal)
	if v.StatsToday != "" {
		section(t("Revenue", "收入统计"), []row{{v.L.Today, v.StatsToday}, {v.L.Month, v.StatsMonth}})
	}
	section(v.L.Notification, v.Meta)
	return b.String()
}

var htmlTmpl = template.Must(template.New("mail").Funcs(template.FuncMap{"dict": dict}).Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:24px 12px;background:{{.P.Page}};font-family:-apple-system,BlinkMacSystemFont,'PingFang SC','Helvetica Neue',Arial,sans-serif;color:{{.P.Text}};">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:520px;margin:0 auto;background:{{.P.Card}};border:1px solid {{.P.Border}};border-radius:14px;overflow:hidden;">
  <tr><td style="height:4px;background:{{.Accent}};"></td></tr>
  <tr><td style="padding:22px 24px 8px;">
    <div style="font-size:13px;color:{{.P.Muted}};">{{.AppName}}{{if .Sandbox}} <span style="display:inline-block;margin-left:6px;padding:1px 8px;border-radius:10px;background:{{.P.Badge}};color:{{.P.OnBadge}};font-size:11px;">{{.L.Sandbox}}</span>{{end}}</div>
    <div style="margin-top:4px;font-size:20px;font-weight:600;">{{.Label}}</div>
    {{if .Amount}}<div style="margin-top:14px;font-size:34px;font-weight:700;color:{{.Accent}};letter-spacing:-0.5px;">{{.Amount}}</div>{{end}}
    {{if .AmountNote}}<div style="margin-top:4px;font-size:13px;color:{{.P.Muted}};">{{.AmountNote}}</div>{{end}}
  </td></tr>
  {{if .StatsToday}}<tr><td style="padding:12px 24px 4px;">
    <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:{{.P.Page}};border-radius:10px;">
      <tr><td style="padding:10px 14px;font-size:13px;"><span style="color:{{.P.Muted}};">{{.L.Today}}</span>&nbsp; {{.StatsToday}}</td></tr>
      <tr><td style="padding:0 14px 10px;font-size:13px;"><span style="color:{{.P.Muted}};">{{.L.Month}}</span>&nbsp; {{.StatsMonth}}</td></tr>
    </table>
  </td></tr>{{end}}
  {{template "section" (dict "Title" .L.Source "Rows" .Source "P" .P)}}
  {{template "section" (dict "Title" .L.Details "Rows" .Details "P" .P)}}
  {{template "section" (dict "Title" .L.Renewal "Rows" .Renewal "P" .P)}}
  {{template "section" (dict "Title" .L.Notification "Rows" .Meta "P" .P "Small" true)}}
  <tr><td style="padding:8px 24px 20px;font-size:11px;color:{{.P.Muted}};">{{.L.Footer}}</td></tr>
</table>
</body></html>
{{define "section"}}{{if .Rows}}<tr><td style="padding:16px 24px 0;">
  <div style="font-size:12px;font-weight:600;color:{{.P.Muted}};margin-bottom:6px;">{{.Title}}</div>
  <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:{{if .Small}}12px{{else}}14px{{end}};">
  {{range .Rows}}<tr>
    <td style="padding:6px 0;border-top:1px solid {{$.P.Border}};color:{{$.P.Muted}};width:34%;vertical-align:top;">{{.K}}</td>
    <td style="padding:6px 0;border-top:1px solid {{$.P.Border}};word-break:break-all;">{{.V}}</td>
  </tr>{{end}}
  </table>
</td></tr>{{end}}{{end}}`))

// dict passes several values to a sub-template: dict "k1" v1 "k2" v2
func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func renderHTML(v mailView) (string, error) {
	var buf bytes.Buffer
	if err := htmlTmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func formatMilli(milli int64, currency string) string {
	if currency == "" {
		return ""
	}
	return i18n.Money(float64(milli)/1000, currency)
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(tz.Location()).Format("2006-01-02 15:04:05")
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, sep)
}
