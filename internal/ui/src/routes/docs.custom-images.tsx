import { createFileRoute, Link } from '@tanstack/react-router'

export const Route = createFileRoute('/docs/custom-images')({
  component: CustomImagesDocs,
})

const examples: {
  language: string
  base: string
  custom: string
  packages: string
  dockerfile: string
  script: string
}[] = [
  {
    language: 'JavaScript',
    base: 'immaiwin/sandbox-node:20',
    custom: 'myorg/sandbox-node-cheerio:20',
    packages: 'cheerio',
    dockerfile: `FROM immaiwin/sandbox-node:20
RUN npm install -g cheerio`,
    script: `const cheerio = require('cheerio')
const $ = cheerio.load(input.html)
const titles = $('h2.title').map((_, el) => $(el).text()).get()
output({ titles })`,
  },
  {
    language: 'Python',
    base: 'immaiwin/sandbox-python:3.12',
    custom: 'myorg/sandbox-python-bs4:3.12',
    packages: 'beautifulsoup4, lxml',
    dockerfile: `FROM immaiwin/sandbox-python:3.12
RUN pip install --no-cache-dir beautifulsoup4 lxml`,
    script: `from bs4 import BeautifulSoup
soup = BeautifulSoup(input["html"], "lxml")
titles = [h2.get_text() for h2 in soup.select("h2.title")]
output({"titles": titles})`,
  },
  {
    language: 'Go',
    base: 'immaiwin/sandbox-go:1.22',
    custom: 'myorg/sandbox-go-goquery:1.22',
    packages: 'goquery',
    dockerfile: `FROM immaiwin/sandbox-go:1.22
RUN mkdir -p /tmp/seed && cd /tmp/seed && \\
    go mod init seed && \\
    go get github.com/PuerkitoBio/goquery@latest && \\
    rm -rf /tmp/seed`,
    script: `import (
    "strings"
    "github.com/PuerkitoBio/goquery"
)

html, _ := input["html"].(string)
doc, _ := goquery.NewDocumentFromReader(strings.NewReader(html))
var titles []string
doc.Find("h2.title").Each(func(_ int, s *goquery.Selection) {
    titles = append(titles, s.Text())
})
output(map[string]any{"titles": titles})`,
  },
  {
    language: 'Rust',
    base: 'immaiwin/sandbox-rust:1.86',
    custom: 'myorg/sandbox-rust-scraper:1.86',
    packages: 'scraper',
    dockerfile: `FROM immaiwin/sandbox-rust:1.86
RUN mkdir -p /tmp/seed/src && \\
    printf '[package]\\nname="seed"\\nversion="0.1.0"\\nedition="2021"\\n[dependencies]\\nscraper="0.22"' \\
      > /tmp/seed/Cargo.toml && \\
    echo 'fn main(){}' > /tmp/seed/src/main.rs && \\
    cd /tmp/seed && cargo build --release && \\
    rm -rf /tmp/seed/src /tmp/seed/target/release/seed*`,
    script: `use scraper::{Html, Selector};

let html = input["html"].as_str().unwrap_or("");
let document = Html::parse_document(html);
let selector = Selector::parse("h2.title").unwrap();
let titles: Vec<String> = document
    .select(&selector)
    .map(|el| el.text().collect::<String>())
    .collect();
output(json!({"titles": titles}));`,
  },
]

function CustomImagesDocs() {
  return (
    <div className="mx-auto max-w-4xl px-6 py-10">
      <Link to="/workflows" className="text-xs text-muted-foreground hover:text-foreground mb-6 inline-block">
        &larr; Back to Workflows
      </Link>

      <h1 className="text-2xl font-bold mb-2">Custom Sandbox Images</h1>
      <p className="text-muted-foreground mb-8">
        Extend base sandbox images with pre-installed packages. Same entrypoint protocol — just more libraries available.
      </p>

      {/* How it works */}
      <section className="mb-10">
        <h2 className="text-lg font-semibold mb-3">How It Works</h2>
        <ol className="list-decimal list-inside space-y-2 text-sm text-muted-foreground">
          <li>
            Create a <code className="text-foreground bg-muted px-1 rounded">Dockerfile</code> that extends a base sandbox image
          </li>
          <li>
            Install your packages (npm, pip, go get, cargo)
          </li>
          <li>
            Build and tag: <code className="text-foreground bg-muted px-1 rounded">docker build -t myorg/my-image:tag .</code>
          </li>
          <li>
            Set the <strong className="text-foreground">Image</strong> field on your Sandbox Script node
          </li>
        </ol>
      </section>

      {/* Rules */}
      <section className="mb-10">
        <h2 className="text-lg font-semibold mb-3">Rules</h2>
        <ul className="list-disc list-inside space-y-1.5 text-sm text-muted-foreground">
          <li>
            <strong className="text-foreground">Must use a matching base image</strong> — the entrypoint reads JSON from stdin, executes code, writes result to stdout. Don't override it.
          </li>
          <li>
            <strong className="text-foreground">Custom images skip the warm pool</strong> — every run is a cold start. Acceptable for infrequent use.
          </li>
          <li>
            <strong className="text-foreground">Image must be available locally</strong> — the sandbox manager pulls if missing, but private registries need <code className="text-foreground bg-muted px-1 rounded">docker login</code> first.
          </li>
          <li>
            <strong className="text-foreground">Leave Image empty for defaults</strong> — pool and standard images work as before.
          </li>
        </ul>
      </section>

      {/* Base images */}
      <section className="mb-10">
        <h2 className="text-lg font-semibold mb-3">Base Images</h2>
        <div className="rounded-md border border-border overflow-hidden">
          <table className="w-full text-sm">
            <thead className="bg-muted/40">
              <tr>
                <th className="text-left px-4 py-2 font-medium">Language</th>
                <th className="text-left px-4 py-2 font-medium">Base Image</th>
              </tr>
            </thead>
            <tbody>
              {examples.map((ex) => (
                <tr key={ex.language} className="border-t border-border">
                  <td className="px-4 py-2">{ex.language}</td>
                  <td className="px-4 py-2 font-mono text-xs">{ex.base}</td>
                </tr>
              ))}
              <tr className="border-t border-border">
                <td className="px-4 py-2">PHP</td>
                <td className="px-4 py-2 font-mono text-xs">immaiwin/sandbox-php:8.3</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      {/* Per-language examples */}
      <section>
        <h2 className="text-lg font-semibold mb-4">Examples</h2>
        <div className="space-y-8">
          {examples.map((ex) => (
            <div key={ex.language} className="rounded-lg border border-border overflow-hidden">
              <div className="px-4 py-2.5 bg-muted/30 border-b border-border flex items-center justify-between">
                <span className="font-medium">{ex.language}</span>
                <span className="text-xs text-muted-foreground">
                  Packages: <code>{ex.packages}</code>
                </span>
              </div>

              <div className="p-4 space-y-4">
                <div>
                  <h4 className="text-xs font-medium text-muted-foreground mb-1.5">Dockerfile</h4>
                  <pre className="bg-zinc-900 rounded-md p-3 text-xs overflow-x-auto">
                    <code>{ex.dockerfile}</code>
                  </pre>
                </div>

                <div>
                  <h4 className="text-xs font-medium text-muted-foreground mb-1.5">Build</h4>
                  <pre className="bg-zinc-900 rounded-md p-3 text-xs overflow-x-auto">
                    <code>docker build -t {ex.custom} .</code>
                  </pre>
                </div>

                <div>
                  <h4 className="text-xs font-medium text-muted-foreground mb-1.5">Sandbox Script</h4>
                  <pre className="bg-zinc-900 rounded-md p-3 text-xs overflow-x-auto">
                    <code>{ex.script}</code>
                  </pre>
                </div>
              </div>
            </div>
          ))}
        </div>
      </section>
    </div>
  )
}
