#!/bin/sh
# Starts hostapd for the Wi-Fi adapters PeakRoute OS has configured (peakroute-wifi.service runs this).
#
# One hostapd process serves every adapter's file at once, which is what lets band steering work (a 2.4 GHz
# network can refuse a device it has seen on 5 GHz). A file whose adapter is not there is left out, and if
# none is there the service fails, so that the failure shows up in the commit that caused it.
#
# The paths can be changed through the environment, which is how the tests run this script.

CONF_DIR="${PEAKROUTE_HOSTAPD_DIR:-/etc/peakroute/hostapd}"
SYS_NET="${PEAKROUTE_SYS_NET:-/sys/class/net}"
HOSTAPD="${PEAKROUTE_HOSTAPD:-/usr/sbin/hostapd}"
PIDFILE="${PEAKROUTE_HOSTAPD_PID:-/run/peakroute-hostapd.pid}"
RFKILL="${PEAKROUTE_RFKILL:-rfkill}"

# a radio that is soft-blocked cannot start an access point
if command -v "$RFKILL" >/dev/null 2>&1; then
    "$RFKILL" unblock wifi >/dev/null 2>&1 || true
fi

args=""
for conf in "$CONF_DIR"/*.conf; do
    [ -f "$conf" ] || continue
    iface=$(sed -n 's/^interface=//p' "$conf" | head -n 1)
    if [ -n "$iface" ] && [ -d "$SYS_NET/$iface" ]; then
        args="$args $conf"
    else
        echo "peakroute-wifi: leaving out $conf: the adapter '$iface' is not present" >&2
    fi
done

if [ -z "$args" ]; then
    echo "peakroute-wifi: none of the configured Wi-Fi adapters is present" >&2
    exit 1
fi

# -B: hostapd detaches once it is running, so a configuration it rejects makes this fail
# shellcheck disable=SC2086
exec "$HOSTAPD" -B -P "$PIDFILE" $args
