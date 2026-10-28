# calendar-backend

Côté serveur du plugin Obsidian calendar-tasks : le serveur CalDAV, son banc de test et le backend Go.

- `davis/` : Davis (sabre/dav, avec le scheduling des invitations), le serveur retenu. Lancé en Podman rootless
  sur `127.0.0.1:9000` par `run.sh`. Copier `davis.env.example` en `davis.env` et remplir les secrets ; la base
  SQLite vit dans `davis/data/` (non versionnée).
- `radicale/` : Radicale, gardé en référence (pas de scheduling). `python3 -m venv .venv &&
  .venv/bin/pip install radicale==3.8.3`, puis `run.sh` (port 5232, sans auth, données dans `cal/`).
- `bench/` : fixtures et vérifications du banc iPhone (`push-fixtures.sh`, `check.sh`), résultats dans
  `TESTS.md`.
- `start.sh` : relance Davis et Radicale après un redémarrage.

- Le backend Go (`cmd/`, `internal/`) : copie les comptes externes dans Davis (Google, Microsoft personnel ou de
  l'école, calendrier publié en ICS), chacun dans la collection que je choisis (« École », « Général »…, à côté de
  mes propres événements et tâches), et renvoie mes réponses aux invitations depuis l'adresse invitée. La page des
  comptes (connexion avec l'utilisateur Davis) ajoute, reconnecte ou retire un compte.

```bash
cp backend.env.example backend.env     # utilisateur Davis, mot de passe, coffre Obsidian, Google / Microsoft
go run ./cmd/calendar-backend run      # page des comptes sur http://localhost:8080 + synchro en continu
go run ./cmd/calendar-backend once     # une passe
go run ./cmd/calendar-backend accounts # comptes, dernière synchro, dernière erreur
go test ./...                          # + DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ pour les tests contre Davis
```

Brancher Google et Microsoft (une fois par serveur) : `docs/comptes.md`.

Sur le VPS : `./deploy.sh` installe ou met à jour Caddy (HTTPS), Davis, CouchDB (synchro du coffre Obsidian) et le
backend, depuis `deploy/` ; il demande ce qui manque dans `deploy/.env` et vérifie tout avant et après. Ensuite,
chaque push sur `main` déploie le backend (tests, image, mise à jour sur le VPS). Tout est décrit dans
`docs/SPEC.md` ; notes pour Claude dans `CLAUDE.md`.
