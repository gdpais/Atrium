// Heartbeat docs site behaviour: mobile drawer, on-this-page highlighting,
// search and Mermaid diagrams. Generated into docs/site/assets by make docs-site.
(function () {
  "use strict";

  var root = document.body.getAttribute("data-root") || "";

  // Mobile sidebar drawer.
  function setupDrawer() {
    var button = document.querySelector(".menu-button");
    var sidebar = document.getElementById("sidebar");
    var scrim = document.querySelector(".scrim");
    if (!button || !sidebar || !scrim) return;
    function setOpen(open) {
      sidebar.classList.toggle("open", open);
      scrim.hidden = !open;
      button.setAttribute("aria-expanded", String(open));
      if (open) {
        var current = sidebar.querySelector("[aria-current=page]") || sidebar.querySelector("a");
        if (current) current.focus();
      }
    }
    button.addEventListener("click", function () { setOpen(!sidebar.classList.contains("open")); });
    scrim.addEventListener("click", function () { setOpen(false); });
    sidebar.addEventListener("click", function (e) {
      if (e.target.closest("a")) setOpen(false);
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && sidebar.classList.contains("open")) {
        setOpen(false);
        button.focus();
      }
    });
  }

  // Highlight the on-this-page entry for the section being read. A section is
  // current once its heading passes a reading line near the top of the
  // window. Sections near the end of a page can never scroll that far, so
  // over the last screen of scrolling the line slides down to the bottom of
  // the window, and each remaining heading becomes current in turn. A section
  // picked from the list stays current until the reader scrolls by hand.
  function setupTocHighlight() {
    var toc = document.querySelector(".toc");
    var links = Array.prototype.slice.call(document.querySelectorAll(".toc a"));
    if (!toc || !links.length) return;
    var targets = links.map(function (a) {
      return document.getElementById(decodeURIComponent(a.getAttribute("href").slice(1)));
    });
    var top = 100;
    var picked = -1;
    var pending = false;

    function update() {
      pending = false;
      var view = window.innerHeight;
      var remaining = document.documentElement.scrollHeight - view - window.scrollY;
      var progress = Math.min(1, Math.max(0, 1 - remaining / view));
      var line = top + (view - top) * progress;
      var active = 0;
      for (var i = 0; i < targets.length; i++) {
        if (targets[i] && targets[i].getBoundingClientRect().top <= line) active = i;
      }
      if (picked >= 0) {
        var r = targets[picked] && targets[picked].getBoundingClientRect();
        if (r && r.top >= 0 && r.top < view) active = picked;
        else picked = -1;
      }
      links.forEach(function (a, i) { a.classList.toggle("active", i === active); });
      // Keep the current entry visible in a long list.
      var link = links[active];
      if (link.offsetTop < toc.scrollTop || link.offsetTop + link.offsetHeight > toc.scrollTop + toc.clientHeight) {
        toc.scrollTop = link.offsetTop - toc.clientHeight / 2;
      }
    }
    function schedule() {
      if (!pending) { pending = true; window.requestAnimationFrame(update); }
    }
    function pick(id) {
      picked = targets.findIndex(function (t) { return t && t.id === id; });
      schedule();
    }
    links.forEach(function (a) {
      a.addEventListener("click", function () { pick(decodeURIComponent(a.getAttribute("href").slice(1))); });
    });
    ["wheel", "touchmove", "keydown"].forEach(function (type) {
      window.addEventListener(type, function () { picked = -1; }, { passive: true });
    });
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    if (window.location.hash) pick(decodeURIComponent(window.location.hash.slice(1)));
    update();
  }

  // Search over page titles, headings and text. The index is loaded on first
  // use as a script so it also works from file:// URLs.
  function setupSearch() {
    var input = document.getElementById("search-input");
    var results = document.getElementById("search-results");
    if (!input || !results) return;
    var box = input.parentNode;
    var loading = false;

    function withIndex(done) {
      if (window.HEARTBEAT_SEARCH) return done();
      if (loading) return;
      loading = true;
      var s = document.createElement("script");
      s.src = root + (document.body.getAttribute("data-search-index") || "assets/search-index.js");
      s.onload = function () { loading = false; done(); };
      s.onerror = function () { loading = false; };
      document.head.appendChild(s);
    }

    function el(tag, className, text) {
      var e = document.createElement(tag);
      if (className) e.className = className;
      if (text) e.textContent = text;
      return e;
    }

    function snippet(text, term) {
      var i = text.toLowerCase().indexOf(term);
      if (i < 0) return "";
      var start = Math.max(0, i - 50);
      var end = Math.min(text.length, i + term.length + 70);
      return (start > 0 ? "…" : "") + text.slice(start, end).trim() + (end < text.length ? "…" : "");
    }

    function search(query) {
      var terms = query.toLowerCase().split(/\s+/).filter(Boolean);
      var found = [];
      (window.HEARTBEAT_SEARCH || []).forEach(function (page) {
        var headings = page.h || [];
        var title = page.t.toLowerCase();
        var hay = title + " " + headings.map(function (h) { return h.t; }).join(" ").toLowerCase() + " " + page.x.toLowerCase();
        if (!terms.every(function (t) { return hay.indexOf(t) >= 0; })) return;
        var score = 0;
        terms.forEach(function (t) {
          if (title.indexOf(t) >= 0) score += 10;
          if (headings.some(function (h) { return h.t.toLowerCase().indexOf(t) >= 0; })) score += 3;
        });
        var section = null;
        for (var i = 0; i < headings.length && !section; i++) {
          var ht = headings[i].t.toLowerCase();
          if (terms.every(function (t) { return ht.indexOf(t) >= 0; })) section = headings[i];
        }
        if (page.a) score -= 20;
        found.push({ page: page, section: section, score: score });
      });
      found.sort(function (a, b) { return b.score - a.score || a.page.t.localeCompare(b.page.t); });
      return { terms: terms, hits: found.slice(0, 15) };
    }

    function render() {
      var query = input.value.trim();
      box.classList.toggle("active", query.length > 0);
      results.hidden = query.length === 0;
      results.textContent = "";
      if (!query) return;
      var r = search(query);
      if (!r.hits.length) {
        results.appendChild(el("p", "empty", window.HEARTBEAT_SEARCH ? "No matches." : "Loading…"));
        return;
      }
      var list = el("ul");
      r.hits.forEach(function (hit) {
        var a = el("a");
        a.href = root + hit.page.u + (hit.section ? "#" + hit.section.id : "");
        var title = el("span", "result-title", hit.page.t);
        if (hit.page.a) title.appendChild(el("span", "result-tag", "archived"));
        a.appendChild(title);
        if (hit.section) a.appendChild(el("span", "result-section", hit.section.t));
        var text = snippet(hit.page.x, r.terms[0]);
        if (text) a.appendChild(el("span", "result-snippet", text));
        var li = el("li");
        li.appendChild(a);
        list.appendChild(li);
      });
      results.appendChild(list);
    }

    input.addEventListener("focus", function () { withIndex(render); });
    input.addEventListener("input", function () { withIndex(render); render(); });
    input.addEventListener("keydown", function (e) {
      if (e.key === "Escape") { input.value = ""; render(); }
      if (e.key === "Enter") {
        var first = results.querySelector("a");
        if (first) window.location.href = first.href;
      }
    });
    document.addEventListener("keydown", function (e) {
      var tag = (document.activeElement && document.activeElement.tagName) || "";
      if (e.key === "/" && tag !== "INPUT" && tag !== "TEXTAREA") {
        e.preventDefault();
        var sidebar = document.getElementById("sidebar");
        var menu = document.querySelector(".menu-button");
        if (menu && window.getComputedStyle(menu.parentNode).display !== "none" && !sidebar.classList.contains("open")) menu.click();
        input.focus();
      }
    });
  }

  // Diagram styling for the HTML site: flat, thin-bordered boxes in tinted
  // colour ramps with a bold title line and lighter detail lines, dashed
  // unfilled groups with left-aligned titles, thin gray connectors and a
  // legend. Light and dark variants follow the page's colour scheme. The
  // Markdown diagram sources stay the single source of content (nodes,
  // edges, labels); only their look changes here. Status classes keep the
  // hues the docs' legends name: blue implemented, amber partial, dashed
  // gray planned or idle.
  function ramp(fill, stroke, title, detail, dashed) {
    return { fill: fill, stroke: stroke, title: title, detail: detail, dashed: !!dashed };
  }
  var diagramThemes = {
    light: {
      ramps: {
        ok: ramp("#e6f1fb", "#185fa5", "#0c447c", "#185fa5"),
        partial: ramp("#faeeda", "#ba7517", "#633806", "#854f0b"),
        idle: ramp("#f8f7f3", "#888780", "#444441", "#5f5e5a", true),
        planned: ramp("#f8f7f3", "#888780", "#444441", "#5f5e5a", true),
        external: ramp("#f1efe8", "#b4b2a9", "#2c2c2a", "#5f5e5a")
      },
      // The known-gap edge stays amber.
      colors: { "#d97706": "#ba7517", "#b45309": "#854f0b" },
      canvas: "#ffffff", line: "#888780", edgeText: "#5f5e5a",
      groupStroke: "#b4b2a9", groupTitle: "#444441", text: "#2c2c2a"
    },
    dark: {
      ramps: {
        ok: ramp("#0c447c", "#85b7eb", "#e6f1fb", "#b5d4f4"),
        partial: ramp("#633806", "#ef9f27", "#faeeda", "#fac775"),
        idle: ramp("#262624", "#888780", "#f1efe8", "#b4b2a9", true),
        planned: ramp("#262624", "#888780", "#f1efe8", "#b4b2a9", true),
        external: ramp("#444441", "#b4b2a9", "#f1efe8", "#d3d1c7")
      },
      colors: { "#d97706": "#ef9f27", "#b45309": "#fac775" },
      canvas: "#1f1e1d", line: "#9c9a92", edgeText: "#b4b2a9",
      groupStroke: "#5f5e5a", groupTitle: "#d3d1c7", text: "#f1efe8"
    }
  };
  var legendLabels = { ok: "Implemented", partial: "Partial", idle: "Idle", planned: "Planned", external: "External, config, people" };

  function classStyle(r) {
    return "fill:" + r.fill + ",stroke:" + r.stroke + ",stroke-width:1px," +
      (r.dashed ? "stroke-dasharray:5 4," : "") + "color:" + r.detail;
  }

  function diagramVars(t, font) {
    var dark = t === diagramThemes.dark;
    return {
      darkMode: dark, fontFamily: font, fontSize: "14px",
      background: t.canvas, primaryColor: t.ramps.external.fill, primaryTextColor: t.text,
      primaryBorderColor: t.ramps.external.stroke, secondaryColor: t.ramps.external.fill,
      tertiaryColor: t.canvas, lineColor: t.line, textColor: t.text,
      clusterBkg: "transparent", clusterBorder: t.groupStroke,
      edgeLabelBackground: t.canvas, titleColor: t.text,
      attributeBackgroundColorOdd: t.canvas, attributeBackgroundColorEven: dark ? "#262624" : "#f8f7f3"
    };
  }

  function diagramCSS(t, flowchart) {
    var rules = [
      ".node rect, .node .label-container { rx: 4px; ry: 4px; }",
      ".cluster rect { fill: none !important; stroke: " + t.groupStroke + " !important; stroke-width: 1px !important; stroke-dasharray: 5 4; rx: 8px; ry: 8px; }",
      ".cluster-label, .cluster-label span, .cluster-label p { font-weight: 600; font-size: 14px; color: " + t.groupTitle + " !important; }",
      ".nodeLabel, .nodeLabel p { font-size: 12px; line-height: 1.5; }",
      // Unstyled edge labels are muted; an edge's own colour (linkStyle) wins.
      ".edgeLabel span:not([style*='color']) { color: " + t.edgeText + "; }",
      ".edgeLabel p, .edgeLabel span { font-size: 12px; }",
      ".edgeLabel p { color: inherit; }",
      ".edgeLabel p, .labelBkg { background-color: " + t.canvas + " !important; }",
      ".flowchart-link { stroke-width: 1px; }",
      ".marker, marker path { fill: " + t.line + "; stroke: " + t.line + "; }",
      ".er.entityBox, .er.attributeBoxOdd, .er.attributeBoxEven { stroke: " + t.ramps.external.stroke + "; }",
      ".er.entityLabel, .er.relationshipLabel { fill: " + t.text + "; }",
      ".er.relationshipLabelBox { fill: " + t.canvas + "; opacity: 1; }",
      ".er.relationshipLine { stroke: " + t.line + "; }"
    ];
    if (flowchart) {
      // The first label line is the component's title, in the ramp's
      // darkest stop; later lines are details in a lighter stop.
      rules.push(".nodeLabel p::first-line { font-size: 14px; font-weight: 600; color: " + t.text + "; }");
      Object.keys(t.ramps).forEach(function (name) {
        rules.push(".node." + name + " .nodeLabel p::first-line { color: " + t.ramps[name].title + "; }");
      });
    }
    return rules.join("\n");
  }

  // Mermaid centres group titles; move them to the group's top-left corner.
  function alignGroupTitles(svg) {
    Array.prototype.forEach.call(svg.querySelectorAll("g.cluster"), function (group) {
      var rect = group.querySelector("rect");
      var label = group.querySelector(".cluster-label");
      if (!rect || !label) return;
      var x = parseFloat(rect.getAttribute("x")) + 14;
      var y = parseFloat(rect.getAttribute("y")) + 10;
      label.setAttribute("transform", "translate(" + x + ", " + y + ")");
    });
  }

  // A legend for the status classes a diagram uses.
  function diagramLegend(source, t) {
    var used = {};
    source.split("\n").forEach(function (line) {
      var m = line.match(/^\s*class\s+\S+\s+(\w+)\s*$/);
      if (m && t.ramps[m[1]]) used[m[1]] = true;
    });
    var names = Object.keys(legendLabels).filter(function (n) { return used[n] && !(n === "planned" && used.idle); });
    if (!names.length) return null;
    var legend = document.createElement("div");
    legend.className = "diagram-legend";
    names.forEach(function (name) {
      var r = t.ramps[name];
      var item = document.createElement("span");
      var swatch = document.createElement("span");
      swatch.className = "diagram-swatch";
      swatch.style.background = r.fill;
      swatch.style.border = "1px " + (r.dashed ? "dashed " : "solid ") + r.stroke;
      item.appendChild(swatch);
      item.appendChild(document.createTextNode(name === "idle" && used.planned ? "Planned or idle" : legendLabels[name]));
      legend.appendChild(item);
    });
    return legend;
  }

  function styleDiagramSource(source, t) {
    return source.split("\n").map(function (line) {
      var m = line.match(/^(\s*)classDef\s+(\w+)\s/);
      if (m && t.ramps[m[2]]) return m[1] + "classDef " + m[2] + " " + classStyle(t.ramps[m[2]]);
      if (/^\s*(linkStyle|style)\s/.test(line)) {
        return line.replace(/#[0-9a-fA-F]{6}\b/g, function (c) { return t.colors[c.toLowerCase()] || c; });
      }
      return line;
    }).join("\n");
  }

  function setupDiagrams() {
    var blocks = Array.prototype.slice.call(document.querySelectorAll(".diagram"));
    if (!blocks.length) return;
    var sources = blocks.map(function (b) { return b.querySelector("code").textContent; });
    function note(text) {
      var p = document.createElement("p");
      p.className = "diagram-error";
      p.textContent = text;
      return p;
    }
    if (!window.mermaid) {
      blocks.forEach(function (b) {
        b.insertBefore(note("Diagram could not be rendered (mermaid.js did not load); showing its source."), b.firstChild);
      });
      return;
    }
    var font = getComputedStyle(document.body).fontFamily;
    var dark = window.matchMedia("(prefers-color-scheme: dark)");
    var generation = 0;
    function renderAll() {
      var run = ++generation;
      var t = diagramThemes[dark.matches ? "dark" : "light"];
      var vars = diagramVars(t, font);
      blocks.reduce(function (prev, block, i) {
        return prev.then(function () {
          if (run !== generation) return;
          window.mermaid.initialize({
            startOnLoad: false,
            securityLevel: "strict",
            theme: "base",
            themeVariables: vars,
            themeCSS: diagramCSS(t, /^\s*(flowchart|graph)\b/.test(sources[i])),
            fontFamily: font,
            flowchart: { curve: "basis", padding: 14, nodeSpacing: 40, rankSpacing: 56 },
            er: { fontSize: 14 }
          });
          return window.mermaid.render("diagram-" + run + "-" + i, styleDiagramSource(sources[i], t)).then(function (out) {
            if (run !== generation) return;
            block.innerHTML = out.svg;
            alignGroupTitles(block.querySelector("svg"));
            var legend = diagramLegend(sources[i], t);
            if (legend) block.appendChild(legend);
            addExpandButton(block);
          }, function (err) {
            if (block.querySelector(".diagram-error")) return;
            block.insertBefore(note("Diagram could not be rendered: " + (err && err.message ? err.message : err)), block.firstChild);
          });
        });
      }, Promise.resolve()).then(function () {
        // Rendering changes the page height; keep an anchor target in view.
        var target = run === 1 && window.location.hash && document.getElementById(decodeURIComponent(window.location.hash.slice(1)));
        if (target && window.scrollY > 0) target.scrollIntoView();
      });
    }
    renderAll();
    if (dark.addEventListener) dark.addEventListener("change", renderAll);
  }

  // Wide diagrams are shrunk to fit the column; the expand button shows one
  // full screen at actual size, scrollable, with a fit-to-screen toggle.
  var viewer = null;
  function openViewer(block) {
    var svg = block.querySelector("svg");
    if (!svg) return;
    if (!viewer) {
      viewer = document.createElement("dialog");
      viewer.className = "diagram-viewer";
      viewer.innerHTML =
        '<div class="diagram-viewer-bar">' +
        '<button type="button" data-action="fit">Fit to screen</button>' +
        '<button type="button" data-action="close">Close</button>' +
        "</div>" +
        '<div class="diagram-viewer-body"></div>';
      document.body.appendChild(viewer);
      viewer.addEventListener("click", function (e) {
        var action = e.target.getAttribute && e.target.getAttribute("data-action");
        if (action === "close") viewer.close();
        if (action === "fit") {
          var fit = viewer.classList.toggle("fit");
          e.target.textContent = fit ? "Actual size" : "Fit to screen";
        }
      });
      viewer.addEventListener("close", function () {
        var body = viewer.querySelector(".diagram-viewer-body");
        var moved = body.querySelector("svg");
        if (moved && viewer.origin) {
          moved.style.width = "";
          viewer.origin.insertBefore(moved, viewer.origin.firstChild);
          var button = viewer.origin.querySelector(".diagram-expand");
          if (button) button.focus();
        }
        viewer.origin = null;
      });
    }
    var box = svg.viewBox && svg.viewBox.baseVal;
    if (box && box.width) svg.style.width = Math.ceil(box.width) + "px";
    viewer.origin = block;
    viewer.classList.remove("fit");
    viewer.querySelector('[data-action="fit"]').textContent = "Fit to screen";
    viewer.querySelector(".diagram-viewer-body").appendChild(svg);
    viewer.showModal();
  }

  function addExpandButton(block) {
    var button = document.createElement("button");
    button.type = "button";
    button.className = "diagram-expand";
    button.textContent = "Expand";
    button.setAttribute("aria-label", "Expand diagram to full screen");
    button.addEventListener("click", function () { openViewer(block); });
    block.appendChild(button);
  }

  function init() {
    setupDrawer();
    setupTocHighlight();
    setupSearch();
    setupDiagrams();
  }
  // This script is deferred, so DOMContentLoaded is still ahead, and it only
  // fires after the deferred mermaid.js script that follows has run.
  document.addEventListener("DOMContentLoaded", init);
})();
