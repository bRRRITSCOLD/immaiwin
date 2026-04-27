FROM php:8.3-alpine

# Non-root user for security
RUN adduser -D sandbox

WORKDIR /sandbox

COPY entrypoint.php .

RUN chown -R sandbox:sandbox /sandbox

USER sandbox

ENTRYPOINT ["php", "/sandbox/entrypoint.php"]
