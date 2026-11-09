// PatientZero's load (ADR 0010) [16.5]: virtual users with stable identities
// drive the entry points, so distinct-user counts and per-user timelines in
// Atrium have a known answer. The traffic mix is in traffic.js.
//
//   k6 run -e BASE_URL=http://localhost:8080 -e PROFILE=steady patientzero.js
//
// Environment (defaults in brackets):
//   BASE_URL     PatientZero's address [http://localhost:8080]
//   PROFILE      smoke | steady | peak | soak | day [steady]
//   DURATION     how long the profile holds its load, a k6 duration such as
//                90s, 10m or 1h30m [smoke 30s, steady 10m, peak 5m at peak,
//                soak 2h, day 24m: the 24 hours of a day, one minute each]
//   RUN_ID       prefix of every session id, so runs never share one [PROFILE-<start time>]
//   SEED         seed of every virtual user's choices [1]
//   THINK_SCALE  multiplies think times and pauses; tests use a small one [1]
import http from 'k6/http';
import exec from 'k6/execution';
import { check, sleep } from 'k6';
import { traffic } from './traffic.js';

// Entry points and identity headers, as PatientZero serves them
// (internal/app/app.go).
const PATH_ORDER_LIST = '/PatientZero_Web/OrderList.aspx';
const PATH_PLACE_ORDER = '/PatientZero_App/screenservices/PatientZero_App/MainFlow/Checkout/ServiceAPIPlaceOrder';
const PATH_API_ORDERS = '/PatientZero_API/rest/Orders/orders';
const HEADER_USER = 'X-PatientZero-User';
const HEADER_SESSION = 'X-PatientZero-Session';
const HEADER_CLIENT_IP = 'X-Forwarded-For';

const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/+$/, '');
const PROFILE = __ENV.PROFILE || 'steady';
const SEED = parseInt(__ENV.SEED || '1', 10);
const THINK_SCALE = parseFloat(__ENV.THINK_SCALE || '1');
const RUN_ID = __ENV.RUN_ID || `${PROFILE}-${new Date().toISOString().replace(/[-:]|\.\d+/g, '')}`;

const DEFAULT_DURATION = { smoke: '30s', steady: '10m', peak: '5m', soak: '2h', day: '24m' };
if (!(PROFILE in DEFAULT_DURATION)) {
  throw new Error(`PROFILE must be one of ${Object.keys(DEFAULT_DURATION).join(', ')}, not ${PROFILE}`);
}
const DURATION = __ENV.DURATION || DEFAULT_DURATION[PROFILE];

const smoke = PROFILE === 'smoke';
const shoppers = traffic.shoppers;
const api = traffic.apiClients;
// The smoke profile is a quick check: two shoppers and one partner.
const shopperUsers = smoke ? 2 : shoppers.users;
const apiClients = smoke ? 1 : api.clients;
const apiRate = smoke ? 30 : api.requestsPerMinute;

// seconds reads a k6 duration made of hours, minutes and seconds (1h30m).
function seconds(duration) {
  const parts = [...duration.matchAll(/(\d+(?:\.\d+)?)(h|m|s)/g)];
  if (parts.length === 0 || parts.map((p) => p[0]).join('') !== duration) {
    throw new Error(`DURATION ${duration} is not made of hours, minutes and seconds`);
  }
  return parts.reduce((sum, [, n, unit]) => sum + n * { h: 3600, m: 60, s: 1 }[unit], 0);
}

// dayStages follows traffic.hourlyRows: one stage per hour of the day, each
// a 24th of the run, its target the steady value times the hour's share of
// the average hour (at least min). k6 ramps linearly to each target.
function dayStages(steady, min) {
  const rows = traffic.hourlyRows;
  const average = rows.reduce((sum, n) => sum + n, 0) / rows.length;
  const hour = `${Math.max(1, Math.round(seconds(DURATION) / rows.length))}s`;
  return rows.map((n) => ({ duration: hour, target: Math.max(min, Math.round((steady * n) / average)) }));
}

function scenarios() {
  if (PROFILE === 'day') {
    const busiest = Math.max(...traffic.hourlyRows) / (traffic.hourlyRows.reduce((sum, n) => sum + n, 0) / 24);
    return {
      shoppers: {
        executor: 'ramping-vus',
        exec: 'shopper',
        startVUs: dayStages(shopperUsers, 1)[23].target,
        stages: dayStages(shopperUsers, 1),
        gracefulRampDown: '30s',
      },
      api: {
        executor: 'ramping-arrival-rate',
        exec: 'apiClient',
        startRate: dayStages(apiRate, 1)[23].target,
        timeUnit: '1m',
        preAllocatedVUs: Math.ceil(apiClients * busiest),
        maxVUs: Math.ceil(apiClients * busiest) * 4,
        stages: dayStages(apiRate, 1),
      },
    };
  }
  if (PROFILE === 'peak') {
    const users = shopperUsers;
    const top = users * traffic.peakFactor;
    const rate = apiRate;
    return {
      shoppers: {
        executor: 'ramping-vus',
        exec: 'shopper',
        startVUs: users,
        stages: [
          { duration: '2m', target: top },
          { duration: DURATION, target: top },
          { duration: '2m', target: users },
        ],
        gracefulRampDown: '30s',
      },
      api: {
        executor: 'ramping-arrival-rate',
        exec: 'apiClient',
        startRate: rate,
        timeUnit: '1m',
        preAllocatedVUs: apiClients * traffic.peakFactor,
        maxVUs: apiClients * traffic.peakFactor * 4,
        stages: [
          { duration: '2m', target: rate * traffic.peakFactor },
          { duration: DURATION, target: rate * traffic.peakFactor },
          { duration: '2m', target: rate },
        ],
      },
    };
  }
  return {
    shoppers: {
      executor: 'constant-vus',
      exec: 'shopper',
      vus: shopperUsers,
      duration: DURATION,
    },
    api: {
      executor: 'constant-arrival-rate',
      exec: 'apiClient',
      rate: apiRate,
      timeUnit: '1m',
      duration: DURATION,
      preAllocatedVUs: apiClients,
      maxVUs: apiClients * 4,
    },
  };
}

export const options = {
  scenarios: scenarios(),
  // Expected failures (refused orders) are marked per request, so any other
  // failure counts.
  thresholds: {
    checks: ['rate>0.99'],
    http_req_failed: ['rate<0.01'],
  },
  // The user names and session ids are the identities Atrium shows; keep
  // them out of k6's per-URL metrics.
  systemTags: ['status', 'method', 'name', 'scenario', 'check', 'expected_response'],
};

// A small seeded generator (mulberry32) per virtual user: the same SEED gives
// every user the same sequence of choices on every run. k6's randomSeed
// would give all users the same sequence.
let rngState = 0;
function random() {
  if (rngState === 0) {
    rngState = (SEED * 0x9e3779b1 + exec.vu.idInTest * 0x85ebca6b) >>> 0 || 1;
  }
  rngState = (rngState + 0x6d2b79f5) >>> 0;
  let t = rngState;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const between = ([lo, hi]) => lo + Math.floor(random() * (hi - lo + 1));
const chance = (share) => random() < share;
const pick = (list) => list[Math.floor(random() * list.length)];
const think = ([lo, hi]) => sleep((lo + random() * (hi - lo)) * THINK_SCALE);
const pad = (n) => String(n).padStart(4, '0');

// The n-th address in 10.<b>.0.0/16, skipping .0 and .255.
const clientIP = (b, n) => `10.${b}.${Math.floor((n - 1) / 254)}.${((n - 1) % 254) + 1}`;

// Shopper n is always shopper.<n> from the same client IP; its sessions are
// numbered within the run.
function shopperIdentity() {
  const n = exec.vu.idInTest;
  return {
    user: `shopper.${pad(n)}`,
    ip: clientIP(20, n),
    session: `${RUN_ID}.v${pad(n)}.${exec.vu.iterationInScenario + 1}`,
  };
}

function orderList(headers) {
  const res = http.get(`${BASE_URL}${PATH_ORDER_LIST}`, { headers, tags: { name: 'OrderList' } });
  check(res, { 'OrderList answers 200': (r) => r.status === 200 });
}

function placeOrder(headers) {
  const invalid = chance(shoppers.invalidOrderShare);
  const body = JSON.stringify({
    productId: invalid ? Math.max(...traffic.products) + 1000 : pick(traffic.products),
    quantity: between(traffic.quantity),
  });
  const res = http.post(`${BASE_URL}${PATH_PLACE_ORDER}`, body, {
    headers: Object.assign({ 'Content-Type': 'application/json' }, headers),
    tags: { name: 'PlaceOrder' },
    responseCallback: invalid ? http.expectedStatuses(400) : http.expectedStatuses(200),
  });
  if (invalid) {
    check(res, { 'a refused order answers 400': (r) => r.status === 400 });
  } else {
    check(res, { 'PlaceOrder answers 200': (r) => r.status === 200 });
  }
}

// One iteration is one session: it opens the order list, then opens it again
// or places orders, and pauses before the user's next session.
export function shopper() {
  const id = shopperIdentity();
  const anonymous = chance(shoppers.anonymousShare);
  const headers = { [HEADER_SESSION]: id.session, [HEADER_CLIENT_IP]: id.ip };
  if (!anonymous) {
    headers[HEADER_USER] = id.user;
  }
  const steps = between(shoppers.stepsPerSession);
  orderList(headers);
  for (let i = 1; i < steps; i++) {
    think(shoppers.thinkSeconds);
    if (!anonymous && chance(shoppers.orderShare)) {
      placeOrder(headers);
    } else {
      orderList(headers);
    }
  }
  think(shoppers.pauseSeconds);
}

// Partner k calls as partner.<k> from its own address, with no session: each
// call gets a new one, as a stateless API client does.
export function apiClient() {
  const k = (exec.scenario.iterationInTest % apiClients) + 1;
  const res = http.get(`${BASE_URL}${PATH_API_ORDERS}`, {
    headers: { [HEADER_USER]: `partner.${pad(k)}`, [HEADER_CLIENT_IP]: clientIP(30, k) },
    tags: { name: 'GetOrders' },
  });
  check(res, { 'GetOrders answers 200': (r) => r.status === 200 });
}
