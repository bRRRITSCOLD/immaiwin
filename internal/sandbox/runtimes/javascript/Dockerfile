FROM node:20-alpine

# Non-root user for security
RUN adduser -D -u 10001 sandbox

WORKDIR /sandbox

COPY entrypoint.js .

RUN chown -R sandbox:sandbox /sandbox

USER sandbox

ENTRYPOINT ["node", "/sandbox/entrypoint.js"]
