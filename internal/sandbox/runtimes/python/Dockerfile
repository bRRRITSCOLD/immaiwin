FROM python:3.12-alpine

# Non-root user for security
RUN adduser -D -u 10001 sandbox

WORKDIR /sandbox

COPY entrypoint.py .

RUN chown -R sandbox:sandbox /sandbox

USER sandbox

ENTRYPOINT ["python3", "/sandbox/entrypoint.py"]
