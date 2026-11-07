// Package site renders the repository's Markdown documentation into a static
// HTML site. The Markdown stays the source of truth; the generated site is
// built in CI and published to GitHub Pages, never committed.
package site

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Sources are the published Markdown files, as repository-relative globs.
// docs/plans/ and docs/reviews/ are untracked decision-review drafts and are
// deliberately absent.
var Sources = []string{
	"docs/README.md",
	"docs/product/*.md",
	"docs/architecture/*.md",
	"docs/architecture/decisions/*.md",
	"docs/guides/*.md",
	"docs/reference/*.md",
	"docs/archive/*.md",
	"services/*/README.md",
}

const (
	homeSource = "docs/README.md"
	archiveDir = "docs/archive"
	// navHeading is the section of docs/README.md that the sidebar mirrors.
	navHeading = "By audience"
)

// Config locates the repository and the published output.
type Config struct {
	// Root is the repository root directory.
	Root string
	// OutDir is the output directory, relative to Root.
	OutDir string
	// RepoURL is the repository's web URL; links to non-page files
	// (code, YAML, SQL, schemas) point at RepoURL/blob/Ref/<path>.
	RepoURL string
	Ref     string
}

type heading struct {
	Level int    `json:"-"`
	Text  string `json:"t"`
	ID    string `json:"id"`
}

type page struct {
	src      string // repository-relative Markdown path
	out      string // site-relative HTML path
	title    string
	archived bool
	source   []byte
	doc      ast.Node
	headings []heading
	// sections split the page's searchable text at its h1-h3 headings, so
	// search can link straight to the section that matches.
	sections []section
	diagram  bool
	// links maps each Link node that points at another page to that page;
	// the sidebar is built from these on the home page.
	links map[*ast.Link]*page
	// anchors are the #fragment links to published pages, checked against
	// the target's heading IDs once every page is parsed.
	anchors []anchorRef
}

// section is the text under one heading, up to the next h1-h3. heading
// indexes page.headings; -1 is text that comes before any heading.
type section struct {
	heading int
	text    []string
}

type anchorRef struct {
	line     int
	target   *page
	fragment string
}

type builder struct {
	cfg   Config
	pages []*page
	bySrc map[string]*page
	md    goldmark.Markdown
	// assets maps each asset path to its versioned URL path.
	assets map[string]string
	// images maps the site path of each image a page embeds to its
	// repository path; the images are copied into the site.
	images map[string]string
}

// Build renders every source page and returns the site's files keyed by
// site-relative, slash-separated path. Output is deterministic.
func Build(cfg Config) (map[string][]byte, error) {
	if cfg.RepoURL == "" || cfg.Ref == "" {
		return nil, errors.New("repository URL and ref are required")
	}
	cfg.RepoURL = strings.TrimSuffix(cfg.RepoURL, "/")
	b := &builder{
		cfg:    cfg,
		bySrc:  map[string]*page{},
		images: map[string]string{},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithRendererOptions(renderer.WithNodeRenderers(
				util.Prioritized(codeBlockRenderer{}, 100),
			)),
		),
	}
	if err := b.discover(); err != nil {
		return nil, err
	}
	var problems []string
	for _, p := range b.pages {
		problems = append(problems, b.parse(p)...)
	}
	problems = append(problems, b.checkAnchors()...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("broken links in Markdown:\n  %s", strings.Join(problems, "\n  "))
	}
	nav, err := b.buildNav()
	if err != nil {
		return nil, err
	}
	index, err := b.searchIndex(nav)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"assets/search-index.js": index,
		"assets/style.css":       styleCSS,
		"assets/site.js":         siteJS,
	}
	for out, src := range b.images {
		content, err := os.ReadFile(filepath.Join(b.cfg.Root, filepath.FromSlash(src)))
		if err != nil {
			return nil, err
		}
		files[out] = content
	}
	// Pages reference assets with a content hash so browsers never reuse a
	// stale copy after a rebuild.
	b.assets = map[string]string{}
	for name, content := range files {
		sum := sha256.Sum256(content)
		b.assets[name] = name + "?v=" + hex.EncodeToString(sum[:6])
	}
	for _, p := range b.pages {
		var body bytes.Buffer
		if err := b.md.Renderer().Render(&body, p.source, p.doc); err != nil {
			return nil, fmt.Errorf("render %s: %w", p.src, err)
		}
		html, err := b.layout(p, nav, wrapTables(body.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("layout %s: %w", p.src, err)
		}
		files[p.out] = html
	}
	return files, nil
}

func (b *builder) discover() error {
	for _, pattern := range Sources {
		matches, err := filepath.Glob(filepath.Join(b.cfg.Root, filepath.FromSlash(pattern)))
		if err != nil {
			return err
		}
		for _, m := range matches {
			rel, err := filepath.Rel(b.cfg.Root, m)
			if err != nil {
				return err
			}
			src := filepath.ToSlash(rel)
			if _, dup := b.bySrc[src]; dup {
				continue
			}
			p := &page{src: src, out: outPath(src), links: map[*ast.Link]*page{}}
			p.archived = strings.HasPrefix(src, archiveDir+"/") && path.Base(src) != "README.md"
			b.pages = append(b.pages, p)
			b.bySrc[src] = p
		}
	}
	if b.bySrc[homeSource] == nil {
		return fmt.Errorf("%s not found under %s", homeSource, b.cfg.Root)
	}
	sort.Slice(b.pages, func(i, j int) bool { return b.pages[i].src < b.pages[j].src })
	return nil
}

// outPath maps docs/<dir>/<name>.md to <dir>/<name>.html and any other
// <path>/<name>.md to <path>/<name>.html; README.md becomes index.html.
func outPath(src string) string {
	out := strings.TrimPrefix(src, "docs/")
	if path.Base(out) == "README.md" {
		return path.Join(path.Dir(out), "index.html")
	}
	return strings.TrimSuffix(out, ".md") + ".html"
}

// parse reads and parses one page, assigns GitHub-style heading IDs, and
// rewrites its links. It returns one message per link that does not resolve.
func (b *builder) parse(p *page) []string {
	source, err := os.ReadFile(filepath.Join(b.cfg.Root, filepath.FromSlash(p.src)))
	if err != nil {
		return []string{err.Error()}
	}
	p.source = source
	p.doc = b.md.Parser().Parse(text.NewReader(source), parser.WithContext(parser.NewContext()))

	var problems []string
	var headingNodes []*ast.Heading
	addText := func(t string) {
		if t == "" {
			return
		}
		if len(p.sections) == 0 {
			p.sections = append(p.sections, section{heading: -1})
		}
		last := &p.sections[len(p.sections)-1]
		last.text = append(last.text, t)
	}
	slugs := newSlugger()
	_ = ast.Walk(p.doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Heading:
			h := heading{Level: n.Level, Text: plainText(n, source)}
			h.ID = slugs.slug(h.Text)
			n.SetAttributeString("id", []byte(h.ID))
			p.headings = append(p.headings, h)
			if n.Level == 1 && p.title == "" {
				p.title = h.Text
			}
			headingNodes = append(headingNodes, n)
			if n.Level <= 3 {
				p.sections = append(p.sections, section{heading: len(p.headings) - 1})
			} else {
				addText(h.Text)
			}
		case *ast.Paragraph, *ast.TextBlock, *east.TableCell:
			addText(plainText(n, source))
		case *ast.FencedCodeBlock:
			if string(n.Language(source)) == "mermaid" {
				p.diagram = true
			}
			return ast.WalkSkipChildren, nil
		case *ast.Image:
			dest, err := b.resolveImage(p, string(n.Destination))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %s", p.src, lineOf(n, source), err))
				return ast.WalkContinue, nil
			}
			n.Destination = []byte(dest)
		case *ast.Link:
			dest, target, err := b.resolve(p, string(n.Destination))
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %s", p.src, lineOf(n, source), err))
				return ast.WalkContinue, nil
			}
			raw := string(n.Destination)
			n.Destination = []byte(dest)
			if target != nil {
				p.links[n] = target
			}
			if _, fragment, ok := strings.Cut(raw, "#"); ok && fragment != "" {
				if strings.HasPrefix(raw, "#") {
					target = p
				}
				if target != nil {
					p.anchors = append(p.anchors, anchorRef{line: lineOf(n, source), target: target, fragment: fragment})
				}
			}
		}
		return ast.WalkContinue, nil
	})
	if p.title == "" {
		p.title = strings.TrimSuffix(path.Base(p.src), ".md")
	}
	// Anchor links are appended after the walk so they stay out of the
	// heading text used for IDs, the table of contents and search.
	for _, n := range headingNodes {
		id, _ := n.AttributeString("id")
		anchor := ast.NewLink()
		anchor.Destination = append([]byte("#"), asBytes(id)...)
		anchor.SetAttributeString("class", []byte("anchor"))
		anchor.SetAttributeString("title", []byte("Link to this section"))
		anchor.AppendChild(anchor, ast.NewString([]byte("#")))
		n.AppendChild(n, anchor)
	}
	return problems
}

// checkAnchors reports every #fragment link to a published page that names
// no heading on that page.
func (b *builder) checkAnchors() []string {
	ids := map[*page]map[string]bool{}
	for _, p := range b.pages {
		ids[p] = map[string]bool{}
		for _, h := range p.headings {
			ids[p][h.ID] = true
		}
	}
	var problems []string
	for _, p := range b.pages {
		for _, a := range p.anchors {
			fragment, err := url.PathUnescape(a.fragment)
			if err != nil || !ids[a.target][fragment] {
				problems = append(problems, fmt.Sprintf("%s:%d: #%s: no such heading in %s", p.src, a.line, a.fragment, a.target.src))
			}
		}
	}
	return problems
}

// resolve rewrites a Markdown link destination for the page's output
// location. Links to other published pages become relative .html links,
// links to any other repository file or directory point at the repository
// web URL, and external or same-page links are kept. It also returns the
// target page when there is one.
func (b *builder) resolve(p *page, dest string) (string, *page, error) {
	if dest == "" {
		return "", nil, errors.New("empty link")
	}
	if strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "//") || hasScheme(dest) {
		return dest, nil, nil
	}
	pathPart, fragment, _ := strings.Cut(dest, "#")
	if fragment != "" {
		fragment = "#" + fragment
	}
	target := path.Clean(path.Join(path.Dir(p.src), pathPart))
	if target == ".." || strings.HasPrefix(target, "../") {
		return "", nil, fmt.Errorf("%s points outside the repository", dest)
	}
	if tp := b.bySrc[target]; tp != nil {
		return relPath(p.out, tp.out) + fragment, tp, nil
	}
	info, err := os.Stat(filepath.Join(b.cfg.Root, filepath.FromSlash(target)))
	if err != nil {
		return "", nil, fmt.Errorf("%s: no such file or directory (%s)", dest, target)
	}
	if info.IsDir() {
		if tp := b.bySrc[path.Join(target, "README.md")]; tp != nil {
			return relPath(p.out, tp.out) + fragment, tp, nil
		}
		return b.repoLink("tree", target), nil, nil
	}
	return b.repoLink("blob", target) + fragment, nil, nil
}

// imageTypes are the image formats a page may embed; they are copied into
// the site next to the pages that use them.
var imageTypes = map[string]bool{".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// resolveImage copies an embedded repository image into the site, at the
// same docs-relative path as the pages, and returns its relative path from
// the page. External images are kept as they are.
func (b *builder) resolveImage(p *page, dest string) (string, error) {
	if strings.HasPrefix(dest, "//") || hasScheme(dest) {
		return dest, nil
	}
	target := path.Clean(path.Join(path.Dir(p.src), dest))
	if target == ".." || strings.HasPrefix(target, "../") {
		return "", fmt.Errorf("image %s points outside the repository", dest)
	}
	if !imageTypes[strings.ToLower(path.Ext(target))] {
		return "", fmt.Errorf("image %s: unsupported type", dest)
	}
	info, err := os.Stat(filepath.Join(b.cfg.Root, filepath.FromSlash(target)))
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("image %s: no such file (%s)", dest, target)
	}
	out := strings.TrimPrefix(target, "docs/")
	b.images[out] = target
	return relPath(p.out, out), nil
}

func (b *builder) repoLink(kind, target string) string {
	if target == "." {
		return b.cfg.RepoURL
	}
	return b.cfg.RepoURL + "/" + kind + "/" + b.cfg.Ref + "/" + target
}

func hasScheme(dest string) bool {
	scheme, _, ok := strings.Cut(dest, ":")
	if !ok || scheme == "" || strings.ContainsAny(scheme, "/#?.") {
		return false
	}
	return true
}

// relPath is the link from one site page to another.
func relPath(from, to string) string {
	rel, err := filepath.Rel(filepath.FromSlash(path.Dir(from)), filepath.FromSlash(to))
	if err != nil {
		return to
	}
	return filepath.ToSlash(rel)
}

// rootPrefix is the relative path from a page back to the site root.
func rootPrefix(out string) string {
	return strings.Repeat("../", strings.Count(out, "/"))
}

// plainText is the rendered text of an inline tree, without markup, which is
// what GitHub slugs heading IDs from.
func plainText(n ast.Node, source []byte) string {
	var b strings.Builder
	var walk func(ast.Node)
	walk = func(n ast.Node) {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			switch c := c.(type) {
			case *ast.Text:
				b.Write(c.Value(source))
				if c.SoftLineBreak() || c.HardLineBreak() {
					b.WriteByte(' ')
				}
			case *ast.String:
				b.Write(c.Value)
			case *ast.AutoLink:
				b.Write(c.Label(source))
			case *ast.RawHTML:
			case *ast.Link:
				if class, _ := c.AttributeString("class"); string(asBytes(class)) != "anchor" {
					walk(c)
				}
			default:
				walk(c)
			}
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func asBytes(v any) []byte {
	b, _ := v.([]byte)
	return b
}

// lineOf is the source line of an inline node's first text, for errors.
func lineOf(n ast.Node, source []byte) int {
	offset := 0
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := c.(*ast.Text); ok && entering {
			offset = t.Segment.Start
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return bytes.Count(source[:offset], []byte("\n")) + 1
}

// wrapTables puts each table in a scroll container so wide tables scroll
// horizontally on their own instead of widening the page.
func wrapTables(body []byte) []byte {
	body = bytes.ReplaceAll(body, []byte("<table>"), []byte(`<div class="table-wrap"><table>`))
	return bytes.ReplaceAll(body, []byte("</table>"), []byte("</table></div>"))
}

// Write replaces the output directory's contents with files, removing files
// the build no longer produces.
func Write(cfg Config, files map[string][]byte) error {
	outDir := filepath.Join(cfg.Root, filepath.FromSlash(cfg.OutDir))
	existing, err := listFiles(outDir)
	if err != nil {
		return err
	}
	for _, rel := range existing {
		if _, keep := files[rel]; !keep {
			if err := os.Remove(filepath.Join(outDir, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
	}
	for _, rel := range sortedKeys(files) {
		dst := filepath.Join(outDir, filepath.FromSlash(rel))
		if current, err := os.ReadFile(dst); err == nil && bytes.Equal(current, files[rel]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, files[rel], 0o644); err != nil {
			return err
		}
	}
	return removeEmptyDirs(outDir)
}

func listFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

func removeEmptyDirs(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return err
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if entries, err := os.ReadDir(dirs[i]); err == nil && len(entries) == 0 {
			if err := os.Remove(dirs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedKeys(files map[string][]byte) []string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
