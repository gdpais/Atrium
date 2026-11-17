// Shared page logic. Needs roiCompute/roiBreakEven (engine.js) and a locale object L.
(function () {
  var DEF_MIN = [[45, 5], [135, 5], [120, 45], [60, 20]];
  var DEFAULTS = {
    mode: "simple",
    devs: 8, salary: 38000, overhead: 23.75, benefits: 3000, hours: 1600,
    perMonth: 0.75, valueAt: "internal", billed: 45,
    releases: 100, regShare: 15, detected: 70, baseline: 20, lateH: 16, earlyH: 2,
    incidents: 6, impactRate: 0, atriumCost: 69000,
    steps: DEF_MIN.map(function (m) { return { manual: m[0], auto: m[1] }; })
  };
  var COLORS = ["--s1", "--s2", "--s3", "--s4"];
  var FIELDS = ["devs", "salary", "overhead", "benefits", "hours", "perMonth", "billed", "releases", "regShare", "detected", "baseline", "lateH", "earlyH", "incidents", "impactRate", "atriumCost"];
  var PCT = { regShare: 1, detected: 1, baseline: 1 };

  function clone(o) { return JSON.parse(JSON.stringify(o)); }
  function load() {
    var out = clone(DEFAULTS);
    try {
      var raw = localStorage.getItem(L.storeKey);
      if (!raw) return out;
      var s = JSON.parse(raw);
      Object.keys(out).forEach(function (k) { if (s[k] !== undefined) out[k] = s[k]; });
      if (!Array.isArray(out.steps) || out.steps.length !== 4) out.steps = clone(DEFAULTS.steps);
    } catch (e) {}
    return out;
  }
  function save() { try { localStorage.setItem(L.storeKey, JSON.stringify(state)); } catch (e) {} }

  var state = load();
  var $ = function (id) { return document.getElementById(id); };
  var nf = function (o) { return new Intl.NumberFormat(L.locale, o); };
  var F = {
    eur: nf({ style: "currency", currency: "EUR", maximumFractionDigits: 0 }).format,
    eur2: nf({ style: "currency", currency: "EUR", minimumFractionDigits: 2, maximumFractionDigits: 2 }).format,
    n0: nf({ maximumFractionDigits: 0 }).format,
    n1: nf({ maximumFractionDigits: 1 }).format,
    n2: nf({ minimumFractionDigits: 2, maximumFractionDigits: 2 }).format,
    pct: function (x) { return nf({ maximumFractionDigits: 0 }).format(x * 100) + "%"; },
    min: fmtMin
  };
  F.sEur = function (x) { return (x > 0 ? "+" : "") + F.eur(x); };
  F.sPct = function (x) { return (x > 0 ? "+" : "") + F.pct(x); };

  function num(v, max) {
    var x = parseFloat(v);
    if (!isFinite(x) || x < 0) x = 0;
    return max != null ? Math.min(x, max) : x;
  }
  function fmtMin(m) {
    m = Math.round(m);
    if (m < 60) return m + " min";
    var h = Math.floor(m / 60), rest = m - h * 60;
    return rest ? h + " h " + rest + " min" : h + " h";
  }
  function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }
  function sign(el, x) { el.classList.toggle("neg", x < 0); el.classList.toggle("pos", x > 0); }

  var tbody = document.querySelector("#steps tbody");
  L.steps.forEach(function (name, i) {
    var tr = document.createElement("tr");
    var cells = '<td><span class="swatch" style="background:var(' + COLORS[i] + ')"></span>' + name + "</td>";
    ["manual", "auto"].forEach(function (col) {
      cells += '<td><span class="input step-input"><input id="step-' + i + "-" + col + '" type="number" min="0" step="5" inputmode="numeric" aria-label="' + name + ", " + L.colAria[col] + '"></span></td>';
    });
    tr.innerHTML = cells;
    tbody.appendChild(tr);
  });
  $("legend").innerHTML = L.steps.map(function (name, i) {
    return '<span><span class="swatch" style="background:var(' + COLORS[i] + ')"></span>' + name + "</span>";
  }).join("");

  function fillInputs() {
    FIELDS.forEach(function (f) { $(f).value = state[f]; });
    $("valueAt").value = state.valueAt;
    state.steps.forEach(function (s, i) {
      ["manual", "auto"].forEach(function (col) { $("step-" + i + "-" + col).value = s[col]; });
    });
  }

  function drawChart(tot) {
    var rows = [[L.chartRows[0], "manual"], [L.chartRows[1], "auto"]];
    var W = 640, labelW = 110, totW = 64, barH = 24, gap = 16, top = 6, axisH = 22;
    var plotW = W - labelW - totW;
    var max = Math.max(tot.manual, tot.auto, 1);
    var stepT = max > 240 ? 60 : 30;
    var maxT = Math.ceil(max / stepT) * stepT;
    var scale = plotW / maxT;
    var H = top + rows.length * (barH + gap) - gap + axisH;
    var s = '<svg viewBox="0 0 ' + W + " " + H + '" role="img">';
    for (var t = 0; t <= maxT; t += stepT) {
      var x = labelW + t * scale;
      s += '<line class="grid" x1="' + x + '" x2="' + x + '" y1="' + top + '" y2="' + (H - axisH + 4) + '"/>';
      s += '<text x="' + x + '" y="' + (H - 4) + '" text-anchor="middle">' + (t === 0 ? "0" : fmtMin(t)) + "</text>";
    }
    rows.forEach(function (r, ri) {
      var y = top + ri * (barH + gap);
      s += '<text class="lbl" x="0" y="' + (y + barH / 2 + 4) + '">' + r[0] + "</text>";
      var x = labelW;
      state.steps.forEach(function (st, i) {
        var w = st[r[1]] * scale;
        if (w > 0) s += '<rect x="' + x + '" y="' + y + '" width="' + Math.max(w - 1.5, 0.5) + '" height="' + barH + '" rx="2" fill="' + cssVar(COLORS[i]) + '"><title>' + L.steps[i] + ": " + st[r[1]] + " min</title></rect>";
        x += w;
      });
      s += '<text class="tot" x="' + (x + 8) + '" y="' + (y + barH / 2 + 4) + '">' + fmtMin(tot[r[1]]) + "</text>";
    });
    $("chart").innerHTML = s + "</svg>";
  }

  function set(id, text) { $(id).textContent = text; }

  function render() {
    var s = state;
    var r = roiCompute(s);
    var c = roiCompute(s, 0.5);
    var be = roiBreakEven(s);
    var beC = roiBreakEven(s, 0.5);
    var ctx = { s: s, r: r, c: c, be: be, beC: beC, F: F };

    document.querySelectorAll("[data-advanced]").forEach(function (el) { el.hidden = !r.adv; });
    document.querySelectorAll(".seg button").forEach(function (b) {
      b.setAttribute("aria-pressed", b.dataset.mode === s.mode ? "true" : "false");
    });
    $("billed").closest(".field").style.opacity = s.valueAt === "billed" ? "1" : "0.55";
    set("mode-note", L.modeNote(ctx));
    $("derived").innerHTML = L.derivedTeam(ctx);
    $("derived-diag").innerHTML = L.derivedDiag(ctx);
    $("derived-debt").innerHTML = L.derivedReg(ctx);
    $("derived-impact").innerHTML = L.derivedImpact(ctx);
    $("derived-cost").innerHTML = L.derivedCost(ctx);

    set("h-manual", fmtMin(r.tot.manual));
    set("h-auto", fmtMin(r.tot.auto));

    set("o-total", F.eur(r.savings));
    set("o-total-sub", L.totalSub(ctx));
    set("o-net", F.sEur(r.net)); sign($("o-net"), r.net);
    set("o-net-sub", L.netSub(ctx));
    set("o-be", L.beValue(ctx));
    set("o-be-sub", L.beSub(ctx));

    set("r-perdiag", fmtMin(r.savedMin));
    set("r-hours", F.n0(r.diagHours) + " h");
    set("r-time", F.eur(r.time));
    set("r-regs", F.n1(r.regs));
    set("r-reghours", F.n0(r.regHours) + " h");
    set("r-debt", F.eur(r.regValue));
    set("r-impact", F.eur(r.impact));
    set("r-total", F.eur(r.savings));
    set("r-cost", F.eur(-r.atriumCost));
    set("r-net", F.sEur(r.net)); sign($("r-net"), r.net);
    set("r-roi", r.roi == null ? "—" : F.sPct(r.roi));
    set("r-freed", F.n0(r.freedHours) + " h · " + F.n2(r.fte) + " " + L.fte);
    set("r-pct-label", L.pctLabel(ctx));
    set("r-pct", r.pct == null ? "—" : F.n1(r.pct * 100) + "%");
    set("tot-manual", fmtMin(r.tot.manual));
    set("tot-auto", fmtMin(r.tot.auto));

    [["c", c, beC], ["e", r, be]].forEach(function (x) {
      set("sc-" + x[0] + "-sav", F.eur(x[1].savings));
      set("sc-" + x[0] + "-net", F.sEur(x[1].net)); sign($("sc-" + x[0] + "-net"), x[1].net);
      set("sc-" + x[0] + "-cov", x[1].coverage == null ? "—" : F.pct(x[1].coverage));
      set("sc-" + x[0] + "-be", x[2] == null ? "—" : F.n0(x[2]));
    });

    drawChart(r.tot);
    $("summary-text").value = L.summary(ctx);
    save();
  }

  var form = $("inputs");
  form.addEventListener("input", function (e) {
    var id = e.target.id;
    if (FIELDS.indexOf(id) >= 0) state[id] = num(e.target.value, PCT[id] ? 100 : null);
    if (id === "valueAt") state.valueAt = e.target.value;
    render();
  });
  form.addEventListener("change", function (e) {
    // Show the clamped value once the field is left.
    if (FIELDS.indexOf(e.target.id) >= 0) e.target.value = state[e.target.id];
    if (e.target.id === "valueAt") { state.valueAt = e.target.value; render(); }
  });
  form.addEventListener("submit", function (e) { e.preventDefault(); });
  tbody.addEventListener("input", function (e) {
    var m = /^step-(\d)-(manual|auto)$/.exec(e.target.id);
    if (!m) return;
    state.steps[+m[1]][m[2]] = num(e.target.value);
    render();
  });
  document.querySelectorAll(".seg button").forEach(function (b) {
    b.addEventListener("click", function () { state.mode = b.dataset.mode; render(); });
  });
  $("reset").addEventListener("click", function () {
    var mode = state.mode;
    state = clone(DEFAULTS);
    state.mode = mode;
    fillInputs();
    render();
  });
  $("copy").addEventListener("click", function () {
    var text = $("summary-text").value;
    var status = $("copy-status");
    function fallback() {
      $("summary-text").focus();
      $("summary-text").select();
      status.textContent = L.copyFallback;
    }
    try { navigator.clipboard.writeText(text).then(function () { status.textContent = L.copied; }, fallback); }
    catch (e) { fallback(); }
  });
  if (window.matchMedia) {
    try { window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", render); } catch (e) {}
  }
  new MutationObserver(render).observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

  fillInputs();
  render();
})();
