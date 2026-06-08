package plugin

import (
	"encoding/json"
	"net/url"
	"sort"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// gc_vm_query_trace instrumentation.
//
// When the plugin runs a monitor (alerting) query against VictoriaMetrics we always
// request the VM execution trace (trace=1) and, if the query was slow, emit a single
// structured log line carrying the trace plus the context we have (rule UID, query,
// endpoint, duration). The groundcover sensor collects the line through the existing
// k8s log pipeline; this phase produces the log line only — no parsing or storage.
//
// See docs: groundcover-private/docs/superpowers/specs/2026-06-07-vm-slow-query-insights-design.md
const (
	// slowQueryLogPrefix is the stable message prefix our logging infra filters on.
	slowQueryLogPrefix = "gc_vm_query_trace"

	// ruleUIDHeader is the header Grafana sets during alerting evaluation. It builds
	// rule metadata {Name,Uid,Type,Version} and emits each as http_X-Rule-<key>
	// (URL-escaped), alongside the FromAlert header.
	ruleUIDHeader = "http_X-Rule-Uid"
	// ruleUIDHeaderCanonical is a defensive fallback in case the header is delivered
	// without the http_ prefix.
	ruleUIDHeaderCanonical = "X-Rule-Uid"
)

// slowQueryThreshold is the hardcoded duration at or above which an alerting query is
// considered slow. It is intentionally not configurable (no config plumbing into the
// plugin); to tune it, change the constant and ship the plugin.
var slowQueryThreshold = 3 * time.Second

// slowQueryLog carries everything needed to decide whether to emit the trace log line
// and what to put in it.
type slowQueryLog struct {
	forAlerting bool
	ruleUID     string
	query       string
	endpoint    string
	duration    time.Duration
	trace       *Trace
}

// ruleUIDFromHeaders extracts the alerting rule UID from the request headers, returning
// "" when no rule UID is present (e.g. non-alerting requests). The header value is
// URL-unescaped; if unescaping fails the raw value is returned.
func ruleUIDFromHeaders(headers map[string]string) string {
	raw := headers[ruleUIDHeader]
	if raw == "" {
		raw = headers[ruleUIDHeaderCanonical]
	}
	if raw == "" {
		return ""
	}
	if unescaped, err := url.QueryUnescape(raw); err == nil {
		return unescaped
	}
	return raw
}

// sortedHeaderKeys returns the header key names (not values) in sorted order, for the
// temporary rule-UID debug log.
func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// logSlowAlertingQuery emits one structured log line for a slow monitor (alerting)
// query, including the raw VM trace JSON. It emits nothing for non-alerting requests or
// fast queries. If the slow query completed but the trace we requested is missing/empty,
// the line is still emitted and flagged as an instrumentation anomaly — that means our
// trace=1 capture is silently broken and we want to know.
func logSlowAlertingQuery(logger log.Logger, p slowQueryLog) {
	if !p.forAlerting || p.duration < slowQueryThreshold {
		return
	}

	args := []interface{}{
		"query", p.query,
		"endpoint", p.endpoint,
		"duration_ms", p.duration.Milliseconds(),
	}
	if p.ruleUID != "" {
		args = append(args, "rule_uid", p.ruleUID)
	}

	traceJSON, anomaly := traceJSON(p.trace)
	args = append(args, "trace", traceJSON)
	if anomaly {
		args = append(args, "trace_anomaly", true)
	}

	logger.Info(slowQueryLogPrefix, args...)
}

// traceJSON marshals the VM trace to JSON and reports whether it is missing/empty. A
// trace is considered an instrumentation anomaly when it is nil or carries no message
// (VM always populates a root message when trace=1 is honoured).
func traceJSON(trace *Trace) (string, bool) {
	if trace == nil || trace.Message == "" {
		return "", true
	}
	b, err := json.Marshal(trace)
	if err != nil {
		return "", true
	}
	return string(b), false
}
