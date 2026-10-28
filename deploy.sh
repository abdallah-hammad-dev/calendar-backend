#!/usr/bin/env bash
# Installs or updates the server on the VPS (spec §8): Caddy (HTTPS), Davis, CouchDB (the vault's sync) and the
# backend, from deploy/. Asks for what deploy/.env lacks and makes the secrets, checks everything before touching the
# VPS (values, SSH, DNS), then after (certificates, a Davis login, no compression on /dav/, CouchDB, the backend).
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

# Compose reads $ in .env, and quotes would be kept: none of them, nor backslashes.
bad=$(grep -nE '^[A-Z_]+=.*[$"'"'"'`\\]' "$env_file" | cut -d= -f1 || true)
[[ -z $bad ]] || die "deploy/.env : pas de \$, de guillemets ni de \\ dans les valeurs (ligne $bad)"
for k in VPS DOMAIN ACME_EMAIL DAVIS_USER DAVIS_EMAIL DAVIS_PASSWORD COUCHDB_USER COUCHDB_PASSWORD; do
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

# --- On the VPS (as root, one sudo) ------------------------------------------------------------------------------

step "Installation sur le VPS (mot de passe sudo de $VPS)"
stage=$(ssh "$VPS" mktemp -d)
scp -q deploy/compose.yaml deploy/Caddyfile deploy/couchdb.ini deploy/update "$VPS:$stage/"
grep -v '^VPS=' "$env_file" | ssh "$VPS" "umask 077 && cat > '$stage/env'"
if [[ -n $keydir ]]; then
  scp -q "$keydir/key.pub" "$VPS:$stage/deploy.pub"
fi
ssh "$VPS" "cat > '$stage/install.sh'" <<'INSTALL'
set -euo pipefail
stage=$1
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
docker compose ps --format '  ✓ {{.Service}} : {{.Status}}'
INSTALL
ssh -t "$VPS" "sudo bash '$stage/install.sh' '$stage'"

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

couch() { curl -s --config - "$@" <<<"user = \"$COUCHDB_USER:$COUCHDB_PASSWORD\""; }
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
  Tableau de bord de Davis : ssh -N -L 9100:127.0.0.1:9000 $VPS, puis http://localhost:9100
EOF
