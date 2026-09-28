package history

import (
	"fmt"
	"net/url"
	"strings"
)

// webSource is one source link an answer engine gave for its reply.
type webSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// maxReplySources is how many sources a web agent's reply lists; the rest
// are counted.
const maxReplySources = 10

// sourcesFooter is the "Sources:" block a web agent's reply ends its
// answer text with: one "- <title> <url>" line per http(s) source, in the
// site's order, each URL once, at most maxReplySources and then an
// "(and N more)" line. It is "" when there is no source. Titles are cut
// to one short line.
func sourcesFooter(sources []webSource) string {
	var lines []string
	seen := map[string]bool{}
	more := 0
	for _, s := range sources {
		u, err := url.Parse(strings.TrimSpace(s.URL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			continue
		}
		link := u.String()
		if seen[link] {
			continue
		}
		seen[link] = true
		if len(lines) == maxReplySources {
			more++
			continue
		}
		title := capRunes(oneLineText(s.Title), 120)
		if title == "" {
			title = u.Hostname()
		}
		lines = append(lines, "- "+title+" "+link)
	}
	if len(lines) == 0 {
		return ""
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("(and %d more)", more))
	}
	return "Sources:\n" + strings.Join(lines, "\n")
}
