package dashboard

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr/nip19"
)

func viewFuncs(version string) template.FuncMap {
	return template.FuncMap{
		"asset": func(path string) string {
			return "/static/" + path + "?v=" + version
		},
		"icon": func(name string) template.HTML {
			return template.HTML(`<svg class="i" aria-hidden="true"><use href="#i-` + template.HTMLEscapeString(name) + `"/></svg>`)
		},
		"truncate": func(n int, s string) string {
			runes := []rune(s)
			if len(runes) <= n {
				return s
			}
			return string(runes[:n]) + "…"
		},
		"ago":    ago,
		"date":   func(t time.Time) string { return t.UTC().Format("2006-01-02") },
		"stamp":  func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
		"clock":  func(t time.Time) string { return t.UTC().Format("15:04:05") },
		"num":    num,
		"short":  short,
		"size":   formatSize,
		"npub":   npub,
		"lines":  lines,
		"letter": letter,
		"join":   strings.Join,
		"add":    func(a, b int) int { return a + b },
		"appsURL": func(q, filter string) string {
			return appsHref(q, filter, 0)
		},
		"statsURL": statsHref,
	}
}

// ago is a coarse relative time: "just now", "5m ago", "3h ago", "12d ago",
// then the date.
func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.UTC().Format("2006-01-02")
	}
}

// num groups digits by thousands.
func num(v any) string {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// short keeps the head and tail of a long hash or key.
func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:8] + "…" + s[len(s)-6:]
}

// npub encodes a hex pubkey, or returns it unchanged when it is not one.
func npub(hex string) string {
	n, err := nip19.EncodePublicKey(hex)
	if err != nil {
		return hex
	}
	return n
}

// lines splits text into trimmed, non-empty lines.
func lines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// letter is the first letter of a name, for the tile behind a missing icon.
func letter(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func formatSize(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
