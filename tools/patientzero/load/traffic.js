// The traffic PatientZero's load generates: who visits, what they do and how
// often. The shapes come from the logging-database survey [3.3] (aggregates
// of one week of a real OutSystems 11 installation's logs); the volume is
// scaled down to what a laptop's kind cluster runs.
export const traffic = {
  // Browser users of the Traditional screen and the Reactive app, one per
  // virtual user, each with a stable username and client IP. The surveyed
  // week had 4,829 users and 13,871 sessions.
  shoppers: {
    users: 20,
    // Share of sessions that browse without signing in (user 0). Anonymous
    // sessions only open screens: placing an order needs a customer.
    anonymousShare: 0.1,
    // Screens and server actions in one session, inclusive. Surveyed
    // sessions ran to hundreds of requests over a working day; these are
    // shorter, so a 10-minute run still shows several sessions per user.
    stepsPerSession: [10, 40],
    // Share of a signed-in session's steps, after its first screen, that
    // place an order instead of opening the order list again.
    orderShare: 0.4,
    // Share of orders the business rules refuse (an unknown product): HTTP
    // 400 and an error row under the same Request_Key. 0.3% of orders is
    // about 0.07% of all requests, inside the 99.9% availability objective
    // of Atrium's SLO alerts, so they stay quiet without a fault; at 3%
    // the steady load alone sat at the slow burn's 0.6%.
    invalidOrderShare: 0.003,
    // Seconds between two steps of a session, and between two sessions of
    // the same user, inclusive.
    thinkSeconds: [1, 5],
    pauseSeconds: [5, 20],
  },
  // Partner systems calling the exposed REST API, without a session. The
  // surveyed week logged about one integration call for every three screen
  // requests (1.43 M to 4.51 M); 20 shoppers make about 400 requests a
  // minute, so 120 calls a minute keeps that ratio. Real integration calls
  // are mostly consumed ones; PatientZero logs exposed ones until the fake
  // REST partner exists.
  apiClients: {
    clients: 3,
    requestsPerMinute: 120,
  },
  // Product ids and quantities a valid order picks from.
  products: [1, 2, 3, 4, 5],
  quantity: [1, 3],
  // The peak profile runs this many times the steady users and API rate.
  // The surveyed week's busiest minute was 3.8 times its average minute,
  // its busiest hour 1.9 times its average hour.
  peakFactor: 3,
  // The day profile's shape: log rows per hour of the day, in thousands,
  // summed over the surveyed week (hours in the installation's own time
  // zone, as far as the survey could tell). Each hour of a day run gets the
  // steady users and API rate times its share of the average hour.
  hourlyRows: [218, 119, 69, 49, 34, 40, 62, 139, 272, 495, 655, 804,
    755, 614, 672, 728, 717, 659, 621, 581, 514, 488, 506, 382],
};
