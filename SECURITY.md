# Security

Report vulnerabilities through [GitHub private vulnerability reporting](https://github.com/morhaf-ko/zerobill/security/advisories/new). Do not put tokens, private repository names or run logs from your own installation in a public issue. Use public issues for defects whose reproduction contains no private data.

zerobill uses `GITHUB_TOKEN`, then `GH_TOKEN`, then your stored GitHub CLI login.
Environment tokens override the stored login; status shows the selected source and account.
No Blacksmith API key is used. With `run --apply` it writes the `CI_RUNNER` and `BOT_RUNNER` repository variables and cancels and reruns workflow runs.

Applied recovery can force-cancel an owned cancellation after five minutes only
when the same attempt has no executed jobs. GitHub's run-level cancellation API
has no atomic attempt guard, so an external state change can race the final read.
Use one applied controller and retain its state; ambiguous recoveries require
operator attention. See the [recovery contract](docs/reference.md#what-run---apply-does).

To narrow what it can do, give it a fine-grained token with access to all of the owner's repositories:

| Need | Permissions |
| --- | --- |
| Always | Repository: Contents, Actions and Metadata read |
| Organization owner | Organization: Self-hosted runners and Administration read |
| Personal account | Repository: Administration read; Account: Plan read |
| `run --apply` | Repository: Actions and Variables write |

Verify a release before running it:

```sh
shasum -a 256 --ignore-missing -c checksums.txt
gh attestation verify zerobill_<version>_<os>_<arch>.tar.gz --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
gh attestation verify oci://ghcr.io/morhaf-ko/zerobill:<version> --repo morhaf-ko/zerobill \
  --signer-workflow morhaf-ko/zerobill/.github/workflows/release.yml
```
