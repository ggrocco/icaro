# Security policy

Ícaro runs semi-trusted agent output inside containers on your machine, so security reports get priority.

## Reporting a vulnerability

Report privately through [GitHub security advisories](https://github.com/ggrocco/icaro/security/advisories/new). Please do not open a public issue for a vulnerability.

Include the affected version (`icaro version --long`), your OS and Docker version, and the steps to reproduce. You will get an acknowledgement within 3 working days and a fix or mitigation plan within 14 days for confirmed issues.

## In scope

- Escaping the container baseline or the mandatory egress allowlist.
- Reading another session's or project's memory, or a secret value, through the API, the MCP server or the CLI.
- Bypassing host-bound integration credentials, the SSRF guard, or webhook signature verification.
- A malicious compose file, workflow or integration pack getting past validation.
- Installing or updating to an artifact that fails its checksum or signature.

The threat model and the mandatory policies are in [blueprint/docs/03-security.md](blueprint/docs/03-security.md). Known residual risks, such as credentials living inside the agent container, are listed in [blueprint/docs/02-decisions-risks.md](blueprint/docs/02-decisions-risks.md#residual-risks).

## Verifying releases

Every release publishes `checksums.txt`, an ed25519 signature `checksums.txt.sig`, and a Sigstore bundle `checksums.txt.sigstore.json`. `install.sh` and `icaro update` refuse an artifact whose checksum or signature does not verify. See [ADR 0021](blueprint/docs/adr/0021-release-signing-and-updates.md).

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp 'https://github.com/ggrocco/icaro/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --ignore-missing -c checksums.txt
```
