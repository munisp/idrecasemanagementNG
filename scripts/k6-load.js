// k6 load test (go-live checklist 3.4): APISIX edge -> case-api -> Postgres/TB.
//   k6 run -e BASE=https://api.idre.example -e TOKEN=$JWT scripts/k6-load.js
// Targets the capacity model in docs/PERFORMANCE.md: 5k req/s sustained per
// tenant burst profile, p95 < 300ms on reads, < 800ms on intake writes.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errors = new Rate('errors');

export const options = {
  scenarios: {
    read_heavy: {
      executor: 'ramping-arrival-rate',
      startRate: 200, timeUnit: '1s',
      preAllocatedVUs: 500, maxVUs: 2000,
      stages: [
        { target: 2000, duration: '2m' },   // ramp
        { target: 5000, duration: '5m' },   // sustained tenant-burst profile
        { target: 0,    duration: '1m' },
      ],
      exec: 'reads',
    },
    write_intake: {
      executor: 'constant-arrival-rate',
      rate: 50, timeUnit: '1s',             // intake submissions
      duration: '8m', preAllocatedVUs: 100,
      exec: 'writes',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<300'],
    'http_req_duration{endpoint:intake}': ['p(95)<800'],
    errors: ['rate<0.01'],
  },
};

const BASE = __ENV.BASE;
const H = { headers: { Authorization: `Bearer ${__ENV.TOKEN}`, 'Content-Type': 'application/json' } };
const TENANT = __ENV.TENANT || 'tx';

export function reads() {
  const r = http.get(`${BASE}/v1/tenants/${TENANT}/cases?limit=50`, H);
  errors.add(r.status !== 200);
  check(r, { 'cases 200': (x) => x.status === 200 });
  sleep(0.1);
}

export function writes() {
  const body = JSON.stringify({
    case_number: `K6-${__VU}-${Date.now()}`,
    service_line: 'K6LOAD', qpa_cents: 180000, plan_type: 'FULLY_INSURED',
  });
  const r = http.post(`${BASE}/v1/tenants/${TENANT}/intakes`, body, H);
  errors.add(r.status !== 200 && r.status !== 201 && r.status !== 202);
  check(r, { 'intake accepted': (x) => [200, 201, 202].includes(x.status) });
}
