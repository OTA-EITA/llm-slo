// Package rules turns the SLOs and budgets in the config into Prometheus
// recording and alerting rules: multiwindow, multi-burn-rate alerts as
// described in the Google SRE Workbook, chapter "Alerting on SLOs".
package rules

import (
	"fmt"
	"io"
	"strings"

	"github.com/OTA-EITA/llm-slo/internal/config"
	"github.com/OTA-EITA/llm-slo/internal/metrics"
	"github.com/OTA-EITA/llm-slo/internal/proxy"
)

// ErrorTypes counts against availability: provider errors, throttling,
// timeouts and responses that broke off. Client errors (other 4xx) and
// callers hanging up are not the provider's fault and are left out.
const ErrorTypes = `5..|429|408|timeout|incomplete|_OTHER`

// Windows used by the recording rules.
var windows = []string{"5m", "30m", "1h", "2h", "6h", "1d", "3d"}

// burn is one alert of the multiwindow, multi-burn-rate scheme.
type burn struct {
	long, short string
	factor      float64
	severity    string
}

// The Workbook's recommended starting point: 2% of a 30-day budget in an
// hour or 5% in six hours pages; 10% in three days opens a ticket.
var burns = []burn{
	{"1h", "5m", 14.4, "page"},
	{"6h", "30m", 6, "page"},
	{"1d", "2h", 3, "ticket"},
	{"3d", "6h", 1, "ticket"},
}

type Format string

const (
	Plain Format = "prometheus" // a rule file for rule_files
	K8s   Format = "k8s"        // a PrometheusRule for the Prometheus Operator
)

// Write writes the rules for cfg. name is used for the PrometheusRule.
func Write(w io.Writer, cfg *config.Config, f Format, name string) error {
	var b strings.Builder
	indent := ""
	if f == K8s {
		fmt.Fprintf(&b, "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: %s\nspec:\n", name)
		indent = "  "
	}
	fmt.Fprintf(&b, "%sgroups:\n", indent)
	for _, s := range cfg.SLOs {
		writeSLO(&b, indent, s)
	}
	if len(cfg.Budgets) > 0 {
		writeBudgets(&b, indent, cfg.Budgets)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func selector(s config.SLO, extra ...string) string {
	var parts []string
	if s.App != "" {
		parts = append(parts, fmt.Sprintf(`app=%q`, s.App))
	}
	if s.Provider != "" {
		parts = append(parts, fmt.Sprintf(`gen_ai_provider_name=%q`, s.Provider))
	}
	if s.Model != "" {
		parts = append(parts, fmt.Sprintf(`gen_ai_request_model=%q`, s.Model))
	}
	parts = append(parts, extra...)
	return "{" + strings.Join(parts, ",") + "}"
}

// errorRatio is the PromQL for the share of bad events over a window.
func errorRatio(s config.SLO, w string) string {
	switch s.Kind {
	case config.Availability:
		all := selector(s, `error_type!="client_closed"`)
		bad := selector(s, fmt.Sprintf(`error_type=~%q`, ErrorTypes))
		return fmt.Sprintf("sum(rate(%s_count%s[%s]))\n/\nsum(rate(%s_count%s[%s]))",
			proxy.MetricDuration, bad, w, proxy.MetricDuration, all, w)
	default:
		// latency: of the successful calls, how many were slower than the threshold
		metric := proxy.MetricDuration
		if s.Kind == config.FirstChunk {
			metric = proxy.MetricFirstChunk
		}
		ok := selector(s, `error_type=""`)
		fast := selector(s, `error_type=""`, leMatcher(s.ThresholdSeconds))
		return fmt.Sprintf("1 - (\n  sum(rate(%s_bucket%s[%s]))\n  /\n  sum(rate(%s_count%s[%s]))\n)",
			metric, fast, w, metric, ok, w)
	}
}

func record(s config.SLO, w string) string { return "llm_slo:error_ratio:rate" + w }

func writeSLO(b *strings.Builder, ind string, s config.SLO) {
	fmt.Fprintf(b, "%s  - name: llm-slo-%s\n%s    rules:\n", ind, s.Name, ind)
	for _, w := range windows {
		fmt.Fprintf(b, "%s      - record: %s\n", ind, record(s, w))
		writeExpr(b, ind+"        ", errorRatio(s, w))
		fmt.Fprintf(b, "%s        labels:\n%s          slo: %s\n", ind, ind, s.Name)
	}
	budget := 1 - s.Objective
	what := map[string]string{
		config.Availability: "calls failed",
		config.Latency:      fmt.Sprintf("calls took longer than %ss", metrics.FormatFloat(s.ThresholdSeconds)),
		config.FirstChunk:   fmt.Sprintf("streams took longer than %ss to the first chunk", metrics.FormatFloat(s.ThresholdSeconds)),
	}[s.Kind]
	for _, br := range burns {
		alert := fmt.Sprintf("LLMSLOBurnRate%s", title(br.severity))
		threshold := br.factor * budget
		expr := fmt.Sprintf(`%s{slo=%q} > %s
and
%s{slo=%q} > %s`,
			record(s, br.long), s.Name, metrics.FormatFloat(threshold),
			record(s, br.short), s.Name, metrics.FormatFloat(threshold))
		fmt.Fprintf(b, "%s      - alert: %s\n", ind, alert)
		writeExpr(b, ind+"        ", expr)
		fmt.Fprintf(b, "%s        labels:\n%s          severity: %s\n%s          slo: %s\n%s          long_window: %s\n",
			ind, ind, br.severity, ind, s.Name, ind, br.long)
		fmt.Fprintf(b, "%s        annotations:\n", ind)
		fmt.Fprintf(b, "%s          summary: %s\n", ind, quote(fmt.Sprintf("SLO %s is burning its error budget %sx too fast", s.Name, metrics.FormatFloat(br.factor))))
		fmt.Fprintf(b, "%s          description: %s\n", ind, quote(fmt.Sprintf(
			"Over the last %s and %s, more than %s%% of %s (objective %s%%). At this rate the 30-day budget lasts %s.",
			br.long, br.short, pct(threshold), what, pct(s.Objective), lasts(br.factor))))
	}
}

func writeBudgets(b *strings.Builder, ind string, budgets []config.Budget) {
	fmt.Fprintf(b, "%s  - name: llm-slo-cost\n%s    rules:\n", ind, ind)
	for _, bg := range budgets {
		sel := fmt.Sprintf(`{app=%q}`, bg.App)
		limit := metrics.FormatFloat(bg.USDPerDay)

		fmt.Fprintf(b, "%s      - alert: LLMDailyBudgetExceeded\n", ind)
		writeExpr(b, ind+"        ", fmt.Sprintf("sum(increase(%s%s[1d])) > %s", proxy.MetricCost, sel, limit))
		fmt.Fprintf(b, "%s        labels:\n%s          severity: ticket\n%s          app: %s\n", ind, ind, ind, quote(bg.App))
		fmt.Fprintf(b, "%s        annotations:\n%s          summary: %s\n", ind, ind,
			quote(fmt.Sprintf("%s spent more than $%s on LLM calls in the last 24h", bg.App, limit)))

		// on pace: the last hour's spend, kept up for a day, would pass the budget
		fmt.Fprintf(b, "%s      - alert: LLMDailyBudgetOnPace\n", ind)
		writeExpr(b, ind+"        ", fmt.Sprintf("sum(rate(%s%s[1h])) * 86400 > %s", proxy.MetricCost, sel, limit))
		fmt.Fprintf(b, "%s        for: 15m\n", ind)
		fmt.Fprintf(b, "%s        labels:\n%s          severity: ticket\n%s          app: %s\n", ind, ind, ind, quote(bg.App))
		fmt.Fprintf(b, "%s        annotations:\n%s          summary: %s\n", ind, ind,
			quote(fmt.Sprintf("%s is spending at a pace of more than $%s a day on LLM calls", bg.App, limit)))
	}
}

// leMatcher selects one bucket. Prometheus 3 rewrites le="5" to le="5.0" on
// ingestion while Prometheus 2 keeps "5", so whole numbers match both.
func leMatcher(v float64) string {
	f := metrics.FormatFloat(v)
	if strings.ContainsAny(f, ".eE") {
		return fmt.Sprintf(`le=%q`, f)
	}
	return fmt.Sprintf(`le=~%q`, f+`|`+f+`\.0`)
}

// writeExpr writes an expr as a YAML literal block so PromQL quotes need no escaping.
func writeExpr(b *strings.Builder, ind, expr string) {
	fmt.Fprintf(b, "%sexpr: |\n", ind)
	for _, line := range strings.Split(expr, "\n") {
		fmt.Fprintf(b, "%s  %s\n", ind, line)
	}
}

// quote makes a YAML double-quoted scalar.
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func title(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

func pct(v float64) string { return metrics.FormatFloat(roundTo(v*100, 6)) }

func roundTo(v float64, digits int) float64 {
	p := 1.0
	for range digits {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func lasts(factor float64) string {
	hours := 30 * 24 / factor
	if hours >= 48 {
		return metrics.FormatFloat(roundTo(hours/24, 1)) + " days"
	}
	return metrics.FormatFloat(roundTo(hours, 1)) + " hours"
}
