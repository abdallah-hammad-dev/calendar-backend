# Banc de test CalDAV + iPhone

Tests de la section « Tests à faire avant de coder » du récap. Serveur retenu : **Davis** (sabre/dav), qui gère
les invitations ; Radicale reste en référence (`../radicale/`).

## Lancer

```bash
../davis/run.sh             # Davis (Podman) sur 127.0.0.1:9000 ; mot de passe admin dans ../davis/davis.env
./push-fixtures.sh          # crée la collection « Tests » (VEVENT + VTODO) d'abd et les éléments de test
./push-fixtures.sh --reset  # repart de zéro (dates recalculées : demain / après-demain)
./check.sh                  # ce que le serveur contient, propriétés utiles seulement (lu en HTTP)
```

Davis : utilisateurs `abd` (abd@bench.test) et `orga` (organisateur@bench.test), mot de passe `x`, créés dans le
dashboard (`http://127.0.0.1:9000/dashboard`). Pour Radicale : `COLL=http://localhost:5232/abd/tests/`.

## Réseau

WSL est en NAT : Tailscale est installé **dans WSL** (nœud `pc`). iOS exige HTTPS (sans SSL il n'envoie pas
le mot de passe et ignore le port), fourni par Tailscale Serve avec un vrai certificat (config persistante) :

```bash
sudo tailscale serve --bg --https=8443 9000   # Davis    → https://pc.example.ts.net:8443/dav/
sudo tailscale serve --bg 5232                # Radicale → https://pc.example.ts.net/
tailscale serve status
```

## iPhone

Réglages → Apps → Calendrier → Comptes calendrier → Ajouter un compte → Autre → Compte CalDAV :

- Serveur : `https://pc.example.ts.net:8443/dav/` (le `:8443` est obligatoire, sinon iOS tombe sur
  Radicale), utilisateur `abd`, mot de passe `x`.
- Activer **Calendriers et Rappels**. Dans Calendrier, activer « Rappels programmés ».
- Test réseau : l'URL dans Safari doit répondre (page Davis).
- Les changements serveur arrivent à la prochaine synchro iOS : tirer vers le bas pour forcer.

## Checklist

| # | Élément | Quoi faire | Ce qu'on regarde dans `./check.sh` |
|---|---|---|---|
| 1 | Test 1 — rappel daté demain 10h | Le voit-on dans Apple Calendar ? | — |
| 2 | même rappel | Le cocher dans Rappels | `STATUS:COMPLETED`, `COMPLETED:`, `PERCENT-COMPLETE:100` |
| 3 | rappel + Test 3 — événement | Renommer / déplacer chacun depuis l'iPhone | `URL:obsidian://…` et `X-TEST-MARKER`, `X-SOURCE-*` toujours là |
| 4 | Test 4 — parente / sous-tâche | Imbriqués ou à plat dans Rappels ? Peut-on créer une sous-tâche sur cette liste ? | `RELATED-TO` sur une sous-tâche créée depuis l'iPhone |
| 5 | Test 5 — invitation | Boutons Accepter / Refuser ? Les toucher | `PARTSTAT` de ta ligne `ATTENDEE` |
| 6 | Tests 6, 7, 8 — début (`DTSTART`) vs échéance (`DUE`) | Quelle date affiche chacun, dans quelle liste (Aujourd'hui / Programmés) ? Puis changer la date du test 6 sur l'iPhone | `DTSTART` et `DUE` du test 6 après la modif |
| 7 | Test 9 — rappel répété chaque semaine | La répétition s'affiche-t-elle ? Le cocher | Le même rappel avance-t-il (`DUE` + 7 j) ou y a-t-il une copie terminée ? |

Note sur le test 5 : Radicale annonce `calendar-user-address-set = /abd/` (pas d'adresse `mailto:`),
d'où la variante B, dont la ligne `ATTENDEE` porte cette valeur. Si aucune des deux n'affiche de
boutons, l'affichage des invitations sera seulement dans Obsidian.

## Résultats Radicale (2026-10-15, iPhone iOS 27.0.1)

| # | Résultat |
|---|---|
| Accès | iOS exige HTTPS : sans SSL il n'envoie pas le mot de passe et ignore le port. Résolu avec `tailscale serve`. |
| Collection mixte | Une collection VEVENT + VTODO apparaît dans Calendrier **et** Rappels. |
| 1 | ✅ Un rappel daté et horaire apparaît dans Calendrier (« Rappels programmés »). |
| 2 | ✅ Cocher écrit `STATUS:COMPLETED`, `COMPLETED:…`, `PERCENT-COMPLETE:100`. |
| 3 | ✅ `URL` et `X-` survivent aux modifs depuis l'iPhone (événement et rappel ; iOS réécrit `URL;VALUE=URI:`). Calendrier affiche l'URL en carte « Ouvrir » ; Rappels ne l'affiche pas, mais un lien dans les notes (`DESCRIPTION`) est visible. |
| 4 | ⚠️ Sous-tâches affichées à plat, pas d'option Sous-tâches sur une liste CalDAV. `RELATED-TO` survit à une modif, mais iOS retire `;RELTYPE=PARENT` (défaut RFC = parent). |
| 5 | ❌ Pas de boutons Accepter / Refuser (Radicale n'a pas le scheduling) ; iOS ne reconnaît pas l'utilisateur comme invité, ni par `/abd/` ni par mailto (même l'adresse iCloud). |
| 6 | Rappel créé sur l'iPhone : fichier = `UID` (UUID), `VTIMEZONE` + `DTSTART`/`DUE` en `TZID=Europe/Paris` (égaux), `VALARM` ajouté, pas d'`URL` ni de `DESCRIPTION`. |
| Fuseau | Heures en UTC → iOS affiche une 2ᵉ heure « (GMT) ». Avec `TZID=Europe/Paris` + `VTIMEZONE` : affichage normal. |
| Synchro | Les changements serveur arrivent à la prochaine synchro iOS (tirer vers le bas pour forcer). |

## Davis (sabre/dav) — test des invitations (2026-10-15)

Banc : `../davis/` (`run.sh`, Podman, `davis.env` avec le mot de passe admin), exposé par
`sudo tailscale serve --bg --https=8443 9000` → compte iPhone `https://pc.example.ts.net:8443/dav/`
(le `:8443` est obligatoire, sinon iOS tombe sur Radicale). Utilisateurs : `abd` (abd@bench.test), `orga`
(organisateur@bench.test), mot de passe `x`. Aucun mail : `INVITE_FROM_ADDRESS` vide.

| Cas | Résultat |
|---|---|
| E1 : invitation écrite par `PUT` direct dans le calendrier d'`abd` (organisateur externe) — le cas du backend | ✅ Boutons Accepter / Peut-être / Refuser + notification. Accepter écrit `PARTSTAT=ACCEPTED` dans la copie d'`abd`. Davis tente la réponse à l'organisateur externe et échoue proprement (`SCHEDULE-STATUS=3.7` sur `ORGANIZER`, aucun mail) |
| E2 : invitation envoyée par un organisateur local (`orga`) | ✅ Livrée dans le calendrier et l'inbox d'`abd`, boutons + notification. « Peut-être » → `TENTATIVE` chez `abd` et chez `orga` (`SCHEDULE-STATUS=2.0`), REPLY dans l'inbox d'`orga`, inbox d'`abd` vidée par iOS |
| Détail | En répondant, iOS réécrit les lignes `ATTENDEE` (ajoute `CUTYPE`, `EMAIL`, retire `RSVP`) : comparer seulement `PARTSTAT` |
| E3 (2026-10-22) : `orga` invite `abd`, réponse écrite par le code de l'extension (`setPartstat` + `PUT` `If-Match`), sans iPhone ni backend | ✅ Copie d'`orga` : `PARTSTAT=ACCEPTED;SCHEDULE-STATUS=2.0` pour `abd`, REPLY dans l'inbox d'`orga`. L'invitation a été livrée dans « Tests » (Davis prend un calendrier VEVENT d'`abd`, pas forcément « Général »). Éléments du test supprimés ensuite |

Ce que Davis fait du mail (code de l'image 5.5.0 lu le 2026-10-21) : il **envoie** seulement (iMIP par SMTP,
`DavisIMipPlugin`, `MAILER_DSN`), toujours depuis `INVITE_FROM_ADDRESS` (une adresse pour tout le serveur, l'invité
en `Reply-To`). Il ne **lit** aucune boîte : l'IMAP de Davis sert seulement à vérifier les mots de passe
(`AUTH_METHOD=IMAP`). Un seul email par utilisateur. Récupérer les invitations de Gmail / Outlook et y répondre
revient donc au backend.

## Davis — tests de base (2026-10-15)

Même comportement qu'avec Radicale (c'est iOS qui décide) :

| # | Résultat |
|---|---|
| 1 | ✅ Rappel visible dans Calendrier, **sans « (GMT) »** (heures en `TZID=Europe/Paris`). |
| 2 | ✅ Coché : `STATUS:COMPLETED`, `COMPLETED`, `PERCENT-COMPLETE:100` ; iOS ajoute un `VALARM`. |
| 3 | ✅ `URL`, `X-` et le lien dans les notes survivent (rappel et événement). Calendrier montre l'URL en carte sous « Localisation », libellée par l'hôte de l'URL (`calendar-tasks` = l'action du protocole). |
| 4 | (parent renommé, pas l'enfant : déjà vérifié sur Radicale, comportement iOS) |
| 5 | ✅ Boutons + acceptation : `PARTSTAT=ACCEPTED`, `SCHEDULE-STATUS=3.7` sur l'organisateur externe (aucun mail). iOS ajoute `X-APPLE-NEEDS-REPLY:TRUE`. |
| 6 | (2026-10-15) Rappels n'affiche que **`DUE`** : test 6 (`DTSTART` 16/10, `DUE` 20/10) → « 20/10/2026 » ; test 8 (`DUE` seul) → sa date ; test 7 (`DTSTART` seul) → **aucune date**. Le « jour prévu » doit donc passer par `DUE` pour exister sur l'iPhone. (Reste à voir : ce que devient `DTSTART` quand on change la date sur l'iPhone.) |

## Bizarrerie iOS

En activant Rappels sur un compte CalDAV, iOS crée tout seul une liste de tâches (VTODO seul) nommée
littéralement `DEFAULT_TASK_CALENDAR_NAME` (bug connu d'iOS). Le plugin la voit comme une collection normale.
(2026-10-17) C'est une liste de tâches normale du compte, sur le serveur : c'est aussi là que vont les tâches
créées dans Obsidian (réglage « Automatique » = la première par nom ; 17 éléments). Le plugin montre le nom tel
quel (une traduction « Rappels » côté plugin a été essayée puis retirée : le plugin ne doit rien faire de propre à
un client) ; la vraie correction est de la renommer sur le serveur. Couleurs lues sur Davis : `#0088FF` (Default
Calendar, créé par Davis), `#007AFF` (Tests et cette liste) — tout est bleu. Renommer et changer la couleur d'une
collection (PROPPATCH `displayname` + `calendar-color`, la propriété de couleur que lisent tous les clients CalDAV)
marche sur Davis et Radicale (test d'intégration). **À vérifier** : que l'iPhone reprend le nouveau nom et la
nouvelle couleur.
WebDAV `MOVE` d'un rappel d'une liste à l'autre marche sur Davis (201 ; l'ancienne adresse répond 404).
(2026-10-18) Les 17 tâches ont été déplacées dans une liste « Tâches » (VTODO seul, orange) et la liste créée par
iOS supprimée ; les notes n'ont pas bougé à la réouverture d'Obsidian. Le lendemain, iOS ne l'avait pas recréée,
mais Davis ne journalise pas les requêtes : on ne sait pas si l'iPhone s'est synchronisé entre-temps. **À
vérifier** : rouvrir Rappels sur l'iPhone, puis relister les collections du serveur. Créer (MKCALENDAR) et
supprimer (DELETE) une collection marchent sur Davis et Radicale (test d'intégration).
(2026-10-18, le soir) L'utilisateur veut des collections « contextes » (Général, Pro, École…) contenant
événements **et** tâches. « Default Calendar » (VEVENT seul, 16 événements) et « Tâches » (VTODO seul, 17 tâches)
ont été fusionnées par `MOVE` dans « Général » (VEVENT + VTODO, `#0090FF`), puis supprimées ; il reste
« Général » et « Tests » (mixte). **À vérifier sur l'iPhone** : « Général » apparaît à la fois dans Calendrier et
dans Rappels ; Davis ne recrée pas de calendrier par défaut ; iOS ne recrée pas sa liste.

## Davis — ETag et compression (2026-10-22)

Le Caddy de l'image `davis-standalone` (`encode zstd gzip`) compresse les réponses de plus de 512 octets quand le
client le demande, et **ajoute alors `-gzip` (ou `-zstd`) à l'en-tête `ETag`** : un `GET` avec
`Accept-Encoding: gzip` d'un élément de 1 ko répond `ETag: "4bdc…578f-gzip"`, et un `PUT` avec ce `If-Match` reçoit
**412**. Sans compression (`Accept-Encoding: identity`, ou un élément de moins de 512 octets) l'ETag est le vrai et
le `If-Match` passe. Les ETags dans le corps des `PROPFIND` / `REPORT` (`getetag`) ne sont pas touchés.

- Le backend envoie `Accept-Encoding: identity` sur toutes ses requêtes (`internal/davis/client.go`) : le client
  HTTP de Go demande gzip par défaut, et le test de bout en bout échouait (412) sur l'invitation (plus de 512
  octets avec son `VTIMEZONE`), pas sur l'événement simple (plus petit).
- **L'extension était touchée** (vérifié le 2026-10-22 par son test d'intégration, le `fetch` de Node demandant gzip,
  comme le fait très probablement Obsidian, non vérifié) : elle relisait un élément par `GET` quand le `PUT` ne renvoyait pas d'ETag (par exemple après
  une réponse à une invitation, où Davis ajoute `SCHEDULE-STATUS`), gardait l'ETag en `-gzip`, et le `PUT`
  suivant du même élément recevait 412 jusqu'à la synchro d'après. Corrigé : `CalDavClient.get` relit par un
  `calendar-multiget` sur la collection (ETag lu dans le corps).
- En prod (B0), un proxy devant Davis qui compresse ferait la même chose : ne pas activer la compression sur les
  chemins `/dav/`, ou le savoir.

## Davis — son journal recopie le mot de passe (2026-10-28)

En `APP_ENV=prod`, Davis journalise chaque exception sabre (404, 412, 403 « sync token » : normales en synchro) avec
sa trace, et le gestionnaire `fingers_crossed` de Monolog vide alors tout son tampon : la requête entière y est
recopiée, **en-têtes compris, dont `Php-Auth-Pw` en clair** (174 occurrences dans `var/log/prod.log` du banc, vérifié
sans afficher les valeurs). La base, elle, ne garde qu'une empreinte (`users.digesta1`, MD5 de
`user:realm:mot de passe` : faible, d'où des mots de passe longs et aléatoires en prod).

- `LOG_FILE_PATH=/dev/null` (variable lue par le `monolog.yaml` de l'image) : plus aucun fichier ; un `PUT` refusé
  (412) ne laisse rien, ni dans `var/log/` ni dans `podman logs` (vérifié le 2026-10-28). Davis fonctionne normalement.
- Le journal vit dans le conteneur, pas dans `data/` : `run.sh` le recrée à chaque lancement, l'ancien part avec.

## Backend B1 contre Davis (2026-10-22)

`DAVIS_TEST_URL=http://127.0.0.1:9000/dav/ go test ./...`, en tant qu'`orga` (jamais `abd`, dont le compte est
sur l'iPhone), dans une collection jetable supprimée à la fin.

| Test | Résultat |
|---|---|
| Client : découverte, `MKCALENDAR` (nom, couleur, VEVENT seul), `PUT` `If-None-Match` / `If-Match`, `sync-collection` | ✅ `calendar-data` est bien renvoyé dans le `sync-collection` de sabre (pas besoin de `multiget`) ; l'ETag du `PUT` est celui du `GET` et du `sync-collection` |
| Jeton inconnu au préfixe de sabre (`http://sabre.io/ns/sync/999999`) | ✅ sabre répond « rien de changé » + le même jeton ; détecté par le jeton courant de la collection, liste complète |
| Jeton d'un autre serveur (`urn:other:token`) | ✅ refusé par sabre, liste complète |
| Miroir d'un flux ICS : invitation, événement simple, série avec exception | ✅ mon `ATTENDEE` réécrit avec l'adresse Davis, `X-SOURCE-ATTENDEE`, `X-SOURCE-ACCOUNT`, `URL:obsidian://…`, heures UTC passées en `TZID=Europe/Paris` + `VTIMEZONE` |
| Réponse « depuis l'iPhone » (`PARTSTAT=ACCEPTED`, paramètres réécrits comme iOS, un `VALARM` ajouté) | ✅ vue par le backend, gardée quand la source change le titre (réponse « en vol »), oubliée quand la source la montre ; l'alarme est gardée |
| Un client renomme un événement miroir | ✅ la source l'emporte à la passe suivante |
| Événement retiré de la source | ✅ supprimé sur Davis |
| Événement supprimé sur Davis | Oublié, pas recréé tant que la source ne le change pas (provisoire, spec §11) |
| Passe sans changement | ✅ rien n'est réécrit (même ETag) |

(2026-10-22) Les propriétés partagées avec l'extension portent maintenant le préfixe `X-CALTASKS-`
(`X-CALTASKS-SOURCE-ACCOUNT`…) ; les résultats plus haut, et les éléments déjà sur le banc, gardent les anciens noms
(`X-SOURCE-ACCOUNT`…). Ce qui y est vérifié (les `X-` survivent aux modifs de l'iPhone) ne dépend pas du nom.

(2026-10-22, le soir) Chaque compte écrit maintenant dans une collection choisie, qui garde mes propres éléments :

| Test | Résultat |
|---|---|
| Collection « contexte » (VEVENT + VTODO) avec mon événement et ma tâche, plus les événements d'un compte | ✅ mes éléments jamais touchés (même ETag du début à la fin) |
| Collection inconnue, ou qui ne contient que des tâches | ✅ refusée à l'ajout du compte, avec la liste de mes collections |
| **Deux éléments avec le même `UID` dans une collection** (deux `PUT`, adresses différentes) | ⚠️ **Davis accepte les deux** (201, 201) : aucune vérification d'unicité. Le backend l'évite lui-même |
| Deux comptes dans la même collection, invités à la même réunion sur deux adresses | ✅ une seule copie ; quand le premier compte la perd, celle du second prend le relais |
| Suppression d'un compte avec ses événements | ✅ seuls les siens sont supprimés |

## Backend B2–B5 contre Davis (2026-10-23)

Même façon de faire (`orga`, collections jetables). Google et Microsoft ne sont pas encore essayés sur de vrais
comptes : leurs adaptateurs sont testés sur des réponses construites d'après la doc des API (spec §11, ce qu'il
faudra vérifier en premier).

| Test | Résultat |
|---|---|
| Relais : j'accepte « sur l'iPhone » | ✅ envoyé une fois, `X-CALTASKS-RSVP-STATE:confirmed` sur l'événement ; rien de renvoyé ni de réécrit à la passe suivante |
| Le fournisseur ne répond pas, puis refuse | ✅ `pending` et réessayé à chaque passe, puis `failed:<raison>` et plus réessayé ; ma réponse reste sur Davis |
| Je réponds autre chose ensuite | ✅ l'état est effacé, la nouvelle réponse part |
| La réponse est changée chez le fournisseur après l'envoi | ✅ la source l'emporte 10 minutes après l'envoi, pas avant ; rien n'est renvoyé |
| Flux ICS (lecture seule) : réponse depuis Davis | ✅ `failed:ce compte est en lecture seule, l'organisateur n'est pas prévenu` (la virgule arrive échappée `\,`, normal en TEXT) |
| Page des comptes : connexion avec l'utilisateur Davis, ajout d'un compte (faux Google), reconnexion, retrait | ✅ ; une reconnexion avec une autre adresse est refusée, un `state` ne sert qu'une fois |
| Binaire `run` : page + synchro, ajout d'un flux ICS par la page | ✅ une passe part tout de suite, l'invitation est dans Davis en moins d'une seconde ; réponse vue en moins de 2 s (`DAVIS_INTERVAL=2s`) |
