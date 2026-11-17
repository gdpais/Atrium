// Atrium ROI engine. Shared by the EN and PT pages and by engine.test.js.
// Plain functions, no DOM, so the same source is embedded in both pages and tested.

function roiCompute(s, f) {
  // f scales the two uncertain savings drivers (minutes saved per diagnosis and the
  // extra share of regressions caught); 1 = as entered, 0.5 = conservative.
  if (f == null) f = 1;
  var adv = s.mode === "advanced";
  var billed = adv && s.valueAt === "billed";
  var cost = adv ? s.salary * (1 + s.overhead / 100) + s.benefits : s.salary;
  var rate = s.hours > 0 ? cost / s.hours : 0;
  var valueRate = billed ? s.billed : rate;
  var payroll = s.devs * cost;

  var tot = { manual: 0, auto: 0 };
  s.steps.forEach(function (st) { tot.manual += st.manual; tot.auto += st.auto; });
  var savedMin = Math.max(0, tot.manual - tot.auto) * f;
  var perDiagH = savedMin / 60;

  // 1. Reactive diagnoses (incidents and complaints).
  var perYear = s.perMonth * s.devs * 12;
  var diagHours = perDiagH * perYear;
  var time = diagHours * valueRate;

  // 2. Regressions from releases, caught right after the deploy instead of late.
  // Only the regressions Atrium catches beyond what the team already catches early today.
  var extraShare = Math.max(0, s.detected - s.baseline) / 100 * f;
  var regs = s.releases * (s.regShare / 100) * extraShare;
  var regGrossH = regs * Math.max(0, s.lateH - s.earlyH);
  // A regression found late surfaces as an incident, so its diagnosis is already one of the
  // reactive diagnoses above. Take that saving out once, so no hour is counted twice.
  var overlapH = Math.min(regs, perYear) * perDiagH;
  var regHours = Math.max(0, regGrossH - overlapH);
  var regValue = regHours * valueRate;

  // 3. Business impact: a user-facing incident lasts until it is diagnosed, so a shorter
  // diagnosis shortens the degradation by the same hours. Only incidents that are among the
  // reactive diagnoses count. Zero unless the client enters its own cost per hour.
  var impIncidents = Math.min(s.incidents, perYear);
  var impactHours = impIncidents * perDiagH;
  var impact = impactHours * s.impactRate;

  var savings = time + regValue + impact;
  var net = savings - s.atriumCost;
  return {
    adv: adv, billed: billed, cost: cost, rate: rate, valueRate: valueRate, payroll: payroll,
    tot: tot, savedMin: savedMin, perDiagH: perDiagH, perYear: perYear,
    diagHours: diagHours, time: time,
    regs: regs, regGrossH: regGrossH, overlapH: overlapH, regHours: regHours, regValue: regValue,
    impIncidents: impIncidents, impactHours: impactHours, impact: impact,
    savings: savings, atriumCost: s.atriumCost, net: net,
    roi: s.atriumCost > 0 ? net / s.atriumCost : null,
    coverage: s.atriumCost > 0 ? savings / s.atriumCost : null,
    paybackMonths: savings > 0 ? s.atriumCost / savings * 12 : Infinity,
    freedHours: diagHours + regHours,
    fte: s.hours > 0 ? (diagHours + regHours) / s.hours : 0,
    // Share of team cost only means something when hours are valued at internal cost.
    pct: billed || payroll <= 0 ? null : (time + regValue) / payroll
  };
}

// Smallest team size whose savings cover the Atrium cost, everything else unchanged.
function roiBreakEven(s, f) {
  if (!(s.atriumCost > 0)) return 0;
  var t = {};
  Object.keys(s).forEach(function (k) { t[k] = s[k]; });
  for (var d = 1; d <= 5000; d++) {
    t.devs = d;
    if (roiCompute(t, f).savings >= s.atriumCost) return d;
  }
  return null;
}

if (typeof module !== "undefined") module.exports = { roiCompute: roiCompute, roiBreakEven: roiBreakEven };
