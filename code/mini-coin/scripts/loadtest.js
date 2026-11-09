import http from 'k6/http';
import { check } from 'k6';

export const options = {
  stages: [
    { duration: '30s', target: 20 },
    { duration: '30s', target: 20 },
    { duration: '10s', target: 0 },
  ],
  thresholds: {
    http_req_duration: ['p(99)<300'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const body = JSON.stringify({
    req_id: `req-${__VU}-${__ITER}`,
    amount_cent: 1,
    channel: 'manual',
  });
  const res = http.post('http://127.0.0.1:8081/v1/recharges', body, {
    headers: {
      'Content-Type': 'application/json',
      Authorization: 'Bearer demo-token',
      'X-Request-Id': `k6-${__VU}-${__ITER}`,
    },
  });
  check(res, { 'status is 201': (r) => r.status === 201 });
}
