# Bitbucket automation relay (MVP)

The plugin can receive signed Bitbucket webhooks and forward matching deliveries
to an existing Kandev **Webhook** automation. This uses the plugin interfaces
available in Kandev 0.88.0; it does not require the native automation-adapter PR.
There is one relay per plugin installation. Configuration is installation-wide,
independent of workspace Bitbucket API connections.

```text
Bitbucket -- signed JSON --> plugin automation-relay webhook
                            verify signature, filter, persist claim
                            -- JSON + X-Webhook-Secret --> Kandev Webhook automation
```

## Setup

1. Create and enable a Kandev automation with the **Webhook** condition. Configure
   its agent, repository, prompt, and concurrency limit as usual. Copy its webhook
   URL and webhook secret.
2. In **Settings > Plugins > Bitbucket**, configure these relay fields and save:

   | Field                            | Value                                                         |
   | -------------------------------- | ------------------------------------------------------------- |
   | Relay Bitbucket product          | `cloud` or `data_center`                                      |
   | Relay repository                 | Exact Cloud `workspace/slug` or Data Center `PROJECT/slug`    |
   | Relay event                      | One event kind from the table below                           |
   | Relay branches                   | Optional comma-separated exact names, such as `main,release`  |
   | Relay CI conclusions             | CI only: optional `successful,failed,stopped`                 |
   | Automation webhook URL           | Full URL ending `/api/v1/automations/webhook/<automation-id>` |
   | Bitbucket webhook signing secret | A signing secret you choose for this webhook                  |
   | Kandev automation webhook secret | The secret copied from the automation                         |
   | Enable automation relay          | On                                                            |

   Both secret fields use Kandev's secret configuration storage. The two secrets
   serve different purposes. Saving plugin settings restarts the plugin.

3. Add a webhook to that Bitbucket repository with this public URL:

   ```text
   https://<your-public-kandev-host>/api/plugins/kandev-plugin-bitbucket/webhooks/automation-relay
   ```

   Set its **Secret** to the Bitbucket signing secret from step 2 and subscribe to
   the corresponding event below. Allow the route through your reverse proxy and
   preserve the raw request body and signature header. Use HTTPS for public ingress.

4. Trigger a matching event and inspect Bitbucket's delivery response and the
   automation's run history.

The destination URL must be reachable **from the plugin process**. A local HTTP
address is supported when both processes share a trusted machine/network;
otherwise use HTTPS to protect the automation secret. Redirects, URL credentials,
query parameters, fragments, and paths outside the automation webhook route are
rejected. The configured destination is operator-controlled and can point to a
private address. Payload fields never select the destination or its credentials.

## Events and filters

| Relay event           | Cloud subscription                                         | Data Center subscription | Branch filter                                  |
| --------------------- | ---------------------------------------------------------- | ------------------------ | ---------------------------------------------- |
| `pull_request_opened` | `pullrequest:created`                                      | `pr:opened`              | PR destination branch                          |
| `pull_request_merged` | `pullrequest:fulfilled`                                    | `pr:merged`              | PR destination branch                          |
| `push`                | `repo:push`                                                | `repo:refs_changed`      | Any changed branch; tags and deletions ignored |
| `ci_result`           | `repo:commit_status_created`, `repo:commit_status_updated` | Unsupported              | Must be empty                                  |

Repository matching is case-insensitive and exact. Branch matching is
case-sensitive and exact; an empty filter accepts all branches. CI accepts only
terminal `SUCCESSFUL`, `FAILED`, and `STOPPED` states with a commit link and status
key. An empty conclusion filter accepts all three. CI conclusions on other event
kinds, or Data Center CI configuration, make the relay unavailable.

The plugin verifies `X-Hub-Signature: sha256=...` against the original body, then
checks the payload's repository and event-specific fields. Data Center also
carries the event key in the signed body. Cloud's event header is not covered by
the signature, so it is a routing hint, not independent proof of the event type.
Configure the intended subscription in Bitbucket and protect deliveries with TLS.

See Atlassian's [Cloud webhook signing documentation](https://support.atlassian.com/bitbucket-cloud/docs/manage-webhooks/)
and [Data Center webhook documentation](https://confluence.atlassian.com/bitbucketserverm0930/manage-webhooks-1431541341.html).
Data Center must use SHA-256 signing. Unsigned delivery tests/pings are rejected.

Forwarding preserves the entire original JSON body. Existing automation template
variables such as `{{webhook.repository.full_name}}` (Cloud) remain available.
If one branch matches a multi-branch push, the **whole push payload** is forwarded
once; the relay does not rewrite it to remove other branches.

## Delivery guarantees and recovery

This MVP chooses **at most one forwarding attempt per exact body and destination
within seven days**. The existing Kandev automation endpoint has no idempotency
key, so automatic retries after a timeout could create duplicate work.

- A claim is persisted in plugin Host state **before** the outbound request.
  Concurrent duplicate requests are serialized. Restart, disable/enable, secret
  rotation, and filter changes preserve duplicate suppression.
- A successful destination response records `forwarded`. This means the endpoint
  accepted the HTTP request; check run history for actual admission and execution.
  Kandev can suppress a run because of its concurrency limit.
- Non-2xx responses, redirects, network failures, and lost completion writes
  require operator review. A claim left by a crash also requires review. Provider
  retries receive the same failure without another forwarding attempt. Even a
  definite 401/429 is not retried automatically in this MVP.
- Check destination run history before manually recreating missing work. Correct
  configuration for future events. Do not clear receipts or vary payload bytes
  to force a resend without first checking whether the original was admitted.
  There is no replay button or background retry worker in this MVP.
- The ledger holds at most 1,000 receipts. Expired entries are pruned on matching
  requests; when full of unexpired entries, new deliveries receive 503 before
  forwarding. State failures/corruption also fail closed. Expiration makes old
  payloads eligible again; this is bounded duplicate suppression, not permanent
  replay protection. Changing the destination URL creates a new identity.
- Receipt state contains hashes, timestamps, outcomes, and destination status
  codes; it contains no bodies or secrets. Disabling preserves state; uninstalling
  removes it. These guarantees assume one host-supervised plugin process and
  intact Host state, not multiple hosts sharing one ledger.

The public response contains a safe `status` and, once matched, a receipt hash:

| HTTP status                    | Meaning                                                                                      |
| ------------------------------ | -------------------------------------------------------------------------------------------- |
| 200 `forwarded` / `duplicate`  | Destination accepted / prior acceptance remembered                                           |
| 204                            | Validly signed payload did not match the filters                                             |
| 401                            | Missing, invalid, ambiguous, or mismatched signature                                         |
| 400 / 405 / 413 / 415          | Invalid payload/header, method, oversized body, or encoding                                  |
| 404                            | Relay disabled (or plugin/route unavailable)                                                 |
| 502 `delivery_requires_review` | An attempt was made or claimed; do not blindly resend                                        |
| 503                            | Configuration/state unavailable or invalid, capacity exhausted, or cancelled before claiming |

Bodies are limited to 1 MiB. The outbound request is bounded by a ten-second
client timeout and the host request context. Destination response bodies,
provider authorization headers, and signing headers are never forwarded back.

## Validation scope

The Go tests cover raw-body signing, both provider payload formats, repository
and branch filters, exact-body duplicate suppression, concurrent delivery,
persistent claims, secret rotation, corrupt/unavailable state, bounded receipt
storage, redirects, and lost HTTP responses. `go test -race ./internal/plugin
-run 'TestRelay|TestWebhookSignature'` exercises the relay's concurrency boundary.
The standard frontend tests still cover the existing plugin surfaces.

Live provider delivery is a separate acceptance check. This implementation does
not add native entries to Kandev's automation condition list, multiple relay
bindings, branch lookup for CI, or durable automatic retry scheduling.

### Packaged smoke test

Run against a **fresh disposable host on the same machine**, with a separate
`KANDEV_HOME_DIR`, `KANDEV_MOCK_AGENT=only`, and the matching `mock-agent` binary
available alongside the backend executable and on its PATH. The host must accept
local test requests without an interactive login. Build the package first:

```sh
make package-host
make verify-package-host
KANDEV_PLUGIN_E2E_URL=http://127.0.0.1:8080 \
KANDEV_PLUGIN_E2E_PACKAGE="$PWD/kandev-plugin-bitbucket-0.4.0.tar.gz" \
node scripts/smoke-automation-relay.mjs
```

The Node 24 script refuses to replace an existing Bitbucket installation. It
installs the package, creates a temporary Git repository and test automation,
and verifies secret masking, signature rejection, task creation, payload template
interpolation, duplicates across plugin restart, filters, and disabling. It then
uninstalls the test plugin and deletes its test automation, workspace, and local
repository. It does not contact Bitbucket or exercise real agent execution.

For this MVP, the Go tests, vet, race tests, package, and smoke test were run
against an unmodified `v0.88.0` checkout. When your sibling SDK checkout differs,
you can verify against a clean release checkout using a temporary modfile:

```sh
cp go.mod /tmp/bitbucket-relay.mod
cp go.sum /tmp/bitbucket-relay.sum
go mod edit -modfile=/tmp/bitbucket-relay.mod \
  -replace=github.com/kandev/kandev=/path/to/clean-kandev/apps/backend
go test -modfile=/tmp/bitbucket-relay.mod ./...
go vet -modfile=/tmp/bitbucket-relay.mod ./...
go test -race -modfile=/tmp/bitbucket-relay.mod ./internal/plugin \
  -run 'TestRelay|TestWebhookSignature'
```
