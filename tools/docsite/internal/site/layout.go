package site

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"path"
	"strings"

	"github.com/yuin/goldmark/ast"
)

// MermaidURL is the only external resource the site loads, and only on
// pages with diagrams. The integrity hash pins the exact file.
const (
	MermaidURL       = "https://cdn.jsdelivr.net/npm/mermaid@11.17.2/dist/mermaid.min.js"
	mermaidIntegrity = "sha384-EOXBFmc3gx5mb+vn0vPvvGqACToJD24hhacX5Yx+8NUUQrHIle/Qi5Bg9o3zKwW2"
)

var (
	//go:embed assets/style.css
	styleCSS []byte
	//go:embed assets/site.js
	siteJS []byte
	//go:embed assets/page.html
	pageHTML string

	pageTemplate = template.Must(template.New("page").Parse(pageHTML))
)

// navGroup is one sidebar section. Items hold pages rather than links
// because hrefs are relative to the page being rendered.
type navGroup struct {
	Label string
	Note  string
	Items []navEntry
}

type navEntry struct {
	Label    string
	Page     *page
	Children []navEntry
}

type navData struct {
	Home    *page
	Groups  []navGroup
	Archive *page
}

// buildNav mirrors the "By audience" section of docs/README.md: each bold
// paragraph starts a group and the page links in the list that follows are
// its entries. Pages in the same directory as a linked README (the ADRs)
// nest under it, pages linked nowhere go to "More pages", and archived pages
// are reachable only from the Archive page.
func (b *builder) buildNav() (navData, error) {
	home := b.bySrc[homeSource]
	nav := navData{Home: home, Archive: b.bySrc[archiveDir+"/README.md"]}
	inNav := map[*page]bool{home: true, nav.Archive: true}

	var section ast.Node
	for n := home.doc.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 2 && plainText(h, home.source) == navHeading {
			section = n.NextSibling()
			break
		}
	}
	for n := section; n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level <= 2 {
			break
		}
		switch n := n.(type) {
		case *ast.Paragraph:
			if em, ok := n.FirstChild().(*ast.Emphasis); ok && em.Level == 2 {
				label, note, _ := strings.Cut(plainText(em, home.source), " (")
				nav.Groups = append(nav.Groups, navGroup{Label: label, Note: strings.TrimSuffix(note, ")")})
			}
		case *ast.List:
			if len(nav.Groups) == 0 {
				continue
			}
			group := &nav.Groups[len(nav.Groups)-1]
			_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
				if link, ok := c.(*ast.Link); ok && entering {
					if target := home.links[link]; target != nil && !inNav[target] {
						group.Items = append(group.Items, navEntry{Label: plainText(link, home.source), Page: target})
						inNav[target] = true
					}
				}
				return ast.WalkContinue, nil
			})
		}
	}
	if len(nav.Groups) == 0 {
		return nav, fmt.Errorf("%s: no audience groups found under %q", homeSource, navHeading)
	}

	for g := range nav.Groups {
		for i, entry := range nav.Groups[g].Items {
			if path.Base(entry.Page.src) != "README.md" || strings.HasPrefix(entry.Page.src, archiveDir+"/") {
				continue
			}
			dir := path.Dir(entry.Page.src)
			for _, p := range b.pages {
				if path.Dir(p.src) == dir && !inNav[p] {
					nav.Groups[g].Items[i].Children = append(nav.Groups[g].Items[i].Children, navEntry{Label: p.title, Page: p})
					inNav[p] = true
				}
			}
		}
	}

	more := navGroup{Label: "More pages"}
	for _, p := range b.pages {
		if !inNav[p] && !strings.HasPrefix(p.src, archiveDir+"/") {
			more.Items = append(more.Items, navEntry{Label: p.title, Page: p})
		}
	}
	if len(more.Items) > 0 {
		nav.Groups = append(nav.Groups, more)
	}
	return nav, nil
}

type link struct {
	Label    string
	Href     string
	Current  bool
	ID       string
	Open     bool
	Children []link
}

type group struct {
	ID    string
	Label string
	Note  string
	Open  bool
	Links []link
}

type pageView struct {
	Title        string
	Root         string
	Home         link
	Groups       []group
	Archive      *link
	Archived     bool
	Crumbs       []link
	Prev, Next   *link
	TOC          []heading
	Body         template.HTML
	SourcePath   string
	SourceURL    string
	EditURL      string
	RepoURL      string
	Diagram      bool
	MermaidURL   string
	MermaidHash  string
	Assets       map[string]string
	IsHome       bool
	GeneratorCmd string
}

// readingOrder is the sidebar flattened: home, then each group's pages with
// their children. It drives the previous and next page links.
func (nav navData) readingOrder() []navEntry {
	order := []navEntry{{Label: "Home", Page: nav.Home}}
	for _, g := range nav.Groups {
		for _, e := range g.Items {
			order = append(order, navEntry{Label: e.Label, Page: e.Page})
			order = append(order, e.Children...)
		}
	}
	return order
}

// groupOf names the sidebar group a page sits in, for breadcrumbs and the
// "where" line of search results. parent is the entry a child nests under.
func (nav navData) groupOf(p *page) (label string, parent *navEntry) {
	switch {
	case p == nav.Home:
		return "", nil
	case p == nav.Archive || p.archived:
		return "Archive", nil
	}
	for _, g := range nav.Groups {
		for i, e := range g.Items {
			if e.Page == p {
				return g.Label, nil
			}
			for _, c := range e.Children {
				if c.Page == p {
					return g.Label, &g.Items[i]
				}
			}
		}
	}
	return "", nil
}

func (b *builder) layout(p *page, nav navData, body []byte) ([]byte, error) {
	toLink := func(label string, target *page) link {
		return link{Label: label, Href: relPath(p.out, target.out), Current: target == p}
	}
	view := pageView{
		Title:        p.title,
		Root:         rootPrefix(p.out),
		Home:         toLink("Home", nav.Home),
		Archived:     p.archived,
		Body:         template.HTML(body),
		SourcePath:   p.src,
		SourceURL:    b.repoLink("blob", p.src),
		EditURL:      b.repoLink("edit", p.src),
		RepoURL:      b.cfg.RepoURL,
		Diagram:      p.diagram,
		MermaidURL:   MermaidURL,
		MermaidHash:  mermaidIntegrity,
		IsHome:       p == nav.Home,
		GeneratorCmd: "make docs-site",
		Assets:       b.assets,
	}
	ids := newSlugger()
	for _, g := range nav.Groups {
		vg := group{ID: ids.slug(g.Label), Label: g.Label, Note: g.Note}
		for _, e := range g.Items {
			l := toLink(e.Label, e.Page)
			l.ID = vg.ID + "-" + ids.slug(e.Label)
			l.Open = l.Current
			for _, c := range e.Children {
				cl := toLink(c.Label, c.Page)
				l.Open = l.Open || cl.Current
				l.Children = append(l.Children, cl)
			}
			vg.Open = vg.Open || l.Open
			vg.Links = append(vg.Links, l)
		}
		view.Groups = append(view.Groups, vg)
	}
	if nav.Archive != nil {
		l := toLink("Archive", nav.Archive)
		l.Current = p == nav.Archive || p.archived
		view.Archive = &l
	}

	if label, parent := nav.groupOf(p); p != nav.Home {
		view.Crumbs = append(view.Crumbs, toLink("Home", nav.Home))
		switch {
		case label == "Archive" && p != nav.Archive && nav.Archive != nil:
			view.Crumbs = append(view.Crumbs, toLink("Archive", nav.Archive))
		case label != "" && label != "Archive":
			view.Crumbs = append(view.Crumbs, link{Label: label})
		}
		if parent != nil {
			view.Crumbs = append(view.Crumbs, toLink(parent.Label, parent.Page))
		}
	}
	order := nav.readingOrder()
	for i, e := range order {
		if e.Page != p {
			continue
		}
		if i > 0 {
			l := toLink(order[i-1].Label, order[i-1].Page)
			view.Prev = &l
		}
		if i+1 < len(order) {
			l := toLink(order[i+1].Label, order[i+1].Page)
			view.Next = &l
		}
		break
	}

	for _, h := range p.headings {
		if h.Level == 2 || h.Level == 3 {
			view.TOC = append(view.TOC, h)
		}
	}
	if len(view.TOC) < 2 {
		view.TOC = nil
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, view); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// searchIndex lists every page once and every h1-h3 section of every page
// with its text, so a result can open the matching section. It is a script
// rather than JSON so search also works when the site is opened from the
// filesystem, where fetch() is blocked.
func (b *builder) searchIndex(nav navData) ([]byte, error) {
	type pageEntry struct {
		Title    string `json:"t"`
		URL      string `json:"u"`
		Group    string `json:"g,omitempty"`
		Archived bool   `json:"r,omitempty"`
		// Order is the page's place in the sidebar; pages outside it sort last.
		Order int `json:"o"`
	}
	type sectionEntry struct {
		Page    int    `json:"p"`
		Heading string `json:"h"`
		Anchor  string `json:"a,omitempty"`
		Level   int    `json:"l"`
		Text    string `json:"x"`
	}
	index := struct {
		Pages    []pageEntry    `json:"pages"`
		Sections []sectionEntry `json:"sections"`
	}{Pages: []pageEntry{}, Sections: []sectionEntry{}}
	order := map[*page]int{}
	for i, e := range nav.readingOrder() {
		order[e.Page] = i
	}
	for i, p := range b.pages {
		o, listed := order[p]
		if !listed {
			o = len(order) + i
		}
		group, parent := nav.groupOf(p)
		if p == nav.Home {
			group = "Home"
		} else if group == "" {
			group = "More pages"
		}
		if parent != nil {
			group += " · " + parent.Label
		}
		index.Pages = append(index.Pages, pageEntry{Title: p.title, URL: p.out, Group: group, Archived: p.archived, Order: o})
		for _, s := range p.sections {
			e := sectionEntry{Page: i, Heading: p.title, Level: 1, Text: strings.Join(s.text, " ")}
			if s.heading >= 0 {
				h := p.headings[s.heading]
				e.Heading, e.Level = h.Text, h.Level
				if h.Level > 1 {
					e.Anchor = h.ID
				}
			}
			index.Sections = append(index.Sections, e)
		}
	}
	data, err := json.Marshal(index)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("// Generated by make docs-site. Do not edit.\nwindow.HEARTBEAT_SEARCH = ")
	out.Write(data)
	out.WriteString(";\n")
	return out.Bytes(), nil
}
