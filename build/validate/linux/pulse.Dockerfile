# PulseAudio server with no sound hardware, for the Pulse backend tests.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends pulseaudio ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m probe
COPY server.sh /usr/local/bin/server.sh
USER probe
