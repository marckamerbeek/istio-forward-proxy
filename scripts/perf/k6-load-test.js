// Load test for the istio-forward-proxy HTTP forward path.
//
// k6's http module sits on top of Go's net/http, so setting HTTP_PROXY
// makes every request here go out in absolute-form (RFC 7230 §5.3.2) via
// the proxy, exactly like a real client configured with HTTP_PROXY would.
// TARGET_URL must be a host allowed by a ServiceEntry the proxy can see.
import http from 'k6/http';
import { check } from 'k6';

const TARGET_URL = __ENV.TARGET_URL || 'http://perf-test.internal/';

// A fixed arrival rate (rather than free-running VUs) keeps the offered load
// deterministic regardless of runner speed or proxy latency, and keeps it
// within what the single-instance mock upstream can sustain — the goal here
// is measuring the proxy's per-request overhead, not the mock's capacity.
export const options = {
  scenarios: {
    steady_load: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 150),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: Number(__ENV.VUS || 20),
      maxVUs: Number(__ENV.MAX_VUS || 50),
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<200', 'p(99)<500'],
  },
  // p(99) isn't in k6's default summary trend stats, but the workflow's
  // job-summary step reads it out of the JSON export, so request it explicitly.
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export default function () {
  const res = http.get(TARGET_URL);
  check(res, {
    'status is 200': (r) => r.status === 200,
  });
}
