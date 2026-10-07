# Security policy

**Report vulnerabilities privately: [open a private advisory](https://github.com/alebak/dokploy-tunnel/security/advisories/new)** (Security tab, "Report a vulnerability"). Do not open a public issue, pull request or discussion for a vulnerability.

dokploy-tunnel is a community project, not affiliated with Dokploy, maintained by [@alebak](https://github.com/alebak).

## Supported versions

dokploy-tunnel is pre-1.0. Only the latest minor release receives security fixes; upgrade to it before reporting.

| Version | Supported |
|---------|-----------|
| Latest `0.x` minor release | Yes |
| Older releases | No |

## How to report

1. Go to the [Security tab](https://github.com/alebak/dokploy-tunnel/security) and choose **Report a vulnerability**.
2. Describe the issue with the details below.
3. Keep it private until a fix is released and the advisory is published.

Include:

- The affected component (`doktunnel`, `doktunnel-companion`, `doktunnel-socket-proxy`, install scripts) and version (`--version`).
- Your OS, and for the companion, the Docker and Dokploy versions.
- Steps to reproduce, and what an attacker gains.
- Any proof of concept, logs or suggested fix. Remove real API keys, hostnames and other secrets first.

## What to expect

This is a solo-maintained project, so responses are best effort:

| Step | Target |
|------|--------|
| Acknowledgement | Within a few days |
| Assessment | Confirmed or declined, with reasons, in the advisory thread |
| Fix | A patch release, then a published advisory crediting you unless you prefer otherwise |

## Scope

In scope, for example:

| Area | Examples |
|------|----------|
| Companion | Bypassing Dokploy authorization, reaching a service the key cannot read, escaping the target checks described in the [README](README.md#how-it-reaches-services), leaking API keys in logs |
| Socket proxy | Any Docker API call or request body it forwards beyond the allowed list in the [README](README.md#socket-proxy) |
| Privileged hosts helper | Making the `hosts privileged-apply` helper write anything other than doktunnel's own entries, or read a file it should refuse |
| CLI credential storage | API keys written outside the OS keyring, or exposed in output, logs or state files |
| Install scripts | Installing an artifact whose checksum does not match |

Out of scope:

- **Dokploy itself.** Report Dokploy vulnerabilities [upstream to Dokploy](https://github.com/Dokploy/dokploy/security).
- **The documented trust-model limits.** Attacks that need what the [Trust model](README.md#trust-model) already grants, such as edit access to a Dokploy compose service or its custom compose command, are known limits, not vulnerabilities in dokploy-tunnel.
- Running the companion or socket proxy against the documented setup, such as publishing the socket proxy's port or serving the companion without TLS.
