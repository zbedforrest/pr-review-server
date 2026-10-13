package feedback

import (
	"fmt"
	"strings"
	"time"

	"pr-review-server/db"
	"pr-review-server/pkg/health"
)

// DigestItem is one stored item as the endpoint and the report show it: the
// short quote, never the whole comment.
type DigestItem struct {
	Source     string    `json:"source"`
	Author     string    `json:"author"`
	RepoOwner  string    `json:"repo_owner"`
	RepoName   string    `json:"repo_name"`
	PRNumber   int       `json:"pr_number"`
	Label      string    `json:"label"`
	Classifier string    `json:"classifier"`
	ReplyClass string    `json:"reply_class,omitempty"`
	Reaction   string    `json:"reaction,omitempty"`
	Quote      string    `json:"quote"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
}

// Digest is a window of stored feedback, ready to render.
type Digest struct {
	Days        int            `json:"days"`
	WindowStart time.Time      `json:"window_start"`
	WindowEnd   time.Time      `json:"window_end"`
	Counts      map[string]int `json:"counts"`
	Items       []DigestItem   `json:"items"`
}

func NewDigest(rows []db.FeedbackItem, start, end time.Time, days int) Digest {
	d := Digest{Days: days, WindowStart: start, WindowEnd: end, Counts: map[string]int{}, Items: make([]DigestItem, 0, len(rows))}
	for _, l := range Labels {
		d.Counts[string(l)] = 0
	}
	for _, r := range rows {
		d.Counts[r.Label]++
		d.Items = append(d.Items, DigestItem{
			Source: r.Source, Author: r.Author, RepoOwner: r.RepoOwner, RepoName: r.RepoName, PRNumber: r.PRNumber,
			Label: r.Label, Classifier: r.Classifier, ReplyClass: r.ReplyClass, Reaction: r.Reaction,
			Quote: quoteOf(r), URL: r.URL, CreatedAt: r.CreatedAt,
		})
	}
	return d
}

// HealthMetrics is the digest as the daily report line wants it: counts,
// every frustrated item, and the newest happy one.
func (d Digest) HealthMetrics(scanned bool, note string) health.FeedbackMetrics {
	m := health.FeedbackMetrics{Scanned: scanned, Note: note, ByLabel: d.Counts, Frustrated: []health.FeedbackQuote{}}
	for _, it := range d.Items {
		q := health.FeedbackQuote{Label: it.Label, Author: it.Author, Quote: it.Quote, URL: it.URL}
		switch {
		case Label(it.Label).IsFrustrated():
			m.Frustrated = append(m.Frustrated, q)
		case it.Label == string(Happy) && m.Happy == nil:
			m.Happy = &q
		}
	}
	return m
}

func quoteOf(r db.FeedbackItem) string {
	if r.Source == SourceReaction {
		return "reacted " + r.Reaction
	}
	return Quote(r.Body)
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
				it.CreatedAt.UTC().Format("01-02 15:04"), it.Author, it.RepoOwner, it.RepoName, it.PRNumber, it.Source, it.Quote, it.URL))
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n\n%s\n", strings.ReplaceAll(string(l), "_", " "), len(lines), strings.Join(lines, "\n"))
	}
	return b.String()
}
