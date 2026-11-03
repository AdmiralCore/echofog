package objects

import (
	"net/url"
	"strings"
)

// MaxShopLink is the byte limit of one NodeCard.shop entry (objects.md §3.5).
const MaxShopLink = 200

// ValidateShopLink checks a shop link against Decision D17: only https:// URLs (t.me included) and
// echofog://contact?… links, at most 200 bytes. Anything else invalidates the card.
//
// Beyond D17 the check rejects what makes a link deceptive or unparsable: control characters,
// spaces and backslashes, user info ("https://t.me@evil.example"), and an empty host.
func ValidateShopLink(s string) error {
	if len(s) == 0 || len(s) > MaxShopLink {
		return invalid("shop link length %d", len(s))
	}
	if err := checkURLChars(s, "shop link"); err != nil {
		return err
	}
	switch {
	case strings.HasPrefix(s, "https://"):
		return validateHTTPS(s, "shop link")
	case strings.HasPrefix(s, "echofog://contact?"):
		if len(s) == len("echofog://contact?") {
			return invalid("shop link: empty contact link")
		}
		return nil
	}
	return invalid("shop link: scheme not allowed")
}

func checkURLChars(s, what string) error {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= 0x20 || c == 0x7f || c == '\\' {
			return invalid("%s: forbidden character 0x%02x", what, c)
		}
	}
	return nil
}

// validateHTTPS checks an https:// URL used in a signed object (shop links, release URLs).
func validateHTTPS(s, what string) error {
	if !strings.HasPrefix(s, "https://") {
		return invalid("%s: not https", what)
	}
	if err := checkURLChars(s, what); err != nil {
		return err
	}
	u, err := url.Parse(s)
	if err != nil {
		return invalid("%s: %v", what, err)
	}
	if u.Scheme != "https" || u.Opaque != "" {
		return invalid("%s: not https", what)
	}
	if u.User != nil {
		return invalid("%s: user info", what)
	}
	if u.Hostname() == "" {
		return invalid("%s: empty host", what)
	}
	return nil
}
