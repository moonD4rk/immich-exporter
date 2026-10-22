package grafana

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	envPrometheus      = "DASHBOARD_PROMETHEUS_URL"
	envVictoriaMetrics = "DASHBOARD_VICTORIAMETRICS_URL"
	// testdata/scrape.yml scrapes the golden metrics as two instances.
	exporterInstances = 2
)

// Range windows and scrape timing legitimately shift values between engines, so only labels are compared there.
var rangeDependent = regexp.MustCompile(`\[|\btimestamp\(`)

var httpClient = &http.Client{Timeout: 15 * time.Second}

type backend struct {
	name, baseURL string
}

type sample struct {
	labels map[string]string
	value  float64
}

// liveBackends returns the backends from testdata/compose.yml once both have scraped
// every exporter instance a few times, and skips the test when they aren't configured.
func liveBackends(t *testing.T) []backend {
	t.Helper()
	prom, vm := os.Getenv(envPrometheus), os.Getenv(envVictoriaMetrics)
	if prom == "" || vm == "" {
		t.Skipf("set %s and %s to run against the backends in testdata/compose.yml", envPrometheus, envVictoriaMetrics)
	}
	bs := []backend{{"prometheus", prom}, {"victoriametrics", vm}}
	deadline := time.Now().Add(2 * time.Minute)
	for _, b := range bs {
		for {
			// Twelve 2s scrapes hold history beyond offset $__interval (15s).
			got, err := b.instant(t.Context(), `count_over_time(up{job="immich-exporter"}[1m]) >= 12`, time.Now())
			if err == nil && len(got) == exporterInstances {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not scrape %d exporter instances in time (got %d series, err %v)",
					b.name, exporterInstances, len(got), err)
			}
			time.Sleep(time.Second)
		}
	}
	return bs
}

func unixSeconds(ts time.Time) string {
	return strconv.FormatFloat(float64(ts.UnixMilli())/1000, 'f', 3, 64)
}

func (b backend) instant(ctx context.Context, expr string, at time.Time) ([]sample, error) {
	return b.get(ctx, "/api/v1/query", url.Values{"query": {expr}, "time": {unixSeconds(at)}})
}

func (b backend) rangeQuery(ctx context.Context, expr string, end time.Time) ([]sample, error) {
	return b.get(ctx, "/api/v1/query_range", url.Values{
		"query": {expr},
		"start": {unixSeconds(end.Add(-20 * time.Second))},
		"end":   {unixSeconds(end)},
		"step":  {"2"},
	})
}

func (b backend) get(ctx context.Context, path string, params url.Values) ([]sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+path+"?"+params.Encode(), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string         `json:"resultType"`
			Result     jsontext.Value `json:"result"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("%s: %s", body.ErrorType, body.Error)
	}
	return parseResult(body.Data.ResultType, body.Data.Result)
}

func parseResult(resultType string, raw jsontext.Value) ([]sample, error) {
	if resultType == "scalar" {
		var point []any
		if err := json.Unmarshal(raw, &point); err != nil {
			return nil, err
		}
		v, err := pointValue(point)
		return []sample{{labels: map[string]string{}, value: v}}, err
	}
	var series []struct {
		Metric map[string]string `json:"metric"`
		Value  []any             `json:"value"`
		Values [][]any           `json:"values"`
	}
	if err := json.Unmarshal(raw, &series); err != nil {
		return nil, fmt.Errorf("%s result: %w", resultType, err)
	}
	out := make([]sample, 0, len(series))
	for i := range series {
		point := series[i].Value
		if n := len(series[i].Values); n > 0 {
			point = series[i].Values[n-1]
		}
		v, err := pointValue(point)
		if err != nil {
			return nil, err
		}
		out = append(out, sample{labels: series[i].Metric, value: v})
	}
	return out, nil
}

func pointValue(point []any) (float64, error) {
	if len(point) != 2 {
		return 0, fmt.Errorf("malformed point %v", point)
	}
	s, ok := point[1].(string)
	if !ok {
		return 0, fmt.Errorf("malformed point value %v", point[1])
	}
	return strconv.ParseFloat(s, 64)
}

func fetchAll(bs []backend, mode string, run func(b backend) ([]sample, error)) (results [][]sample, problems []string) {
	results = make([][]sample, len(bs))
	for i, b := range bs {
		got, err := run(b)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s %s: %v", b.name, mode, err))
		case len(got) == 0:
			problems = append(problems, fmt.Sprintf("%s %s: no data", b.name, mode))
		}
		results[i] = got
	}
	return results, problems
}

// check runs one dashboard query on every backend and reports anything that would make
// the panel render differently, or not at all, depending on the backend.
func check(ctx context.Context, bs []backend, q *query, at time.Time) []string {
	shown := q.showsMetricName()
	results, problems := fetchAll(bs, "instant", func(b backend) ([]sample, error) {
		return b.instant(ctx, q.expr, at)
	})
	if len(problems) == 0 {
		values := !rangeDependent.MatchString(withoutStrings(q.expr))
		problems = diffSamples(bs, results, "instant", shown, values)
	}
	if !q.rangeQuery {
		return problems
	}
	results, rangeProblems := fetchAll(bs, "range", func(b backend) ([]sample, error) {
		return b.rangeQuery(ctx, q.expr, at)
	})
	if len(rangeProblems) == 0 {
		rangeProblems = diffSamples(bs, results, "range", shown, false)
	}
	return append(problems, rangeProblems...)
}

func diffSamples(bs []backend, results [][]sample, mode string, keepName, compareValues bool) []string {
	ref, other := bySeries(results[0], keepName), bySeries(results[1], keepName)
	var problems []string
	for _, k := range slices.Sorted(maps.Keys(ref)) {
		ov, ok := other[k]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s: %s only on %s", mode, k, bs[0].name))
		case compareValues && !sameValues(ref[k], ov):
			problems = append(problems, fmt.Sprintf("%s: %s is %v on %s but %v on %s", mode, k, ref[k], bs[0].name, ov, bs[1].name))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(other)) {
		if _, ok := ref[k]; !ok {
			problems = append(problems, fmt.Sprintf("%s: %s only on %s", mode, k, bs[1].name))
		}
	}
	return problems
}

func bySeries(samples []sample, keepName bool) map[string][]float64 {
	out := map[string][]float64{}
	for i := range samples {
		k := seriesKey(samples[i].labels, keepName)
		out[k] = append(out[k], samples[i].value)
	}
	for _, vs := range out {
		slices.Sort(vs)
	}
	return out
}

func seriesKey(labels map[string]string, keepName bool) string {
	parts := make([]string, 0, len(labels))
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		if name == "__name__" && !keepName {
			continue
		}
		parts = append(parts, name+"="+strconv.Quote(labels[name]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sameValues(a, b []float64) bool {
	return slices.EqualFunc(a, b, func(x, y float64) bool {
		if math.IsNaN(x) || math.IsNaN(y) {
			return math.IsNaN(x) && math.IsNaN(y)
		}
		return x == y || math.Abs(x-y) <= 1e-9*math.Max(math.Abs(x), math.Abs(y))
	})
}

func TestDashboardQueriesAgreeAcrossBackends(t *testing.T) {
	bs := liveBackends(t)
	_, qs := loadQueries(t)
	at := time.Now()
	for i := range qs {
		q := &qs[i]
		t.Run(q.name, func(t *testing.T) {
			for _, p := range check(t.Context(), bs, q, at) {
				t.Error(p)
			}
		})
	}
}

// TestCheckFlagsKnownIncompatibilities pins each Prometheus/VictoriaMetrics divergence the
// check exists for to a query it must reject, next to look-alikes it must accept.
func TestCheckFlagsKnownIncompatibilities(t *testing.T) {
	bs := liveBackends(t)
	at := time.Now()
	cases := []struct {
		name   string
		q      query
		reject bool
	}{
		{"compatible aggregate", query{expr: `sum(immich_assets)`}, false},
		{"compatible range query", query{expr: `sum by (type) (immich_assets)`, rangeQuery: true}, false},
		{"label-less aggregate matched like a scalar (#17)", query{expr: `immich_assets_geotagged / clamp_min(sum(immich_assets), 1)`}, true},
		{"MetricsQL implicit rate window", query{expr: `rate(immich_exporter_scrape_errors_total)`}, true},
		{"MetricsQL default operator", query{expr: `immich_assets_favorite default 0`}, true},
		{"MetricsQL WITH template", query{expr: `WITH (x = immich_assets) sum(x)`}, true},
		{"MetricsQL numeric suffix", query{expr: `immich_people < 1Ki`}, true},
		{"Prometheus experimental function", query{expr: `sort_by_label(immich_people, "instance")`}, true},
		{"NaN dropped by VictoriaMetrics", query{expr: `(immich_assets_favorite - immich_assets_favorite) / 0`}, true},
		{"metric name kept only by VictoriaMetrics", query{expr: `clamp_min(immich_people, 1)`}, true},
		{"metric name difference hidden by the legend", query{expr: `clamp_min(immich_people, 1)`, legendFormat: "{{instance}}"}, false},
		{"duplicate series once two instances are scraped", query{expr: `immich_people_named / on() immich_people`}, true},
	}
	for i := range cases {
		c := &cases[i]
		t.Run(c.name, func(t *testing.T) {
			problems := check(t.Context(), bs, &c.q, at)
			switch {
			case c.reject && len(problems) == 0:
				t.Errorf("%s: the check accepted it", c.q.expr)
			case !c.reject && len(problems) > 0:
				t.Errorf("%s: the check rejected it: %v", c.q.expr, problems)
			}
		})
	}
}
