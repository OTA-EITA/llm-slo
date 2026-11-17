package rules

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OTA-EITA/llm-slo/internal/config"
)

const cfgJSON = `{
  "slos": [
    {"name": "bot-availability", "kind": "availability", "objective": 0.99, "app": "bot"},
    {"name": "bot-latency", "kind": "latency", "objective": 0.95, "threshold_seconds": 5, "app": "bot"},
    {"name": "bot-ttfc", "kind": "time_to_first_chunk", "objective": 0.99, "threshold_seconds": 1.5, "app": "bot", "model": "m-1"}
  ],
  "budgets": [{"app": "bot", "usd_per_day": 10}]
}`

func generate(t *testing.T, f Format) string {
	t.Helper()
	c, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Write(&b, c, f, "llm-slo"); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestContent(t *testing.T) {
	out := generate(t, Plain)
	for _, want := range []string{
		`- name: llm-slo-bot-availability`,
		`sum(rate(gen_ai_client_inference_duration_seconds_count{app="bot",error_type=~"5..|429|408|timeout|incomplete|_OTHER"}[1h]))`,
		`sum(rate(gen_ai_client_inference_duration_seconds_count{app="bot",error_type!="client_closed"}[1h]))`,
		// whole-number thresholds match both "5" and Prometheus 3's "5.0"
		`gen_ai_client_inference_duration_seconds_bucket{app="bot",error_type="",le=~"5|5\\.0"}`,
		`gen_ai_client_inference_time_to_first_chunk_seconds_bucket{app="bot",gen_ai_request_model="m-1",error_type="",le="1.5"}`,
		`llm_slo:error_ratio:rate1h{slo="bot-availability"} > 0.144`,
		`llm_slo:error_ratio:rate3d{slo="bot-latency"} > 0.05`,
		`llm_slo:error_ratio:rate5m{slo="bot-availability"} > 0.144`,
		`sum(increase(llm_slo_cost_usd_total{app="bot"}[1d])) > 10`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	k := generate(t, K8s)
	if !strings.HasPrefix(k, "apiVersion: monitoring.coreos.com/v1\nkind: PrometheusRule\nmetadata:\n  name: llm-slo\nspec:\n  groups:\n") {
		t.Errorf("bad PrometheusRule header:\n%s", k[:200])
	}
}

// The rest runs promtool when it is installed (CI installs it).
func promtool(t *testing.T) string {
	p, err := exec.LookPath("promtool")
	if err != nil {
		t.Skip("promtool not installed")
	}
	return p
}

func TestPromtoolCheck(t *testing.T) {
	pt := promtool(t)
	dir := t.TempDir()
	f := filepath.Join(dir, "rules.yml")
	os.WriteFile(f, []byte(generate(t, Plain)), 0o644)
	out, err := exec.Command(pt, "check", "rules", f).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool check rules: %v\n%s", err, out)
	}
}

// promtool unit tests feed synthetic series through the generated rules and
// check what fires, so the PromQL is tested, not just parsed.
const unitTests = `rule_files: [rules.yml]
evaluation_interval: 1m
tests:
  - interval: 1m
    input_series:
      # 100 good and 50 failed calls a minute: 33% errors against a 1% budget
      - series: 'gen_ai_client_inference_duration_seconds_count{app="bot",error_type=""}'
        values: '0+100x60'
      - series: 'gen_ai_client_inference_duration_seconds_count{app="bot",error_type="503"}'
        values: '0+50x60'
      # callers hanging up do not count
      - series: 'gen_ai_client_inference_duration_seconds_count{app="bot",error_type="client_closed"}'
        values: '0+1000x60'
      # 80 of 100 successful calls within 5s; written the Prometheus 3 way
      - series: 'gen_ai_client_inference_duration_seconds_bucket{app="bot",error_type="",le="5.0"}'
        values: '0+80x60'
      # one dollar a minute against a ten dollar daily budget
      - series: 'llm_slo_cost_usd_total{app="bot",gen_ai_request_model="m-1"}'
        values: '0+1x60'
    promql_expr_test:
      # 50 / 150: the 1000 hang-ups a minute are not in the denominator
      - expr: round(llm_slo:error_ratio:rate5m{slo="bot-availability"}, 0.001)
        eval_time: 10m
        exp_samples:
          - labels: '{slo="bot-availability"}'
            value: 0.333
      - expr: round(llm_slo:error_ratio:rate5m{slo="bot-latency"}, 0.001)
        eval_time: 10m
        exp_samples:
          - labels: '{slo="bot-latency"}'
            value: 0.2
      - expr: count by (alertname, slo, long_window) (ALERTS{alertstate="firing", slo=~"bot-.*"})
        eval_time: 10m
        exp_samples:
          # availability: 33% > 14.4% and > 6% pages, > 3% and > 1% tickets
          - labels: '{alertname="LLMSLOBurnRatePage", slo="bot-availability", long_window="1h"}'
            value: 1
          - labels: '{alertname="LLMSLOBurnRatePage", slo="bot-availability", long_window="6h"}'
            value: 1
          - labels: '{alertname="LLMSLOBurnRateTicket", slo="bot-availability", long_window="1d"}'
            value: 1
          - labels: '{alertname="LLMSLOBurnRateTicket", slo="bot-availability", long_window="3d"}'
            value: 1
          # latency: 20% slow against 5%: above 3x (15%) and 1x, below 6x (30%)
          - labels: '{alertname="LLMSLOBurnRateTicket", slo="bot-latency", long_window="1d"}'
            value: 1
          - labels: '{alertname="LLMSLOBurnRateTicket", slo="bot-latency", long_window="3d"}'
            value: 1
    alert_rule_test:
      - alertname: LLMDailyBudgetOnPace
        eval_time: 10m
        exp_alerts: []
      - alertname: LLMDailyBudgetOnPace
        eval_time: 30m
        exp_alerts:
          - exp_labels: {severity: ticket, app: bot}
            exp_annotations:
              summary: "bot is spending at a pace of more than $10 a day on LLM calls"
      - alertname: LLMDailyBudgetExceeded
        eval_time: 5m
        exp_alerts: []
      - alertname: LLMDailyBudgetExceeded
        eval_time: 20m
        exp_alerts:
          - exp_labels: {severity: ticket, app: bot}
            exp_annotations:
              summary: "bot spent more than $10 on LLM calls in the last 24h"
`

func TestPromtoolUnit(t *testing.T) {
	pt := promtool(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rules.yml"), []byte(generate(t, Plain)), 0o644)
	os.WriteFile(filepath.Join(dir, "test.yml"), []byte(unitTests), 0o644)
	cmd := exec.Command(pt, "test", "rules", "test.yml")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("promtool test rules: %v\n%s", err, out)
	}
}
