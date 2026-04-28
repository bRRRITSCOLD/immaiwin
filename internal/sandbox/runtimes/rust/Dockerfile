FROM rust:1.86-alpine

RUN apk add --no-cache musl-dev jq

WORKDIR /sandbox

# Pre-init cargo project with serde_json — deps compile once during build
RUN mkdir -p /sandbox/project/src && \
    printf '[package]\nname = "sandbox_run"\nversion = "0.1.0"\nedition = "2021"\n\n[dependencies]\nserde_json = { version = "1", features = ["preserve_order"] }\n' > /sandbox/project/Cargo.toml && \
    printf 'use serde_json::Value;\nfn main() { let _: Value = Value::Null; }\n' > /sandbox/project/src/main.rs && \
    cd /sandbox/project && \
    cargo build --release

COPY entrypoint.sh .
RUN chmod +x entrypoint.sh

RUN adduser -D -u 10001 sandbox
RUN chown -R sandbox:sandbox /sandbox /tmp

USER sandbox

ENTRYPOINT ["/sandbox/entrypoint.sh"]
