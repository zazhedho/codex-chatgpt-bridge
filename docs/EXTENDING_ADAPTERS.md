# Extending adapters

The MVP keeps provider-specific execution outside the core. It has no arbitrary shell tool and no privileged runner.

Before adding an adapter:

1. Define a narrow request/response type and validate it at the trust boundary.
2. Keep provider credentials out of the Codex process and task environment.
3. Deny unknown operations and assign each supported operation a risk level.
4. Bind approval to the canonical target, environment, exact parameters, expiry, and artifact SHA-256 when executable content is used.
5. Require one-time approval through an independent authenticated local channel.
6. Make retries idempotent or reject ambiguous/replayed requests; never replay after restart.
7. Bound output, time, and concurrency; redact logs; test rejection, expiry, altered inputs, replay, and recovery.

Keep interfaces small and near the service that consumes them. Provider code belongs in its adapter package; services should depend on a narrow capability, not a provider SDK. Do not add an adapter until its isolation, credentials, and approval rules are reviewed.
