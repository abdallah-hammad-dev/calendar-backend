#!/usr/bin/env bash
# Crée la collection de test (VEVENT + VTODO) et y pousse les éléments des tests iPhone.
#   ./push-fixtures.sh            crée ce qui manque
#   ./push-fixtures.sh --reset    supprime la collection et repart de zéro (dates recalculées)
# Variables : COLL (URL de la collection, défaut : collection « tests » d'abd sur le Davis local),
# CAL_USER / CAL_PASS, ME_EMAIL (ton adresse côté serveur, = email de l'utilisateur Davis).
# Radicale : COLL=http://localhost:5232/abd/tests/ ME_EMAIL=… ./push-fixtures.sh
set -euo pipefail

CAL_USER=${CAL_USER:-abd}
CAL_PASS=${CAL_PASS:-x}
COLL=${COLL:-http://127.0.0.1:9000/dav/calendars/$CAL_USER/tests/}
ME_EMAIL=${ME_EMAIL:-abd@bench.test}
TZ_NAME=Europe/Paris

dav() { curl -sS -u "$CAL_USER:$CAL_PASS" -o /dev/null -w "%{http_code}" "$@"; }
utc() { date -u -d "@$(date -d "$1" +%s)" +%Y%m%dT%H%M%SZ; }
loc() { TZ=$TZ_NAME date -d "$1" +%Y%m%dT%H%M%S; }   # heure locale, écrite avec TZID (sinon iOS affiche « (GMT) »)

NOW=$(utc now)
D1_10=$(loc "tomorrow 10:00")
D1_14=$(loc "tomorrow 14:00")
D1_15=$(loc "tomorrow 15:00")
D1_18=$(loc "tomorrow 18:00")
D2_11=$(loc "2 days 11:00")
D2_12=$(loc "2 days 12:00")
day() { TZ=$TZ_NAME date -d "$1" +%Y%m%d; }        # date seule (VALUE=DATE)

VTIMEZONE='BEGIN:VTIMEZONE
TZID:Europe/Paris
BEGIN:DAYLIGHT
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
TZNAME:CEST
DTSTART:19700329T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
TZNAME:CET
DTSTART:19701025T030000
RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU
END:STANDARD
END:VTIMEZONE'

if [[ ${1:-} == --reset ]]; then
  echo "DELETE collection: $(dav -X DELETE "$COLL")"
fi

code=$(dav -X MKCALENDAR -H "Content-Type: application/xml" --data-binary @- "$COLL" <<'XML'
<?xml version="1.0" encoding="utf-8"?>
<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:set><D:prop>
    <D:displayname>Tests</D:displayname>
    <C:supported-calendar-component-set>
      <C:comp name="VEVENT"/><C:comp name="VTODO"/>
    </C:supported-calendar-component-set>
  </D:prop></D:set>
</C:mkcalendar>
XML
)
case $code in
  201) echo "MKCALENDAR $COLL: créée" ;;
  405|409) echo "MKCALENDAR $COLL: existe déjà ($code)" ;;
  *) echo "MKCALENDAR $COLL: échec ($code) — le serveur tourne ?"; exit 1 ;;
esac

# put <uid> <composant iCalendar sans VCALENDAR>
put() {
  local uid=$1 body
  body=$(printf '%s\n%s\n' "$VTIMEZONE" "$(cat)" | sed 's/$/\r/')
  body=$(printf 'BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//organisation//calendar-bench//FR\r\n%s\r\nEND:VCALENDAR\r\n' "$body")
  printf '%-22s %s\n' "$uid" "$(printf '%s' "$body" | dav -X PUT -H "Content-Type: text/calendar; charset=utf-8" \
    -H "If-None-Match: *" --data-binary @- "$COLL$uid.ics")"
}

echo "PUT (201 = créé, 412 = déjà là) :"

# Tests 1, 2, 3 : rappel daté + horaire, avec URL, lien dans les notes et propriétés X- à surveiller.
put test-rappel <<EOF
BEGIN:VTODO
UID:test-rappel
DTSTAMP:$NOW
CREATED:$NOW
SUMMARY:Test 1 — rappel daté demain 10h
DTSTART;TZID=$TZ_NAME:$D1_10
DUE;TZID=$TZ_NAME:$D1_10
STATUS:NEEDS-ACTION
URL:obsidian://calendar-tasks?uid=test-rappel
X-CALTASKS-SOURCE-ACCOUNT:bench
X-TEST-MARKER:doit-survivre
DESCRIPTION:Le cocher sur l'iPhone puis lancer ./check.sh\\n\\nobsidian://calendar-tasks?uid=test-rappel
END:VTODO
EOF

# Test 3 côté calendrier : événement avec URL + X-, à modifier depuis Apple Calendar.
put test-evenement <<EOF
BEGIN:VEVENT
UID:test-evenement
DTSTAMP:$NOW
SUMMARY:Test 3 — événement avec lien Obsidian
DTSTART;TZID=$TZ_NAME:$D1_14
DTEND;TZID=$TZ_NAME:$D1_15
URL:obsidian://calendar-tasks?uid=test-evenement
X-CALTASKS-SOURCE-ACCOUNT:bench
X-SOURCE-ID:abc123
X-TEST-MARKER:doit-survivre
DESCRIPTION:Déplacer ou renommer sur l'iPhone puis lancer ./check.sh
END:VEVENT
EOF

# Test 4 : parent + enfant reliés par RELATED-TO;RELTYPE=PARENT.
put test-parent <<EOF
BEGIN:VTODO
UID:test-parent
DTSTAMP:$NOW
SUMMARY:Test 4 — tâche parente
DTSTART;TZID=$TZ_NAME:$D1_18
DUE;TZID=$TZ_NAME:$D1_18
STATUS:NEEDS-ACTION
END:VTODO
EOF

put test-enfant <<EOF
BEGIN:VTODO
UID:test-enfant
DTSTAMP:$NOW
SUMMARY:Test 4 — sous-tâche (enfant)
DTSTART;TZID=$TZ_NAME:$D1_18
DUE;TZID=$TZ_NAME:$D1_18
STATUS:NEEDS-ACTION
RELATED-TO;RELTYPE=PARENT:test-parent
END:VTODO
EOF

# Test 5 : invitation d'un organisateur externe ; ma ligne ATTENDEE = mon adresse côté serveur.
put test-invitation <<EOF
BEGIN:VEVENT
UID:test-invitation
DTSTAMP:$NOW
SEQUENCE:0
SUMMARY:Test 5 — invitation (organisateur externe)
DTSTART;TZID=$TZ_NAME:$D2_11
DTEND;TZID=$TZ_NAME:$D2_12
ORGANIZER;CN=Organisateur Test:mailto:organisateur@example.com
ATTENDEE;CN=Organisateur Test;PARTSTAT=ACCEPTED;ROLE=CHAIR:mailto:organisateur@example.com
ATTENDEE;CN=Invité;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;ROLE=REQ-PARTICIPANT:mailto:$ME_EMAIL
END:VEVENT
EOF

# Tests 6, 7, 8 : « quand je la fais » (DTSTART) vs deadline (DUE). Que montre l'iPhone, dans quelle liste,
# et que devient DTSTART quand on change la date sur l'iPhone ?
put test-debut-et-echeance <<EOF
BEGIN:VTODO
UID:test-debut-et-echeance
DTSTAMP:$NOW
SUMMARY:Test 6 — à faire demain, deadline dans 5 jours
DTSTART;VALUE=DATE:$(day tomorrow)
DUE;VALUE=DATE:$(day "5 days")
STATUS:NEEDS-ACTION
END:VTODO
EOF

put test-debut-seul <<EOF
BEGIN:VTODO
UID:test-debut-seul
DTSTAMP:$NOW
SUMMARY:Test 7 — à faire après-demain, sans deadline
DTSTART;VALUE=DATE:$(day "2 days")
STATUS:NEEDS-ACTION
END:VTODO
EOF

put test-echeance-seule <<EOF
BEGIN:VTODO
UID:test-echeance-seule
DTSTAMP:$NOW
SUMMARY:Test 8 — deadline dans 4 jours seulement
DUE;VALUE=DATE:$(day "4 days")
STATUS:NEEDS-ACTION
END:VTODO
EOF

# Test 9 : rappel qui se répète chaque semaine. L'iPhone affiche-t-il la répétition ? Et quand on le coche :
# avance-t-il la date du même rappel, ou crée-t-il une copie terminée ?
put test-repetition <<EOF
BEGIN:VTODO
UID:test-repetition
DTSTAMP:$NOW
SUMMARY:Test 9 — se répète chaque semaine
DTSTART;TZID=$TZ_NAME:$D1_10
DUE;TZID=$TZ_NAME:$D1_10
RRULE:FREQ=WEEKLY
STATUS:NEEDS-ACTION
END:VTODO
EOF
