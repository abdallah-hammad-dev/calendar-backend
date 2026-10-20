#!/usr/bin/env bash
# Affiche, pour chaque élément de la collection de test, les propriétés que les tests surveillent (lu en HTTP,
# donc valable pour Davis comme pour Radicale). Les éléments créés depuis l'iPhone apparaissent aussi.
# Mêmes variables que push-fixtures.sh (COLL, CAL_USER, CAL_PASS).
set -euo pipefail

CAL_USER=${CAL_USER:-abd}
CAL_PASS=${CAL_PASS:-x}
COLL=${COLL:-http://127.0.0.1:9000/dav/calendars/$CAL_USER/tests/}
origin=$(printf '%s' "$COLL" | grep -oE '^https?://[^/]+')

hrefs=$(curl -sS -u "$CAL_USER:$CAL_PASS" -X PROPFIND -H "Depth: 1" "$COLL" \
  | grep -oE '<[a-zA-Z]*:?href>[^<]*\.ics</[a-zA-Z]*:?href>' | sed -E 's/<[^>]*>//g')
[[ -n $hrefs ]] || { echo "Collection vide ou absente : $COLL (lancer ./push-fixtures.sh)"; exit 1; }

for h in $hrefs; do
  echo "== ${h##*/}"
  # Déplie les lignes repliées (RFC 5545 §3.1) avant de filtrer.
  curl -sS -u "$CAL_USER:$CAL_PASS" "$origin$h" | tr -d '\r' | sed -e ':a;N;$!ba;s/\n[ \t]//g' | sed '/BEGIN:VTIMEZONE/,/END:VTIMEZONE/d' \
    | grep -E '^(SUMMARY|STATUS|COMPLETED|PERCENT-COMPLETE|DUE|DTSTART|URL|X-|RELATED-TO|ORGANIZER|ATTENDEE|DESCRIPTION|LAST-MODIFIED)' \
    | sed 's/^/   /'
done
