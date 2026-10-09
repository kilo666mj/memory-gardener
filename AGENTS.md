# AGENTS.md - memory-gardener

After Go changes, run:

```sh
gofmt -w .
go test -race ./...
go vet ./...
```

Keep this repository free of private DNS names, internal zones, private
addresses, credentials and deployment inventory. Real values belong in the
gitignored `ansible/inventory`, `ansible/host_vars/*.yml` or
`ansible/group_vars/*/secret.yml`, or in an Ansible Vault.

memory-gardener is a daily oneshot job, not a server. Deploy with
`ansible/playbook.yml`, which installs a systemd timer, checks that
`memory-gardener -version` reports the deployed commit and makes one read-only
`-no-judge` run. Run `ansible-playbook --syntax-check playbook.yml` after
playbook changes.

Never let the gardener write to Wayminder without a person's answer in
Taskboard. Keep policy defaults generic: real repository lists, host suffixes
and answerers belong in the private deploy inventory.

Use the shared modules in `docs/kits.md` for MCP, sign-in, notifications and
web push rather than re-implementing them. `errcheck` is blocking: handle or
explicitly discard every error, and preserve the primary error when deferred
cleanup also fails.
