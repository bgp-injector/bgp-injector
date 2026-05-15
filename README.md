# bgp-injector

A Kubernetes DaemonSet BGP speaker that announces IP prefixes on behalf of annotated pods, peering with the local Calico node via [GoBGP](https://github.com/osrg/gobgp).

One speaker pod runs per node. It watches all pods on that node and, for each pod carrying the prefix annotations, announces the configured prefixes with the pod's own IP as the next-hop. Routes are announced only while the pod is ready (configurable) and withdrawn immediately on unreadiness or deletion.

## How it works

1. A pod is annotated with `bgp-injector.github.io/routedIPv4Prefixes` (and/or IPv6).
2. The speaker DaemonSet pod on the same node detects the annotation via a pod informer.
3. The speaker announces the prefixes over a BGP session to the local Calico node, with next-hop set to the pod's IP.
4. If `gateOnReady` is enabled (default), routes are only active while the pod's `Ready` condition is `True`. Pods with no readiness probe become ready as soon as their containers are running.
5. On pod deletion or unreadiness, routes are withdrawn immediately.
6. On speaker shutdown (SIGTERM), all routes are withdrawn before the BGP session drops.

## Annotations

| Annotation | Type | Description |
|---|---|---|
| `bgp-injector.github.io/routedIPv4Prefixes` | JSON string array | IPv4 prefixes to announce in CIDR notation, e.g. `'["192.0.2.0/24"]'` |
| `bgp-injector.github.io/routedIPv6Prefixes` | JSON string array | IPv6 prefixes to announce in CIDR notation, e.g. `'["2001:db8::/32"]'` |
| `bgp-injector.github.io/gateOnReady` | bool string | Withdraw routes when not ready (overrides the cluster default) |

BGP session parameters (AS numbers, peer address) are configured at the DaemonSet level and apply to all pods on the node.

## Installation

### Prerequisites

- Kubernetes 1.19+
- Calico with BGP enabled

### Helm

```sh
helm repo add bgp-injector https://bgp-injector.github.io/bgp-injector
helm repo update

helm install bgp-injector bgp-injector/bgp-injector \
  --namespace bgp-injector \
  --create-namespace \
  --set bgpDefaults.localAs=65002 \
  --set bgpDefaults.remoteAs=65001
```

By default `BGP_PEER_ADDRESS` is set to the node's IP (`status.hostIP`), which is where the Calico BGP daemon listens. Override `bgpDefaults.peerAddress` if your setup uses a different address.

### Annotate a pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
  annotations:
    bgp-injector.github.io/routedIPv4Prefixes: '["192.0.2.0/24", "198.51.100.0/24"]'
    bgp-injector.github.io/routedIPv6Prefixes: '["2001:db8::/32"]'  # optional
    bgp-injector.github.io/gateOnReady: "true"                       # default: true
spec:
  containers:
  - name: app
    image: my-app:latest
    readinessProbe:
      httpGet:
        path: /ready
        port: 8080
```

## Helm values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/bgp-injector/speaker` | Speaker image |
| `image.tag` | Chart `appVersion` | Speaker image tag |
| `bgpDefaults.localAs` | `""` | Local AS number (required) |
| `bgpDefaults.remoteAs` | `""` | Remote AS number (required) |
| `bgpDefaults.peerAddress` | `""` | IPv4 BGP peer address; defaults to node IP if not set |
| `bgpDefaults.peerAddressV6` | `""` | IPv6 BGP peer address; required to announce IPv6 prefixes |
| `bgpDefaults.gateOnReady` | `true` | Withdraw routes when pod is not ready |
| `bgpDefaults.gracefulRestartTime` | `0` | BGP graceful restart time in seconds (0 = disabled). When set, routes are held by the peer during restarts. |
| `extraEnv` | `[]` | Additional environment variables for the speaker container |
| `resources` | `{}` | Speaker container resource requests/limits |
| `nodeSelector` | `{}` | Node selector for the DaemonSet |
| `tolerations` | `[]` | Tolerations for the DaemonSet |

## Calico configuration

### BGPPeer

Create a `BGPPeer` on each Calico node to accept sessions from the speaker DaemonSet pods. Use `peerSelector` to match the speaker pods by label so Calico automatically peers with whichever speaker pod is local to each node:

```yaml
apiVersion: projectcalico.org/v3
kind: BGPPeer
metadata:
  name: bgp-injector
spec:
  nodeSelector: all()
  peerSelector: app.kubernetes.io/name == 'bgp-injector'
  asNumber: 65002  # must match bgpDefaults.localAs
```

Refer to the [Calico BGP documentation](https://docs.tigera.io/calico/latest/networking/configuring/bgp) for full BGPPeer options.

### FelixConfiguration

Two FelixConfiguration settings are required for bgp-injector routes to work correctly.

**`removeExternalRoutes: false`** — By default Felix removes any routes on workload interfaces that it did not program itself. Since bgp-injector routes are installed by BIRD (Calico's BGP daemon) rather than directly by Felix, Felix will delete them on every reconciliation cycle and flush the associated conntrack entries, causing periodic packet loss. Setting this to `false` tells Felix to leave routes with an unrecognised protocol alone.

**`workloadSourceSpoofing: Any`** — Required for pods that send outbound traffic sourced from bgp-injector-advertised prefixes. Without this, Felix's RPF (reverse-path filtering) enforcement will drop packets whose source IP does not match the pod's own IP.

```yaml
apiVersion: projectcalico.org/v3
kind: FelixConfiguration
metadata:
  name: default
spec:
  removeExternalRoutes: false
  workloadSourceSpoofing: Any
```

### Pod spoofing annotation

Each pod that needs to originate traffic from a bgp-injector-advertised prefix must declare those prefixes via the `cni.projectcalico.org/allowedSourcePrefixes` annotation. Calico uses this to install the necessary iptables/eBPF rules to permit outbound traffic with those source addresses.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
  annotations:
    bgp-injector.github.io/routedIPv4Prefixes: '["192.0.2.0/24", "198.51.100.0/24"]'
    cni.projectcalico.org/allowedSourcePrefixes: '["192.0.2.0/24", "198.51.100.0/24"]'
spec:
  containers:
  - name: app
    image: my-app:latest
```

The `allowedSourcePrefixes` annotation must list every prefix the pod will use as a source address. It does not need to match the bgp-injector prefixes exactly — only include the prefixes that the pod actually originates traffic from. Pods that only receive traffic on bgp-injector-advertised addresses (i.e. the traffic is forwarded elsewhere) do not need this annotation.
