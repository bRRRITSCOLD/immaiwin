# Custom sandbox image: Rust + scraper (CSS selector HTML parsing)
#
# Build:
#   docker build -t myorg/sandbox-rust-scraper:1.86 .
#
# Usage:
#   Set "Image" field on SandboxScript node to: myorg/sandbox-rust-scraper:1.86
#
# The base image provides the entrypoint protocol (stdin JSON → code exec → stdout JSON).
# This image pre-compiles the scraper crate so sandbox scripts can use it.

FROM burrow/sandbox-rust:1.86

# Pre-compile scraper crate into a dummy project so it's cached
RUN mkdir -p /tmp/scraper-seed/src && \
    echo '[package]\nname = "seed"\nversion = "0.1.0"\nedition = "2021"\n\n[dependencies]\nscraper = "0.22"' > /tmp/scraper-seed/Cargo.toml && \
    echo 'fn main() {}' > /tmp/scraper-seed/src/main.rs && \
    cd /tmp/scraper-seed && \
    cargo build --release 2>/dev/null && \
    rm -rf /tmp/scraper-seed/src /tmp/scraper-seed/target/release/seed*

# Example sandbox script:
#
#   use scraper::{Html, Selector};
#
#   let html = input["html"].as_str().unwrap_or("");
#   let document = Html::parse_document(html);
#   let selector = Selector::parse("h2.title").unwrap();
#   let titles: Vec<String> = document
#       .select(&selector)
#       .map(|el| el.text().collect::<String>())
#       .collect();
#   output(json!({"titles": titles}));
