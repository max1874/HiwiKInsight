package asc

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

// announceDays limits sales messages to recent reports: a daily report usually arrives the
// next morning, and a server that was down for a few days still reports what it missed.
const announceDays = 7

// salesMessage is the chat message for one day's report: the purchases and refunds of apps
// that have no App Store server notification URL, whose sales are otherwise only seen here.
// names holds app names by bundle ID and product names by product ID. ok is false when
// there is nothing to say.
func salesMessage(period string, recs []store.SalesRecord, names map[string]string, live map[string]bool, currency string) (subject, body string, ok bool) {
	t := i18n.T
	var lines []string
	sales, refunds := 0, 0
	for _, r := range recs {
		if (r.Category != "iap" && r.Category != "subscription") || r.Units == 0 || r.ProceedsPerUnit == 0 || live[r.BundleID] {
			continue
		}
		var amount string
		if r.Proceeds != nil {
			amount = i18n.Money(*r.Proceeds, currency)
		} else {
			amount = i18n.Money(r.Units*r.ProceedsPerUnit, r.ProceedsCurrency)
		}
		line := fmt.Sprintf(t("%s · %s · %s · proceeds %s (customer paid %s)", "%s · %s · %s · 到手 %s（顾客付 %s）"),
			names[r.BundleID], cmp.Or(names[r.SKU], r.Title), r.Country, amount, i18n.Money(r.CustomerPrice, r.CustomerCurrency))
		if n := int(r.Units); n > 0 {
			sales += n
		} else {
			refunds -= n
			line = t("Refund", "退款") + " · " + line
		}
		if r.Units > 1 || r.Units < -1 {
			line += fmt.Sprintf(" ×%d", int(r.Units))
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", "", false
	}
	subject = fmt.Sprintf(t("Sales report %s · %d sales", "销售报表 %s · %d 笔收入"), period, sales)
	if refunds > 0 {
		subject += fmt.Sprintf(t(", %d refunds", "，%d 笔退款"), refunds)
	}
	return subject, strings.Join(lines, "\n"), true
}
