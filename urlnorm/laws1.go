// Law-list data, version 1. These tables are the versioned artifact of
// doc 03 section 7: editing them is a law change and bumps LawVersion,
// which the corpus-stats suite then quantifies as a fold delta.
package urlnorm

import "strings"

// trackingExact are law 3's stripped tracking parameters: click and
// campaign identifiers that never change the response body.
var trackingExact = map[string]bool{
	"gclid":       true, // Google Ads
	"gclsrc":      true,
	"dclid":       true, // DoubleClick
	"gbraid":      true, // Google Ads, iOS attribution
	"wbraid":      true,
	"srsltid":     true, // Google Merchant
	"fbclid":      true, // Facebook
	"igshid":      true, // Instagram
	"msclkid":     true, // Microsoft Ads
	"twclid":      true, // Twitter/X
	"ttclid":      true, // TikTok
	"li_fat_id":   true, // LinkedIn
	"yclid":       true, // Yandex
	"mc_cid":      true, // Mailchimp campaign
	"mc_eid":      true, // Mailchimp member
	"vero_id":     true,
	"oly_anon_id": true, // Omeda
	"oly_enc_id":  true,
	"_ga":         true, // Google Analytics cross-domain
	"_gl":         true,
	"s_cid":       true, // Adobe Analytics
}

// trackingPrefix are stripped by prefix match.
var trackingPrefix = []string{
	"utm_", // utm_source, utm_medium, utm_campaign, utm_term, utm_content, and kin
}

// sessionExact are law 4's phpsessid-family parameters: server session
// ids that make one page look like millions of URLs.
var sessionExact = map[string]bool{
	"phpsessid":  true,
	"sessionid":  true,
	"session_id": true,
	"sess_id":    true,
	"cfid":       true, // ColdFusion
	"cftoken":    true,
	"zenid":      true, // Zen Cart
	"oscsid":     true, // osCommerce
}

// sessionPrefix catches IIS's ASPSESSIONIDxxxxxxxx pattern.
var sessionPrefix = []string{
	"aspsessionid",
}

// strippedParam reports whether a lowercased query key falls under law
// 3 or law 4.
func strippedParam(key string) bool {
	if trackingExact[key] || sessionExact[key] {
		return true
	}
	for _, p := range trackingPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	for _, p := range sessionPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}
