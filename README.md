# poc-routing (Envoy + Lua + Go)

## Goal
Prove that Envoy with a Lua filter can dynamically route a specific API (`/join`) to an exact container instance running behind Docker Compose scaling.

## Components
- `envoy`: entrypoint on port 10000 (admin 9901)
- `server` (Go): simple HTTP service exposing:
  - `/join?client_id=...` logs a registration on the current container
  - `/where?client_id=...` returns the target container hostname:port calculated deterministically
  - `/health`
- `docker-compose`: runs Envoy and a scalable `server` service

## How routing works
- Compose runs multiple `server` replicas with predictable DNS names: `<project>-server-1`, `<project>-server-2`, ... on the default network.
- The Go `server` implements `/where` that maps `client_id` to an index in `[1..REPLICAS]` via hash, then returns `hostport = SERVICE_PREFIX-<idx>:<port>`.
- Envoy has two routes:
  - Route A: prefix `/join`
    - HTTP Lua filter runs only on this route
    - Lua extracts `client_id`, calls `/where?client_id=...` on the `resolver` cluster (points to service name `server`), parses `hostport` and sets `:authority` to it
    - The request is then forwarded by `router` to `dynamic_forward_proxy_cluster`, which dials the hostname from `:authority` per request
  - Route B: prefix `/` (fallback)
  - For non-join calls (like `/where` itself), traffic goes directly to the `resolver` cluster

### Route matching order
- Envoy matches the first route that applies. `/join` hits the `/join` route (A). Everything else hits the `/` route (B).

### Why Dynamic Forward Proxy (DFP)?
- Changing `:authority` only changes the Host header, not the upstream target.
- DFP enables per-request upstream resolution/dialing by hostname; the Lua sets `:authority`, DFP resolves it and dials that instance.

## Detailed request flow

- Route match:
  - For `/join`, the first route with prefix `/join` matches. The fallback route with prefix `/` is not used (first match wins, order matters).
  - For anything else, the `/` route matches.

- Filter chain on the matched route (in order):
  1) Lua filter runs
     - If path starts with `/join`: extracts `client_id`, calls resolver cluster (`/where?client_id=...`), gets `hostport`, sets `:authority` to that `hostport`, and logs the steps.
     - Otherwise returns immediately (no-op).
  2) dynamic_forward_proxy filter
     - For `/join`, it reads the `:authority` set by Lua, resolves that hostname (`poc-routing-server-<idx>`) via DNS, and populates the shared DNS cache.
  3) Router
     - For `/join`, forwards to cluster `dynamic_forward_proxy_cluster` which dials the resolved `host:port` from the DNS cache.
     - For non-`/join`, forwards to the `resolver` cluster (service name `server`), which reaches any replica.

## Repository layout
```
poc-routing/
 ├── docker-compose.yaml
 ├── envoy.yaml
 ├── lua/
 │   ├── routing.lua
 ├── server/
 │   ├── main.go
 │   ├── go.mod
 │   └── Dockerfile
 └── client/
     ├── main.go     # run locally, not in Compose
     └── go.mod
```

## Prerequisites
- Docker Desktop (or Docker Engine + Compose plugin)
- Minikube (if run on Kubernetes) 
- Go >= 1.21 (to run the client locally)

### Server configuration (cross‑platform)
The server computes the exact target instance using env variables, working for both Docker Compose and K8s.

- `SERVICE_PREFIX`
  - Compose: `poc-routing-server`
  - K8s: `server`
- `SERVICE_SUFFIX`
  - Compose: empty (unset)
  - K8s: `.server-headless.poc-routing.svc.cluster.local`
- `REPLICAS`
  - Total desired instances. Used for consistent index calculation.
- `INDEX_MODE`
  - `hash` (default) uses FNV hash of `client_id`
  - `numeric` uses integer `client_id` directly
- `INDEX_BASE`
  - Compose: `1` (names like `prefix-1`..`prefix-N`)
  - K8s: `0` (StatefulSet ordinals `prefix-0`..`prefix-(N-1)`)
- `PORT`
  - Service port of the server container (default `8081`).

Final upstream host used by Envoy Lua:
```
hostport = SERVICE_PREFIX + "-" + <idx> + SERVICE_SUFFIX + ":" + PORT
```
where `<idx>` is computed with `INDEX_MODE`, `INDEX_BASE`, and `REPLICAS`.

## Run on Docker
1) Start the stack with 2 replicas (adjust `REPLICAS` env in `docker-compose.yaml` if needed):
```
docker compose up -d --build --scale server=2
```
2) Wait for Envoy to be ready:
```
curl -fsS http://localhost:9901/ready && echo ready
```

### Verify
- Check which instance `/where` resolves to for `client_id=123`:
```
curl 'http://localhost:10000/where?client_id=123'
# → {"client_id":"123","hostport":"poc-routing-server-<idx>:8081"}
```
- Call `/join` for that `client_id` (Lua will resolve target and forward via DFP):
```
curl -v 'http://localhost:10000/join?client_id=123'
```
- Inspect server logs for the selected instance:
```
docker compose logs server --no-log-prefix --since=2m | grep "/join"
```
- Optional: observe Envoy logs for Lua debug lines:
```
docker compose logs envoy --no-log-prefix --since=2m | grep -n "Lua:"
```

## Run on Kubernetes (Minikube)

### Files
- `minikube/server-statefulset.yaml`: StatefulSet `server` (replicas=2) + headless Service `server-headless`
- `minikube/envoy-deployment.yaml`: Envoy Deployment + NodePort Service (10000→30080, 9901→30081)
- `minikube/kustomization.yaml`: generates ConfigMap `envoy-config` from `../envoy.yaml` and `../lua/routing.lua`

### Steps (from project root)
1) Start Minikube and build the server image into the cluster (optional but typical):
```
minikube start
eval $(minikube -p minikube docker-env)
docker build -t poc-routing-server:latest ./server
```
2) Apply manifests with kustomize:
```
kubectl create ns poc-routing
kubectl -n poc-routing apply -k minikube/
```
3) Test the deployment:

**Option A: Using Minikube service forwarding (Recommended)**
```bash
# Get the forwarded URLs
minikube service envoy -n poc-routing --url

# This will output something like:
# http://127.0.0.1:64719
# http://127.0.0.1:64720

# Test with the forwarded URL:
curl "http://127.0.0.1:64719/where?client_id=123"
curl -v "http://127.0.0.1:64719/join?client_id=123"
```

**Option B: Using NodePort directly (requires tunnel)**
```bash
# Start Minikube tunnel in a separate terminal (keep it running)
minikube tunnel

# Then test with NodePort:
MINIKUBE_IP=$(minikube ip)
curl "http://$MINIKUBE_IP:30080/where?client_id=123"
curl -v "http://$MINIKUBE_IP:30080/join?client_id=123"
```

**Option C: Using kubectl port-forward**
```bash
# Port forward the service
kubectl port-forward -n poc-routing service/envoy 10000:10000

# Test locally
curl "http://localhost:10000/where?client_id=123"
curl -v "http://localhost:10000/join?client_id=123"
```

## Troubleshooting

### Minikube External Access Issues

**Problem**: `curl: (28) Failed to connect to 192.168.49.2 port 30080 after 75002 ms: Couldn't connect to server`

**Root Cause**: Minikube with Docker driver on macOS requires port forwarding for external access to NodePort services.

**Solutions**:
1. **Use Minikube service forwarding** (Recommended):
   ```bash
   minikube service envoy -n poc-routing --url
   # Use the returned localhost URLs
   ```

2. **Use Minikube tunnel**:
   ```bash
   minikube tunnel  # Keep running in separate terminal
   # Then use NodePort directly
   ```

3. **Use kubectl port-forward**:
   ```bash
   kubectl port-forward -n poc-routing service/envoy 10000:10000
   ```

### Common Issues

**503/504 on `/join`**:
- DFP couldn't resolve/connect to `hostport`
- Verify `/where` output; check endpoints for headless service; confirm resolver cluster target

**Envoy not responding**:
- Check pod status: `kubectl get pods -n poc-routing`
- Check Envoy logs: `kubectl logs -n poc-routing deployment/envoy`
- Verify service endpoints: `kubectl get endpoints -n poc-routing`

**Lua errors**:
- `resolver status=nil`: Header accessor differences; code reads either `get(":status")` or `[":status"]`
- `object used outside of proper scope`: Don't retain `headers` across `httpCall`; re-fetch after call

**Kubernetes DNS issues**:
- `kube-dns` Service fronts `coredns` Pods
- Use K8s API (Services/Endpoints/EndpointSlices) to enumerate names

## Tuning / Notes
- Predictable names: Compose creates container DNS names `<project>-server-<idx>`. By default the project is the folder name `poc-routing`.
- Service envs:
  - `SERVICE_PREFIX` (default in compose): `poc-routing-server`
  - `REPLICAS` (default in compose): `2`
  - `PORT`: `8081`
- If DNS resolution for DFP fails in your environment, you can add Docker DNS explicitly in `envoy.yaml` under `dns_cache_config`:
```
dns_resolvers:
  - socket_address: { address: 127.0.0.11, port_value: 53 }
```

## Clean up
```
docker compose down -v
```
