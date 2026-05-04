package prompt_test

import (
	"strings"
	"testing"

	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/prompt"
)

func TestRenderInterpolatesIssueFields(t *testing.T) {
	b, err := prompt.New("{{ issue.identifier }} — {{ issue.title }}")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := b.Render(issue.Issue{Identifier: "MT-1", Title: "alpha"}, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "MT-1 — alpha" {
		t.Fatalf("got %q want %q", got, "MT-1 — alpha")
	}
}

func TestRenderHandlesAttemptNumber(t *testing.T) {
	b, _ := prompt.New("attempt {{ attempt.number }} for {{ issue.identifier }}")
	first, _ := b.Render(issue.Issue{Identifier: "MT-1", Title: "t"}, nil)
	if first != "attempt 0 for MT-1" {
		t.Fatalf("first: got %q", first)
	}
	n := uint32(3)
	retry, _ := b.Render(issue.Issue{Identifier: "MT-1", Title: "t"}, &n)
	if retry != "attempt 3 for MT-1" {
		t.Fatalf("retry: got %q", retry)
	}
}

func TestRenderExposesOptionalFields(t *testing.T) {
	body := "{{ issue.identifier }} | branch={{ issue.branch_name }} | url={{ issue.url }}"
	b, _ := prompt.New(body)
	branch := "1-fix"
	url := "https://example.com/1"
	got, err := b.Render(issue.Issue{
		Identifier: "MT-1", Title: "t", BranchName: &branch, URL: &url,
	}, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, "branch=1-fix") {
		t.Fatalf("missing branch: %q", got)
	}
	if !strings.Contains(got, "url=https://example.com/1") {
		t.Fatalf("missing url: %q", got)
	}
}

func TestNewSurfacesParseErrors(t *testing.T) {
	// Invalid Liquid tag — `endfor` without matching `for`.
	_, err := prompt.New("{% endfor %}")
	if err == nil {
		t.Fatal("expected parse error for orphan endfor")
	}
}

func TestRenderHandlesBlockedBy(t *testing.T) {
	body := "{% for b in issue.blocked_by %}{{ b.identifier }}={{ b.state }};{% endfor %}"
	b, _ := prompt.New(body)
	got, err := b.Render(issue.Issue{
		Identifier: "MT-1", Title: "t",
		BlockedBy: []issue.Blocker{
			{Identifier: "MT-X", State: "Todo"},
			{Identifier: "MT-Y", State: "Done"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "MT-X=Todo;MT-Y=Done;" {
		t.Fatalf("got %q", got)
	}
}
