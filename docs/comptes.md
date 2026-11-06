# Brancher Google et Microsoft

Une fois par serveur, avant d'ajouter des comptes sur la page des comptes. Chacun le fait pour son propre backend,
avec ses propres comptes : rien n'est partagé.

Il faut l'adresse de la page des comptes, `BASE_URL` (sur le VPS : `https://accounts.<domaine>`). L'« URI de
redirection » à donner à Google et à Microsoft est `BASE_URL/oauth/callback` ; la page l'affiche dans « Réglages ».
Pour essayer en local, ajoute aussi `http://localhost:8080/oauth/callback` (les deux fournisseurs acceptent
`localhost` en HTTP).

Les valeurs obtenues vont, au choix (jamais dans git) :

- **sur la page des comptes**, « Réglages », « Connexion Google / Microsoft » : ID, secret (et annuaire pour
  Microsoft), pris en compte tout de suite, gardés dans la base du backend. Le secret ne s'affiche plus ensuite ;
  le laisser vide garde celui enregistré. Chaque carte rappelle où trouver ces valeurs ;
- **dans l'environnement** : `backend.env`, ou `deploy/.env` puis `./deploy.sh`. Il l'emporte : la page montre
  alors le réglage sans pouvoir le changer. Vider ces valeurs (et redéployer) rend la main à la page.

```
GOOGLE_CLIENT_ID=…
GOOGLE_CLIENT_SECRET=…
MICROSOFT_CLIENT_ID=…
MICROSOFT_CLIENT_SECRET=…
MICROSOFT_TENANT=common
```

La page propose alors « Se connecter à Google / Microsoft ». Changer d'ID client (ou d'annuaire) oblige à
reconnecter les comptes déjà ajoutés : la page le dit.

N'ajoute pas aussi ces comptes Google ou Microsoft sur l'iPhone : tout apparaîtrait deux fois.

## Google (Gmail)

Sur <https://console.cloud.google.com>, avec ton compte Google :

1. **Projet** : en haut, sélecteur de projet → Nouveau projet, par exemple `calendar-backend`.
2. **API** : APIs et services → Bibliothèque → « Google Calendar API » → Activer.
3. **Écran de consentement** (Google Auth Platform) → Premiers pas : nom de l'application (par exemple
   `Calendar Tasks Sync` ; ni « Google » ni une marque), ton adresse, audience **Externe** (« Interne » n'existe
   que pour Google Workspace), adresse de contact.
4. **Branding** (exigé avant de publier) : page d'accueil `BASE_URL`, règles de confidentialité
   `BASE_URL/privacy` (la page des comptes la sert, publique), domaine autorisé : le domaine racine (pour
   `accounts.cal.exemple.fr` : `exemple.fr`), pas de logo (un logo demande une validation par Google).
5. **Accès aux données** → Ajouter ou supprimer des champs d'application : `openid`, `…/auth/userinfo.email`, et
   `https://www.googleapis.com/auth/calendar.events` (s'il n'est pas dans la liste : « Ajouter manuellement ») →
   Mettre à jour → Enregistrer.
6. **Audience** → **Publier l'application** (statut « En production »). Indispensable : une application laissée
   « En test » perd sa connexion au bout de 7 jours. Pas besoin de la faire valider par Google pour un usage
   perso : à la connexion, Google affiche « Google n'a pas validé cette application » → Paramètres avancés →
   Accéder à <nom de l'application>.
7. **Clients** → Créer un client → type **Application Web** → URI de redirection autorisés : ton
   `BASE_URL/oauth/callback` (et celui de `localhost` pour les essais) → Créer.
8. Copie l'**ID client** et le **code secret du client** tout de suite : Google ne montre le secret qu'à la
   création (sinon, en ajouter un nouveau).

Ce que le backend demande : lire et modifier tes événements (copie dans Davis, tes réponses aux invitations, plus
tard les liens Meet) et ton adresse. Pas tes mails.

## Microsoft (Outlook.com, école, travail)

Sur <https://entra.microsoft.com> (ou le portail Azure → Microsoft Entra ID), avec un compte qui a un annuaire.
Depuis juin 2024, une application doit être inscrite dans un annuaire, et un compte personnel n'en reçoit plus
d'office : il faut passer par un compte Azure ou le programme développeur Microsoft 365, et des utilisateurs
signalent des blocages ou une carte bancaire demandée (pas essayé, `bench/TESTS.md`). Un compte d'école a déjà le
sien : voir « Compte de l'école » plus bas.

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

`MICROSOFT_TENANT` reste `common` : comptes personnels et de n'importe quelle organisation (sauf application
inscrite dans l'annuaire de l'école, ci-dessous).

**Compte de l'école** : Microsoft renvoie vers un administrateur une application récente de plusieurs annuaires, sans
éditeur vérifié, qui demande plus que les droits de base, et une école peut aussi bloquer les applications qu'elle ne
connaît pas (message `AADSTS65001` ou `AADSTS90094`, « besoin de l'approbation d'un administrateur »). Deux essais,
dans l'ordre :

1. **Inscrire l'application dans l'annuaire de l'école**, connecté avec le compte de l'école, si elle le permet.
   Types de comptes : **cet annuaire organisationnel uniquement** ; `MICROSOFT_TENANT` = l'« ID de l'annuaire
   (locataire) » de la page de l'application, au lieu de `common`. Seuls les comptes de l'école peuvent alors se
   connecter, et ses administrateurs voient l'application.
2. **Le plan B** (spec §4) : publier le calendrier de l'école en lien ICS (Outlook web → Calendrier → Paramètres →
   Calendriers partagés → Publier un calendrier) et l'ajouter sur la page comme « calendrier publié » ; il est en
   lecture seule (tes réponses ne partent pas).

## Ajouter un compte

Page des comptes → connecte-toi avec ton utilisateur Davis → « Ajouter un compte Google » (ou Microsoft) → choisis
le nom et la collection où ses événements arrivent (une des tiennes : « Général », « École »…) → connexion chez
Google ou Microsoft → retour sur la page. La première copie peut prendre quelques minutes.

Si un compte affiche « La connexion à ce compte a expiré », clique « Reconnecter » : il garde ses événements, et
les réponses qui n'avaient pas pu partir sont renvoyées.
