package servernode

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// RenderStatusPanel is the operator panel of `v2 status`: one screen, no secrets, no traffic to
// outside services unless the caller already asked for an update check.
func RenderStatusPanel(st StatusInfo, upd *UpdateInfo, updateErr error, lang string, width int, color bool) string {
	if width < 40 {
		width = 40
	}
	label := func(l string) string { return padRight(l, 11) }
	row := func(name, value string) string {
		w := width - 13
		if w < 10 {
			w = 10
		}
		lines := strings.Split(wrapText(value, w), "\n")
		out := " " + label(name) + lines[0]
		for _, rest := range lines[1:] {
			out += "\n " + label("") + rest
		}
		return out
	}

	version := st.Version
	if version == "" {
		version = BuildVersion
	}
	var rows []string
	rows = append(rows, row(Message(lang, "panel.version"), version))
	rows = append(rows, row(Message(lang, "panel.visibility"), visibilityText(st.Visibility, lang)))
	roles := strings.Join(st.Roles, ", ")
	if roles == "" {
		roles = Message(lang, "panel.none")
	}
	rows = append(rows, row(Message(lang, "panel.roles"), roles))

	carriers := strings.Join(st.Listen, ", ")
	if carriers == "" {
		carriers = Message(lang, "panel.none")
	}
	rows = append(rows, row(Message(lang, "panel.carriers"), carriers))

	sessions := strconv.Itoa(st.Sessions)
	if st.Limits.Sessions > 0 {
		sessions += " / " + strconv.Itoa(st.Limits.Sessions)
	}
	rows = append(rows, row(Message(lang, "panel.sessions"), sessions))

	rows = append(rows, row(Message(lang, "panel.traffic"),
		Message(lang, "panel.traffic-value", humanBytes(st.TrafficDayIn), humanBytes(st.TrafficDayOut),
			humanBytes(st.TrafficIn), humanBytes(st.TrafficOut))))

	cover := Message(lang, "panel.no-cover")
	if st.PublicCard || st.CoverCertNotAfter > 0 {
		if st.CoverCertNotAfter > 0 {
			days := int(timeUntilDays(st.CoverCertNotAfter))
			cover = Message(lang, "panel.cert", days)
		} else {
			cover = Message(lang, "panel.cover")
		}
	}
	rows = append(rows, row(Message(lang, "panel.cover"), cover))

	if st.Invites > 0 {
		rows = append(rows, row(Message(lang, "panel.invites"), strconv.Itoa(st.Invites)))
	}
	if st.PublicLink != "" {
		rows = append(rows, row(Message(lang, "panel.link"), st.PublicLink))
	}

	switch {
	case upd != nil:
		line := Message(lang, "panel.update.available", upd.Version)
		if upd.MinVersion != "" {
			line += Message(lang, "panel.update.min", upd.MinVersion)
		}
		rows = append(rows, row(Message(lang, "panel.update"), line))
	case updateErr != nil:
		rows = append(rows, row(Message(lang, "panel.update"), Message(lang, "panel.update.error", updateErr.Error())))
	default:
		rows = append(rows, row(Message(lang, "panel.update"), Message(lang, "panel.update.check")))
	}

	warnings := append([]string(nil), st.Warnings...)
	sort.Strings(warnings)
	for _, w := range warnings {
		rows = append(rows, row(Message(lang, "panel.warning"), w))
	}

	title := " EchoFog v2 · " + Message(lang, "panel.title") + " "
	body := strings.Join(rows, "\n")
	if !color {
		return title + "\n" + strings.Repeat("─", width) + "\n" + body
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.AdaptiveColor{Light: "25", Dark: "39"}).
		Width(width).
		Padding(0, 1)
	return box.Render(lipgloss.NewStyle().Bold(true).Render(strings.TrimSpace(title)) + "\n" + body)
}

func visibilityText(v, lang string) string {
	if v == "hidden" {
		return Message(lang, "panel.visibility.hidden")
	}
	return Message(lang, "panel.visibility.visible")
}

// timeUntilDays is the whole days left before a Unix time; zero or negative means expired.
func timeUntilDays(unix int64) int64 {
	return (unix - nowUnix()) / 86400
}

// nowUnix is a seam for the panel and the setup tests.
var nowUnix = func() int64 { return time.Now().Unix() }

func padRight(s string, n int) string {
	if len([]rune(s)) >= n {
		return s + " "
	}
	return s + strings.Repeat(" ", n-len([]rune(s)))
}

// humanBytes is a short decimal size for the panel.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
