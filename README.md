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

- Le backend Go (`cmd/`, `internal/`) : copie les comptes externes dans Davis (une collection miroir par compte)
  et lit mes réponses aux invitations. Pour l'instant (B1) une seule sorte de compte, un flux ICS publié
  (lecture seule) ; Google, Microsoft et le relais des réponses viennent ensuite.

```bash
cp backend.env.example backend.env     # utilisateur Davis, mot de passe, nom du coffre Obsidian
go run ./cmd/calendar-backend add-feed -name "École (miroir)" -address moi@ecole.fr -url https://…/calendar.ics
go run ./cmd/calendar-backend once     # une passe ; « run » tourne en continu
go run ./cmd/calendar-backend accounts # comptes, dernière synchro, dernière erreur
go test ./...                          # + DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ pour les tests contre Davis
```

Pas encore fait : les comptes Google / Microsoft, le relais des réponses, la page des comptes, le déploiement sur
VPS (HTTPS, nom de domaine). Tout est décrit dans `docs/SPEC.md` ; notes pour Claude dans `CLAUDE.md`.
