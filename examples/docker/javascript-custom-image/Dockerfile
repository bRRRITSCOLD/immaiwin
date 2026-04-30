# Custom sandbox image: Node.js + cheerio (jQuery-like HTML parsing)
#
# Build:
#   docker build -t myorg/sandbox-node-cheerio:20 .
#
# Usage:
#   Set "Image" field on SandboxScript node to: myorg/sandbox-node-cheerio:20
#
# The base image provides the entrypoint protocol (stdin JSON → code exec → stdout JSON).
# This image just adds extra npm packages on top.

FROM burrow/sandbox-node:20

# Install cheerio globally so require('cheerio') works in sandbox scripts
RUN npm install -g cheerio

# Example sandbox script:
#
#   const cheerio = require('cheerio')
#   const $ = cheerio.load(input.html)
#   const titles = $('h2.title').map((_, el) => $(el).text()).get()
#   output({ titles })
