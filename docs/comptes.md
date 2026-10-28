# Brancher Google et Microsoft

Une fois par serveur, avant d'ajouter des comptes sur la page des comptes. Chacun le fait pour son propre backend,
avec ses propres comptes : rien n'est partagé.

Il faut l'adresse de la page des comptes, `BASE_URL` (par exemple `https://comptes.mondomaine.fr`). L'« URI de
redirection » à donner à Google et à Microsoft est `BASE_URL/oauth/callback` ; la page l'affiche dans « Réglages ».
Pour essayer en local, ajoute aussi `http://localhost:8080/oauth/callback` (les deux fournisseurs acceptent
`localhost` en HTTP).

Les valeurs obtenues vont dans `backend.env` (ou le `.env` du déploiement), jamais dans git :

```
GOOGLE_CLIENT_ID=…
GOOGLE_CLIENT_SECRET=…
MICROSOFT_CLIENT_ID=…
MICROSOFT_CLIENT_SECRET=…
```

Puis relance le backend : la page propose alors « Ajouter un compte Google / Microsoft ».

N'ajoute pas aussi ces comptes Google ou Microsoft sur l'iPhone : tout apparaîtrait deux fois.

## Google (Gmail)

Sur <https://console.cloud.google.com>, avec ton compte Google :

1. **Projet** : en haut, sélecteur de projet → Nouveau projet, par exemple `calendar-backend`.
2. **API** : APIs et services → Bibliothèque → « Google Calendar API » → Activer.
3. **Écran de consentement** (Google Auth Platform) → Commencer : nom de l'application (`Calendrier`), ton
   adresse, audience **Externe**, adresse de contact.
4. **Accès aux données** → Ajouter des champs d'application : `…/auth/calendar.events`, `openid`,
   `…/auth/userinfo.email` → Enregistrer.
5. **Audience** → **Publier l'application** (statut « En production »). Indispensable : une application laissée
   « En test » perd sa connexion au bout de 7 jours. Pas besoin de la faire valider par Google pour un usage
   perso : à la connexion, Google affiche « Google n'a pas validé cette application » → Paramètres avancés →
   Accéder à Calendrier.
6. **Clients** → Créer un client → type **Application Web** → URI de redirection autorisés : ton
   `BASE_URL/oauth/callback` (et celui de `localhost` pour les essais) → Créer.
7. Copie l'**ID client** et le **code secret du client** tout de suite : Google ne montre le secret qu'à la
   création (sinon, en ajouter un nouveau).

Ce que le backend demande : lire et modifier tes événements (copie dans Davis, tes réponses aux invitations, plus
tard les liens Meet) et ton adresse. Pas tes mails.

## Microsoft (Outlook.com, école, travail)

Sur <https://entra.microsoft.com> (ou le portail Azure → Microsoft Entra ID), avec un compte Microsoft. S'il
n'a pas encore d'annuaire, Microsoft propose d'en créer un (gratuit).

1. **Inscriptions d'applications** → Nouvelle inscription :
   - nom : `calendar-backend` ;
   - types de comptes : **comptes dans un annuaire organisationnel (tout locataire Microsoft Entra ID) et comptes
     Microsoft personnels** ;
   - URI de redirection : plateforme **Web**, ton `BASE_URL/oauth/callback`.
2. Copie l'**ID d'application (client)** : c'est `MICROSOFT_CLIENT_ID`.
3. **Certificats et secrets** → Nouveau secret client → durée (24 mois au plus) → copie la **Valeur** (pas
   l'« ID de secret ») : c'est `MICROSOFT_CLIENT_SECRET`. Note la date d'expiration : il faudra en créer un
   nouveau avant, sinon les comptes Microsoft se déconnectent.
4. **Authentification** : ajoute l'URI `http://localhost:8080/oauth/callback` pour les essais si tu veux.
5. **Autorisations des API** → Ajouter → Microsoft Graph → Autorisations déléguées : `Calendars.ReadWrite`,
   `offline_access`, `openid`, `email`.

`MICROSOFT_TENANT` reste `common` : comptes personnels et de n'importe quelle organisation.

**Compte de l'école** : certaines écoles bloquent les applications qu'elles ne connaissent pas (message
`AADSTS65001` ou `AADSTS90094`, « besoin de l'approbation d'un administrateur »). Le plan B (spec §4) : publier le
calendrier de l'école en lien ICS (Outlook web → Calendrier → Paramètres → Calendriers partagés → Publier un
calendrier) et l'ajouter sur la page comme « calendrier publié » ; il est en lecture seule (tes réponses ne
partent pas).

## Ajouter un compte

Page des comptes → connecte-toi avec ton utilisateur Davis → « Ajouter un compte Google » (ou Microsoft) → choisis
le nom et la collection où ses événements arrivent (une des tiennes : « Général », « École »…) → connexion chez
Google ou Microsoft → retour sur la page. La première copie peut prendre quelques minutes.

Si un compte affiche « La connexion à ce compte a expiré », clique « Reconnecter » : il garde ses événements, et
les réponses qui n'avaient pas pu partir sont renvoyées.
