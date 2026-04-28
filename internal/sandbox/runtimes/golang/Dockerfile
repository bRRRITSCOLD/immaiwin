FROM golang:1.22-alpine

# Shared build cache location for root (build) and sandbox (runtime)
ENV GOCACHE=/sandbox/.cache/go-build

WORKDIR /sandbox

# Pre-warm build cache — compile all stdlib packages user code imports
RUN mkdir -p /tmp/warmup && \
    printf 'module warmup\ngo 1.22\n' > /tmp/warmup/go.mod && \
    printf 'package main\nimport (\n\t"encoding/json"\n\t"fmt"\n\t"math"\n\t"os"\n\t"sort"\n\t"strconv"\n\t"strings"\n\t"time"\n)\nvar (\n\t_ = json.Marshal\n\t_ = fmt.Sprintf\n\t_ = math.Pi\n\t_ = os.Exit\n\t_ = sort.Ints\n\t_ = strconv.Itoa\n\t_ = strings.Contains\n\t_ = time.Now\n)\nfunc main() {}\n' > /tmp/warmup/main.go && \
    cd /tmp/warmup && go build -o /dev/null . && \
    rm -rf /tmp/warmup

COPY entrypoint.go go.mod ./
RUN go build -o entrypoint entrypoint.go

RUN adduser -D -u 10001 sandbox
RUN chown -R sandbox:sandbox /sandbox

USER sandbox

ENTRYPOINT ["/sandbox/entrypoint"]
