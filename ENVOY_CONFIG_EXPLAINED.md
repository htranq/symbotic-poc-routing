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

## Summary

The Envoy configuration implements a sophisticated routing system that:

1. **Routes general requests** (`/where`, `/health`) to any available server instance
2. **Routes specific requests** (`/join`) to calculated server instances based on business logic
3. **Uses Lua scripting** to implement the business logic for target calculation
4. **Uses Dynamic Forward Proxy** to enable per-request DNS resolution
5. **Maintains performance** through DNS caching and efficient filter execution

This architecture allows for both load-balanced and deterministic routing within the same Envoy instance, making it perfect for scenarios where you need to route certain requests to specific backend instances while maintaining high availability and performance.
