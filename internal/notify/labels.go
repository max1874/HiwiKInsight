package notify

import (
	"fmt"

	"github.com/RedHiwiK/HiwiKInsight/internal/i18n"
	"github.com/RedHiwiK/HiwiKInsight/internal/store"
)

type pair = i18n.Pair

// Labels of App Store notification types; "TYPE/SUBTYPE" is looked up before "TYPE".
var typeLabels = map[string]pair{
	"ONE_TIME_CHARGE":        {"Purchase", "新购买"},
	"SUBSCRIBED":             {"New subscription", "新订阅"},
	"SUBSCRIBED/RESUBSCRIBE": {"Resubscribed", "重新订阅"},
	"DID_RENEW":              {"Renewal", "续订"},
	"DID_RENEW/" + store.TrialConversionSubtype:     {"Trial converted", "试用转付费"},
	"DID_FAIL_TO_RENEW":                             {"Renewal payment failed", "续订扣款失败"},
	"DID_CHANGE_RENEWAL_STATUS/AUTO_RENEW_DISABLED": {"Auto-renew turned off", "关闭自动续订"},
	"DID_CHANGE_RENEWAL_STATUS/AUTO_RENEW_ENABLED":  {"Auto-renew turned on", "开启自动续订"},
	"DID_CHANGE_RENEWAL_PREF":                       {"Plan changed", "更改订阅方案"},
	"EXPIRED":                                       {"Subscription expired", "订阅过期"},
	"GRACE_PERIOD_EXPIRED":                          {"Grace period ended", "宽限期结束"},
	"OFFER_REDEEMED":                                {"Offer redeemed", "兑换优惠"},
	"REFUND":                                        {"Refund", "退款"},
	"REFUND_REVERSED":                               {"Refund reversed", "退款撤销"},
	"REFUND_DECLINED":                               {"Refund declined", "退款申请被拒"},
	"CONSUMPTION_REQUEST":                           {"Refund requested", "用户申请退款"},
	"REVOKE":                                        {"Family Sharing revoked", "家庭共享撤销"},
	"TEST":                                          {"Test notification", "测试通知"},
}

// TypeLabel returns the display label of a notification type and subtype.
func TypeLabel(typ, subtype string) string {
	if subtype != "" {
		if p, ok := typeLabels[typ+"/"+subtype]; ok {
			return p.String()
		}
	}
	return i18n.Lookup(typeLabels, typ)
}

// RevenueSign is +1 for income, -1 for refunds and 0 for everything else (not counted as money).
var RevenueSign = map[string]int{
	"ONE_TIME_CHARGE": 1,
	"SUBSCRIBED":      1,
	"DID_RENEW":       1,
	"REFUND_REVERSED": 1,
	"REFUND":          -1,
}

var productTypeLabels = map[string]pair{
	"Auto-Renewable Subscription": {"Auto-renewable subscription", "自动续期订阅"},
	"Non-Consumable":              {"Non-consumable (lifetime)", "非消耗型（买断）"},
	"Consumable":                  {"Consumable", "消耗型"},
	"Non-Renewing Subscription":   {"Non-renewing subscription", "非续期订阅"},
}

var storefrontNames = map[string]pair{
	"CHN": {"China mainland", "中国大陆"}, "HKG": {"Hong Kong", "中国香港"}, "MAC": {"Macao", "中国澳门"},
	"TWN": {"Taiwan", "中国台湾"}, "USA": {"United States", "美国"}, "CAN": {"Canada", "加拿大"},
	"GBR": {"United Kingdom", "英国"}, "DEU": {"Germany", "德国"}, "FRA": {"France", "法国"},
	"JPN": {"Japan", "日本"}, "KOR": {"South Korea", "韩国"}, "SGP": {"Singapore", "新加坡"},
	"MYS": {"Malaysia", "马来西亚"}, "AUS": {"Australia", "澳大利亚"}, "NZL": {"New Zealand", "新西兰"},
	"THA": {"Thailand", "泰国"}, "VNM": {"Vietnam", "越南"}, "IDN": {"Indonesia", "印度尼西亚"},
	"PHL": {"Philippines", "菲律宾"}, "IND": {"India", "印度"}, "BRA": {"Brazil", "巴西"},
	"ESP": {"Spain", "西班牙"}, "ITA": {"Italy", "意大利"}, "NLD": {"Netherlands", "荷兰"},
}

// StorefrontName returns the name of an App Store storefront (ISO alpha-3), or the code.
func StorefrontName(code string) string { return i18n.Lookup(storefrontNames, code) }

var offerTypeLabels = map[int]pair{
	1: {"Introductory offer", "首购优惠"}, 2: {"Promotional offer", "促销优惠"},
	3: {"Offer code", "优惠码"}, 4: {"Win-back offer", "挽回优惠"},
}

var offerDiscountLabels = map[string]pair{
	"FREE_TRIAL": {"free trial", "免费试用"}, "PAY_AS_YOU_GO": {"pay as you go", "按期优惠价"},
	"PAY_UP_FRONT": {"pay up front", "预付优惠价"}, "ONE_TIME": {"one-time", "一次性优惠"},
}

var expirationIntentLabels = map[int]pair{
	1: {"Cancelled by the customer", "用户主动取消"}, 2: {"Billing error", "扣款失败"},
	3: {"Did not consent to a price increase", "不同意涨价"}, 4: {"Product unavailable", "商品已下架"},
	5: {"Other", "其他原因"},
}

func intLabel(m map[int]pair, k int) string {
	if p, ok := m[k]; ok {
		return p.String()
	}
	return fmt.Sprint(k)
}
