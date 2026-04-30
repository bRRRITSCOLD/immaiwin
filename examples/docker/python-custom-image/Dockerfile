# Custom sandbox image: Python + BeautifulSoup4 + lxml (HTML parsing)
#
# Build:
#   docker build -t myorg/sandbox-python-bs4:3.12 .
#
# Usage:
#   Set "Image" field on SandboxScript node to: myorg/sandbox-python-bs4:3.12
#
# The base image provides the entrypoint protocol (stdin JSON → code exec → stdout JSON).
# This image just adds extra pip packages on top.

FROM burrow/sandbox-python:3.12

# Install BeautifulSoup4 + lxml parser
RUN pip install --no-cache-dir beautifulsoup4 lxml

# Example sandbox script:
#
#   from bs4 import BeautifulSoup
#   soup = BeautifulSoup(input["html"], "lxml")
#   titles = [h2.get_text() for h2 in soup.select("h2.title")]
#   output({"titles": titles})
