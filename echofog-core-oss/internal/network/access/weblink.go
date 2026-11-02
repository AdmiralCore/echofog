package access

import "strings"

// Web links. Messengers do not make echofog:// links tappable, so a link to give people also has
// an https form: a static page (site/join in the repository) that carries the EchoFog link after
// "#" and opens it in the app, or offers to download the app first.
//
//	echofog://join?v=1&net=echofog&id=…  ⇄  https://admiralcore.github.io/echofog/join/#join?v=1&net=echofog&id=…
//
// What follows "#" is never sent to the server, and a messenger that previews the page does not
// see it, so a hidden node's secret stays between the people who have the link.

// WebLinkBase is the page the https form of a link points at. A fork with its own page sets it.
var WebLinkBase = "https://admiralcore.github.io/echofog/join/#"

const appScheme = "echofog://"

// WebLink is the https form of an echofog:// link; "" for anything else.
func WebLink(link string) string {
	link = strings.TrimSpace(link)
	if len(link) <= len(appScheme) || !strings.EqualFold(link[:len(appScheme)], appScheme) {
		return ""
	}
	return WebLinkBase + link[len(appScheme):]
}

// FromWebLink turns the https form of a link back into the echofog:// link it carries, from any
// page whose fragment is one ("#join?…", "#relay/…", "#group/…", "#contact/…", or a whole
// "#echofog://…"). Other text comes back as it is.
func FromWebLink(text string) string {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		if d := dropJoinSlash(t); d != t {
			return d
		}
		return text
	}
	i := strings.IndexByte(t, '#')
	if i < 0 {
		return text
	}
	frag := t[i+1:]
	lf := strings.ToLower(frag)
	switch {
	case strings.HasPrefix(lf, appScheme):
		return dropJoinSlash(frag)
	case strings.HasPrefix(lf, "join?"), strings.HasPrefix(lf, "join/?"), strings.HasPrefix(lf, "relay/"), strings.HasPrefix(lf, "group/"), strings.HasPrefix(lf, "contact/"):
		return dropJoinSlash(appScheme + frag)
	}
	return text
}

// dropJoinSlash takes off the "/" a browser or the OS may put before the "?" of an
// echofog://join?… link on its way to the app (echofog://join/?…): the link is the same.
func dropJoinSlash(link string) string {
	const slashed = appScheme + "join/?"
	if len(link) < len(slashed) || !strings.EqualFold(link[:len(slashed)], slashed) {
		return link
	}
	return link[:len(slashed)-2] + link[len(slashed)-1:]
}
