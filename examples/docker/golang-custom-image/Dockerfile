# Custom sandbox image: Go + goquery (jQuery-like HTML parsing)
#
# Build:
#   docker build -t myorg/sandbox-go-goquery:1.22 .
#
# Usage:
#   Set "Image" field on SandboxScript node to: myorg/sandbox-go-goquery:1.22
#
# The base image provides the entrypoint protocol (stdin JSON → code exec → stdout JSON).
# This image pre-downloads goquery so sandbox scripts can import it.

FROM burrow/sandbox-go:1.22

# Pre-download goquery + cascadia into the module cache
RUN mkdir -p /tmp/goquery-seed && \
    cd /tmp/goquery-seed && \
    go mod init seed && \
    go get github.com/PuerkitoBio/goquery@latest && \
    rm -rf /tmp/goquery-seed

# Example sandbox script:
#
#   import (
#       "strings"
#       "github.com/PuerkitoBio/goquery"
#   )
#
#   html, _ := input["html"].(string)
#   doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
#   if err != nil {
#       panic(err)
#   }
#   var titles []string
#   doc.Find("h2.title").Each(func(_ int, s *goquery.Selection) {
#       titles = append(titles, s.Text())
#   })
#   output(map[string]any{"titles": titles})
