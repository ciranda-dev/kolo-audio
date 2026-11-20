# PulseAudio server on musl, to show the static test binary runs there.
FROM alpine:3.22
RUN apk add --no-cache pulseaudio \
 && adduser -D probe
COPY server.sh /usr/local/bin/server.sh
USER probe
