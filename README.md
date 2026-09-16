# Teleport Telegram

Approve or deny Teleport role Access Requests directly in a private Telegram chat.
A small Go service watches Teleport, shows the complete reason and role scope, and
applies a decision only after the configured Telegram user presses a button.

This project is exclusively a Telegram integration. It has no approval web UI,
public webhook, other messaging integrations, or automatic approval rules. Its
only HTTP endpoints are Kubernetes health probes.

## How it works

```mermaid
sequenceDiagram
    participant Agent as Requesting identity
    participant Teleport as Teleport Auth
    participant Bridge as Dedicated approver
    participant Telegram as Telegram API
    participant Human as Allowed human
    Agent->>Teleport: Request temporary role + reason
    Teleport-->>Bridge: Pending request (watch / reconciliation)
    Bridge->>Telegram: Private message with Approve / Deny buttons
    Telegram-->>Human: Request, full role scope, deadlines
    Human->>Telegram: Press button
    Bridge->>Telegram: Fetch callback through HTTPS long polling
    Telegram-->>Bridge: Sender ID + chat + message + callback nonce
    Bridge->>Teleport: Re-read request and verify state / expiry
    Bridge->>Bridge: Persist attempted-decision marker
    Bridge->>Teleport: Approve or deny using its own mTLS identity
    Bridge->>Telegram: Show authoritative result; remove buttons
    Agent->>Teleport: Obtain short-lived certificate if approved
```

The bridge does not issue credentials or execute the requested operation.
Approval grants the requested **role's full permissions**; the reason is a
requester-supplied explanation, not an enforced command or namespace restriction.
Teleport limits the elevated certificate, independently of the bridge staying up.

## Security boundary — read before installing

There are two independent credentials:

- **Telegram bot token:** authenticates the service to Telegram. It does not grant
  Teleport access. The service accepts only one numeric Telegram user ID, in that
  user's private chat, bound to a stored message ID and random 128-bit nonce.
  A forwarded button, matching display name, or request UUID is not authority.
- **Teleport client identity:** authenticates the service to Teleport using mutual
  TLS. Its role has only `access_request: [list, read, update]`, without resource
  access, user/role administration, request creation or certificate signing.
  Ordinary users cannot get these permissions just by calling the same API.

**A stolen approver identity can approve requests without Telegram.** The human
button check is enforced by this process, not cryptographically verified by
Teleport. Similarly, a compromised Telegram account/session is indistinguishable
from its owner. A stolen bot token can interfere with delivery or impersonate the
bot in chat, so both secrets matter. HTTPS and the Telegram service are trusted;
this is not end-to-end signed human authorization.

Never let the requesting agent read, replace or mint the approver's identity, or
modify its code, startup approver ID, ServiceAccount, join policy, runtime secrets,
volumes, image, release/deployment pipeline or underlying host. NetworkPolicy and
SealedSecrets are useful controls but do not protect secrets from a pod/node or
cluster administrator who can read them at runtime.

For an agent with bounded Kubernetes operations, keep it unable to:

- Read/list/watch protected Secrets; exec/attach/debug the approver; create or
  modify workloads in its namespace; mount its PVC; mint its ServiceAccount token.
- Change RBAC, admission policy, namespaces, Teleport operator resources, or the
  GitOps controllers that enforce those restrictions.
- Reach the same powers indirectly through privileged/hostPath pods, node proxy
  access, writable hosts/hypervisors, backup restore or operator-specific CRDs.
- Replace the deployed image through Git or the registry. Pin a digest whose
  selection is controlled by a human outside the agent's write permissions.

RBAC is additive. Inspect all bindings, including aggregated roles and inherited
credentials; a check against one role is insufficient. A promise in an agent's
instructions not to edit Git does not enforce any of these controls. Use an
owner-controlled deployment repository or enforced protection that the agent
cannot bypass. Source contributions and image builds need not confer deployment
rights. See Kubernetes' [RBAC escalation guidance](https://kubernetes.io/docs/concepts/security/rbac-good-practices/).

If the agent may become cluster/host administrator, a different namespace in that
cluster cannot provide this boundary. Put **both the approver and Teleport's
approval authority**, including their hosts, credentials and deployment control,
outside the infrastructure that the agent can administer. Moving only the bot
leaves the Teleport server itself open to tampering. This service cannot create
that infrastructure boundary for you, and makes no unconditional isolation claim.

The reviewer role can update requests cluster-wide. Requester/role allowlists are
additional application policy, not per-request Teleport RBAC. Existing trusted
Teleport administrators remain able to resolve requests using their own credentials.

## Compatibility and behavior

- Tested with **Teleport Community 18.11.1**. The official Go API dependency is
  pinned to source commit `27eb07217a0efca4a5d2dd88bce7472cba5f406a`.
- Uses the administrative `SetAccessRequestState` API used by `tctl request
  approve/deny`, not Enterprise review rules. Community's supported manual path
  is documented in [Role Access Requests](https://goteleport.com/docs/identity-governance/access-requests/oss-role-requests/).
  The full approval web UI/reviewer workflows are Enterprise features.
- Only pending role requests from one configured requester and for configured
  roles are actionable. Scheduled, resource and dry-run requests are unsupported.
- Maximum request window is 15 minutes from creation, with one second of tolerance
  for Teleport's separately sampled timestamps. Actual deadlines are never extended.
- Reasons and role scopes are rendered as plain text, with control characters
  removed. Requests too long to show completely are withheld and logged.
- Only outbound HTTPS to Telegram is needed. The phone need not reach Teleport.
  A particular airline's messaging-only service still needs a real connectivity test.

No maintained Telegram Access Request plugin was identified in the official
[plugin catalogue](https://goteleport.com/docs/identity-governance/access-requests/plugins/)
or GitHub searches when this was implemented. The project reuses Teleport's
client library and the standard library for Telegram HTTP calls.

## Configuration

Set the sole human approver **at process startup**:

```bash
export TELEGRAM_APPROVER_ID=123456789
```

A missing, zero, negative or nonnumeric value stops startup. There is no default
approver, username lookup, chat command for enrollment, or fallback to another user.
The numeric value in this README is an example, not a built-in account.

Copy [`examples/config.json`](examples/config.json) to a private local config:

| JSON field | Meaning |
|---|---|
| `auth_address` | Reachable Teleport Auth gRPC address, normally port 3025 |
| `identity_file` | Renewable Machine ID output `identity` file; mounted read-only |
| `token_file` | File containing the dedicated Telegram bot token |
| `state_file` | Durable callback journal; one writer only |
| `cluster` | Human-readable cluster label shown in messages; set it accurately |
| `requester` | Exact Teleport username whose requests may be handled |
| `role_scopes` | Allowed role names mapped to accurate, human-readable descriptions |

Every field is required. A role missing from `role_scopes` fails closed. These
scope descriptions must be maintained alongside actual Teleport/RBAC policies.
The `cluster` label does not replace TLS certificate validation.

## Create the Telegram bot

1. Use Telegram's verified [@BotFather](https://t.me/BotFather) and `/newbot` to
   create a dedicated bot. Keep its token in an operator-owned secret manager.
2. Disable group invitations using `/setjoingroups`. Open the bot's private chat
   and send `/start`, so it can send you messages.
3. Run `python3 scripts/telegram-bootstrap.py identify` from your own workstation.
   It prompts for the token without echo and prints numeric IDs only from pending
   private `/start` messages. Verify which ID is yours before configuring it.
   Do this before starting the bridge; it must be the sole update poller.
4. Supply the token through a mounted private file/Secret. Never put it in a
   command argument, image, issue, chat transcript or Git. The process reads it
   on startup; restart after rotation.

An existing Telegram webhook causes startup to fail. Remove it intentionally or
use another bot; the service will not silently take over an existing integration.

For a SealedSecrets installation, this optional helper emits only ciphertext:

```bash
python3 scripts/telegram-bootstrap.py seal --cert /path/to/cluster-public-cert.pem \
  > telegram-secret.yaml
```

It seals the key `token` for the Secret `teleport-approvals/telegram-approver` by
default; use `--namespace` if you choose another namespace. The public
certificate must belong to your trusted controller. The helper requires
`kubeseal` and passes plaintext via stdin only. Sealing protects the Git copy;
it does not protect a running Secret from its cluster administrators.

## Build and run

Go **1.26.8** and a C compiler for the race detector:

```bash
git clone https://github.com/rubasace/teleport-telegram.git
cd teleport-telegram
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o teleport-telegram .
TELEGRAM_APPROVER_ID=123456789 ./teleport-telegram -config /path/to/config.json
```

Provision a dedicated Machine ID bot as described below or with Teleport's
[Machine ID documentation](https://goteleport.com/docs/machine-workload-identity/).
Run `tbot` in a separate trusted process/container to renew its output identity.
Do not give this service the requester's credentials or mount its identity into
the requester's environment. For a non-Kubernetes host, choose an appropriate
supported workload-attestation join method; the Kubernetes example is not a
portable host-enrollment token.

The Dockerfile builds the same static binary in a distroless, nonroot image:

```bash
docker build -t teleport-telegram:local .
docker run --rm --name teleport-telegram \
  --user 65532:65532 --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  -e TELEGRAM_APPROVER_ID=123456789 \
  -v /trusted/config:/config:ro \
  -v /trusted/identity-output:/identity:ro \
  -v /trusted/secrets:/secrets:ro \
  -v /trusted/telegram-state:/state \
  teleport-telegram:local -config /config/config.json
```

Prepare the state directory writable by UID 65532 and readable only by trusted
operators. Give that UID read access to the mounted token and identity. No port
publication is required. The process binds health probes on port 8080.

## Kubernetes installation

Use this option only if requesting identities cannot administer the management
namespace, cluster nodes, Teleport authority or the deployment path described in
the security boundary above. A trusted administrator performs installation.

1. Review [`examples/teleport-resources.yaml`](examples/teleport-resources.yaml).
   It creates the approver role, Machine ID bot and Kubernetes join token through
   Teleport's operator in namespace `teleport`. The join policy allows exactly
   `teleport-approvals:telegram-approver`. Install the operator/CRDs first.
2. Review [`examples/kubernetes.yaml`](examples/kubernetes.yaml). Replace the
   cluster/proxy hostname, Auth service address, requester, roles/scopes, numeric
   approver ID, storage class and image. Match the projected token audience to the
   actual Teleport cluster name. Keep the namespaces/join policy in sync.
3. Put the bot token in the `teleport-approvals/telegram-approver` Secret, key
   `token`, using your secret manager or the helper above. No real secret is
   included in this repository.
4. Build/publish the image, review it and pin its **digest** in the Deployment.
   The example intentionally has zero replicas, approver ID 0 and an invalid
   bootstrap image. Set replicas to exactly 1 only after reviewing all values.
5. Apply the resources from your trusted administration environment, wait for the
   operator and `tbot` to enroll, then wait for `/readyz` to succeed. Use the live
   acceptance checks below before allowing real elevated operations.

The example uses a dedicated ServiceAccount with no Kubernetes RoleBindings. Only
`tbot` mounts a short-lived projected ServiceAccount token; the bridge reads only
its renewable output identity. Neither container shares volumes with a requester.
Internal credentials live in memory, the decision journal on a dedicated PVC.
Both containers run nonroot with dropped capabilities and read-only root filesystems.
There is no Service or Ingress. NetworkPolicy permits DNS, HTTPS and Teleport Auth;
HTTPS egress is not an FQDN allowlist.

CI runs tests/race detection, vet, a static build and a disposable Community
integration test. Image publication is a separate manually dispatched workflow
on `main`, producing a commit-tagged image and digest. It never deploys anything.
Configuring human-controlled deployment and repository protection is an operator
responsibility; a manual workflow alone is not a security approval gate.

## Tests and live acceptance

Unit tests cover wrong senders/chats, forwarded/forged/stale buttons, expiry,
role/requester restrictions, changed requests, external resolution, restarts,
ambiguous mutations, storage failure and HTTP token redaction.

A repeatable real Community integration test requires Teleport 18.11.1 binaries:

```bash
bash test-integration.sh /path/to/teleport /path/to/tctl /path/to/go
```

It starts a disposable Auth server on loopback port 13025, creates empty fixture
roles, verifies restricted permissions and decisions, and deletes its credentials
on exit. It needs no Telegram token and never accesses a production cluster.

Before production, verify with real credentials and a safe test resource:

- The intended human can approve and deny; another account cannot. The requesting
  identity cannot approve itself or obtain the approver's identity.
- The role's full scope matches the message. Approve, consume the certificate,
  test an authorized operation and verify expiry prevents further access.
- The elevated agent cannot read protected Secrets, create/mutate privileged
  workloads, exec/debug them, mint their tokens, edit RBAC/operator resources or
  replace deployed code through GitOps. Test *both* base and elevated identities.
- Pending prompts survive restart. Duplicate clicks and ambiguous responses cannot
  replay a mutation. External resolutions eventually remove the buttons.
- Certificate renewal, Telegram/Teleport outages, PVC persistence and recovery
  work. Test the intended restricted mobile network separately.

Telegram buttons were also exercised successfully with a real human account
against a disposable Community server during development. That does not replace
verification of your production deployment or its privilege boundaries.

## Failure handling and operation

Teleport remains the source of truth. Watches are hints: full reconciliation runs
on startup, reconnect and every 30 seconds. API connections reload renewed identity
files; watch connections rotate every five minutes. `/healthz` reports process
health; `/readyz` fails after two minutes without successful reconciliation or
Telegram polling. A missing initial identity may briefly keep readiness false.

One process owns the journal through an OS file lock. Writes use atomic rename
and fsync. An attempted decision is durable **before** the Teleport mutation.
An ambiguous result is never automatically retried; inspect Teleport before
resolving the request manually or creating another. Later terminal results update
the original message. A denial wins over an approval on the server.

The administrative API has no client-supplied expected revision. The bridge
serializes its own decisions and rechecks before mutation, but cannot make that
check atomic with a concurrent external administrator. Teleport's list cache can
briefly lag a successful write; reconciliation converges to the server's state.

A lost send response can cause a duplicate notification; only the recorded
message ID is actionable. Do not run multiple pollers with different journals for
one bot. Corrupt/unwritable state stops the process. Do not delete state casually.
Changing the configured approver or cluster requires explicit journal migration;
old prompts must never silently authorize a new destination.

Resolution reasons include Telegram user/chat/message IDs. Teleport's audit
principal is the bridge, not the human's Teleport identity. Logs never include the
bot token or raw Telegram HTTP errors. No LLM processes approval callbacks.

If the service is down, requests remain pending and existing certificates expire
normally. Trusted administrators can still use `tctl request approve/deny`.
For compromise, stop the service, revoke/reset its Teleport credentials, rotate
its Telegram token through BotFather and inspect request audit events. Rotation
does not undo earlier approvals or already-issued certificates. Restore reviewed
code and fresh credentials from the trusted management side.
