# Notifications

A *channel* is a delivery target for the events of this instance. Channels are
created in **Notifications** and stored in `notification_channels`; each row is a
name, a `type`, a `config` document (JSON, secrets encrypted), the subscribed
`events` and an enabled flag.

| Kind | Transport | When to use |
|---|---|---|
| `smtp` | SMTP over `starttls`/`ssl`/plain | e-mail alerts |
| `webhook` | HTTP request with method, headers and body template | Slack-compatible hooks, n8n, internal endpoints |
| `shoutrrr` | any [shoutrrr](https://containrrr.dev/shoutrrr/) URL | Discord, Telegram, Gotify, ntfy, … |

The form is generated from the server: `GET /api/notifications` returns the
`kinds` with their field schema (`channelSchemas` in
`internal/handlers/notifications.go`), so the SPA never hardcodes a field list.

## 1. Events

| Event | Published when | Payload keys |
|---|---|---|
| `sync.success` | a run finished with `success` | `job`, `job_id`, `run_id`, `files_created`, `files_updated`, `files_deleted`, `conflicts`, `bytes_transferred` |
| `sync.run_failed` | a run finished with `failed` or `partial` | same as above plus `errors` |
| `sync.error` | the engine could not start or complete a run | `job`, `job_id`, `error` |
| `account.connected` | an OAuth flow stored a new account | `provider`, `email`, `id` |
| `account.error` | a token refresh failed (reconnect required) | `provider`, `account`, `error` |
| `test` | the *Send a test notification* button | — |

A channel with an empty `events` list receives nothing; `test` is always
delivered (it bypasses the subscription) so an operator can validate a channel
before subscribing it.

Messages are rendered in English and without HTML on purpose
(`renderMessage`): every transport must be able to display them, and the
translations live in the UI. Each delivery carries
`{event, title, message, timestamp, data}`; when `APP_URL` is set a `link` key is
added to `data` (the dashboard URL) and reaches a webhook template as
`{data.link}`.

## 2. Channel configuration

### `smtp`

| Key | Type | Required | Notes |
|---|---|---|---|
| `host` | text | ✔ | SMTP host name |
| `port` | int | | default `587` |
| `username` | text | | |
| `password` | password (**secret**) | | encrypted at rest, masked in the API |
| `from` | text | ✔ | sender address |
| `from_name` | text | | display name |
| `to` | text | ✔ | one or more recipients, comma separated |
| `subject` | text | | subject prefix |
| `encryption` | select | | `starttls` (default), `ssl`, `none` |
| `use_html` | bool | | default `false` — when off the body is plain text |

### `webhook`

| Key | Type | Required | Notes |
|---|---|---|---|
| `url` | text | ✔ | absolute `http://`/`https://` URL with a host, e.g. `https://hooks.example.com/sync`; see the rules below |
| `method` | select | | `POST` (default), `PUT`, `PATCH` |
| `content_type` | text | | default `application/json` |
| `headers` | json | | e.g. `{"X-Api-Key":"…"}`; a header whose name looks secret (`token`, `auth`, `api_key`, …) is encrypted |
| `body_template` | textarea | | placeholders below; empty → the full JSON payload is sent |
| `secret` | password (**secret**) | | when set, the body is signed with HMAC-SHA256 |
| `timeout_seconds` | int | | default `20` |

The `url` is validated twice, because a channel is a stored setting that a later
delivery has to trust:

* **Shape** (when the channel is saved, and when the test button runs): an
  absolute `http`/`https` URL with a host, an optional port between 1 and 65535,
  and an optional path, query and fragment. Anything else is refused with the
  reason in the form — a non-http scheme (`ftp://`, `javascript:`), a relative
  path (`/hook`), a missing host (`http://`), credentials in the authority
  (`http://user:pass@host/`, which can also hide the real destination), a
  malformed host and any space or control character.
* **Destination** (when the connection is opened): the name is resolved once and
  the address that is actually dialled has to be publicly routable — the answer
  is checked, and it is the checked address that is connected to, so a name
  cannot pass the check and then resolve somewhere private (DNS rebinding).
  Loopback, private (RFC 1918 / `fc00::/7`), link-local (including
  `169.254.169.254`, the cloud metadata endpoint), carrier-grade NAT
  (`100.64.0.0/10`, which is also what Tailscale uses), TEST-NET, benchmarking,
  `240.0.0.0/4` and NAT64 addresses are refused with
  `webhook destination is not publicly routable`.

Two caveats about that second check:

* When `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` are set, the request goes through
  the proxy: the check then applies to the proxy address and what the proxy may
  reach is its own policy.
* The destination check belongs to the `webhook` kind. A `shoutrrr` URL (and the
  `smtp` host) is handed to the shoutrrr engine unchanged, so a LAN mail relay or
  a `generic://` service keeps working and is not affected by the setting.

To deliver to a target on your own network, set
`NOTIFY_ALLOW_PRIVATE_TARGETS=true` ([configuration.md §5](configuration.md)); the
refusal exists because the worker runs inside the Sync container, so without it a
stored channel could be pointed at the API itself, the database or another
container. A delivery follows at most 5 redirects, and a redirect that changes
the host drops `X-Sync-Signature` and every configured header, so an endpoint
cannot forward an API key to a third party.

Placeholders understood by `body_template` (a literal replacement, never an
expression language):

| Placeholder | Expands to |
|---|---|
| `{event}` | `sync.success`, `test`, … |
| `{title}` | human readable title, e.g. `Sync completed: Nightly Google → OneDrive` |
| `{message}` | the sentence of the event |
| `{json}` | the full payload as compact JSON (handy as a JSON string value) |
| `{timestamp}` | the delivery timestamp |
| `{data.<key>}` | one payload value, e.g. `{data.job}`, `{data.files_created}`, `{data.link}` |

Example (n8n/Slack-style JSON body):

```json
{"text": "[{event}] {title}\n{message}", "job": "{data.job}", "run": "{data.run_id}"}
```

### `shoutrrr`

| Key | Type | Required | Notes |
|---|---|---|---|
| `url` | password (**secret**) | ✔ | the whole URL is a secret (it embeds the token), e.g. `slack://token-a/token-b/token-c@channel`, `telegram://token@telegram?chats=@mychannel`, `discord://token@id`, `gotify://host/token`, `ntfy://topic` |

See the shoutrrr documentation for the complete URL catalogue; any service the
library supports works without a Sync change.

## 3. Secrets

Two rules decide what is encrypted (`isSecretKey` / `kindSecretKeys`):

1. a configuration key whose **name** is `password`, `pass`, `secret`, `token`,
   `api_key`, `apikey`, `access_key`, `authorization` or `auth`;
2. the whole `url` of a `shoutrrr` channel (it embeds the service credentials).

Secrets are encrypted individually with the `SecretBox` (`v1:` AES-256-GCM) before
the `config` document is stored, and every API answer replaces them with
`********` (`maskConfig`). To update a channel without retyping its secrets, send
the mask back: a value equal to `********` keeps the stored ciphertext
(`sealValue`). Provider error messages are sanitized before they are logged or
stored, so a secret never leaks through an exception text.

## 4. Delivery semantics

* `Publish` (called by the sync engine and the OAuth service) fans out to every
  enabled channel subscribed to the event **in a detached goroutine** with its own
  bounded context (30 s). A dead mail server can therefore never slow down or
  fail a sync run; `Notifier.Wait()` drains in-flight deliveries on shutdown.
* `POST /api/notifications/:id/test` delivers synchronously (`Test`), so the
  button can report the real outcome. A refusal of the remote end answers
  `502 delivery_failed` with the reason — unlike other server errors, this one
  shows its detail because only the operator can act on it.
* Each attempt updates `last_status`, `last_error` and `last_used_at`, which is
  what the notifications screen displays next to the channel.

## 5. API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/notifications` | channels + `kinds` (form schema) + `events` |
| `POST` | `/api/notifications` | create a channel |
| `GET` | `/api/notifications/:id` | one channel (secrets masked) |
| `PUT` | `/api/notifications/:id` | update name/type/config/events |
| `PUT` | `/api/notifications/:id/enabled` | `{"enabled": true}` |
| `POST` | `/api/notifications/:id/test` | deliver a test message |
| `DELETE` | `/api/notifications/:id` | delete the channel |

See [api.md](api.md) for payloads.

## 6. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| test answers `delivery_failed` | the remote end refused (wrong port, blocked outbound, bad token) | the message carries the transport error; verify host/port/credentials |
| SMTP `starttls` fails | the server wants implicit TLS | switch `encryption` to `ssl` (usually port 465) |
| nothing is delivered although the channel works | the channel is not subscribed to the event | tick the events (or use `test`, which always delivers) |
| webhook body is empty | `body_template` is set but expands to nothing | use `{json}` or `{message}`; literal text is kept as-is |
| test answers `delivery_failed` with `webhook destination is not publicly routable` | the destination is loopback, private, link-local or another reserved range, and the opt-in is off | deliver to a public endpoint, or set `NOTIFY_ALLOW_PRIVATE_TARGETS=true` for a target on your own network |
| the form refuses the URL of a channel that used to work | the saved value is not an absolute `http`/`https` URL with a host (relative path, `ftp://`, embedded credentials, a space or a control character) | correct the URL; see the shape rules above |
| a redirect answers 502 | the endpoint redirects more than 5 times | point the channel at the final URL |
| secret shows as `********` after saving | that is the mask | send `********` again to keep it, or type a new value |
| frequent `account.error` notifications | refresh token revoked | reconnect the account ([oauth.md](oauth.md)) |
