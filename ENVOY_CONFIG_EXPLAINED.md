# Envoy Configuration Explained

This document provides a detailed breakdown of the Envoy configuration and how dynamic routing works in this POC.

## Table of Contents
- [Overview](#overview)
- [Configuration Structure](#configuration-structure)
- [Request Flow Examples](#request-flow-examples)
- [Filter Chain Execution](#filter-chain-execution)
- [Clusters Explained](#clusters-explained)
- [Why This Architecture?](#why-this-architecture)
- [Common Misconceptions](#common-misconceptions)

## Overview

The Envoy configuration implements dynamic routing where:
- `/where?client_id=123` → Routes to **any available server instance** (load balanced)
- `/join?client_id=123` → Routes to **specific server instance** (deterministic based on client_id)

## Configuration Structure

### 1. Listener Configuration
```yaml
listeners:
  - name: listener_0
    address:
      socket_address:
        address: 0.0.0.0
        port_value: 10000
```
- **Purpose**: Envoy listens on all interfaces on port 10000
- **Result**: All incoming HTTP requests hit this listener

### 2. HTTP Connection Manager
```yaml
- name: envoy.filters.network.http_connection_manager
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
    stat_prefix: ingress_http
```
- **Purpose**: Converts raw TCP traffic into HTTP requests
- **Result**: HTTP requests are parsed and ready for routing

### 3. Route Configuration - THE KEY PART

```yaml
routes:
  - match: { prefix: "/join" }     # Route A: /join requests
    route:
      cluster: dynamic_forward_proxy_cluster
    typed_per_filter_config:
      envoy.filters.http.lua:
        source_code:
          filename: /etc/envoy/lua/routing.lua
  - match: { prefix: "/" }         # Route B: Everything else
    route:
      cluster: resolver
```

#### Route Matching Logic:
1. **First match wins** - Envoy checks routes in order
2. **`/join?client_id=123`** → Matches Route A (prefix `/join`)
3. **`/where?client_id=123`** → Matches Route B (prefix `/`)
4. **`/health`** → Matches Route B (prefix `/`)

### 4. HTTP Filters Chain - EXECUTION ORDER

```yaml
http_filters:
  - name: envoy.filters.http.lua                    # Filter 1
  - name: envoy.filters.http.dynamic_forward_proxy  # Filter 2  
  - name: envoy.filters.http.router                 # Filter 3
```

**Filter Execution Order:**
1. **Lua Filter** runs first
2. **Dynamic Forward Proxy Filter** runs second
3. **Router Filter** runs last

## Request Flow Examples

### Example 1: `/where?client_id=123` Request

```
1. Request arrives: GET /where?client_id=123
2. Route matching: Matches Route B (prefix "/")
3. Filter chain execution:
   - Lua Filter: Does nothing (not configured for this route)
   - DFP Filter: Does nothing (:authority header not modified)
   - Router Filter: Forwards to "resolver" cluster
4. Resolver cluster: Routes to any server instance (round-robin)
5. Response: {"client_id":"123","hostport":"server-1:8081"}
```

**Why DFP does nothing:**
- No `:authority` header is set by Lua (Lua only runs for `/join`)
- DFP only activates when `:authority` header is modified
- Request goes directly to `resolver` cluster with original `:authority`

### Example 2: `/join?client_id=123` Request

```
1. Request arrives: GET /join?client_id=123
2. Route matching: Matches Route A (prefix "/join")
3. Filter chain execution:
   - Lua Filter: 
     * Extracts client_id=123
     * Makes httpCall to resolver: GET /where?client_id=123
     * Gets response: {"client_id":"123","hostport":"server-1:8081"}
     * Sets :authority header to "server-1:8081"
   - DFP Filter:
     * Reads :authority header ("server-1:8081")
     * Resolves DNS for "server-1"
     * Caches the IP address
   - Router Filter:
     * Forwards to "dynamic_forward_proxy_cluster"
     * DFP cluster dials the resolved IP:port
4. Response: Request reaches the specific server instance
```

## Filter Chain Execution

### Lua Filter Behavior

#### For `/join` requests:
```lua
function envoy_on_request(handle)
  -- 1. Extract client_id from query string
  local client_id = extract_client_id(path)
  
  -- 2. Call resolver to get target hostport
  local resp_body = httpCall("resolver", "/where?client_id=" .. client_id)
  
  -- 3. Parse hostport from response
  local hostport = parse_hostport(resp_body)
  
  -- 4. Set :authority header to target hostport
  handle:headers():replace(":authority", hostport)
end
```

#### For `/where` requests:
- **Lua filter does nothing** (not configured for this route)
- Request proceeds with original `:authority` header

### Dynamic Forward Proxy Filter Behavior

#### For `/join` requests:
- **Reads modified `:authority` header** (set by Lua)
- **Performs DNS resolution** for the hostname in `:authority`
- **Caches the resolved IP** for future requests
- **Prepares for dynamic routing**

#### For `/where` requests:
- **Does nothing** because `:authority` header is unchanged
- **No DNS resolution needed** (static cluster)

### Router Filter Behavior

#### For `/join` requests:
- **Routes to `dynamic_forward_proxy_cluster`**
- **Uses resolved IP from DFP cache**
- **Connects to specific server instance**

#### For `/where` requests:
- **Routes to `resolver` cluster**
- **Uses static DNS resolution**
- **Load balances across available instances**

## Clusters Explained

### 1. Resolver Cluster
```yaml
- name: resolver
  type: STRICT_DNS
  lb_policy: ROUND_ROBIN
  load_assignment:
    endpoints:
      - lb_endpoints:
          - endpoint:
              address: server  # Service name
              port_value: 8081
```

**Purpose**: Routes to any available server instance
- **DNS Resolution**: Resolves "server" service name to IPs
- **Load Balancing**: Round-robin between available instances
- **Use Case**: General requests that don't need specific instance routing

### 2. Dynamic Forward Proxy Cluster
```yaml
- name: dynamic_forward_proxy_cluster
  lb_policy: CLUSTER_PROVIDED
  cluster_type:
    name: envoy.clusters.dynamic_forward_proxy
  dns_cache_config:
    name: dynamic_forward_proxy_cache
    dns_lookup_family: V4_ONLY
```

**Purpose**: Routes to dynamically resolved hostnames
- **DNS Resolution**: Per-request DNS resolution based on `:authority` header
- **Caching**: Caches resolved IPs for performance
- **Use Case**: Specific instance routing based on business logic

## Why This Architecture?

### The Problem:
- We need to route `/join` requests to **specific server instances**
- But we don't know which instance until we calculate it based on `client_id`
- Standard load balancing routes to any available instance

### The Solution:
1. **Lua script** calculates the target instance by calling `/where`
2. **Dynamic Forward Proxy** enables per-request DNS resolution
3. **Two clusters** handle different routing needs:
   - `resolver`: For general requests (any instance)
   - `dynamic_forward_proxy_cluster`: For specific instance routing

### Key Insight:
**Changing `:authority` header alone doesn't change the upstream target** - that's why we need Dynamic Forward Proxy to actually resolve and dial the new hostname.

## Common Misconceptions

### ❌ Wrong: "DFP runs for all requests"
**Correct**: DFP only runs when `:authority` header is modified by Lua

### ❌ Wrong: "Lua runs for all requests"
**Correct**: Lua only runs for `/join` requests due to per-route configuration

### ❌ Wrong: "Router always uses the same cluster"
**Correct**: Router uses different clusters based on route matching:
- `/join` → `dynamic_forward_proxy_cluster`
- Everything else → `resolver`

### ❌ Wrong: "DNS resolution happens at cluster level"
**Correct**: DNS resolution happens at different levels:
- `resolver` cluster: Static DNS resolution (service name → IPs)
- `dynamic_forward_proxy_cluster`: Per-request DNS resolution (hostname → IP)

## DFP Setup: Yes, You Need Both!

### 1. **DFP Filter** (HTTP Filter)
```yaml
- name: envoy.filters.http.dynamic_forward_proxy
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig
    dns_cache_config:
      name: dynamic_forward_proxy_cache
      dns_lookup_family: V4_ONLY
```

### 2. **DFP Cluster** (Cluster Type)
```yaml
- name: dynamic_forward_proxy_cluster
  lb_policy: CLUSTER_PROVIDED
  cluster_type:
    name: envoy.clusters.dynamic_forward_proxy
    typed_config:
      "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig
      dns_cache_config:
        name: dynamic_forward_proxy_cache
        dns_lookup_family: V4_ONLY
```

## How DNS Resolution Works: Step by Step

Let me trace through what happens when Lua sets `:authority` to `"server-1.server-headless.poc-routing.svc.cluster.local:8081"`:

### Step 1: Lua Sets the Authority Header
```lua
-- In routing.lua
local hostport = "server-1.server-headless.poc-routing.svc.cluster.local:8081"  -- From /where response
hdr:replace(":authority", hostport)  -- Sets :authority header
```

### Step 2: DFP Filter Intercepts the Request
```yaml
# DFP Filter configuration
- name: envoy.filters.http.dynamic_forward_proxy
  typed_config:
    dns_cache_config:
      name: dynamic_forward_proxy_cache  # Shared cache name
```

**What happens:**
1. DFP filter reads `:authority` header: `"server-1.server-headless.poc-routing.svc.cluster.local:8081"`
2. Extracts hostname: `"server-1.server-headless.poc-routing.svc.cluster.local"`
3. Checks DNS cache for `"server-1.server-headless.poc-routing.svc.cluster.local"`
4. If not cached, performs DNS lookup
5. Caches the resolved IP (e.g., `10.244.0.5`)

### Step 3: DNS Resolution Process

**DNS Resolution in Action:**
```
Query: server-1.server-headless.poc-routing.svc.cluster.local
DNS Server: 10.96.0.10:53 (Kubernetes DNS)
Response: 10.244.0.5
```

## How DNS Resolution Works: Detailed Breakdown

### The Complete Flow for `/join?client_id=123`

#### Step 1: Lua Script Execution
```lua
-- Lua extracts client_id and calls /where
local resp_body = '{"client_id":"123","hostport":"server-1.server-headless.poc-routing.svc.cluster.local:8081"}'
local hostport = "server-1.server-headless.poc-routing.svc.cluster.local:8081"

-- Sets :authority header
hdr:replace(":authority", hostport)
```

#### Step 2: DFP Filter DNS Resolution
```yaml
# DFP Filter configuration
- name: envoy.filters.http.dynamic_forward_proxy
  typed_config:
    dns_cache_config:
      name: dynamic_forward_proxy_cache
      dns_lookup_family: V4_ONLY
```

**What happens inside DFP Filter:**
1. **Reads `:authority` header**: `"server-1.server-headless.poc-routing.svc.cluster.local:8081"`
2. **Extracts hostname**: `"server-1.server-headless.poc-routing.svc.cluster.local"`
3. **Checks DNS cache**: Looks for `"server-1.server-headless.poc-routing.svc.cluster.local"` in `dynamic_forward_proxy_cache`
4. **If not cached**: Performs DNS lookup using Kubernetes DNS (10.96.0.10:53)
5. **DNS Resolution**: 
   ```
   Query: server-1.server-headless.poc-routing.svc.cluster.local
   Response: 10.244.0.5
   ```
6. **Caches result**: Stores `server-1.server-headless.poc-routing.svc.cluster.local → 10.244.0.5`
7. **Prepares request**: Ready for routing to `10.244.0.5:8081`

#### Step 3: DFP Cluster Routing
```yaml
- name: dynamic_forward_proxy_cluster
  lb_policy: CLUSTER_PROVIDED
  cluster_type:
    name: envoy.clusters.dynamic_forward_proxy
    typed_config:
      dns_cache_config:
        name: dynamic_forward_proxy_cache  # Same cache as filter
```

**What happens inside DFP Cluster:**
1. **Reads from DNS cache**: Gets `10.244.0.5` for `server-1.server-headless.poc-routing.svc.cluster.local`
2. **Establishes connection**: Connects to `10.244.0.5:8081`
3. **Forwards request**: Sends the original `/join?client_id=123` request

## Why You Need Both Components

### DFP Filter (HTTP Filter)
- **Purpose**: DNS resolution and caching
- **When it runs**: Before routing, after Lua modifies `:authority`
- **What it does**: Resolves hostnames to IPs and caches them

### DFP Cluster (Cluster Type)
- **Purpose**: Actual connection and routing
- **When it runs**: During routing phase
- **What it does**: Uses cached IPs to connect to specific instances

## DNS Resolution in Action

### What Actually Happens:

1. **Lua sets `:authority`**: `"server-1.server-headless.poc-routing.svc.cluster.local:8081"`

2. **DFP Filter performs DNS lookup**:
   ```
   Query: server-1.server-headless.poc-routing.svc.cluster.local
   DNS Server: 10.96.0.10:53 (Kubernetes DNS)
   Response: 10.244.0.5
   ```

3. **DFP Filter caches the result**:
   ```
   Cache Entry:
   Key: server-1.server-headless.poc-routing.svc.cluster.local
   Value: 10.244.0.5
   ```

4. **DFP Cluster uses cached IP**:
   ```
   Connection: 10.244.0.5:8081
   Request: GET /join?client_id=123
   ```

## Complete DNS Resolution Process

### What Actually Happens:

1. **Lua sets `:authority`**: `"server-1.server-headless.poc-routing.svc.cluster.local:8081"`

2. **DFP Filter performs DNS lookup**:
   ```
   Query: server-1.server-headless.poc-routing.svc.cluster.local
   DNS Server: 10.96.0.10:53 (Kubernetes DNS)
   Response: 10.244.0.5
   ```

3. **DFP Filter caches the result**:
   ```
   Cache Entry:
   Key: server-1.server-headless.poc-routing.svc.cluster.local
   Value: 10.244.0.5
   ```

4. **DFP Cluster uses cached IP**:
   ```
   Connection: 10.244.0.5:8081
   Request: GET /join?client_id=123
   ```

## Key Points About DFP Setup

### 1. **Shared DNS Cache**
Both the DFP Filter and DFP Cluster use the same cache name:
```yaml
dns_cache_config:
  name: dynamic_forward_proxy_cache  # Same name in both
```

### 2. **DNS Lookup Family**
```yaml
dns_lookup_family: V4_ONLY  # Only IPv4 addresses
```

### 3. **Why This Architecture Works**
- **DFP Filter**: Handles the DNS resolution and caching
- **DFP Cluster**: Handles the actual connection using cached IPs
- **Shared Cache**: Ensures the cluster uses the same resolved IPs as the filter

## Verification Commands

### 1. **Check Pod Status and IPs**
```bash
# Check all pods and their IPs
kubectl get pods -n poc-routing -o wide

# Expected output:
# NAME                     READY   STATUS    RESTARTS   AGE     IP           NODE
# envoy-xxx                1/1     Running   0          3d12h   10.244.0.4   minikube
# server-0                 1/1     Running   0          3d12h   10.244.0.3   minikube
# server-1                 1/1     Running   0          3d12h   10.244.0.5   minikube
```

### 2. **Test DNS Resolution**
```bash
# Test DNS resolution for server instances
kubectl run test-dns --image=busybox --rm -it --restart=Never -- nslookup server-0.server-headless.poc-routing.svc.cluster.local

# Expected output:
# Name: server-0.server-headless.poc-routing.svc.cluster.local
# Address: 10.244.0.3

kubectl run test-dns --image=busybox --rm -it --restart=Never -- nslookup server-1.server-headless.poc-routing.svc.cluster.local

# Expected output:
# Name: server-1.server-headless.poc-routing.svc.cluster.local
# Address: 10.244.0.5
```

### 3. **Test Internal Routing**
```bash
# Test /where endpoint (should work from any pod)
kubectl run test-pod --image=busybox --rm -it --restart=Never -- wget -qO- "http://envoy.poc-routing.svc.cluster.local:10000/where?client_id=123"

# Expected output:
# {"client_id":"123","hostport":"server-1.server-headless.poc-routing.svc.cluster.local:8081"}

# Test /join endpoint (should route to specific instance)
kubectl run test-pod --image=busybox --rm -it --restart=Never -- wget -qO- "http://envoy.poc-routing.svc.cluster.local:10000/join?client_id=123"

# Expected output:
# {"assigned":"server-1:8081","client_id":"123","status":"ok"}
```

### 4. **Test External Access**
```bash
# Get Minikube service URLs
minikube service envoy -n poc-routing --url

# Test with forwarded URL (replace with actual URL from above command)
curl "http://127.0.0.1:XXXXX/where?client_id=123"
curl -v "http://127.0.0.1:XXXXX/join?client_id=123"
```

### 5. **Check Envoy Logs**
```bash
# Check Envoy logs for Lua debug messages
kubectl logs -n poc-routing deployment/envoy | grep "Lua:"

# Expected output:
# Lua: /join path /join?client_id=123
# Lua: resolving client_id=123
# Lua: resolver status(raw)=200
# Lua: parsed hostport=server-1.server-headless.poc-routing.svc.cluster.local:8081
```

### 6. **Check Server Logs**
```bash
# Check server logs for /join requests
kubectl logs -n poc-routing server-0 | grep "/join"
kubectl logs -n poc-routing server-1 | grep "/join"

# Expected output (only one server should have the log):
# 2025/09/26 13:21:22 /join: client_id=123 assigned to server-1:8081
```

### 7. **Verify DNS Cache (Advanced)**
```bash
# Check Envoy admin interface for DNS cache stats
kubectl port-forward -n poc-routing service/envoy 9901:9901

# In another terminal:
curl "http://localhost:9901/stats" | grep dynamic_forward_proxy

# Look for cache hit/miss statistics
```

## Summary

The Envoy configuration implements a sophisticated routing system that:

1. **Routes general requests** (`/where`, `/health`) to any available server instance
2. **Routes specific requests** (`/join`) to calculated server instances based on business logic
3. **Uses Lua scripting** to implement the business logic for target calculation
4. **Uses Dynamic Forward Proxy** to enable per-request DNS resolution
5. **Maintains performance** through DNS caching and efficient filter execution

This architecture allows for both load-balanced and deterministic routing within the same Envoy instance, making it perfect for scenarios where you need to route certain requests to specific backend instances while maintaining high availability and performance.
