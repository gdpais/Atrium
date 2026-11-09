// The docs site generator is its own module so the services' module (what
// ships) does not depend on its Markdown parser.
module atrium/tools/docsite

go 1.27.3

require github.com/yuin/goldmark v1.8.7
