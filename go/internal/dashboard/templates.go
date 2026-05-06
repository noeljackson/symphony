// Package dashboard renders the SPEC v3 §13.7.1 server-rendered HTML
// dashboard. The wire shape is Datastar-style hypermedia: the page at
// `/` is one initial render; live updates are pushed as SSE
// `datastar-merge-fragments` events from `/dashboard/sse`.
//
// Templates are stdlib `html/template` for now. A future PR can
// migrate to typed `templ` components without changing the Datastar
// contract — only the rendering layer.
package dashboard

import (
	_ "embed"
	"html/template"
	"strings"

	"github.com/noeljackson/symphony/go/internal/orchestrator"
	"github.com/noeljackson/symphony/go/internal/state"
)

//go:embed assets/page.html
var pageHTML string

//go:embed assets/running.html
var runningHTML string

//go:embed assets/retrying.html
var retryingHTML string

//go:embed assets/totals.html
var totalsHTML string

//go:embed assets/dashboard.css
var dashboardCSS string

// CSS returns the static stylesheet served at /dashboard.css.
func CSS() string { return dashboardCSS }

var (
	pageTmpl     = template.Must(template.New("page").Funcs(funcs).Parse(pageHTML))
	runningTmpl  = template.Must(template.New("running").Funcs(funcs).Parse(runningHTML))
	retryingTmpl = template.Must(template.New("retrying").Funcs(funcs).Parse(retryingHTML))
	totalsTmpl   = template.Must(template.New("totals").Funcs(funcs).Parse(totalsHTML))
)

// PageView is the data passed to the full-page render.
type PageView struct {
	GeneratedAt string
	Counts      Counts
	Running     []RunningRow
	Retrying    []RetryRow
	Totals      TotalsView
}

// FragmentView is the per-update data sent over SSE.
type FragmentView struct {
	Counts   Counts
	Running  []RunningRow
	Retrying []RetryRow
	Totals   TotalsView
}

// Counts mirrors §13.7.2's `counts`.
type Counts struct {
	Running  int
	Retrying int
}

// RunningRow is one row in the dashboard's running table.
type RunningRow struct {
	IssueID         string
	IssueIdentifier string
	State           string
	SessionID       string
	TurnCount       uint32
	LastEvent       string
	LastMessage     string
	StartedAt       string
	LastEventAt     string
	InputTokens     uint64
	OutputTokens    uint64
	TotalTokens     uint64
}

// RetryRow is one row in the retrying table.
type RetryRow struct {
	IssueID         string
	IssueIdentifier string
	Attempt         uint32
	DueIn           string
	Error           string
}

// TotalsView is the agent_totals card.
type TotalsView struct {
	InputTokens    uint64
	OutputTokens   uint64
	TotalTokens    uint64
	SecondsRunning float64
	CostUSD        string
	CostUSDToday   string
}

// RenderPage returns the full HTML page for `/`.
func RenderPage(view PageView) (string, error) {
	var b strings.Builder
	if err := pageTmpl.Execute(&b, view); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderRunning renders only the `<section id="running">` fragment.
func RenderRunning(view FragmentView) (string, error) {
	var b strings.Builder
	if err := runningTmpl.Execute(&b, view); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderRetrying renders only the `<section id="retrying">` fragment.
func RenderRetrying(view FragmentView) (string, error) {
	var b strings.Builder
	if err := retryingTmpl.Execute(&b, view); err != nil {
		return "", err
	}
	return b.String(), nil
}

// RenderTotals renders only the `<section id="totals">` fragment.
func RenderTotals(view FragmentView) (string, error) {
	var b strings.Builder
	if err := totalsTmpl.Execute(&b, view); err != nil {
		return "", err
	}
	return b.String(), nil
}

var funcs = template.FuncMap{
	"len":  func(s any) int { return tmplLen(s) },
	"trim": strings.TrimSpace,
}

func tmplLen(v any) int {
	switch x := v.(type) {
	case []RunningRow:
		return len(x)
	case []RetryRow:
		return len(x)
	case string:
		return len(x)
	}
	return 0
}

// FromSnapshot adapts an orchestrator snapshot to the dashboard view.
func FromSnapshot(snap orchestrator.Snapshot) PageView {
	return PageView{
		GeneratedAt: snap.GeneratedAt.Format("2006-01-02 15:04:05 MST"),
		Counts: Counts{
			Running:  len(snap.Running),
			Retrying: len(snap.Retrying),
		},
		Running:  runningRows(snap),
		Retrying: retryRows(snap),
		Totals:   totalsFrom(snap.AgentTotals),
	}
}

// FromSnapshotFragment is the SSE-fragment shape (no `GeneratedAt`).
func FromSnapshotFragment(snap orchestrator.Snapshot) FragmentView {
	return FragmentView{
		Counts: Counts{
			Running:  len(snap.Running),
			Retrying: len(snap.Retrying),
		},
		Running:  runningRows(snap),
		Retrying: retryRows(snap),
		Totals:   totalsFrom(snap.AgentTotals),
	}
}

func runningRows(snap orchestrator.Snapshot) []RunningRow {
	out := make([]RunningRow, 0, len(snap.Running))
	for _, r := range snap.Running {
		row := RunningRow{
			IssueID:         r.IssueID,
			IssueIdentifier: r.Identifier,
			State:           r.State,
			SessionID:       r.SessionID,
			TurnCount:       r.TurnCount,
			LastEvent:       r.LastEvent,
			LastMessage:     r.LastMessage,
			StartedAt:       r.StartedAt.Format("15:04:05 MST"),
			InputTokens:     r.InputTokens,
			OutputTokens:    r.OutputTokens,
			TotalTokens:     r.TotalTokens,
		}
		if r.LastEventAt != nil {
			row.LastEventAt = r.LastEventAt.Format("15:04:05 MST")
		}
		out = append(out, row)
	}
	return out
}

func retryRows(snap orchestrator.Snapshot) []RetryRow {
	out := make([]RetryRow, 0, len(snap.Retrying))
	for _, r := range snap.Retrying {
		due := "now"
		if r.DueInMS > 1000 {
			due = formatDuration(r.DueInMS)
		}
		out = append(out, RetryRow{
			IssueID:         r.IssueID,
			IssueIdentifier: r.Identifier,
			Attempt:         r.Attempt,
			DueIn:           due,
			Error:           r.Error,
		})
	}
	return out
}

func totalsFrom(t state.AgentTotals) TotalsView {
	out := TotalsView{
		InputTokens:    t.InputTokens,
		OutputTokens:   t.OutputTokens,
		TotalTokens:    t.TotalTokens,
		SecondsRunning: t.SecondsRunning,
		CostUSD:        "—",
		CostUSDToday:   "—",
	}
	if t.CostUSD != nil {
		out.CostUSD = formatCostUSD(*t.CostUSD)
	}
	if t.CostUSDToday != nil {
		out.CostUSDToday = formatCostUSD(*t.CostUSDToday)
	}
	return out
}

func formatCostUSD(v float64) string {
	// Render with 2 decimal places, rounding toward zero so we never
	// over-promise spend.
	cents := int64(v * 100)
	dollars := cents / 100
	frac := cents % 100
	if frac < 0 {
		frac = -frac
	}
	if dollars == 0 && cents < 0 {
		return "-$0.0" + onePad(frac)
	}
	return formatDollars(dollars) + "." + onePad(frac)
}

func formatDollars(d int64) string {
	if d < 0 {
		return "-$" + integerToString(-d)
	}
	return "$" + integerToString(d)
}

func integerToString(d int64) string {
	if d == 0 {
		return "0"
	}
	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}
	digits := []byte{}
	for d > 0 {
		digits = append([]byte{byte('0' + d%10)}, digits...)
		d /= 10
	}
	b.Write(digits)
	return b.String()
}

func onePad(n int64) string {
	if n < 10 {
		return "0" + integerToString(n)
	}
	return integerToString(n)
}

func formatDuration(ms int64) string {
	s := ms / 1000
	if s < 60 {
		return integerToString(s) + "s"
	}
	m := s / 60
	if m < 60 {
		return integerToString(m) + "m"
	}
	h := m / 60
	return integerToString(h) + "h"
}
