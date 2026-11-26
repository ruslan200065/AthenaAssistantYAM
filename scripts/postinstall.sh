#!/bin/sh
# Athena YAM package postinstall.
#
# Law of this package: never rewrite anything that already works on the
# station.  We do not touch S99luna, quasar.cfg, quasar-bridge, native Alice
# or the existing local.sendspin plugin.  We only add one local Luna plugin
# directory and append its id to Luna's enabled/services lists.
set -eu

LUNA_PLUGINS=/data/quazar/luna/data/plugins
LIVE="$LUNA_PLUGINS/athena.assistant"
SRC="$YAM_PKG/share/athena.assistant"

if [ ! -d "$SRC" ]; then
    echo "athena: package payload not found at $SRC" >&2
    exit 1
fi

mkdir -p "$LUNA_PLUGINS"
if [ -d "$LIVE" ]; then
    # Update in place. Moving the whole plugin away and copying it back needs
    # twice the space; on a station with a small /data that fails the update.
    if [ -f "$LIVE/bin/athena" ]; then
        cp "$LIVE/bin/athena" "$LIVE/bin/athena.prev" 2>/dev/null || true
    fi
    echo "athena: updating plugin in place at $LIVE"
else
    mkdir -p "$LIVE"
    echo "athena: installing plugin at $LIVE"
fi
cp -a "$SRC/." "$LIVE/"
chmod 0755 "$LIVE/bin/athena" 2>/dev/null || true

for f in enabled.json services.json; do
    p="$LUNA_PLUGINS/$f"
    [ -f "$p" ] || echo '[]' > "$p"
    [ -f "$p.athena.bak" ] || cp -p "$p" "$p.athena.bak" 2>/dev/null || true
    if ! grep -q '"athena.assistant"' "$p"; then
        ids=$(sed -e 's/^[[:space:]]*\[//' -e 's/][[:space:]]*$//' -e 's/"//g' -e 's/,/ /g' "$p")
        tmp="$p.tmp.$$"
        printf '[' > "$tmp"
        first=1
        for id in $ids; do
            [ -n "$id" ] || continue
            [ "$first" = 1 ] || printf ', ' >> "$tmp"
            printf '"%s"' "$id" >> "$tmp"
            first=0
        done
        [ "$first" = 1 ] || printf ', ' >> "$tmp"
        printf '"athena.assistant"]\n' >> "$tmp"
        mv "$tmp" "$p"
    fi
done

echo "athena: plugin copied to $LIVE"
echo "athena: not restarting Luna; use POST /api/plugins/rescan then /api/plugins/athena.assistant/service/enable"
echo "athena: UI will be at http://<station>:8781"
