package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// capturedLog records a single logger call for assertions.
type capturedLog struct {
	level string
	msg   string
	args  []interface{}
}

// fakeLogger implements log.Logger and records every call so tests can assert
// on what (if anything) was emitted.
type fakeLogger struct {
	logs []capturedLog
}

func (f *fakeLogger) Debug(msg string, args ...interface{}) {
	f.logs = append(f.logs, capturedLog{"debug", msg, args})
}
func (f *fakeLogger) Info(msg string, args ...interface{}) {
	f.logs = append(f.logs, capturedLog{"info", msg, args})
}
func (f *fakeLogger) Warn(msg string, args ...interface{}) {
	f.logs = append(f.logs, capturedLog{"warn", msg, args})
}
func (f *fakeLogger) Error(msg string, args ...interface{}) {
	f.logs = append(f.logs, capturedLog{"error", msg, args})
}
func (f *fakeLogger) With(_ ...interface{}) log.Logger         { return f }
func (f *fakeLogger) Level() log.Level                         { return log.Debug }
func (f *fakeLogger) FromContext(_ context.Context) log.Logger { return f }

// field returns the value logged under key in args (logged as key/value pairs).
func field(args []interface{}, key string) (interface{}, bool) {
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == key {
			return args[i+1], true
		}
	}
	return nil, false
}

func TestRuleUIDFromHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{
			name:    "primary key http_X-Rule-Uid",
			headers: map[string]string{"http_X-Rule-Uid": "abc123"},
			want:    "abc123",
		},
		{
			name:    "url-escaped value is unescaped",
			headers: map[string]string{"http_X-Rule-Uid": "rule%20one"},
			want:    "rule one",
		},
		{
			name:    "canonicalized fallback X-Rule-Uid",
			headers: map[string]string{"X-Rule-Uid": "xyz"},
			want:    "xyz",
		},
		{
			name:    "absent header returns empty",
			headers: map[string]string{"FromAlert": "true"},
			want:    "",
		},
		{
			name:    "empty value returns empty",
			headers: map[string]string{"http_X-Rule-Uid": ""},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ruleUIDFromHeaders(tt.headers); got != tt.want {
				t.Fatalf("ruleUIDFromHeaders() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQueryType(t *testing.T) {
	tests := []struct {
		name string
		q    Query
		want string
	}{
		{"instant", Query{Instant: true, Range: false}, "instant"},
		{"range explicit", Query{Range: true}, "range"},
		{"default is range", Query{}, "range"},
		{"range wins when both", Query{Instant: true, Range: true}, "range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.queryType(); got != tt.want {
				t.Fatalf("queryType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLogSlowAlertingQuery(t *testing.T) {
	const slow = 4 * time.Second
	const fast = 100 * time.Millisecond

	t.Run("slow alerting eval with trace emits one line with fields", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: true,
			ruleUID:     "rule-1",
			query:       "up",
			queryType:   "instant",
			duration:    slow,
			trace:       &Trace{Duration: 4000, Message: "execution time"},
		})

		if len(l.logs) != 1 {
			t.Fatalf("expected exactly 1 log line, got %d", len(l.logs))
		}
		got := l.logs[0]
		if got.msg != slowQueryLogPrefix {
			t.Fatalf("msg = %q, want %q", got.msg, slowQueryLogPrefix)
		}
		if v, ok := field(got.args, "rule_uid"); !ok || v != "rule-1" {
			t.Fatalf("rule_uid = %v (ok=%v), want rule-1", v, ok)
		}
		if v, ok := field(got.args, "query"); !ok || v != "up" {
			t.Fatalf("query = %v (ok=%v), want up", v, ok)
		}
		if v, ok := field(got.args, "query_type"); !ok || v != "instant" {
			t.Fatalf("query_type = %v (ok=%v), want instant", v, ok)
		}
		if v, ok := field(got.args, "duration_ms"); !ok || v != int64(4000) {
			t.Fatalf("duration_ms = %v (ok=%v), want 4000", v, ok)
		}
		v, ok := field(got.args, "trace")
		if !ok {
			t.Fatalf("trace field missing")
		}
		// The trace is logged as a structured value (nested JSON object), not a
		// stringified blob, so upstream tooling can address its fields.
		tr, ok := v.(*Trace)
		if !ok || tr == nil {
			t.Fatalf("trace should be logged as a structured *Trace, got %T", v)
		}
		if tr.Message != "execution time" {
			t.Fatalf("trace.Message = %q, want %q", tr.Message, "execution time")
		}
		if _, anomaly := field(got.args, "trace_anomaly"); anomaly {
			t.Fatalf("did not expect trace_anomaly flag for a valid trace")
		}
	})

	t.Run("slow alerting eval with nil trace is flagged as anomaly", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: true,
			ruleUID:     "rule-1",
			query:       "up",
			queryType:   "range",
			duration:    slow,
			trace:       nil,
		})
		if len(l.logs) != 1 {
			t.Fatalf("expected exactly 1 log line, got %d", len(l.logs))
		}
		v, ok := field(l.logs[0].args, "trace_anomaly")
		if !ok || v != true {
			t.Fatalf("trace_anomaly = %v (ok=%v), want true", v, ok)
		}
	})

	t.Run("slow alerting eval with empty trace is flagged as anomaly", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: true,
			query:       "up",
			queryType:   "range",
			duration:    slow,
			trace:       &Trace{}, // no message, no children
		})
		if len(l.logs) != 1 {
			t.Fatalf("expected exactly 1 log line, got %d", len(l.logs))
		}
		if v, ok := field(l.logs[0].args, "trace_anomaly"); !ok || v != true {
			t.Fatalf("trace_anomaly = %v (ok=%v), want true", v, ok)
		}
	})

	t.Run("fast alerting eval emits nothing", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: true,
			query:       "up",
			queryType:   "instant",
			duration:    fast,
			trace:       &Trace{Message: "execution time"},
		})
		if len(l.logs) != 0 {
			t.Fatalf("expected no log lines for a fast eval, got %d", len(l.logs))
		}
	})

	t.Run("slow non-alerting request emits nothing", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: false,
			query:       "up",
			queryType:   "instant",
			duration:    slow,
			trace:       &Trace{Message: "execution time"},
		})
		if len(l.logs) != 0 {
			t.Fatalf("expected no log lines for a non-alerting request, got %d", len(l.logs))
		}
	})

	t.Run("slow alerting eval without rule_uid omits the field gracefully", func(t *testing.T) {
		l := &fakeLogger{}
		logSlowAlertingQuery(l, slowQueryLog{
			forAlerting: true,
			ruleUID:     "",
			query:       "up",
			queryType:   "instant",
			duration:    slow,
			trace:       &Trace{Message: "execution time"},
		})
		if len(l.logs) != 1 {
			t.Fatalf("expected exactly 1 log line, got %d", len(l.logs))
		}
		if _, ok := field(l.logs[0].args, "rule_uid"); ok {
			t.Fatalf("rule_uid should be omitted when empty")
		}
	})
}

// withThreshold temporarily overrides the hardcoded slow-query threshold for a test and
// restores it afterwards.
func withThreshold(t *testing.T, d time.Duration) {
	t.Helper()
	prev := slowQueryThreshold
	slowQueryThreshold = d
	t.Cleanup(func() { slowQueryThreshold = prev })
}

// traceVectorBody is a VM response carrying a trace, for the integration tests.
const traceVectorBody = `{"status":"success","data":{"resultType":"vector","result":[]},"trace":{"duration_msec":4200,"message":"execution: up"}}`

// newTracingInstance spins up a test VM server that records the query it received and
// returns body, and a DatasourceInstance wired to a capturing logger.
func newTracingInstance(t *testing.T, body string, status int) (*DatasourceInstance, *fakeLogger, *url.Values) {
	t.Helper()
	var lastQuery url.Values
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		lastQuery = r.URL.Query()
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}
	mux.HandleFunc(instantQueryPath, handler)
	mux.HandleFunc(rangeQueryPath, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	l := &fakeLogger{}
	di := &DatasourceInstance{
		url:         srv.URL,
		httpClient:  srv.Client(),
		logger:      l,
		queryParams: url.Values{},
		settings:    DataSourceInstanceSettings{HTTPMethod: http.MethodGet},
	}
	return di, l, &lastQuery
}

func instantDataQuery() backend.DataQuery {
	return backend.DataQuery{
		RefID:     "A",
		QueryType: instantQueryPath,
		TimeRange: backend.TimeRange{From: time.Unix(1670324000, 0), To: time.Unix(1670324477, 0)},
		JSON:      []byte(`{"refId":"A","instant":true,"range":false,"expr":"up"}`),
	}
}

func TestQueryAlertingForcesTraceAndLogsWhenSlow(t *testing.T) {
	withThreshold(t, 0) // any duration counts as slow
	di, l, lastQuery := newTracingInstance(t, traceVectorBody, 0)

	resp := di.query(context.Background(), instantDataQuery(), true, "rule-42")
	if resp.Error != nil {
		t.Fatalf("unexpected query error: %v", resp.Error)
	}

	if got := lastQuery.Get("trace"); got != "1" {
		t.Fatalf("expected trace=1 on the VM request, got %q", got)
	}
	if len(l.logs) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d", len(l.logs))
	}
	got := l.logs[0]
	if got.msg != slowQueryLogPrefix {
		t.Fatalf("msg = %q, want %q", got.msg, slowQueryLogPrefix)
	}
	if v, ok := field(got.args, "rule_uid"); !ok || v != "rule-42" {
		t.Fatalf("rule_uid = %v (ok=%v), want rule-42", v, ok)
	}
	if v, ok := field(got.args, "query_type"); !ok || v != "instant" {
		t.Fatalf("query_type = %v (ok=%v), want instant", v, ok)
	}
	if v, ok := field(got.args, "query"); !ok || v != "up" {
		t.Fatalf("query = %v (ok=%v), want up", v, ok)
	}
	if v, ok := field(got.args, "trace"); !ok {
		t.Fatalf("trace field missing")
	} else if tr, ok := v.(*Trace); !ok || tr == nil || tr.Message == "" {
		t.Fatalf("expected a structured non-empty *Trace, got %T (%v)", v, v)
	}
	if _, anomaly := field(got.args, "trace_anomaly"); anomaly {
		t.Fatalf("did not expect anomaly flag for a populated trace")
	}
}

func TestQueryNonAlertingDoesNotTraceOrLog(t *testing.T) {
	withThreshold(t, 0) // even though "slow", non-alerting must not log
	di, l, lastQuery := newTracingInstance(t, traceVectorBody, 0)

	resp := di.query(context.Background(), instantDataQuery(), false, "")
	if resp.Error != nil {
		t.Fatalf("unexpected query error: %v", resp.Error)
	}

	if got := lastQuery.Get("trace"); got != "" {
		t.Fatalf("expected no trace param for non-alerting request, got %q", got)
	}
	if len(l.logs) != 0 {
		t.Fatalf("expected no log lines for a non-alerting request, got %d", len(l.logs))
	}
}

func TestQueryFastAlertingDoesNotLogButStillTraces(t *testing.T) {
	withThreshold(t, time.Hour) // nothing is slow
	di, l, lastQuery := newTracingInstance(t, traceVectorBody, 0)

	resp := di.query(context.Background(), instantDataQuery(), true, "rule-42")
	if resp.Error != nil {
		t.Fatalf("unexpected query error: %v", resp.Error)
	}

	if got := lastQuery.Get("trace"); got != "1" {
		t.Fatalf("expected trace=1 even for a fast alerting query, got %q", got)
	}
	if len(l.logs) != 0 {
		t.Fatalf("expected no log lines for a fast query, got %d", len(l.logs))
	}
}

func TestQueryErrorDoesNotLog(t *testing.T) {
	withThreshold(t, 0)
	di, l, _ := newTracingInstance(t, `{"status":"error"}`, http.StatusInternalServerError)

	resp := di.query(context.Background(), instantDataQuery(), true, "rule-42")
	if resp.Error == nil {
		t.Fatalf("expected an error response from a 500")
	}
	if len(l.logs) != 0 {
		t.Fatalf("expected no log lines for a query error, got %d", len(l.logs))
	}
}
