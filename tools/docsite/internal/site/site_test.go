package site

import (
	"bytes"
	"encoding/json"
	"html"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testRepoURL = "https://example.test/atrium"

// fixtureRoot is a small documentation tree kept with the tests, so they
// exercise the generator, not the repository's current docs. The real docs
// are validated by the build itself (make docs-site), which fails on broken
// links, images and anchors.
const fixtureRoot = "testdata/repo"

func buildFixtureSite(t *testing.T) (Config, map[string][]byte) {
	t.Helper()
	cfg := Config{Root: fixtureRoot, OutDir: "docs/site", RepoURL: testRepoURL, Ref: "main"}
	files, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, files
}

var (
	hrefPattern = regexp.MustCompile(`\s(?:href|src)="([^"]*)"`)
	idPattern   = regexp.MustCompile(`\sid="([^"]*)"`)
)

// TestSiteLinksResolve checks every link in the generated site: links
// between pages must reach an existing page and, when they carry one, an
// existing anchor; links to repository files must name a file or directory
// that exists.
func TestSiteLinksResolve(t *testing.T) {
	cfg, files := buildFixtureSite(t)
	ids := map[string]map[string]bool{}
	for name, content := range files {
		if strings.HasSuffix(name, ".html") {
			ids[name] = map[string]bool{}
			for _, m := range idPattern.FindAllSubmatch(content, -1) {
				ids[name][html.UnescapeString(string(m[1]))] = true
			}
		}
	}
	checked, repoLinks := 0, 0
	for name, content := range files {
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		for _, m := range hrefPattern.FindAllSubmatch(content, -1) {
			raw := html.UnescapeString(string(m[1]))
			checked++
			if strings.HasPrefix(raw, cfg.RepoURL+"/") {
				repoLinks++
				checkRepoLink(t, cfg, name, raw)
				continue
			}
			if raw == MermaidURL || hasScheme(raw) || strings.HasPrefix(raw, "//") {
				continue
			}
			target, fragment, _ := strings.Cut(raw, "#")
			target, _, _ = strings.Cut(target, "?")
			if target == "" {
				target = name
			} else {
				target = path.Clean(path.Join(path.Dir(name), target))
			}
			if _, ok := files[target]; !ok {
				t.Errorf("%s: link %q: no page %s in the site", name, raw, target)
				continue
			}
			if fragment == "" {
				continue
			}
			anchor, err := url.PathUnescape(fragment)
			if err != nil {
				t.Errorf("%s: link %q: %v", name, raw, err)
			} else if !ids[target][anchor] {
				t.Errorf("%s: link %q: no id %q in %s", name, raw, anchor, target)
			}
		}
	}
	if checked < 100 || repoLinks == 0 {
		t.Fatalf("checked %d links (%d to repository files); link extraction is probably broken", checked, repoLinks)
	}
}

func checkRepoLink(t *testing.T, cfg Config, page, link string) {
	t.Helper()
	rest := strings.TrimPrefix(link, cfg.RepoURL+"/")
	rest, _, _ = strings.Cut(rest, "#")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || (parts[0] != "blob" && parts[0] != "tree" && parts[0] != "edit") || parts[1] != cfg.Ref {
		t.Errorf("%s: malformed repository link %q", page, link)
		return
	}
	info, err := os.Stat(filepath.Join(cfg.Root, filepath.FromSlash(parts[2])))
	if err != nil {
		t.Errorf("%s: repository link %q: %v", page, link, err)
		return
	}
	if info.IsDir() != (parts[0] == "tree") { // blob and edit links name files
		t.Errorf("%s: repository link %q uses the wrong kind for %s", page, link, parts[2])
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	_, first := buildFixtureSite(t)
	_, second := buildFixtureSite(t)
	if len(first) != len(second) {
		t.Fatalf("builds produced %d and %d files", len(first), len(second))
	}
	for name, content := range first {
		if !bytes.Equal(content, second[name]) {
			t.Errorf("%s differs between builds", name)
		}
	}
}

func TestSiteContents(t *testing.T) {
	_, files := buildFixtureSite(t)
	for _, want := range []string{
		"index.html",
		"product/overview.html",
		"architecture/overview.html",
		"architecture/decisions/index.html",
		"architecture/decisions/0001-example.html",
		"guides/setup.html",
		"archive/index.html",
		"archive/old-plan.html",
		"services/demo/index.html",
	} {
		if files[want] == nil {
			t.Errorf("missing %s", want)
		}
	}

	if !strings.Contains(string(files["archive/old-plan.html"]), `class="archived-banner"`) {
		t.Error("archived pages must carry the archived banner")
	}
	for name, content := range files {
		if strings.HasSuffix(name, ".html") && !strings.HasPrefix(name, "archive/") &&
			bytes.Contains(content, []byte("old-plan.html")) {
			t.Errorf("%s: archived pages must stay out of the sidebar", name)
		}
	}

	home := string(files["index.html"])
	for _, group := range []string{"New readers", "Developers"} {
		if !regexp.MustCompile(`(?s)<h2 class="nav-group-title">\s*<button class="nav-group-toggle"[^>]*>\s*<span>` + group + `</span>`).MatchString(home) {
			t.Errorf("sidebar is missing the %q audience group", group)
		}
	}
	if !strings.Contains(home, `<p class="nav-note">clients, newcomers</p>`) {
		t.Error("an audience group's parenthetical must become its note")
	}
	// The ADR nests under the decisions index it shares a directory with.
	if !regexp.MustCompile(`(?s)decisions/index\.html"[^<]*>Decisions</a>.*?<ul class="nav-list nav-children" id="[^"]+" data-collapsed>.*?0001-example\.html`).MatchString(home) {
		t.Error("ADR pages must nest, collapsed, under the decisions index in the sidebar")
	}
	adr := string(files["architecture/decisions/0001-example.html"])
	if !regexp.MustCompile(`<ul class="nav-list nav-children" id="[^"]+">`).MatchString(adr) {
		t.Error("the ADR list must start open on an ADR page")
	}
	if !strings.Contains(string(files["architecture/overview.html"]), `<div class="table-wrap"><table>`) {
		t.Error("tables must be wrapped in a scroll container")
	}
	if !strings.Contains(home, testRepoURL+"/blob/main/config/app.yaml") ||
		!strings.Contains(home, testRepoURL+"/tree/main/docs/guides") {
		t.Error("links to repository files and directories must point at the repository")
	}
}

// Every page but home carries breadcrumbs that name its sidebar group, and
// pages link to their neighbours in sidebar order.
func TestBreadcrumbsAndPager(t *testing.T) {
	_, files := buildFixtureSite(t)
	home := string(files["index.html"])
	if strings.Contains(home, `class="breadcrumbs"`) || strings.Contains(home, `rel="prev"`) {
		t.Error("the home page has no breadcrumbs and no previous page")
	}
	if !strings.Contains(home, `<a class="pager-link pager-next" href="product/overview.html" rel="next">`) {
		t.Error("home must link to the first sidebar page as next")
	}
	adr := string(files["architecture/decisions/0001-example.html"])
	for _, want := range []string{
		`<li><a href="../../index.html">Home</a></li>`,
		`<li><span>Developers</span></li>`,
		`<li><a href="index.html">Decisions</a></li>`,
		`<li><span aria-current="page">0001. Example decision</span></li>`,
		`href="index.html" rel="prev"`,
	} {
		if !strings.Contains(adr, want) {
			t.Errorf("ADR page is missing %s", want)
		}
	}
	if !strings.Contains(adr, testRepoURL+"/edit/main/docs/architecture/decisions/0001-example.md") {
		t.Error("pages must link to their source for editing")
	}
	old := string(files["archive/old-plan.html"])
	if !strings.Contains(old, `<li><a href="index.html">Archive</a></li>`) || strings.Contains(old, `class="pager"`) {
		t.Error("archived pages sit under the archive in breadcrumbs and have no pager")
	}
}

// The search index lists each page once, in sidebar order, and each h1-h3
// section with its own text and anchor.
func TestSearchIndexSections(t *testing.T) {
	_, files := buildFixtureSite(t)
	raw := string(files["assets/search-index.js"])
	raw = strings.TrimSuffix(strings.TrimSpace(raw[strings.Index(raw, "=")+1:]), ";")
	var index struct {
		Pages []struct {
			URL      string `json:"u"`
			Archived bool   `json:"r"`
			Order    int    `json:"o"`
		} `json:"pages"`
		Sections []struct {
			Page    int    `json:"p"`
			Heading string `json:"h"`
			Anchor  string `json:"a"`
			Level   int    `json:"l"`
			Text    string `json:"x"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(raw), &index); err != nil {
		t.Fatal(err)
	}
	byURL := map[string]int{}
	for i, p := range index.Pages {
		byURL[p.URL] = i
	}
	setup, ok := byURL["guides/setup.html"]
	if !ok {
		t.Fatal("guides/setup.html is not in the search index")
	}
	if index.Pages[byURL["index.html"]].Order != 0 || index.Pages[byURL["product/overview.html"]].Order != 1 {
		t.Error("pages must carry their sidebar order")
	}
	if !index.Pages[byURL["archive/old-plan.html"]].Archived {
		t.Error("archived pages must be marked in the index")
	}
	var levels []int
	for _, s := range index.Sections {
		if s.Page != setup {
			continue
		}
		levels = append(levels, s.Level)
		if s.Level > 1 && s.Anchor == "" {
			t.Errorf("section %q has no anchor", s.Heading)
		}
		if s.Level == 1 && s.Anchor != "" {
			t.Errorf("the page section %q must link to the page, not an anchor", s.Heading)
		}
	}
	if len(levels) < 2 || levels[0] != 1 {
		t.Errorf("setup guide sections have levels %v; want the page first, then its sections", levels)
	}
}

// Mermaid diagrams must reach mermaid.js exactly as written in the Markdown.
func TestDiagramSourcesUnchanged(t *testing.T) {
	cfg, files := buildFixtureSite(t)
	source, err := os.ReadFile(filepath.Join(cfg.Root, "docs/architecture/overview.md"))
	if err != nil {
		t.Fatal(err)
	}
	fences := regexp.MustCompile("(?s)```mermaid\n(.*?)```").FindAllSubmatch(source, -1)
	rendered := regexp.MustCompile(`(?s)<pre class="diagram-source"><code>(.*?)</code></pre>`).FindAllSubmatch(files["architecture/overview.html"], -1)
	if len(fences) == 0 || len(fences) != len(rendered) {
		t.Fatalf("found %d mermaid blocks in Markdown and %d in HTML", len(fences), len(rendered))
	}
	for i := range fences {
		if got := html.UnescapeString(string(rendered[i][1])); got != string(fences[i][1]) {
			t.Errorf("diagram %d changed between Markdown and HTML", i+1)
		}
	}
	if !bytes.Contains(files["architecture/overview.html"], []byte(MermaidURL)) {
		t.Error("pages with diagrams must load mermaid.js")
	}
	if bytes.Contains(files["guides/setup.html"], []byte(MermaidURL)) {
		t.Error("pages without diagrams must not load mermaid.js")
	}
}

// Embedded images are copied into the site byte for byte.
func TestImagesCopied(t *testing.T) {
	cfg, files := buildFixtureSite(t)
	if !strings.Contains(string(files["architecture/overview.html"]), `src="diagrams/flow.svg"`) {
		t.Error("overview does not embed diagrams/flow.svg")
	}
	want, err := os.ReadFile(filepath.Join(cfg.Root, "docs/architecture/diagrams/flow.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(files["architecture/diagrams/flow.svg"], want) {
		t.Error("architecture/diagrams/flow.svg is missing or differs from the repository file")
	}
}

// The build is what validates the real docs, so it must fail, naming the
// file and line, on anything a reader would find broken.
func TestBuildRejectsBrokenReferences(t *testing.T) {
	for _, tc := range []struct{ name, page, want string }{
		{"missing page", "See [nowhere](missing.md).", "docs/guides/a.md:3: missing.md: no such file"},
		{"missing anchor", "See [b](b.md#nope).", "docs/guides/a.md:3: #nope: no such heading in docs/guides/b.md"},
		{"missing same-page anchor", "See [above](#nope).", "docs/guides/a.md:3: #nope: no such heading in docs/guides/a.md"},
		{"missing image", "![Gone](gone.svg)", "docs/guides/a.md:3: image gone.svg: no such file"},
		{"outside the repository", "See [up](../../../x.md).", "docs/guides/a.md:3: ../../../x.md points outside the repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "docs/README.md", "# Docs\n\n## By audience\n\n**Readers**\n\n- [A](guides/a.md)\n")
			writeFile(t, root, "docs/guides/a.md", "# A\n\n"+tc.page+"\n")
			writeFile(t, root, "docs/guides/b.md", "# B\n\n## Real heading\n")
			_, err := Build(Config{Root: root, OutDir: "docs/site", RepoURL: testRepoURL, Ref: "main"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSlugMatchesGitHub(t *testing.T) {
	s := newSlugger()
	for _, tc := range []struct{ in, want string }{
		{"Core systems", "core-systems"},
		{"`integrations.yaml`", "integrationsyaml"},
		{"DB collector (`:8082`)", "db-collector-8082"},
		{"Prometheus → Alertmanager", "prometheus--alertmanager"},
		{"A. API / control-plane service (`apps/api`)", "a-api--control-plane-service-appsapi"},
		{"snake_case & Co.", "snake_case--co"},
		{"Core systems", "core-systems-1"},
		{"Core systems", "core-systems-2"},
		{"Core systems 1", "core-systems-1-1"},
	} {
		if got := s.slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOutPath(t *testing.T) {
	for src, want := range map[string]string{
		"docs/README.md":                        "index.html",
		"docs/product/overview.md":              "product/overview.html",
		"docs/architecture/decisions/README.md": "architecture/decisions/index.html",
		"services/db-collector/README.md":       "services/db-collector/index.html",
	} {
		if got := outPath(src); got != want {
			t.Errorf("outPath(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestWriteRemovesStaleFiles(t *testing.T) {
	root := t.TempDir()
	cfg := Config{Root: root, OutDir: "site"}
	if err := Write(cfg, map[string][]byte{"index.html": []byte("old"), "a/b.html": []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := Write(cfg, map[string][]byte{"index.html": []byte("new"), "c.html": []byte("c")}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"index.html": "new", "c.html": "c"} {
		got, err := os.ReadFile(filepath.Join(root, "site", name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "site", "a")); !os.IsNotExist(err) {
		t.Error("Write must remove files and directories the build no longer produces")
	}
}

var (
	logoPathPattern  = regexp.MustCompile(`<path [^>]*?(?:fill="(#[0-9A-Fa-f]{6})"|class="lg-([a-z-]+)") d="([^"]+)"`)
	logoTokenPattern = regexp.MustCompile(`--logo-([a-z-]+):\s*(#[0-9a-f]{6});`)
)

// TestHeaderLogoMatchesBrand keeps the header logo inlined in page.html in
// step with its sources: the paths must be those of docs/brand/logo.svg, in
// the same order, and the --logo-* tokens must carry the fills of logo.svg in
// the light theme and of logo-dark.svg in both dark blocks of style.css.
func TestHeaderLogoMatchesBrand(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	brand := filepath.Join("..", "..", "..", "..", "docs", "brand")
	light := logoPathPattern.FindAllStringSubmatch(read(filepath.Join(brand, "logo.svg")), -1)
	dark := logoPathPattern.FindAllStringSubmatch(read(filepath.Join(brand, "logo-dark.svg")), -1)

	page := read(filepath.Join("assets", "page.html"))
	start := strings.Index(page, `<svg class="brand-logo"`)
	if start < 0 {
		t.Fatal("page.html has no header logo (svg.brand-logo)")
	}
	header := logoPathPattern.FindAllStringSubmatch(page[start:start+strings.Index(page[start:], "</svg>")], -1)
	if len(light) == 0 || len(header) != len(light) || len(dark) != len(light) {
		t.Fatalf("path counts differ: header %d, logo.svg %d, logo-dark.svg %d", len(header), len(light), len(dark))
	}

	// The tokens appear three times: the light theme, the dark media query
	// and the explicit dark theme.
	var themes []map[string]string
	for _, m := range logoTokenPattern.FindAllStringSubmatch(read(filepath.Join("assets", "style.css")), -1) {
		// A token that repeats starts the next theme block.
		if len(themes) == 0 || themes[len(themes)-1][m[1]] != "" {
			themes = append(themes, map[string]string{})
		}
		themes[len(themes)-1][m[1]] = m[2]
	}
	if len(themes) != 3 {
		t.Fatalf("style.css defines the --logo-* tokens in %d blocks, want 3", len(themes))
	}

	for i, h := range header {
		class := h[2]
		if class == "" {
			t.Errorf("header path %d has a fixed fill, want an lg-* class", i)
			continue
		}
		if h[3] != light[i][3] {
			t.Errorf("header path %d (lg-%s) differs from logo.svg", i, class)
		}
		for j, want := range []string{light[i][1], dark[i][1], dark[i][1]} {
			if got := themes[j][class]; got != strings.ToLower(want) {
				t.Errorf("--logo-%s in block %d of style.css is %q, want %q", class, j+1, got, strings.ToLower(want))
			}
		}
	}
}

var (
	faviconPattern  = regexp.MustCompile(`<link rel="icon" href="data:image/svg\+xml,([^"]+)"`)
	betweenTagsGaps = regexp.MustCompile(`>\s+<`)
)

// TestBrandCopiesMatch keeps the other copies of the arch listed in
// docs/brand/README.md in step: mark*.svg is logo*.svg without the wordmark,
// icon.svg and app-badge.svg draw the arch of mark-dark.svg, and the favicon
// inlined in page.html is icon.svg.
func TestBrandCopiesMatch(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	brand := filepath.Join("..", "..", "..", "..", "docs", "brand")
	paths := func(name string) [][]string {
		return logoPathPattern.FindAllStringSubmatch(read(filepath.Join(brand, name)), -1)
	}

	for _, pair := range [][2]string{{"mark.svg", "logo.svg"}, {"mark-dark.svg", "logo-dark.svg"}} {
		mark, logo := paths(pair[0]), paths(pair[1])
		// The wordmark is the last path of the lockup.
		if len(mark) == 0 || len(mark) != len(logo)-1 {
			t.Fatalf("%s has %d paths, want those of %s (%d) without the wordmark", pair[0], len(mark), pair[1], len(logo))
		}
		for i := range mark {
			if mark[i][1] != logo[i][1] || mark[i][3] != logo[i][3] {
				t.Errorf("%s path %d differs from %s", pair[0], i, pair[1])
			}
		}
	}

	// The tiles have their own keystone colours, so only the shapes and the
	// two tiles' fills are compared.
	arch, icon, badge := paths("mark-dark.svg"), paths("icon.svg"), paths("app-badge.svg")
	if len(icon) != len(arch) || len(badge) != len(arch) {
		t.Fatalf("path counts differ: icon.svg %d, app-badge.svg %d, mark-dark.svg %d", len(icon), len(badge), len(arch))
	}
	for i := range arch {
		if icon[i][3] != arch[i][3] {
			t.Errorf("icon.svg path %d differs from mark-dark.svg", i)
		}
		if badge[i][3] != arch[i][3] || badge[i][1] != icon[i][1] {
			t.Errorf("app-badge.svg path %d differs from icon.svg", i)
		}
	}

	m := faviconPattern.FindStringSubmatch(read(filepath.Join("assets", "page.html")))
	if m == nil {
		t.Fatal("page.html has no inlined SVG favicon")
	}
	favicon, err := url.PathUnescape(m[1])
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(betweenTagsGaps.ReplaceAllString(strings.TrimSpace(read(filepath.Join(brand, "icon.svg"))), "><"), `"`, "'")
	if favicon != want {
		t.Error("the favicon in page.html differs from docs/brand/icon.svg")
	}
}
