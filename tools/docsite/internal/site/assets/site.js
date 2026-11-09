// Atrium docs site behaviour: mobile drawer, collapsible menus, colour
// theme, on-this-page highlighting, the search dialog, search highlights and
// Mermaid diagrams. Generated into docs/site/assets by make docs-site.
(function () {
  "use strict";

  var root = document.body.getAttribute("data-root") || "";
  var docEl = document.documentElement;

  // localStorage can be missing or throw (private windows, file:// in some
  // browsers); every use goes through these.
  function load(key) {
    try { return window.localStorage.getItem(key); } catch (e) { return null; }
  }
  function save(key, value) {
    try {
      if (value === null) window.localStorage.removeItem(key);
      else window.localStorage.setItem(key, value);
    } catch (e) { /* not persisted */ }
  }

  // A fragment such as #%E0 is not valid percent-encoding; decoding must not
  // throw, or the rest of the page's behaviour would not start.
  function decodeId(fragment) {
    try { return decodeURIComponent(fragment); } catch (e) { return fragment; }
  }

  function isTyping(target) {
    var tag = ((target && target.tagName) || "").toLowerCase();
    return tag === "input" || tag === "textarea" || tag === "select" || !!(target && target.isContentEditable);
  }

  // ---------------------------------------------------------------- drawer
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

  // ------------------------------------------------------------------ menus
  // Groups open and close from their heading, and the choice is remembered.
  // The group holding the current page always opens. Child lists (the ADRs)
  // start closed unless the current page is one of them.
  var NAV_KEY = "atrium-docs-nav";
  function setupNav() {
    var sidebar = document.getElementById("sidebar");
    if (!sidebar) return;
    var closed = {};
    try { closed = JSON.parse(load(NAV_KEY) || "{}") || {}; } catch (e) { closed = {}; }

    Array.prototype.forEach.call(sidebar.querySelectorAll(".nav-group"), function (group) {
      var id = group.getAttribute("data-group");
      var toggle = group.querySelector(".nav-group-toggle");
      function set(open) {
        group.classList.toggle("collapsed", !open);
        toggle.setAttribute("aria-expanded", String(open));
      }
      set(!closed[id] || group.hasAttribute("data-current"));
      toggle.addEventListener("click", function () {
        var open = toggle.getAttribute("aria-expanded") !== "true";
        set(open);
        if (open) delete closed[id]; else closed[id] = true;
        save(NAV_KEY, JSON.stringify(closed));
      });
    });

    Array.prototype.forEach.call(sidebar.querySelectorAll(".nav-children-toggle"), function (toggle) {
      var list = document.getElementById(toggle.getAttribute("aria-controls"));
      if (!list) return;
      toggle.addEventListener("click", function () {
        var open = toggle.getAttribute("aria-expanded") !== "true";
        toggle.setAttribute("aria-expanded", String(open));
        if (open) list.removeAttribute("data-collapsed"); else list.setAttribute("data-collapsed", "");
      });
    });

    // Bring the current page into view in a long sidebar.
    var current = sidebar.querySelector("a[aria-current=page]");
    if (current) {
      var s = sidebar.getBoundingClientRect(), c = current.getBoundingClientRect();
      if (c.top < s.top || c.bottom > s.bottom) {
        sidebar.scrollTop += c.top - s.top - sidebar.clientHeight / 3;
      }
    }
  }

  // ------------------------------------------------------------------ theme
  // Automatic (follow the system), light or dark; the inline script in the
  // page head applies a stored choice before first paint.
  var THEME_KEY = "atrium-docs-theme";
  var darkQuery = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : { matches: false };
  function isDark() {
    var t = docEl.getAttribute("data-theme");
    return t === "dark" || (t !== "light" && darkQuery.matches);
  }
  function setupTheme() {
    var button = document.querySelector(".theme-button");
    if (!button) return;
    var modes = ["auto", "light", "dark"];
    var names = { auto: "automatic", light: "light", dark: "dark" };
    function show(mode) {
      button.setAttribute("data-mode", mode);
      var next = modes[(modes.indexOf(mode) + 1) % modes.length];
      var label = "Colour theme: " + names[mode] + ". Switch to " + names[next] + ".";
      button.setAttribute("aria-label", label);
      button.title = label;
    }
    show(docEl.getAttribute("data-theme") || "auto");
    button.addEventListener("click", function () {
      var mode = modes[(modes.indexOf(button.getAttribute("data-mode")) + 1) % modes.length];
      if (mode === "auto") docEl.removeAttribute("data-theme"); else docEl.setAttribute("data-theme", mode);
      save(THEME_KEY, mode === "auto" ? null : mode);
      show(mode);
      window.dispatchEvent(new Event("atrium-theme"));
    });
  }

  // --------------------------------------------------------- on this page
  // Highlight the on-this-page entry for the section being read. A section is
  // current once its heading passes a reading line near the top of the
  // window. Sections near the end of a page can never scroll that far, so
  // over the last screen of scrolling the line slides down to the bottom of
  // the window, and each remaining heading becomes current in turn. A section
  // picked from the list stays current until the reader scrolls by hand.
  function setupTocHighlight() {
    var toc = document.querySelector(".toc");
    var links = Array.prototype.slice.call(document.querySelectorAll(".toc li a"));
    if (!toc || !links.length) return;
    var targets = links.map(function (a) {
      return document.getElementById(decodeId(a.getAttribute("href").slice(1)));
    });
    var top = 120;
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
      a.addEventListener("click", function () { pick(decodeId(a.getAttribute("href").slice(1))); });
    });
    ["wheel", "touchmove", "keydown"].forEach(function (type) {
      window.addEventListener(type, function () { picked = -1; }, { passive: true });
    });
    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    if (window.location.hash) pick(decodeId(window.location.hash.slice(1)));
    update();
  }

  // ----------------------------------------------------------------- search
  // A dialog over the page, opened with Ctrl+K / Cmd+K or "/". The index
  // lists every page and every h1-h3 section with its text; it is loaded as
  // a script on first use so search also works from file:// URLs.
  function norm(s) {
    return (s || "").toLowerCase().normalize("NFD").replace(/[̀-ͯ]/g, "");
  }
  function esc(s) {
    return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }
  function reEsc(s) { return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }

  // A query is words that must all match, or one "exact phrase" in quotes.
  function parseQuery(q) {
    q = q.trim();
    var m = q.match(/^"(.+)"$/);
    if (m) { var exact = norm(m[1]).trim(); return { exact: true, phrase: exact, terms: [exact] }; }
    var terms = norm(q).split(/\s+/).filter(Boolean);
    return { exact: false, phrase: terms.length > 1 ? terms.join(" ") : null, terms: terms };
  }

  var ICONS = {
    page: '<svg viewBox="0 0 20 20" width="15" height="15" aria-hidden="true"><path d="M5 2.5h6.5L15 6v11.5H5z M11.5 2.5V6H15" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/></svg>',
    section: '<svg viewBox="0 0 20 20" width="15" height="15" aria-hidden="true"><path d="M7.5 3L6 17M14 3l-1.5 14M3.5 7.5h13M3 12.5h13" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>',
    text: '<svg viewBox="0 0 20 20" width="15" height="15" aria-hidden="true"><path d="M4 5h12M4 9h12M4 13h8" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>'
  };

  function setupSearch() {
    var box = document.getElementById("search");
    var input = document.getElementById("search-input");
    var list = document.getElementById("search-results");
    var meta = document.getElementById("search-meta");
    if (!box || !input || !list || !meta) return;
    var data = null, pages = null, failed = false, waiting = null;
    var options = [], selected = -1, timer = null, lastFocus = null;

    var mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");
    Array.prototype.forEach.call(document.querySelectorAll("[data-mod-key]"), function (k) { k.textContent = mac ? "⌘" : "Ctrl"; });

    // Callers that arrive while the index loads (a hover prefetch, then the
    // click that opens the dialog) all run once it is in.
    function withIndex(done) {
      if (data) return done();
      if (failed) return;
      meta.textContent = "Loading the search index…";
      if (waiting) { waiting.push(done); return; }
      waiting = [done];
      var s = document.createElement("script");
      s.src = root + (document.body.getAttribute("data-search-index") || "assets/search-index.js");
      s.onload = function () {
        var index = window.ATRIUM_SEARCH || { pages: [], sections: [] };
        pages = index.pages.map(function (p) { p._t = norm(p.t); return p; });
        data = index.sections.map(function (s) {
          var page = pages[s.p];
          return { page: page, h: s.h, a: s.a || "", l: s.l, x: s.x || "", _h: norm(s.h), _x: norm(s.x) };
        });
        var calls = waiting;
        waiting = null;
        calls.forEach(function (f) { f(); });
      };
      s.onerror = function () {
        waiting = null; failed = true;
        meta.textContent = "The search index could not be loaded.";
      };
      document.head.appendChild(s);
    }

    function count(hay, t) {
      var c = 0, i = hay.indexOf(t);
      while (i !== -1 && c < 20) { c++; i = hay.indexOf(t, i + t.length); }
      return c;
    }

    function search(q) {
      var p = parseQuery(q), out = [];
      var wordStart = p.terms.map(function (t) { return new RegExp("(^|[^a-z0-9])" + reEsc(t)); });
      data.forEach(function (e) {
        var score = 0, inHead = true;
        for (var i = 0; i < p.terms.length; i++) {
          var t = p.terms[i];
          var h = e._h.indexOf(t) !== -1, pt = e.page._t.indexOf(t) !== -1, c = count(e._x, t);
          if (!h && !pt && !c) return;
          if (!h && !(pt && e.l === 1)) inHead = false;
          score += (h ? 30 : 0) + (pt ? 12 : 0) + Math.min(c, 8) * 2;
          if (h && wordStart[i].test(e._h)) score += 10;
        }
        if (p.phrase) {
          if (e._h.indexOf(p.phrase) !== -1) score += 40;
          else if (e._x.indexOf(p.phrase) !== -1) score += 15;
          else if (p.exact) return;
        }
        if (e.l === 1 && inHead) score += 25;
        if (e.l === 2) score += 3;
        // Results are listed by group: titles and headings that match, then
        // mentions in the text, then archived pages.
        var rank = e.page.r ? 2 : inHead ? 0 : 1;
        out.push({ e: e, score: score, rank: rank });
      });
      out.sort(function (a, b) { return a.rank - b.rank || b.score - a.score || a.e.page.t.localeCompare(b.e.page.t); });
      return out;
    }

    function mark(text, p) {
      var terms = (p.phrase && !p.exact ? [p.phrase] : []).concat(p.terms)
        .filter(function (t) { return t.length > 0; })
        .sort(function (a, b) { return b.length - a.length; });
      if (!terms.length) return esc(text);
      // Match on the raw text and escape each piece, so a query such as
      // "amp" never splits an entity like &amp;.
      var re = new RegExp("(" + terms.map(reEsc).join("|") + ")", "gi");
      return text.split(re).map(function (piece, i) {
        return i % 2 ? "<mark>" + esc(piece) + "</mark>" : esc(piece);
      }).join("");
    }

    function snippet(e, p) {
      var x = e.x, nx = x.length === e._x.length ? e._x : x.toLowerCase(), pos = -1;
      if (p.phrase) pos = nx.indexOf(p.phrase);
      for (var i = 0; i < p.terms.length && pos === -1; i++) pos = nx.indexOf(p.terms[i]);
      if (pos === -1) return x.length > 160 ? x.slice(0, 160) + "…" : x;
      var start = Math.max(0, pos - 60), end = Math.min(x.length, pos + 130);
      while (start > 0 && start > pos - 80 && x[start - 1] !== " ") start--;
      return (start > 0 ? "…" : "") + x.slice(start, end).trim() + (end < x.length ? "…" : "");
    }

    function href(e, q) {
      return root + e.page.u + (q ? "?hl=" + encodeURIComponent(q.trim()) : "") + (e.a ? "#" + e.a : "");
    }

    function option(i, url, icon, where, title, text, tags) {
      return '<li role="option" id="search-option-' + i + '">' +
        '<a href="' + esc(url) + '" tabindex="-1">' +
        '<span class="search-icon">' + ICONS[icon] + "</span>" +
        '<span class="search-where">' + esc(where) + "</span>" +
        '<span class="search-title">' + title + (tags || "") + "</span>" +
        (text ? '<span class="search-snippet">' + text + "</span>" : "") +
        "</a></li>";
    }

    // With no query the dialog lists every current page by sidebar group, so
    // it doubles as a quick page switcher.
    function renderPages() {
      var html = [], group = null, n = 0;
      pages.slice().sort(function (a, b) { return a.o - b.o; }).forEach(function (page) {
        if (page.r) return;
        if (page.g !== group) {
          group = page.g;
          html.push('<li class="search-group" role="presentation">' + esc(group) + "</li>");
        }
        html.push(option(n++, root + page.u, "page", group, esc(page.t)));
      });
      meta.textContent = "Jump to a page, or type to search every section.";
      return html;
    }

    function render() {
      var q = input.value, html;
      if (q.trim().replace(/"/g, "").length < 2) {
        html = renderPages();
      } else {
        var p = parseQuery(q), hits = search(q), shown = hits.slice(0, 50);
        var pageCount = {};
        hits.forEach(function (r) { pageCount[r.e.page.u] = 1; });
        var pc = Object.keys(pageCount).length;
        meta.textContent = hits.length
          ? hits.length + (hits.length === 1 ? " match" : " matches") + " in " + pc + (pc === 1 ? " page" : " pages") +
            (hits.length > shown.length ? " · showing the best " + shown.length : "")
          : "";
        html = [];
        var group = null;
        shown.forEach(function (r, i) {
          var g = ["Pages and sections", "Mentions in the text", "Archived pages"][r.rank];
          if (g !== group) { html.push('<li class="search-group" role="presentation">' + g + "</li>"); group = g; }
          var e = r.e, where = e.page.g + (e.l > 1 ? " · " + e.page.t : "");
          var tags = e.page.r ? '<span class="search-tag archived">Archived</span>' : "";
          html.push(option(i, href(e, q), e.l === 1 ? "page" : (r.rank === 1 ? "text" : "section"), where, mark(e.h, p), e.x ? mark(snippet(e, p), p) : "", tags));
        });
        if (!shown.length) {
          html.push('<li class="search-empty" role="presentation">No results for “' + esc(q.trim()) + "”. Try fewer or shorter words.</li>");
        }
      }
      list.innerHTML = html.join("");
      options = Array.prototype.slice.call(list.querySelectorAll("li[role=option]"));
      select(options.length ? 0 : -1, true);
    }

    function select(i, keepScroll) {
      if (options[selected]) { options[selected].classList.remove("selected"); options[selected].removeAttribute("aria-selected"); }
      selected = i;
      if (!options[i]) { input.removeAttribute("aria-activedescendant"); return; }
      options[i].classList.add("selected");
      options[i].setAttribute("aria-selected", "true");
      input.setAttribute("aria-activedescendant", options[i].id);
      if (!keepScroll) options[i].scrollIntoView({ block: "nearest" });
      else list.scrollTop = 0;
    }

    function open() {
      if (!box.hidden) { input.select(); return; }
      lastFocus = document.activeElement;
      box.hidden = false;
      document.body.style.overflow = "hidden";
      input.focus();
      input.select();
      withIndex(render);
    }
    function close() {
      box.hidden = true;
      document.body.style.overflow = "";
      if (lastFocus && lastFocus.focus) lastFocus.focus();
    }

    Array.prototype.forEach.call(document.querySelectorAll("[data-search-open]"), function (b) {
      b.addEventListener("click", open);
      b.addEventListener("pointerenter", function () { withIndex(function () {}); }, { once: true });
    });
    // A result on the current page only changes the address's #section, so
    // the browser does not load a new page: close the dialog when one is
    // followed, unless it opens in a new tab.
    box.addEventListener("click", function (e) {
      if (e.target.closest("[data-search-close]")) close();
      else if (e.target.closest("li[role=option] a") && !(e.metaKey || e.ctrlKey || e.shiftKey || e.button)) close();
    });
    // Coming back to a page from the browser history restores it as it was
    // left, dialog included.
    window.addEventListener("pageshow", function (e) {
      if (e.persisted && !box.hidden) close();
    });
    list.addEventListener("mousemove", function (e) {
      var li = e.target.closest("li[role=option]");
      var i = options.indexOf(li);
      if (i >= 0 && i !== selected) select(i, true);
    });
    input.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(function () { if (data) render(); }, 80);
    });
    input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown" || (e.ctrlKey && e.key === "n")) { e.preventDefault(); if (options.length) select((selected + 1) % options.length); }
      else if (e.key === "ArrowUp" || (e.ctrlKey && e.key === "p")) { e.preventDefault(); if (options.length) select((selected - 1 + options.length) % options.length); }
      else if (e.key === "Enter") {
        var a = options[selected] && options[selected].querySelector("a");
        if (a) {
          e.preventDefault();
          if (e.metaKey || e.ctrlKey) window.open(a.href, "_blank");
          else { close(); window.location.href = a.href; }
        }
      }
    });
    // Focus stays in the dialog while it is open.
    box.addEventListener("keydown", function (e) {
      if (e.key === "Tab") { e.preventDefault(); input.focus(); }
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && !box.hidden) { e.preventDefault(); close(); return; }
      var mod = (e.metaKey || e.ctrlKey) && !e.altKey && (e.key === "k" || e.key === "K");
      if (mod || (e.key === "/" && !e.metaKey && !e.ctrlKey && !e.altKey && !isTyping(e.target))) {
        e.preventDefault();
        if (mod && !box.hidden) close(); else open();
      }
    });
  }

  // After opening a search result (?hl=words), mark the words on the page and
  // offer previous / next / clear. The first match at or after the linked
  // section is shown.
  function setupHighlights() {
    var q = null;
    try { q = new URLSearchParams(window.location.search).get("hl"); } catch (e) { return; }
    var article = document.querySelector("article.markdown");
    var bar = document.getElementById("hlbar");
    if (!q || !article || !bar) return;
    var input = document.getElementById("search-input");
    if (input) input.value = q;

    var p = parseQuery(q);
    var terms = p.exact ? [p.phrase] : p.terms;
    // Unquoted words that appear together on the page highlight as a phrase.
    if (!p.exact && p.phrase && norm(article.textContent).indexOf(p.phrase) !== -1) terms = [p.phrase];
    terms = terms.filter(function (t) { return t.length >= 2; }).sort(function (a, b) { return b.length - a.length; });
    if (!terms.length) return;
    var re = new RegExp(terms.map(reEsc).join("|"), "gi");
    var walker = document.createTreeWalker(article, NodeFilter.SHOW_TEXT, {
      acceptNode: function (n) {
        if (!n.nodeValue.trim() || n.parentNode.closest("script, style, button, .anchor, .diagram, mark")) return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      }
    });
    var nodes = [], node;
    while ((node = walker.nextNode())) nodes.push(node);
    var marks = [];
    nodes.forEach(function (n) {
      if (marks.length > 500) return;
      var text = n.nodeValue, folded = norm(text);
      if (folded.length !== text.length) folded = text.toLowerCase();
      re.lastIndex = 0;
      var m, last = 0, frag = null;
      while ((m = re.exec(folded)) !== null) {
        if (!frag) frag = document.createDocumentFragment();
        frag.appendChild(document.createTextNode(text.slice(last, m.index)));
        var mk = document.createElement("mark");
        mk.className = "hl";
        mk.textContent = text.slice(m.index, m.index + m[0].length);
        frag.appendChild(mk);
        marks.push(mk);
        last = m.index + m[0].length;
        if (m[0].length === 0) re.lastIndex++;
      }
      if (frag) { frag.appendChild(document.createTextNode(text.slice(last))); n.parentNode.replaceChild(frag, n); }
    });
    if (!marks.length) return;

    var text = document.getElementById("hlbar-text");
    // The first match at or after the section the address names.
    function firstInSection() {
      var target = window.location.hash ? document.getElementById(decodeId(window.location.hash.slice(1))) : null;
      if (!target) return 0;
      for (var i = 0; i < marks.length; i++) {
        if (target.contains(marks[i]) || (target.compareDocumentPosition(marks[i]) & Node.DOCUMENT_POSITION_FOLLOWING)) return i;
      }
      return 0;
    }
    var cur = firstInSection();
    function show(i, scroll) {
      if (marks[cur]) marks[cur].classList.remove("current");
      cur = (i + marks.length) % marks.length;
      var m = marks[cur];
      var d = m.closest("details");
      if (d) d.open = true;
      m.classList.add("current");
      if (scroll) m.scrollIntoView({ block: "center" });
      text.textContent = (cur + 1) + " of " + marks.length + (marks.length > 500 ? "+" : "") + " · “" + q + "”";
    }
    function clear() {
      marks.forEach(function (m) { m.replaceWith(document.createTextNode(m.textContent)); });
      article.normalize();
      bar.hidden = true;
      try {
        var url = new URL(window.location.href);
        url.searchParams.delete("hl");
        window.history.replaceState(null, "", url.pathname + url.search + url.hash);
      } catch (e) { /* the address keeps ?hl */ }
    }
    bar.addEventListener("click", function (e) {
      var action = e.target.closest("[data-hl]");
      if (!action) return;
      var a = action.getAttribute("data-hl");
      if (a === "prev") show(cur - 1, true);
      if (a === "next") show(cur + 1, true);
      if (a === "clear") clear();
    });
    bar.hidden = false;
    // Scroll only when the match is not already in view (the browser has
    // scrolled to the linked section by now).
    window.setTimeout(function () {
      var r = marks[cur].getBoundingClientRect();
      show(cur, r.top < 0 || r.bottom > window.innerHeight);
    }, 60);
    // Another result on this page, or a link to one of its sections: count
    // from there, without scrolling away from the section the reader picked.
    window.addEventListener("hashchange", function () {
      if (!bar.hidden) show(firstInSection(), false);
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
    var generation = 0;
    function renderAll() {
      var run = ++generation;
      var t = diagramThemes[isDark() ? "dark" : "light"];
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
        var target = run === 1 && window.location.hash && document.getElementById(decodeId(window.location.hash.slice(1)));
        if (target && window.scrollY > 0) target.scrollIntoView();
      });
    }
    renderAll();
    if (darkQuery.addEventListener) darkQuery.addEventListener("change", renderAll);
    window.addEventListener("atrium-theme", renderAll);
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

  // The compact on-this-page box reads best under the page title.
  function placeInlineToc() {
    var toc = document.querySelector(".toc-inline");
    var title = document.querySelector("article.markdown > h1");
    if (toc && title) title.insertAdjacentElement("afterend", toc);
  }

  function init() {
    placeInlineToc();
    setupDrawer();
    setupNav();
    setupTheme();
    setupTocHighlight();
    setupSearch();
    setupHighlights();
    setupDiagrams();
  }
  // This script is deferred, so DOMContentLoaded is still ahead, and it only
  // fires after the deferred mermaid.js script that follows has run.
  document.addEventListener("DOMContentLoaded", init);
})();
