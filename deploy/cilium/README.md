# Cilium + eBPF — kernel-level hardening for the IDRE platform

## What Cilium and eBPF add here

This platform's threat model is unusual: **50 state tenants share one
cluster**, each legally an isolated data domain, and the vault holds the
ciphertext and key material for all of them. Every existing isolation
mechanism — JWT tenant claims, schema-per-tenant routing, Permify ReBAC,
the vault's encryption — lives in *software the platform controls*. eBPF
adds a layer the platform's own bugs cannot bypass: policy enforced in the
kernel, below the application entirely.

| Capability | What it buys this platform |
| --- | --- |
| **Cilium network policies (L3–L7)** | Tenant-tier segmentation in the kernel: only `case-api` may reach the vault; workers have *no world egress at all* (this makes "local Ollama only, no vendor LLM" enforceable, not aspirational); Stripe egress pinned to `api.stripe.com:443/POST` by FQDN — plain k8s NetworkPolicy cannot do DNS-aware rules |
| **WireGuard transparent encryption** | Every cross-node pod flow encrypted in-kernel, zero app changes, no sidecars. The network-layer complement to the vault's at-rest encryption — interstate tenant data never crosses a node boundary in plaintext |
| **kube-proxy replacement (socket LB)** | Service handling in eBPF; no iptables to rot at 500+ services, lower latency per hop |
| **Hubble** | Flow-level observability — every connection, DNS query, and *dropped packet with the verdict reason*. This is the network equivalent of `public.audit_log`: when someone asks "did tenant X's data ever touch tenant Y's pods?", Hubble answers with evidence |
| **Tetragon** | Runtime enforcement inside pods: a shell or `curl` in any platform pod is killed in-kernel and exported as a SIEM event; vault key-directory access is watched |
| **Bandwidth manager (eBPF EDT)** | A runaway worker can't starve the case API of NIC time |
| **Egress gateway (dormant)** | When a state's bank/lockbox feed demands a pinned source IP, enable it and assign per-tenant egress IPs |

## Position in the stack

```
Caddy (TLS, headers) ─▶ APISIX (OIDC, rate limits) ─▶ case-api ─▶ vault
        │                                              │
   WAF (open-appsec module)                    CILIUM + eBPF (this layer):
                                               who may talk to whom,
                                               encrypted, observed,
                                               runtime-enforced
```

Cilium does not replace APISIX (L7 API policy), Keycloak (identity), or
Permify (object authz). It answers a different question: *if any of those
fail, what can the attacker still reach?* — and makes the answer "nothing".

## Files

| File | Purpose |
| --- | --- |
| `deploy/helm-values/cilium.yaml` | CNI install: kube-proxy replacement, WireGuard, Hubble, L7 proxy |
| `deploy/helm-values/tetragon.yaml` | Runtime security agent config |
| `deploy/kubernetes/01-cilium-network-policies.yaml` | Default-deny + per-workload allow-lists, FQDN world egress |
| `deploy/kubernetes/02-tetragon-tracing-policies.yaml` | Shell-kill and vault key-watch tracing policies |
| `deploy/helmfile.yaml` | `cilium` + `tetragon` releases, ordered before the fleet |

## Rollout

```bash
# 1. Set the API server endpoint (required for kube-proxy replacement)
#    edit helm-values/cilium.yaml: k8sServiceHost / k8sServicePort

# 2. Install the dataplane first
helmfile -l name=cilium -l name=tetragon sync

# 3. Verify the eBPF dataplane before applying policies
cilium status --wait
cilium connectivity test

# 4. Apply policies (default-deny + allows)
kubectl apply -f kubernetes/01-cilium-network-policies.yaml
kubectl apply -f kubernetes/02-tetragon-tracing-policies.yaml

# 5. Watch for unexpected drops — every denial is labeled with its reason
hubble observe --verdict DROPPED --follow

# 6. After a soak period with zero unexpected drops, flip
#    policyEnforcementMode: always in helm-values/cilium.yaml — from then on,
#    any pod NOT covered by a policy is fully isolated by construction.
```

## Notes

- **Compose dev is unaffected** — Cilium is a CNI; docker-compose has no
  pod network to program. The compose edge hardening (Caddy tier) and these
  policies are two expressions of the same allow-list on two runtimes.
- Label assumptions: platform workloads use `app.kubernetes.io/part-of: idre`
  and `app: <name>` (already true in `kubernetes/apps.yaml`); chart-managed
  services are matched by namespace + name label — verify chart label keys
  (`app.kubernetes.io/name`) against the installed releases on first apply.
- Hubble UI is in `idre-observ`; expose it through the same ingress as
  Grafana, never publicly.
- FQDN egress requires the L7 proxy (`l7Proxy: true`, set) and CoreDNS
  visibility (the cluster-wide DNS policy in file 01).
