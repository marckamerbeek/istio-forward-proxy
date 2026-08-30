// Load test for the istio-forward-proxy HTTP forward path.
//
// k6's http module sits on top of Go's net/http, so setting HTTP_PROXY
// makes every request here go out in absolute-form (RFC 7230 §5.3.2) via
// the proxy, exactly like a real client configured with HTTP_PROXY would.
// TARGET_URL must be a host allowed by a ServiceEntry the proxy can see.
import http from 'k6/http';
import { check } from 'k6';

const TARGET_URL = __ENV.TARGET_URL || 'http://perf-test.internal/';

export const options = {
  scenarios: {
    steady_load: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 20),
      duration: __ENV.DURATION || '30s',
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<200', 'p(99)<500'],
  },
};

export default function () {
  const res = http.get(TARGET_URL);
  check(res, {
    'status is 200': (r) => r.status === 200,
  });
}
