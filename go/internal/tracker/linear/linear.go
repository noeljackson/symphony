// Package linear implements the [tracker.Tracker] interface against the
// Linear GraphQL API (SPEC v3 §5.3.1.A).
//
// One Client instance owns one project's tracker view. Auth is via API key
// in the `Authorization` header; pagination is single-page (Linear's default
// 50 nodes) which is sufficient for typical projects. Larger projects can
// raise the page size via Config.PageSize.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/noeljackson/symphony/go/internal/issue"
)

// Config configures a Linear Client.
type Config struct {
	Endpoint       string
	APIKey         string
	ProjectSlug    string
	ActiveStates   []string
	TerminalStates []string
	HTTPClient     *http.Client // optional; defaults to a 30s-timeout client
	PageSize       int          // optional; defaults to 50
}

// Client is a Linear GraphQL Tracker implementation.
type Client struct {
	cfg    Config
	http   *http.Client
	pageSz int
}

// New constructs a Client. Returns an error when Config has missing
// required fields; the error matches the SPEC §6.3 ConfigError shape so
// callers can surface it consistently.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("linear.New: APIKey is required")
	}
	if strings.TrimSpace(cfg.ProjectSlug) == "" {
		return nil, fmt.Errorf("linear.New: ProjectSlug is required")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	cfg.Endpoint = endpoint
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	pg := cfg.PageSize
	if pg <= 0 {
		pg = 50
	}
	return &Client{cfg: cfg, http: hc, pageSz: pg}, nil
}

// FetchCandidateIssues implements [tracker.Tracker]. Returns issues in the
// configured project whose state is in ActiveStates.
func (c *Client) FetchCandidateIssues(ctx context.Context) ([]issue.Issue, error) {
	all, err := c.fetchProjectIssues(ctx)
	if err != nil {
		return nil, err
	}
	return filterByStates(all, c.cfg.ActiveStates), nil
}

// FetchIssueStatesByIDs implements [tracker.Tracker]. Linear identifies
// issues by UUID; this query asks for the named issues directly.
func (c *Client) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]issue.Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	all, err := c.fetchProjectIssues(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]issue.Issue, 0, len(ids))
	for _, i := range all {
		if _, ok := want[i.ID]; ok {
			out = append(out, i)
		}
	}
	return out, nil
}

// FetchIssuesByStates implements [tracker.Tracker]. Used at startup for
// terminal-workspace cleanup (§8.6).
func (c *Client) FetchIssuesByStates(ctx context.Context, states []string) ([]issue.Issue, error) {
	all, err := c.fetchProjectIssues(ctx)
	if err != nil {
		return nil, err
	}
	return filterByStates(all, states), nil
}

func filterByStates(in []issue.Issue, states []string) []issue.Issue {
	if len(states) == 0 {
		return nil
	}
	out := make([]issue.Issue, 0, len(in))
	for _, i := range in {
		for _, s := range states {
			if strings.EqualFold(i.State, s) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

const projectIssuesQuery = `
query SymphonyProjectIssues($slug: String!, $first: Int!) {
  project(id: $slug) {
    issues(first: $first) {
      nodes {
        id
        identifier
        title
        description
        priority
        branchName
        url
        createdAt
        updatedAt
        state { name }
        labels { nodes { name } }
        inverseRelations {
          nodes {
            type
            issue {
              id
              identifier
              state { name }
            }
          }
        }
      }
    }
  }
}
`

type graphqlResponse struct {
	Data   *projectIssuesData `json:"data"`
	Errors []graphqlError     `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type projectIssuesData struct {
	Project *projectNode `json:"project"`
}

type projectNode struct {
	Issues struct {
		Nodes []issueNode `json:"nodes"`
	} `json:"issues"`
}

type issueNode struct {
	ID          string     `json:"id"`
	Identifier  string     `json:"identifier"`
	Title       string     `json:"title"`
	Description *string    `json:"description"`
	Priority    *float64   `json:"priority"`
	BranchName  *string    `json:"branchName"`
	URL         *string    `json:"url"`
	CreatedAt   *time.Time `json:"createdAt"`
	UpdatedAt   *time.Time `json:"updatedAt"`
	State       *struct {
		Name string `json:"name"`
	} `json:"state"`
	Labels *struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	InverseRelations *struct {
		Nodes []struct {
			Type  string `json:"type"`
			Issue *struct {
				ID         string `json:"id"`
				Identifier string `json:"identifier"`
				State      *struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
}

// fetchProjectIssues runs the GraphQL query and projects the response onto
// the canonical Issue shape. Linear's `inverseRelations` with type
// `blocks` are surfaced as Blocker entries on the result.
func (c *Client) fetchProjectIssues(ctx context.Context) ([]issue.Issue, error) {
	body, err := json.Marshal(map[string]any{
		"query": projectIssuesQuery,
		"variables": map[string]any{
			"slug":  c.cfg.ProjectSlug,
			"first": c.pageSz,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("linear http %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var parsed graphqlResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("linear graphql: %s", parsed.Errors[0].Message)
	}
	if parsed.Data == nil || parsed.Data.Project == nil {
		return nil, fmt.Errorf("linear: project %q not found", c.cfg.ProjectSlug)
	}

	out := make([]issue.Issue, 0, len(parsed.Data.Project.Issues.Nodes))
	for _, n := range parsed.Data.Project.Issues.Nodes {
		out = append(out, projectIssue(n))
	}
	return out, nil
}

func projectIssue(n issueNode) issue.Issue {
	i := issue.Issue{
		ID:          n.ID,
		Identifier:  n.Identifier,
		Title:       n.Title,
		Description: n.Description,
		BranchName:  n.BranchName,
		URL:         n.URL,
		CreatedAt:   n.CreatedAt,
		UpdatedAt:   n.UpdatedAt,
	}
	if n.Priority != nil {
		p := int(*n.Priority)
		i.Priority = &p
	}
	if n.State != nil {
		i.State = n.State.Name
	}
	if n.Labels != nil {
		for _, l := range n.Labels.Nodes {
			i.Labels = append(i.Labels, l.Name)
		}
	}
	if n.InverseRelations != nil {
		for _, rel := range n.InverseRelations.Nodes {
			if rel.Type != "blocks" || rel.Issue == nil {
				continue
			}
			b := issue.Blocker{ID: rel.Issue.ID, Identifier: rel.Issue.Identifier}
			if rel.Issue.State != nil {
				b.State = rel.Issue.State.Name
			}
			i.BlockedBy = append(i.BlockedBy, b)
		}
	}
	return i
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
