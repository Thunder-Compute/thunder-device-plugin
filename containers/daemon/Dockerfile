FROM debian:bookworm-slim
RUN apt-get update     && apt-get install -y --no-install-recommends ca-certificates util-linux     && rm -rf /var/lib/apt/lists/*
COPY thunder-device-plugin-daemon /usr/local/bin/thunder-device-plugin-daemon
ENTRYPOINT ["/usr/local/bin/thunder-device-plugin-daemon"]
