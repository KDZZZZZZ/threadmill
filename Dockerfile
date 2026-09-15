FROM debian:bookworm-slim

ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends bubblewrap ca-certificates git bash \
 && rm -rf /var/lib/apt/lists/*

COPY threadmill /usr/local/bin/threadmill
COPY deploy/container-entrypoint.sh /usr/local/bin/threadmill-container
RUN chmod 0755 /usr/local/bin/threadmill /usr/local/bin/threadmill-container \
 && useradd --create-home --uid 1000 --shell /bin/bash threadmill

ENV THREADMILL_ROOT=/workspace
WORKDIR /workspace
USER threadmill
ENTRYPOINT ["/usr/local/bin/threadmill-container"]
CMD ["-web", "-listen", "0.0.0.0:8787"]
