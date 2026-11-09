// Command docsite renders the Markdown documentation into the static HTML
// site under docs/site. The output is not committed: CI builds it and
// publishes it to GitHub Pages.
package main

import (
	"flag"
	"fmt"
	"log"

	"atrium/tools/docsite/internal/site"
)

func main() {
	cfg := site.Config{}
	flag.StringVar(&cfg.Root, "root", ".", "repository root")
	flag.StringVar(&cfg.OutDir, "out", "docs/site", "output directory, relative to -root")
	flag.StringVar(&cfg.RepoURL, "repo-url", "", "repository web URL used for links to source files (required)")
	flag.StringVar(&cfg.Ref, "ref", "master", "branch or tag used for links to source files")
	flag.Parse()

	files, err := site.Build(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := site.Write(cfg, files); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("docsite: wrote %d files to %s\n", len(files), cfg.OutDir)
}
