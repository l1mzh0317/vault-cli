---
name: plugin-update
description: >-
  Use when the user wants to manually update / upgrade the Vault plugin itself to
  the latest published version. Triggers: "更新 vault 插件", "升级 vault plugin",
  "拉取最新插件", "update the vault plugin", "vault plugin 有新版吗", "plugin-update".
  Refreshes the marketplace cache and updates the installed `vault@vault-plugin`
  to the newest version on GitHub, then reminds the user to restart. Read-only on
  config/data — it only touches the plugin's own version. NOT for updating vault
  documents/sessions (that's vault-mcp) or switching servers (that's vault-manager).
---

# Vault Plugin — manual self-update

Pull the latest **published** version of the `vault` plugin from its marketplace
(`l1mzh0317/vault-plugin`) and update the local install. Use this when auto-update
is off or the user wants to force a refresh now.

This only changes the plugin's **version**. It does not touch the vault registry,
token, logging flag, or any documents — safe to run anytime, repeatable.

## Steps

1. **Show the current version** (so the user sees before/after):
   ```bash
   ls -d ~/.claude/plugins/cache/vault-plugin/vault/*/ 2>/dev/null | sed 's#.*/vault/##;s#/##' | sort -V | tail -1
   ```

2. **Refresh the marketplace cache** (pulls the latest `marketplace.json` from GitHub):
   ```bash
   claude plugin marketplace update vault-plugin
   ```

3. **Update the plugin** to the newest published version:
   ```bash
   claude plugin update vault@vault-plugin
   ```
   - If it reports `updated from X to Y` → tell the user the new version and that a
     **Claude Code restart** is required to load it.
   - If it reports already up to date / no update → tell the user they're on the
     latest; nothing to do.

4. **On network/marketplace error** (clone/refresh failed): report the error
     verbatim and stop. Do NOT force-reinstall or delete the cache.

## Notes

- Updates only land if the plugin author **bumped the version** in `plugin.json`
  (a pinned `version` means pushed commits without a bump produce no update).
- The plugin's own `userConfig` (vault URL/token) is preserved across updates.
- To update the *content* you store in a vault, use `vault-mcp`. To switch which
  vault you're on, use `vault-manager`. This skill only updates the plugin code.
