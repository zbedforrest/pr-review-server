package ensemble

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pr-review-server/pkg/reviewer/llm"
	"pr-review-server/pkg/reviewer/types"
)

// Completer runs one schema-enforced completion (llm.OpenRouterClient).
type Completer interface {
	CompleteStructured(ctx context.Context, req llm.StructuredRequest) (string, llm.Call, error)
}

// LLMAuthor asks a model to write the merged review from the members.
type LLMAuthor struct {
	Client        Completer
	Model         string
	ProviderOrder []string
	MaxTokens     int
}

// Write returns the model's merged draft. Sources in the draft are member ids.
func (a LLMAuthor) Write(ctx context.Context, clusters []Cluster, runSummaries []string) (Draft, llm.Call, error) {
	prompt, aliases := authorPrompt(clusters, runSummaries)
	maxTokens := a.MaxTokens
	if maxTokens == 0 {
		maxTokens = 16000
	}
	out, call, err := a.Client.CompleteStructured(ctx, llm.StructuredRequest{
		Model: a.Model, System: authorSystem, User: prompt,
		SchemaName: "merged_review", Schema: json.RawMessage(authorSchema),
		MaxTokens: maxTokens, ProviderOrder: a.ProviderOrder,
	})
	if err != nil {
		return Draft{}, call, err
	}
	d, err := parseAuthored(out, aliases)
	return d, call, err
}

const authorSystem = `You merge several independent code reviews of the same pull request into one review.
Each input finding has an id like S3. Rules:
- Findings in the same cluster usually describe the same issue. Merge them into one finding unless they describe different mechanisms; then keep them separate.
- Keep every issue. Every input id must appear in the sources of at least one output finding.
- Do not add issues, facts or code that no input finding states. Reword only to combine.
- Keep the most specific details: file, symbol, trigger, and user-visible impact.
- A finding's importance may not be higher than the highest importance among its sources.
- Give each output finding a key M1, M2, ... and list the keys of the findings that must be fixed first in summary.priority.`

const authorSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["findings", "summary"],
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["key", "sources", "file_path", "line_number", "importance", "comment_body"],
        "properties": {
          "key": {"type": "string"},
          "sources": {"type": "array", "items": {"type": "string"}},
          "file_path": {"type": "string"},
          "line_number": {"type": "integer"},
          "importance": {"type": "string", "enum": ["CRITICAL", "MEDIUM", "LOW"]},
          "comment_body": {"type": "string"}
        }
      }
    },
    "summary": {
      "type": "object",
      "additionalProperties": false,
      "required": ["verdict", "upshot", "priority"],
      "properties": {
        "verdict": {"type": "string", "enum": ["approve", "approve_suggestions", "request_changes"]},
        "upshot": {"type": "string"},
        "priority": {"type": "array", "items": {"type": "string"}}
      }
    }
  }
}`

// authorPrompt renders the clusters with short opaque ids (models shorten or
// rewrite structured ids like "r2:A-1") and returns the alias map back to
// member ids.
func authorPrompt(clusters []Cluster, runSummaries []string) (string, map[string]string) {
	aliases := map[string]string{}
	var b strings.Builder
	n := 0
	for _, c := range clusters {
		fmt.Fprintf(&b, "## Cluster %s (raised by %d of the reviews)\n", c.ID, c.Support())
		for _, m := range c.Members {
			n++
			alias := fmt.Sprintf("S%d", n)
			aliases[alias] = m.ID
			f := m.Finding
			imp := f.Importance
			if imp == "" {
				imp = "UNSPECIFIED"
			}
			fmt.Fprintf(&b, "### %s [%s] %s:%d\n%s\n\n", alias, imp, f.FilePath, f.LineNumber, strings.TrimSpace(f.CommentBody))
		}
	}
	if len(runSummaries) > 0 {
		b.WriteString("## Each review's own summary\n")
		for i, s := range runSummaries {
			if s = strings.TrimSpace(s); s != "" {
				fmt.Fprintf(&b, "- Review %d: %s\n", i+1, s)
			}
		}
	}
	return b.String(), aliases
}

type authored struct {
	Findings []struct {
		Key         string   `json:"key"`
		Sources     []string `json:"sources"`
		FilePath    string   `json:"file_path"`
		LineNumber  int      `json:"line_number"`
		Importance  string   `json:"importance"`
		CommentBody string   `json:"comment_body"`
	} `json:"findings"`
	Summary struct {
		Verdict  string   `json:"verdict"`
		Upshot   string   `json:"upshot"`
		Priority []string `json:"priority"`
	} `json:"summary"`
}

func parseAuthored(out string, aliases map[string]string) (Draft, error) {
	var a authored
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		return Draft{}, fmt.Errorf("ensemble: merge author reply is not the schema: %w", err)
	}
	d := Draft{Summary: &types.SummaryBlock{Verdict: a.Summary.Verdict, Upshot: a.Summary.Upshot, PriorityIDs: a.Summary.Priority}}
	for _, f := range a.Findings {
		var srcs []string
		for _, s := range f.Sources {
			if id, ok := aliases[strings.TrimSpace(s)]; ok {
				srcs = append(srcs, id)
			} else {
				srcs = append(srcs, s)
			}
		}
		d.Findings = append(d.Findings, types.LineComment{
			ID: f.Key, FilePath: f.FilePath, LineNumber: f.LineNumber,
			Importance: f.Importance, CommentBody: f.CommentBody, Sources: srcs,
		})
	}
	return d, nil
}

// MergeResult is a merged review plus how it was produced.
type MergeResult struct {
	Result
	Clusters int `json:"clusters"`
	// Method is "author" when the model wrote the merge, "deterministic"
	// when it was not configured or failed.
	Method    string   `json:"method"`
	AuthorErr string   `json:"author_error,omitempty"`
	Call      llm.Call `json:"author_call"`
}

// Merge merges the runs' findings: cluster, author (if given), then guard.
func Merge(ctx context.Context, runs [][]types.LineComment, author *LLMAuthor, opts Options) MergeResult {
	members := Members(runs)
	clusters := ClusterMembers(members, DefaultLineWindow)
	out := MergeResult{Clusters: len(clusters), Method: "deterministic"}
	draft := Deterministic(clusters)
	if author != nil && len(members) > 0 {
		d, call, err := author.Write(ctx, clusters, runSummaries(runs))
		out.Call = call
		if err != nil {
			out.AuthorErr = err.Error()
		} else {
			draft, out.Method = d, "author"
		}
	}
	out.Result = Guard(draft, members, opts)
	return out
}

func runSummaries(runs [][]types.LineComment) []string {
	var out []string
	for _, findings := range runs {
		s := ""
		for _, f := range findings {
			if !isSummary(f) {
				continue
			}
			if f.Summary != nil && f.Summary.Upshot != "" {
				s = f.Summary.Upshot
			} else {
				s = f.CommentBody
			}
		}
		out = append(out, s)
	}
	return out
}
