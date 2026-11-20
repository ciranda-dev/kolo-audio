# PipeWire with a session manager (WirePlumber) and its pulse server, with no
# sound hardware. Stream creation needs the session manager to link streams.
# The user configuration drops WirePlumber's bluetooth component: it loads the
# systemd-logind monitor, and WirePlumber exits when logind is not there.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends pipewire pipewire-pulse wireplumber pulseaudio-utils dbus ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m probe
COPY server.sh /usr/local/bin/server.sh
USER probe
RUN mkdir -p /home/probe/.config/wireplumber \
 && sed '/bluetooth.lua/d' /usr/share/wireplumber/wireplumber.conf > /home/probe/.config/wireplumber/wireplumber.conf
