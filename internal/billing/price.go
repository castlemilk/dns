package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// zeroDecimalCurrencies are the currencies Stripe quotes without minor units.
// Formatting one as if it had cents would show "¥6.00" for a ¥600 price.
var zeroDecimalCurrencies = map[string]struct{}{
	"bif": {}, "clp": {}, "djf": {}, "gnf": {}, "jpy": {}, "kmf": {}, "krw": {},
	"mga": {}, "pyg": {}, "rwf": {}, "ugx": {}, "vnd": {}, "vuv": {}, "xaf": {},
	"xof": {}, "xpf": {},
}

// currencySymbols covers the currencies this product prices in. Anything else
// is rendered as its ISO code, which is honest rather than wrong.
var currencySymbols = map[string]string{
	"usd": "$", "aud": "$", "cad": "$", "nzd": "$", "sgd": "$",
	"eur": "€", "gbp": "£", "jpy": "¥",
}

// formatPriceLabel renders the one price string the whole product shows:
// "$6 / domain / month". It is formatted here, server-side, because the price
// has exactly one source — the Stripe price the prober read — and a client-side
// format would be a second one waiting to disagree.
func formatPriceLabel(price Price) string {
	if price.UnitAmount <= 0 || price.Interval == "" {
		return ""
	}
	return fmt.Sprintf("%s / domain / %s", formatMoney(price.UnitAmount, price.Currency), price.Interval)
}

// formatMoney renders a minor-unit amount in its currency.
func formatMoney(amount int64, currency string) string {
	code := strings.ToLower(strings.TrimSpace(currency))
	symbol, known := currencySymbols[code]
	if _, zeroDecimal := zeroDecimalCurrencies[code]; zeroDecimal {
		if known {
			return fmt.Sprintf("%s%d", symbol, amount)
		}
		return fmt.Sprintf("%s %d", strings.ToUpper(code), amount)
	}
	whole := amount / 100
	cents := amount % 100
	digits := fmt.Sprintf("%d", whole)
	if cents != 0 {
		digits = fmt.Sprintf("%d.%02d", whole, cents)
	}
	if known {
		return symbol + digits
	}
	if code == "" {
		return digits
	}
	return strings.ToUpper(code) + " " + digits
}

// shortHash is a stable, non-reversible tag for an idempotency key. Stripe
// stores idempotency keys and shows them in the dashboard, so an email address
// must never be one.
func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}
