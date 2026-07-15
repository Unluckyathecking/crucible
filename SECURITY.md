# Security policy

## Supported versions

Crucible has not published a tagged stable release. Security fixes target the latest
commit on `main`; older commits, forks, and product adaptations are not maintained by
this repository. Pin a reviewed commit and monitor upstream changes.

## Reporting a vulnerability

Do not open a public issue, discussion, or pull request containing vulnerability
details or secrets.

Use GitHub's **Report a vulnerability** button on the repository Security page to
start a private report:

<https://github.com/Unluckyathecking/crucible/security/advisories/new>

Include the affected commit, component, impact, reproduction steps or proof of
concept, and any suggested mitigation. If private reporting is unavailable, open a
minimal issue asking the maintainer to establish a private channel; do not include
technical details in that issue.

The maintainer will acknowledge a report when available, assess severity and scope,
coordinate a fix, and credit reporters who want attribution. Response or remediation
times are not guaranteed. Please allow a reasonable remediation window before public
disclosure.

## Scope

Reports about Crucible source code and the example deployment assets are in scope.
Product-specific forks, exposed credentials, social engineering, denial-of-service
testing against live systems, and third-party services are outside this project's
control; report third-party issues to their owners.
