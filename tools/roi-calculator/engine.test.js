const { test, expect } = require("bun:test");
const { roiCompute, roiBreakEven } = require("./engine.js");

const base = () => ({
  mode: "simple", devs: 8, salary: 38000, overhead: 23.75, benefits: 3000, hours: 1600,
  perMonth: 0.75, valueAt: "internal", billed: 45,
  releases: 100, regShare: 15, detected: 70, baseline: 20, lateH: 16, earlyH: 2,
  incidents: 6, impactRate: 0, atriumCost: 69000,
  steps: [
    { manual: 45, auto: 5 }, { manual: 135, auto: 5 }, { manual: 120, auto: 45 }, { manual: 60, auto: 20 }
  ]
});
const close = (a, b) => expect(a).toBeCloseTo(b, 2);

test("simplified defaults, worked by hand", () => {
  const r = roiCompute(base());
  close(r.rate, 23.75);                 // 38000 / 1600
  close(r.perDiagH, 4.75);              // (360 - 75) / 60
  close(r.perYear, 72);                 // 0.75 * 8 * 12
  close(r.time, 8122.5);                // 342 h * 23.75
  close(r.regs, 7.5);                   // 100 * 15% * (70% - 20%)
  close(r.regGrossH, 105);              // 7.5 * (16 - 2)
  close(r.overlapH, 35.625);            // 7.5 * 4.75, already in the diagnoses
  close(r.regHours, 69.375);
  close(r.regValue, 1647.656);
  close(r.impact, 0);
  close(r.savings, 9770.156);
  close(r.net, 9770.156 - 69000);
  close(r.fte, (342 + 69.375) / 1600);
});

test("advanced defaults use the full cost to the company", () => {
  const r = roiCompute({ ...base(), mode: "advanced" });
  close(r.cost, 50025);                 // 38000 * 1.2375 + 3000
  close(r.rate, 31.265625);
  close(r.savings, (342 + 69.375) * 31.265625);
});

test("simplified is always a floor of advanced at internal cost", () => {
  for (const devs of [1, 8, 40]) for (const incidents of [0, 6]) {
    const s = { ...base(), devs, incidents, impactRate: 500 };
    expect(roiCompute(s).savings).toBeLessThanOrEqual(roiCompute({ ...s, mode: "advanced" }).savings + 1e-9);
  }
});

test("no hour counted twice: a caught regression saves lateH - earlyH in total", () => {
  // One regression a year, one diagnosis a year: total hours freed = 16 - 2.
  const s = { ...base(), devs: 1, perMonth: 1 / 12, releases: 1, regShare: 100, detected: 100, baseline: 0 };
  const r = roiCompute(s);
  close(r.freedHours, 14);
});

test("overlap never exceeds the reactive diagnoses", () => {
  const r = roiCompute({ ...base(), perMonth: 0 });
  close(r.overlapH, 0);
  close(r.regHours, 105);
});

test("baseline at or above detection gives no regression saving", () => {
  close(roiCompute({ ...base(), baseline: 70 }).regValue, 0);
  close(roiCompute({ ...base(), baseline: 90 }).regValue, 0);
});

test("incident impact uses the hours of diagnosis saved and the client's cost per hour", () => {
  const r = roiCompute({ ...base(), impactRate: 1000 });
  close(r.impact, 6 * 4.75 * 1000);
  // Incidents beyond the reactive diagnoses are not counted.
  close(roiCompute({ ...base(), incidents: 500, impactRate: 1 }).impact, 72 * 4.75);
});

test("billed mode values hours at the billed rate and drops the share of team cost", () => {
  const r = roiCompute({ ...base(), mode: "advanced", valueAt: "billed" });
  close(r.valueRate, 45);
  close(r.time, 342 * 45);
  expect(r.pct).toBeNull();
});

test("conservative halves the uncertain drivers", () => {
  const c = roiCompute(base(), 0.5);
  close(c.perDiagH, 2.375);
  close(c.regs, 3.75);
  close(c.time, 8122.5 / 2);            // 2.375 h * 72 * 23.75
  close(c.regHours, 3.75 * 14 - 3.75 * 2.375); // fix hours per regression are not scaled
  expect(c.savings).toBeLessThan(roiCompute(base()).savings);
});

test("ROI, coverage and payback", () => {
  const r = roiCompute({ ...base(), atriumCost: 9770.156 });
  close(r.roi, 0);
  close(r.coverage, 1);
  close(r.paybackMonths, 12);
  expect(roiCompute({ ...base(), atriumCost: 0 }).roi).toBeNull();
});

test("break-even team size is the first size that covers the cost", () => {
  const s = base();
  const d = roiBreakEven(s);
  expect(roiCompute({ ...s, devs: d }).savings).toBeGreaterThanOrEqual(69000);
  expect(roiCompute({ ...s, devs: d - 1 }).savings).toBeLessThan(69000);
  expect(roiBreakEven({ ...s, atriumCost: 0 })).toBe(0);
  expect(roiBreakEven({ ...s, steps: s.steps.map(x => ({ manual: 10, auto: 10 })) })).toBeNull();
});
