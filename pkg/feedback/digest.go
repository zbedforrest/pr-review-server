package feedback

import (
	"fmt"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/health"
)

// Digest is a window of stored feedback, ready to render.
type Digest struct {
	Days        int               `json:"days"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
	Counts      map[string]int    `json:"counts"`
	Items       []db.FeedbackItem `json:"items"`
}

func NewDigest(items []db.FeedbackItem, start, end time.Time, days int) Digest {
	d := Digest{Days: days, WindowStart: start, WindowEnd: end, Counts: map[string]int{}, Items: items}
	for _, l := range Labels {
		d.Counts[string(l)] = 0
	}
	for _, it := range items {
		d.Counts[it.Label]++
	}
	if d.Items == nil {
		d.Items = []db.FeedbackItem{}
	}
	return d
}

// HealthMetrics is the digest as the daily report line wants it: counts,
// every frustrated item, and the newest happy one.
func (d Digest) HealthMetrics(scanned bool, note string) health.FeedbackMetrics {
	m := health.FeedbackMetrics{Scanned: scanned, Note: note, ByLabel: d.Counts, Frustrated: []health.FeedbackQuote{}}
	for _, it := range d.Items {
		q := health.FeedbackQuote{Label: it.Label, Author: it.Author, Quote: quoteOf(it), URL: it.URL}
		switch {
		case Label(it.Label).IsFrustrated():
			m.Frustrated = append(m.Frustrated, q)
		case it.Label == string(Happy) && m.Happy == nil:
			m.Happy = &q
		}
	}
	return m
}

func quoteOf(it db.FeedbackItem) string {
	if it.Source == SourceReaction {
		return "reacted " + it.Reaction
	}
	return Quote(it.Body)
}

// Markdown renders the full list for people.
func (d Digest) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# PRism author feedback: last %d day", d.Days)
	if d.Days != 1 {
		b.WriteString("s")
	}
	fmt.Fprintf(&b, "\n\nWindow %s to %s UTC. %d happy, %d neutral, %d frustrated, %d very frustrated.\n",
		d.WindowStart.UTC().Format("2006-01-02 15:04"), d.WindowEnd.UTC().Format("2006-01-02 15:04"),
		d.Counts[string(Happy)], d.Counts[string(Neutral)], d.Counts[string(Frustrated)], d.Counts[string(VeryFrustrated)])
	if len(d.Items) == 0 {
		b.WriteString("\nNo author feedback in the window.\n")
		return b.String()
	}
	for _, l := range []Label{VeryFrustrated, Frustrated, Happy, Neutral} {
		var lines []string
		for _, it := range d.Items {
			if it.Label != string(l) {
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s @%s on %s/%s#%d (%s): \"%s\" %s",
				it.CreatedAt.UTC().Format("01-02 15:04"), it.Author, it.RepoOwner, it.RepoName, it.PRNumber, it.Source, quoteOf(it), it.URL))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n\n%s\n", strings.ReplaceAll(string(l), "_", " "), len(lines), strings.Join(lines, "\n"))
	}
	return b.String()
}
