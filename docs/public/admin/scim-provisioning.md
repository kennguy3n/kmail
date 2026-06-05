---
title: SCIM provisioning guide
description: Automatically provision and de-provision KMail users and groups from your identity provider using SCIM 2.0.
category: Admin
order: 40
updated: 2026-06-05
---

KMail implements **SCIM 2.0** (RFC 7643/7644) so your identity provider
(Okta, Entra ID/Azure AD, OneLogin, JumpCloud, …) can create, update,
and deactivate users and groups automatically.

## 1. Create a SCIM token

1. Go to **Admin → SCIM** (or `POST /api/v1/tenants/{id}/scim/tokens`).
2. Generate a token and copy it — it's shown only once.
3. Store it in your IdP as the SCIM **bearer token**.

You can list and revoke tokens at any time:

- `GET /api/v1/tenants/{id}/scim/tokens`
- `DELETE /api/v1/tenants/{id}/scim/tokens/{tokenId}`

## 2. Configure your IdP

Point your IdP's SCIM connector at:

```
Base URL: https://api.kmail.kchat.dev/scim/v2
Token:    <the bearer token from step 1>
```

KMail advertises its capabilities at:

- `GET /scim/v2/ServiceProviderConfig`
- `GET /scim/v2/ResourceTypes`
- `GET /scim/v2/Schemas`

## 3. Supported operations

**Users** (`/scim/v2/Users`):

| Method   | Path                    | Purpose                          |
| -------- | ----------------------- | -------------------------------- |
| `GET`    | `/scim/v2/Users`        | List/filter users.               |
| `POST`   | `/scim/v2/Users`        | Provision a new user.            |
| `GET`    | `/scim/v2/Users/{id}`   | Fetch a user.                    |
| `PUT`    | `/scim/v2/Users/{id}`   | Replace a user.                  |
| `PATCH`  | `/scim/v2/Users/{id}`   | Partial update (e.g. activate).  |
| `DELETE` | `/scim/v2/Users/{id}`   | De-provision a user.             |

**Groups** (`/scim/v2/Groups`): `GET`, `POST`, `GET {id}`,
`PATCH {id}`, `DELETE {id}`.

## Deactivation vs. deletion

Setting a user's `active` attribute to `false` (a `PATCH`) suspends
sign-in and mail flow but preserves the mailbox per your retention
policy. A `DELETE` removes the user and schedules data deletion. Most
IdPs send a `PATCH active:false` on offboarding — configure deletion
behaviour in **Admin → SCIM** to match your compliance needs.

## Tips

- Map your IdP's `userName` to the user's full KMail email address.
- Use **group push** to drive shared-inbox or distribution membership.
- Filtering supports `userName eq "alice@yourdomain.com"` for IdP
  reconciliation.

See the [API reference](/docs/api) for full request/response schemas.
