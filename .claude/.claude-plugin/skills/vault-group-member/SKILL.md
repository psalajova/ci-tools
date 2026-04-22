---
name: vault-group-member
description: "Manage membership of HashiCorp Vault KV secret collection groups — add or remove users from the identity group that controls access to a selfservice KV path. Use this skill whenever the user wants to grant or revoke someone's access to Vault secrets, add/remove a person from a Vault secret collection, change ownership of Vault KV entries, or manage Vault group membership. Also trigger when the user mentions vault.ci.openshift.org group changes, selfservice secret access, or secret-collection-manager policies."
---

# Vault Group Member Management

This skill manages user membership in HashiCorp Vault identity groups that control access to KV secret collections on `vault.ci.openshift.org`.

## Background

On `vault.ci.openshift.org`, secret collections live under the `kv` mount at paths like `kv/data/selfservice/<collection>/*`. Access is controlled by identity groups — each collection has a corresponding group and policy named `secret-collection-manager-managed-<collection-name>`. Adding a user to the group grants them access to all secrets in that collection. Removing them revokes access.

The critical thing to understand: `vault write` on group membership is a **full replace**, not an append. You must always include the complete list of existing member entity IDs when updating, or you will accidentally remove people.

## Prerequisites

Before starting a Claude CLI session, the user must run these commands in their terminal:

```bash
export VAULT_ADDR='https://vault.ci.openshift.org'
vault login -method=oidc
```

This sets the Vault server address and authenticates via OIDC. The resulting token will be available to the Claude CLI session.

## Inputs

Ask the user for:
1. **The collection path** — e.g., `openshift-qe` (the part after `selfservice/`)
2. **The username(s)** to add or remove
3. **The operation** — add (default) or remove

## Workflow

### Step 1: Verify Prerequisites and Authentication

First, check that `VAULT_ADDR` is set:

```bash
echo "$VAULT_ADDR"
```

If `VAULT_ADDR` is empty or not set to `https://vault.ci.openshift.org`, **stop immediately** and tell the user:

> ⚠️ Prerequisites not met. Before running this skill, you need to run these commands in your terminal **before** starting the Claude CLI session:
>
> ```bash
> export VAULT_ADDR='https://vault.ci.openshift.org'
> vault login -method=oidc
> ```
>
> Then start a new Claude CLI session.

Do **not** attempt to run `export VAULT_ADDR` or `vault login` from within the CLI session — OIDC login requires browser interaction that won't work here.

If `VAULT_ADDR` is set correctly, proceed to check authentication:

```bash
vault token lookup
```

If this fails or shows limited policies, the user likely has a stale `VAULT_TOKEN` env var overriding their OIDC login. Guide them to exit the CLI session and run:

```bash
unset VAULT_TOKEN
vault login -method=oidc
```

Then verify admin access:

```bash
vault token capabilities sys/policies/acl/
```

They need `create, delete, list, read, sudo, update` on sys paths to manage groups.

### Step 2: Find the Policy

The policy naming convention is `secret-collection-manager-managed-<collection-name>`. Verify it exists:

```bash
vault policy read secret-collection-manager-managed-<collection-name>
```

The policy should contain paths like:
- `kv/data/selfservice/<collection>/*` with `create, update, read`
- `kv/metadata/selfservice/<collection>/*` with `list, delete`

If the exact policy name doesn't work, search:

```bash
vault policy list 2>/dev/null | while read p; do
  vault policy read "$p" 2>/dev/null | grep -q "<collection-name>" && echo "Found: $p"
done
```

### Step 3: Read the Current Group Membership

```bash
vault read identity/group/name/secret-collection-manager-managed-<collection-name>
```

Extract the `member_entity_ids` list. This is the current set of users with access.

### Step 4: Resolve Entity IDs to Names

Map each entity ID to a human-readable name so the user can verify the current members:

```bash
for id in <space-separated-entity-ids>; do
  name=$(vault read -field=name "identity/entity/id/$id" 2>/dev/null)
  echo "$id -> $name"
done
```

Present this list to the user so they can see who currently has access.

### Step 5: Look Up the New User's Entity

```bash
vault read identity/entity/name/<username>
```

Extract the `id` field. If the entity doesn't exist, inform the user — the person may need to log into Vault at least once via OIDC before they have an entity.

Check if the user's entity ID is already in the group's `member_entity_ids`:
- If **adding** and already present: inform the user, no action needed
- If **removing** and not present: inform the user, no action needed

### Step 6: Construct and Present the Command

Build the `vault write` command with the **full list** of member entity IDs (existing + new for adds, existing - target for removes).

**For adding:**
```bash
vault write identity/group/name/secret-collection-manager-managed-<collection-name> \
  member_entity_ids="<all-existing-ids-comma-separated>,<new-id>"
```

**For removing:**
```bash
vault write identity/group/name/secret-collection-manager-managed-<collection-name> \
  member_entity_ids="<all-existing-ids-minus-removed-comma-separated>"
```

Always show the command to the user and explain:
- This replaces the entire member list
- Verify the count: "This will update the group from N to M members"
- List who is being added/removed by name

Wait for the user to confirm before executing.

### Step 7: Verify

After the user runs the command, verify the change:

```bash
vault read identity/group/name/secret-collection-manager-managed-<collection-name> | grep member_entity_ids
```

For adds, also verify from the user's perspective:
```bash
vault read identity/entity/name/<username>
```

Check that `direct_group_ids` now includes the group ID.

## Handling Multiple Users

If the user wants to add/remove multiple people at once, look up all entity IDs first, check for duplicates/presence, then construct a single `vault write` command with all changes applied at once.

## Common Issues

- **403 on vault commands**: The `VAULT_TOKEN` env var may be set to a stale token. Run `unset VAULT_TOKEN` then `vault login -method=oidc`.
- **Entity not found**: The user hasn't logged into Vault before. They need to authenticate at least once via OIDC to create their entity.
- **Policy not found**: The collection name might not match the policy naming convention exactly. Use the search approach from Step 2.
- **Lost members after write**: This happens when you forget to include all existing IDs. Always copy the full `member_entity_ids` list from the current group state before modifying.
