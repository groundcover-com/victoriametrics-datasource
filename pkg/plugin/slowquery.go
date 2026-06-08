package plugin

import (
	"encoding/json"
	"net/url"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// gc_vm_query_trace instrumentation.
//
// When the plugin runs a monitor (alerting) query against VictoriaMetrics we always
// request the VM execution trace (trace=1) and, if the query was slow, emit a single
// structured log line carrying the trace plus the context we have (rule UID, query,
// query type, duration). The groundcover sensor collects the line through the existing
// k8s log pipeline; this phase produces the log line only — no parsing or storage.
//
// See docs: groundcover-private/docs/superpowers/specs/2026-06-07-vm-slow-query-insights-design.md
const (
	// slowQueryEvent is the stable identifier our logging infra filters on. It is logged
	// as a dedicated "event" field (not the message) so filtering/grouping is an exact
	// field match, decoupled from the human-readable message wording.
	slowQueryEvent = "gc_vm_query_trace"
	// slowQueryMessage is the human-readable @message for the log line.
	slowQueryMessage = "slow VM monitor query"

	// ruleUIDHeader is the header Grafana sets during alerting evaluation. It builds
	// rule metadata {Name,Uid,Type,Version} and emits each as http_X-Rule-<key>
	// (URL-escaped), alongside the FromAlert header. Confirmed in a live eval: the rule
	// UID arrives under this exact key (the canonical "X-Rule-Uid" is never populated).
	ruleUIDHeader = "http_X-Rule-Uid"
)

// slowQueryThreshold is the hardcoded duration at or above which an alerting query is
// considered slow. It is intentionally not configurable (no config plumbing into the
// plugin); to tune it, change the constant and ship the plugin.
var slowQueryThreshold = 3 * time.Second

// slowQueryLog carries everything needed to decide whether to emit the trace log line
// and what to put in it.
type slowQueryLog struct {
	forAlerting bool
	orgID       int64
	ruleUID     string
	query       string
	queryType   string
	duration    time.Duration
	trace       *Trace
}

// ruleUIDFromHeaders extracts the alerting rule UID from the request headers, returning
// "" when no rule UID is present (e.g. non-alerting requests). The header value is
// URL-unescaped; if unescaping fails the raw value is returned.
func ruleUIDFromHeaders(headers map[string]string) string {
	raw := headers[ruleUIDHeader]
	if raw == "" {
		return ""
	}
	if unescaped, err := url.QueryUnescape(raw); err == nil {
		return unescaped
	}
	return raw
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

	// event carries the stable identifier our logging infra filters on. org_id identifies
	// the Grafana org (one per customer), so a slow query can be attributed to a tenant —
	// Grafana is a shared multi-org deployment, so the log's origin alone does not tell us
	// which customer it belongs to.
	args := []interface{}{
		"event", slowQueryEvent,
		"org_id", p.orgID,
		"query", p.query,
		"query_type", p.queryType,
		"duration_ms", p.duration.Milliseconds(),
	}
	if p.ruleUID != "" {
		args = append(args, "rule_uid", p.ruleUID)
	}

	// Log the trace as a JSON string. Although the SDK logger emits JSON, Grafana's
	// plugin-log bridge re-renders nested struct values via logfmt as Go map[...] output
	// (not JSON), so a *Trace would land unparseable in the collected logs. A marshaled
	// string survives the bridge as valid, field-addressable JSON.
	traceJSON, anomaly := traceToJSON(p.trace)
	args = append(args, "trace", traceJSON)
	if anomaly {
		args = append(args, "trace_anomaly", true)
	}

	logger.Info(slowQueryMessage, args...)
}

// traceToJSON marshals the VM trace to a JSON string and reports whether it is
// missing/empty. A trace is an instrumentation anomaly when it is nil or carries no
// message (VM always populates a root message when trace=1 is honoured).
func traceToJSON(trace *Trace) (string, bool) {
	if trace == nil || trace.Message == "" {
		return "", true
	}
	b, err := json.Marshal(trace)
	if err != nil {
		return "", true
	}
	return string(b), false
}
