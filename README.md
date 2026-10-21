# calendar-backend

Côté serveur du plugin Obsidian calendar-tasks : le serveur CalDAV et son banc de test.

- `davis/` : Davis (sabre/dav, avec le scheduling des invitations), le serveur retenu. Lancé en Podman rootless
  sur `127.0.0.1:9000` par `run.sh`. Copier `davis.env.example` en `davis.env` et remplir les secrets ; la base
  SQLite vit dans `davis/data/` (non versionnée).
- `radicale/` : Radicale, gardé en référence (pas de scheduling). `python3 -m venv .venv &&
  .venv/bin/pip install radicale==3.8.3`, puis `run.sh` (port 5232, sans auth, données dans `cal/`).
- `bench/` : fixtures et vérifications du banc iPhone (`push-fixtures.sh`, `check.sh`), résultats dans
  `TESTS.md`.
- `start.sh` : relance Davis et Radicale après un redémarrage.

Pas encore fait : le backend Go (comptes externes en miroir, relais des réponses aux invitations) et le
déploiement sur VPS (HTTPS, nom de domaine). Tout est décrit dans `docs/SPEC.md` ; notes pour Claude dans
`CLAUDE.md`.
