#!/usr/bin/env bash
# Installs or updates the server on the VPS (spec §8): Caddy (HTTPS), Davis, CouchDB (the vault's sync), the
# backend and, once asked for, livesync-bridge (the vault for the Claude connector, spec §10.3), from deploy/. Asks
# for what deploy/.env lacks and makes the secrets, checks everything before touching the VPS (values, SSH, DNS,
# LiveSync's database), then after (certificates, a Davis login, no compression on /dav/, CouchDB, the backend).
# Safe to run again: after a change in deploy/ or deploy/.env, or on a new VPS. Asks for the VPS's sudo password once.
# The backend's code comes from CI (.github/workflows/deploy.yml): this script sets CI's secrets and its SSH key,
# which can only run deploy/update. --new-key replaces that key.
set -euo pipefail
cd "$(dirname "$0")"

env_file=deploy/.env
repo=abdallah-hammad-dev/calendar-backend
new_key=false
case ${1-} in
  "") ;;
  --new-key) new_key=true ;;
  *) echo "usage: ./deploy.sh [--new-key]" >&2; exit 2 ;;
esac

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok() { printf '  ✓ %s\n' "$*"; }
die() { printf '  ✗ %s\n' "$*" >&2; exit 1; }
# retry CMD…: tries for a minute.
retry() { for _ in $(seq 30); do "$@" && return 0; sleep 2; done; return 1; }

for tool in ssh scp ssh-keygen dig curl gh openssl; do
  command -v "$tool" >/dev/null || die "$tool manque sur cette machine"
done

# --- deploy/.env: what's missing is asked, secrets are made ------------------------------------------------------

[[ -f $env_file ]] || echo "# The server's settings and secrets (deploy.sh). Never committed: keep a copy somewhere safe." > "$env_file"
chmod 600 "$env_file"
get() { sed -n "s/^$1=//p" "$env_file" | tail -n 1; }
put() {
  local rest
  rest=$(grep -v "^$1=" "$env_file" || true)
  { [[ -n $rest ]] && printf '%s\n' "$rest"; printf '%s=%s\n' "$1" "$2"; } > "$env_file"
}
ask() { # KEY question [default]
  [[ -z $(get "$1") ]] || return 0
  local v
  read -r -p "  $2${3:+ [$3]} : " v
  v=${v:-${3-}}
  [[ -n $v ]] || die "$1 est obligatoire"
  put "$1" "$v"
}
random() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c 32; }
made() { [[ -n $(get "$1") ]] || put "$1" "$(random)"; }
optional() { grep -q "^$1=" "$env_file" || put "$1" "${2-}"; }

step "Réglages (deploy/.env)"
ask VPS "Hôte SSH du VPS" my-vps
ask DOMAIN "Domaine (cal., sync. et accounts. en dessous)" my.example.com
ask ACME_EMAIL "Adresse pour Let's Encrypt (avis sur les certificats)" "$(git config user.email || true)"
ask DAVIS_USER "Ton utilisateur Davis (iPhone, Obsidian, page des comptes)" abd
ask DAVIS_EMAIL "Ton adresse dans Davis (celle de tes invitations)" "$(get ACME_EMAIL)"
if [[ -z $(get DAVIS_PASSWORD) ]]; then
  read -r -s -p "  Mot de passe Davis (vide = j'en fais un ; 16 caractères au moins : lettres, chiffres, . _ -) : " v
  echo
  put DAVIS_PASSWORD "${v:-$(random)}"
fi
ask COUCHDB_USER "Utilisateur CouchDB (synchro du vault)" "$(get DAVIS_USER)"
for k in COUCHDB_PASSWORD COUCHDB_SECRET DAVIS_APP_SECRET DAVIS_ADMIN_PASSWORD; do made "$k"; done
optional TIMEZONE Europe/Paris
optional MICROSOFT_TENANT common
for k in VAULT GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET MICROSOFT_CLIENT_ID MICROSOFT_CLIENT_SECRET; do optional "$k"; done

# The Claude connector's vault (spec §10.3): livesync-bridge reads and writes my LiveSync database on the VPS, so it
# needs what decrypts it. Asked once; no database = no vault there (the connector has the calendar and tasks only).
# The passphrase is kept in base64 (it may hold characters .env can't), none of this goes into the VPS's .env.
couch() { curl -s --config - "$@" <<<"user = \"$(get COUCHDB_USER):$(get COUCHDB_PASSWORD)\""; }
if ! grep -q '^LIVESYNC_DATABASE=' "$env_file"; then
  echo "  Connecteur Claude : ton vault sur le VPS (livesync-bridge, spec §10.3). Vide = pas de vault là-bas."
  dbs=$(couch --max-time 10 "https://sync.$(get DOMAIN)/_all_dbs" | tr -d '[]"' | tr ',' '\n' | grep -v '^_' || true)
  only=""
  [[ $(grep -c . <<<"$dbs") != 1 ]] || only=$dbs
  [[ -z $dbs ]] || echo "  Bases dans CouchDB : $(paste -sd ' ' <<<"$dbs")"
  read -r -p "  Base LiveSync du vault (LiveSync : Remote Database → Database name)${only:+ [$only]} : " v
  put LIVESYNC_DATABASE "${v:-$only}"
fi
if [[ -n $(get LIVESYNC_DATABASE) ]]; then
  if [[ -z $(get LIVESYNC_PASSPHRASE_B64) ]]; then
    read -r -s -p "  Phrase de passe de LiveSync (son chiffrement de bout en bout) : " v
    echo
    [[ -n $v ]] || die "la phrase de passe est obligatoire avec un vault chiffré"
    put LIVESYNC_PASSPHRASE_B64 "$(printf '%s' "$v" | base64 -w 0)"
  fi
  if ! grep -q '^LIVESYNC_ID_KEY=' "$env_file"; then
    read -r -s -p "  Code de récupération (LiveSync : Show current recovery code, sls-id-v1:… ; vide si ton vault n'en a pas) : " v
    echo
    put LIVESYNC_ID_KEY "$v"
  fi
fi

# Compose reads $ in .env, and quotes would be kept: none of them, nor backslashes.
bad=$(grep -nE '^[A-Z_]+=.*[$"'"'"'`\\]' "$env_file" | cut -d= -f1 || true)
[[ -z $bad ]] || die "deploy/.env : pas de \$, de guillemets ni de \\ dans les valeurs (ligne $bad)"
for k in VPS DOMAIN ACME_EMAIL DAVIS_USER DAVIS_EMAIL DAVIS_PASSWORD COUCHDB_USER COUCHDB_PASSWORD LIVESYNC_DATABASE; do
  printf -v "$k" '%s' "$(get "$k")"
done

# --- Before touching the VPS -------------------------------------------------------------------------------------

step "Vérifications avant de toucher au VPS"
[[ $DOMAIN =~ ^([a-z0-9-]+\.)+[a-z]{2,}$ ]] || die "DOMAIN : « $DOMAIN » n'est pas un domaine"
for k in ACME_EMAIL DAVIS_EMAIL; do
  [[ ${!k} =~ ^[^@[:space:]]+@[^@[:space:]]+\.[a-z]{2,}$ ]] || die "$k : « ${!k} » n'est pas une adresse"
done
[[ $DAVIS_USER =~ ^[a-z0-9._-]+$ ]] || die "DAVIS_USER : minuscules, chiffres, . _ -"
[[ $DAVIS_PASSWORD =~ ^[A-Za-z0-9._-]{16,}$ ]] || die "DAVIS_PASSWORD : 16 caractères au moins, lettres, chiffres, . _ -"
for p in GOOGLE MICROSOFT; do
  [[ -z $(get ${p}_CLIENT_ID) && -z $(get ${p}_CLIENT_SECRET) || -n $(get ${p}_CLIENT_ID) && -n $(get ${p}_CLIENT_SECRET) ]] ||
    die "${p}_CLIENT_ID et ${p}_CLIENT_SECRET vont ensemble (docs/comptes.md)"
done
if [[ -n $LIVESYNC_DATABASE ]]; then
  # CouchDB's names, those a URL takes as they are.
  [[ $LIVESYNC_DATABASE =~ ^[a-z][a-z0-9_-]*$ ]] || die "LIVESYNC_DATABASE : « $LIVESYNC_DATABASE » n'est pas un nom de base CouchDB"
  [[ $LIVESYNC_DATABASE != *-bridge-copy ]] || die "LIVESYNC_DATABASE : la vraie base, pas la copie d'essai"
  base64 -d <<<"$(get LIVESYNC_PASSPHRASE_B64)" >/dev/null 2>&1 || die "LIVESYNC_PASSPHRASE_B64 : pas du base64"
  [[ $(get LIVESYNC_ID_KEY) =~ ^((sls-id-v1:)?[0-9a-f]{64})?$ ]] ||
    die "LIVESYNC_ID_KEY : le code de récupération de LiveSync (sls-id-v1: et 64 caractères hexadécimaux)"
  command -v python3 >/dev/null || die "python3 manque sur cette machine (il écrit la config du pont)"
fi
ok "réglages"

ssh -o BatchMode=yes -o ConnectTimeout=10 "$VPS" true || die "ssh $VPS ne marche pas"
ip=$(ssh -G "$VPS" | awk '$1 == "hostname" {print $2}')
[[ $ip =~ ^[0-9.]+$ ]] || ip=$(dig +short A "$ip" | grep -E '^[0-9.]+$' | head -n 1)
[[ -n $ip ]] || die "adresse IPv4 du VPS introuvable"
# CI trusts the host key this machine already trusts.
known=$(ssh-keygen -F "$ip" | grep -v '^#' || true)
[[ -n $known ]] || die "aucune clé d'hôte connue pour $ip dans ~/.ssh/known_hosts"
ok "ssh $VPS ($ip)"

v6=$(ssh "$VPS" "ip -6 -o addr show scope global | awk '{print \$4}' | cut -d/ -f1")
for sub in cal sync accounts; do
  name=$sub.$DOMAIN
  a=$(dig +short A "$name" @1.1.1.1 | grep -E '^[0-9.]+$' | sort -u | paste -sd ' ' || true)
  [[ $a == "$ip" ]] ||
    die "$name pointe vers « ${a:-rien} » : chez Cloudflare, un enregistrement A $name → $ip, proxy désactivé (nuage gris)"
  for x in $(dig +short AAAA "$name" @1.1.1.1 | grep ':' || true); do
    grep -qx "$x" <<<"$v6" || die "$name a un AAAA ($x) qui n'est pas le VPS : supprime-le (Let's Encrypt l'essaierait)"
  done
done
ok "DNS : cal., sync. et accounts.$DOMAIN → $ip"

gh repo view "$repo" --json name >/dev/null 2>&1 || die "gh n'a pas accès à $repo (gh auth login)"
ok "GitHub : $repo"

# The bridge works on a copy of the database first (spec §10.3: what to check before it touches the real one), then
# on the real one, each time from an empty mirror. /opt/calendar/bridge-database says which one it has.
copy=$LIVESYNC_DATABASE-bridge-copy bridge_db="" bridge_from="" make_copy=false
if [[ -n $LIVESYNC_DATABASE ]]; then
  code=$(couch -o /dev/null -w '%{http_code}' --max-time 10 "https://sync.$DOMAIN/$LIVESYNC_DATABASE")
  [[ $code == 200 ]] || die "CouchDB : pas de base « $LIVESYNC_DATABASE » (réponse $code ; LIVESYNC_DATABASE de deploy/.env)"
  bridge_from=$(ssh "$VPS" 'cat /opt/calendar/bridge-database 2>/dev/null' || true)
  case $bridge_from in
    "")
      read -r -p "  Premier branchement du pont : d'abord sur une copie de ta base, pour vérifier (conseillé) ? [O/n] " v
      if [[ $v =~ ^[nN] ]]; then bridge_db=$LIVESYNC_DATABASE; else bridge_db=$copy make_copy=true; fi ;;
    "$copy")
      read -r -p "  Le pont tourne sur la copie d'essai. Passer à ta vraie base (miroir refait, copie supprimée) ? [o/N] " v
      if [[ $v =~ ^[oOyY] ]]; then bridge_db=$LIVESYNC_DATABASE; else bridge_db=$copy; fi ;;
    "$LIVESYNC_DATABASE") bridge_db=$LIVESYNC_DATABASE ;;
    *)
      read -r -p "  Le pont passe de « $bridge_from » à « $LIVESYNC_DATABASE » : son miroir est refait de zéro. Continuer ? [o/N] " v
      [[ $v =~ ^[oOyY] ]] || die "rien n'a changé sur le VPS"
      bridge_db=$LIVESYNC_DATABASE ;;
  esac
  ok "pont : base $bridge_db${bridge_from:+ (avant : $bridge_from)}"
fi

# CI's key: made again when either half is missing (or --new-key). The private half goes to GitHub only.
keydir="" stage="" tunnel=""
cleanup() {
  [[ -z $tunnel ]] || kill "$tunnel" 2>/dev/null || true
  [[ -z $keydir ]] || rm -rf "$keydir"
  [[ -z $stage ]] || ssh "$VPS" "rm -rf '$stage'" 2>/dev/null || true
}
trap cleanup EXIT
has_key=$(ssh "$VPS" 'test -s /var/lib/deploy/.ssh/authorized_keys && echo yes' || true)
has_secret=$(gh secret list -R "$repo" --json name -q '.[].name' | grep -x VPS_SSH_KEY || true)
if $new_key || [[ $has_key != yes || -z $has_secret ]]; then
  keydir=$(mktemp -d)
  ssh-keygen -q -t ed25519 -N '' -C "github-actions@$repo" -f "$keydir/key"
fi

# --- The test copy of the database ------------------------------------------------------------------------------

if $make_copy; then
  step "Copie de ta base pour l'essai du pont ($copy)"
  # Replicated by CouchDB itself (3.x: both ends as URLs, with the admin's login). Its _local documents aren't, so
  # LiveSync's are copied by hand: the milestone (without it the bridge never watches the database) and the sync
  # parameters (E2EE's salt: without them the bridge makes new ones and can't read my notes). bench/TESTS.md.
  COUCH_URL=https://sync.$DOMAIN COUCH_USER=$COUCHDB_USER COUCH_PASSWORD=$COUCHDB_PASSWORD \
    SOURCE=$LIVESYNC_DATABASE TARGET=$copy python3 -I -c '
import base64, json, os, sys, urllib.error, urllib.request
e = os.environ
auth = "Basic " + base64.b64encode((e["COUCH_USER"] + ":" + e["COUCH_PASSWORD"]).encode()).decode()
def call(method, path, body=None, timeout=60):
    req = urllib.request.Request(e["COUCH_URL"] + "/" + path, method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": auth, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as err:
        if method == "DELETE" and err.code == 404:
            return {}
        sys.exit("CouchDB : %s %s : %s %r" % (method, path.split("/")[0], err.code, err.read()[:200]))
src, dst = e["SOURCE"], e["TARGET"]
end = lambda db: {"url": "http://127.0.0.1:5984/" + db,
    "auth": {"basic": {"username": e["COUCH_USER"], "password": e["COUCH_PASSWORD"]}}}
call("DELETE", dst)
if not call("POST", "_replicate", {"source": end(src), "target": end(dst), "create_target": True}, timeout=1800).get("ok"):
    sys.exit("CouchDB n’a pas copié la base")
ids = [r["id"] for r in call("GET", src + "/_local_docs")["rows"]
       if r["id"].startswith(("_local/obsidian_livesync", "_local/obsydian_livesync"))]
if "_local/obsydian_livesync_milestone" not in ids:
    sys.exit("pas de réglages LiveSync (_local/obsydian_livesync_milestone) dans " + src)
for i in ids:
    doc = call("GET", src + "/" + i)
    doc.pop("_rev", None)
    call("PUT", dst + "/" + i, doc)
print("  ✓ copie : %d documents (ta base : %d), %d documents de réglages de LiveSync"
      % (call("GET", dst)["doc_count"], call("GET", src)["doc_count"], len(ids)))' || die "copie de la base"
fi

# --- On the VPS (as root, one sudo) ------------------------------------------------------------------------------

step "Installation sur le VPS (mot de passe sudo de $VPS)"
stage=$(ssh "$VPS" mktemp -d)
scp -q deploy/compose.yaml deploy/Caddyfile deploy/couchdb.ini deploy/update "$VPS:$stage/"
# The VPS's .env: compose's values. VAULT_DIR follows from the bridge; LiveSync's secrets go to the bridge alone.
{
  grep -vE '^(VPS|VAULT_DIR|LIVESYNC_[A-Z0-9_]+)=' "$env_file"
  echo "VAULT_DIR=${bridge_db:+/vault}"
} | ssh "$VPS" "umask 077 && cat > '$stage/env'"
if [[ -n $keydir ]]; then
  scp -q "$keydir/key.pub" "$VPS:$stage/deploy.pub"
fi
# The bridge's config (livesync-bridge's types.ts): my database, decrypted as LiveSync does on my devices (my
# settings, read 2026-11-04: legacy path identity, case-insensitive names, path obfuscation with the passphrase),
# and the folder the backend works in. useRemoteTweaks takes the database's chunk settings. Straight to the VPS.
if [[ -n $bridge_db ]]; then
  DB=$bridge_db COUCH_USER=$COUCHDB_USER COUCH_PASSWORD=$COUCHDB_PASSWORD \
    PASSPHRASE_B64=$(get LIVESYNC_PASSPHRASE_B64) ID_KEY=$(get LIVESYNC_ID_KEY) python3 -I -c '
import base64, json, os
e = os.environ
phrase = base64.b64decode(e["PASSPHRASE_B64"]).decode()
couchdb = {
    "type": "couchdb", "name": "livesync", "group": "vault",
    "url": "http://couchdb:5984", "database": e["DB"],
    "username": e["COUCH_USER"], "password": e["COUCH_PASSWORD"],
    "passphrase": phrase, "obfuscatePassphrase": phrase,
    "baseDir": "", "useRemoteTweaks": True,
    "customChunkSize": 0, "minimumChunkSize": 20,
    "handleFilenameCaseSensitive": False, "pathNormalizationVersion": 0,
}
if e["ID_KEY"]:
    couchdb["idDerivationKey"] = e["ID_KEY"]
folder = {
    "type": "storage", "name": "vault", "group": "vault",
    "baseDir": "./data/notes/", "scanOfflineChanges": True,
    "handleFilenameCaseSensitive": False, "pathNormalizationVersion": 0,
}
print(json.dumps({"peers": [couchdb, folder]}, indent=2))' | ssh "$VPS" "umask 077 && cat > '$stage/bridge.json'"
  printf '%s\n' "$bridge_db" | ssh "$VPS" "cat > '$stage/bridge-database'"
fi
bridge_reset=keep
[[ -z $bridge_db || -z $bridge_from || $bridge_from == "$bridge_db" ]] || bridge_reset=reset
ssh "$VPS" "cat > '$stage/install.sh'" <<'INSTALL'
set -euo pipefail
stage=$1 bridge_reset=$2
dir=/opt/calendar
cd "$stage"

# Docker from Docker's own repository (the user's choice, 2026-10-29), as its install guide does it.
apt_get() { DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 -qq "$@" >/dev/null; }
if ! docker compose version >/dev/null 2>&1; then
  echo "  … Docker (dépôt de Docker)"
  apt_get update
  apt_get install -y ca-certificates curl
  install -d -m 755 /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  . /etc/os-release
  printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
    "${UBUNTU_CODENAME:-$VERSION_CODENAME}" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/docker.sources
  apt_get update
  apt_get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker >/dev/null 2>&1
# Docker's repository joins Ubuntu's security updates in unattended-upgrades (Docker restarts, the containers too).
echo 'Unattended-Upgrade::Origins-Pattern { "origin=Docker,archive=${distro_codename}"; };' \
  > /etc/apt/apt.conf.d/51unattended-upgrades-docker

# A file mounted alone keeps its old content in a running container: the service is restarted when it changed.
restart=()
if [[ -f $dir/compose.yaml ]]; then
  cmp -s Caddyfile "$dir/Caddyfile" || restart+=(caddy)
  cmp -s couchdb.ini "$dir/couchdb.ini" || restart+=(couchdb)
fi
install -d -m 755 "$dir"
install -m 644 compose.yaml Caddyfile "$dir/"
# CouchDB's entrypoint chowns all of /opt/couchdb, and fails on a read-only file it doesn't own: its user's.
install -m 644 -o 5984 -g 5984 couchdb.ini "$dir/"
install -m 755 update "$dir/update"
install -m 600 env "$dir/.env"

# The deploy user: no password, no shell of its own (its key's forced command), no sudo but update.
id deploy >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/deploy --shell /bin/sh deploy
usermod -p '*' deploy
echo 'deploy ALL=(root) NOPASSWD: /opt/calendar/update' > /etc/sudoers.d/calendar-deploy.new
chmod 440 /etc/sudoers.d/calendar-deploy.new
visudo -cqf /etc/sudoers.d/calendar-deploy.new
mv /etc/sudoers.d/calendar-deploy.new /etc/sudoers.d/calendar-deploy
if [[ -f deploy.pub ]]; then
  install -d -m 755 /var/lib/deploy/.ssh
  printf 'restrict,command="sudo -n /opt/calendar/update \\"$SSH_ORIGINAL_COMMAND\\"" %s\n' "$(cat deploy.pub)" \
    > /var/lib/deploy/.ssh/authorized_keys
  chmod 644 /var/lib/deploy/.ssh/authorized_keys
fi

cd "$dir"
docker compose pull -q caddy davis couchdb
# Davis's php-fpm runs as www-data (82): its SQLite file is made in /data.
docker compose run --rm --no-deps --user root --entrypoint chown davis 82:82 /data
services=(caddy davis couchdb)
if docker image inspect calendar-backend:current >/dev/null 2>&1; then
  services+=(backend)
fi

# The vault for the Claude connector (spec §10.3). On another database the mirror starts from nothing: the files,
# the connector's history and trash beside them, and what the bridge remembers of both sides.
if [[ $bridge_reset == reset ]]; then
  echo "  … miroir du vault refait de zéro"
  docker compose --profile vault rm -sf bridge >/dev/null 2>&1 || true
  docker volume rm -f calendar_bridge-state >/dev/null
  docker compose run --rm owner 'find /vault -mindepth 1 -delete'
  # The backend opens its folders beside notes/ at start.
  if [[ " ${services[*]} " == *" backend "* ]]; then restart+=(backend); fi
fi
docker compose run --rm owner
if [[ -f bridge.json ]]; then
  export COMPOSE_PROFILES=vault
  # Its config holds my passphrase: root's, readable by the bridge's group only.
  install -d -m 750 -o root -g 1993 "$dir/bridge"
  if [[ -f $dir/bridge/config.json ]] && ! cmp -s bridge.json "$dir/bridge/config.json"; then
    restart+=(bridge)
  fi
  install -m 640 -o root -g 1993 bridge.json "$dir/bridge/config.json"
  install -m 644 bridge-database "$dir/bridge-database"
  docker image inspect livesync-bridge:1c25230 >/dev/null 2>&1 || {
    echo "  … image de livesync-bridge (construite depuis GitHub)"
    docker compose --profile vault build -q bridge
  }
  services+=(bridge)
else
  # Not (or no longer) asked for: stopped, its secrets gone. Its volumes stay, and bridge-database too: on the same
  # database again it goes on from where it was.
  docker compose --profile vault rm -sf bridge >/dev/null 2>&1 || true
  rm -rf "$dir/bridge"
fi
docker compose up -d --remove-orphans "${services[@]}"
if ((${#restart[@]})); then
  docker compose restart "${restart[@]}"
fi
for i in $(seq 30); do
  if out=$(docker compose exec -T -u www-data davis php /var/www/davis/bin/console \
    doctrine:migrations:migrate --no-interaction --allow-no-migration 2>&1); then
    break
  fi
  if ((i == 30)); then echo "$out" >&2; exit 1; fi
  sleep 2
done
echo "  ✓ Davis : base à jour"
if [[ -f bridge.json ]]; then
  # Its heartbeat, as its healthcheck reads it: up once both peers are (CouchDB's changes feed watched). A first
  # pass over a big vault can take longer than this wait: then said, not failed.
  up=false
  for _ in $(seq 60); do
    if docker compose exec -T bridge deno eval "const h = JSON.parse(Deno.readTextFileSync('/tmp/lsb-health.json'));
      if (Date.now() - h.ts > 60000 || !h.ok) Deno.exit(1)" >/dev/null 2>&1; then
      up=true
      break
    fi
    sleep 5
  done
  files=$(docker compose exec -T bridge find /app/data/notes -type f | wc -l)
  if $up; then
    echo "  ✓ pont : en marche, $files fichiers dans le miroir"
  else
    echo "  … pont : pas encore prêt après 5 minutes ($files fichiers dans le miroir) ; docker compose -f $dir/compose.yaml logs bridge"
  fi
fi
docker compose ps --format '  ✓ {{.Service}} : {{.Status}}'
INSTALL
ssh -t "$VPS" "sudo bash '$stage/install.sh' '$stage' '$bridge_reset'"

# --- After ---------------------------------------------------------------------------------------------------------

step "Vérifications après"
for sub in cal sync accounts; do
  retry curl -s -o /dev/null --max-time 5 "https://$sub.$DOMAIN/" ||
    die "pas de certificat valable sur $sub.$DOMAIN (sur le VPS : docker compose -f /opt/calendar/compose.yaml logs caddy)"
done
ok "certificats Let's Encrypt"

loc=$(curl -s -o /dev/null -w '%{redirect_url}' "https://cal.$DOMAIN/.well-known/caldav")
[[ $loc == "https://cal.$DOMAIN/dav"* ]] || die "/.well-known/caldav renvoie vers « $loc »"
code=$(curl -s -o /dev/null -w '%{http_code}' "https://cal.$DOMAIN/")
[[ $code == 404 ]] || die "le tableau de bord de Davis est public (https://cal.$DOMAIN/ : $code)"
ok "Davis : /dav/ et /.well-known/caldav publics, le tableau de bord non"

# The principal's address set, as the backend reads it (calendar-user-address-set). Password on stdin, not argv.
davis() {
  curl -s --config - "$@" "https://cal.$DOMAIN/dav/principals/$DAVIS_USER/" <<<"user = \"$DAVIS_USER:$DAVIS_PASSWORD\""
}
davis_ok() {
  davis -X PROPFIND -H 'Depth: 0' -H 'Content-Type: application/xml' --data '<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-user-address-set/></d:prop></d:propfind>' |
    grep -qiF "mailto:$DAVIS_EMAIL<"
}
until davis_ok; do
  if [[ -z $tunnel ]]; then
    ssh -N -o ExitOnForwardFailure=yes -L 127.0.0.1:9100:127.0.0.1:9000 "$VPS" &
    tunnel=$!
    sleep 2
  fi
  cat <<EOF
  Davis ne connaît pas encore ton utilisateur avec cette adresse et ce mot de passe. Une fois :
    1. ouvre http://localhost:9100 (le tableau de bord, par un tunnel SSH : il n'est pas public)
    2. connecte-toi : admin, mot de passe DAVIS_ADMIN_PASSWORD de deploy/.env
    3. http://localhost:9100/users/new : identifiant $DAVIS_USER, adresse $DAVIS_EMAIL, mot de passe
       DAVIS_PASSWORD de deploy/.env (s'il existe déjà : vérifie son adresse et son mot de passe)
EOF
  read -r -p "  Entrée quand c'est fait… "
done
if [[ -n $tunnel ]]; then kill "$tunnel" 2>/dev/null || true; tunnel=""; fi
ok "Davis : connexion de $DAVIS_USER ($DAVIS_EMAIL)"

enc=$(davis -X PROPFIND -H 'Depth: 0' -H 'Accept-Encoding: gzip, br, zstd' -D - -o /dev/null | tr -d '\r' |
  grep -i '^content-encoding:' || true)
[[ -z $enc ]] || die "Davis compresse sur /dav/ ($enc) : les ETag changent et If-Match échoue"
ok "/dav/ sans compression"

couch_ready() { [[ $(couch "https://sync.$DOMAIN/_node/_local/_config/chttpd/require_valid_user") == '"true"' ]]; }
retry couch_ready || die "CouchDB : pas de connexion, ou réglages de LiveSync absents"
code=$(curl -s -o /dev/null -w '%{http_code}' "https://sync.$DOMAIN/")
[[ $code == 401 ]] || die "sync.$DOMAIN répond $code sans mot de passe (attendu : 401)"
code=$(curl -s -o /dev/null -w '%{http_code}' "https://sync.$DOMAIN/_utils/")
[[ $code == 404 ]] || die "l'interface de CouchDB est publique (/_utils : $code)"
ok "CouchDB : réglages de LiveSync, mot de passe exigé, interface non publique"

if [[ -n $keydir ]]; then
  out=$(ssh -T -i "$keydir/key" -o IdentitiesOnly=yes -o BatchMode=yes "deploy@$ip" </dev/null 2>&1 || true)
  [[ $out == *"usage: update"* ]] || die "la clé de CI n'arrive pas au script update : $out"
  gh secret set VPS_SSH_KEY -R "$repo" < "$keydir/key" >/dev/null
  ok "clé SSH de CI : elle ne peut que lancer update"
fi
gh secret set VPS_HOST -R "$repo" --body "$ip" >/dev/null
gh secret set VPS_KNOWN_HOSTS -R "$repo" --body "$known" >/dev/null
ok "secrets GitHub : VPS_SSH_KEY, VPS_HOST, VPS_KNOWN_HOSTS"

if [[ -n $bridge_db && $bridge_from == "$copy" && $bridge_db != "$copy" ]]; then
  couch -X DELETE -o /dev/null "https://sync.$DOMAIN/$copy"
  ok "copie d'essai supprimée ($copy)"
fi

healthy() { [[ $(curl -s --max-time 5 "https://accounts.$DOMAIN/healthz") == ok ]]; }
if retry healthy; then
  ok "backend : https://accounts.$DOMAIN répond"
else
  echo "  … le backend ne répond pas encore : il arrive par CI (git push sur main, ou gh workflow run deploy.yml -R $repo)."
  echo "    S'il est déjà déployé : docker compose -f /opt/calendar/compose.yaml logs backend, sur le VPS."
fi

cat <<EOF

Prêt.
  Calendrier (iPhone : compte CalDAV ; Obsidian)
    serveur https://cal.$DOMAIN/dav/   utilisateur $DAVIS_USER   mot de passe : DAVIS_PASSWORD de deploy/.env
  Synchro du vault (Self-hosted LiveSync)
    https://sync.$DOMAIN   utilisateur $COUCHDB_USER   mot de passe : COUCHDB_PASSWORD de deploy/.env
  Page des comptes : https://accounts.$DOMAIN (ton utilisateur Davis)
    URI de redirection pour Google et Microsoft : https://accounts.$DOMAIN/oauth/callback (docs/comptes.md)
  Connecteur Claude : https://accounts.$DOMAIN/mcp (claude.ai : Réglages → Connecteurs → connecteur personnalisé)
    vault : ${bridge_db:-pas sur le VPS (calendrier et tâches seulement)}
  Tableau de bord de Davis : ssh -N -L 9100:127.0.0.1:9000 $VPS, puis http://localhost:9100
EOF
