package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// getSelf returns this container's host:port string using env PORT and os.Hostname().
func getSelf() string {
	hostname, _ := os.Hostname()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	return fmt.Sprintf("%s:%s", hostname, port)
}

// pickByHashLegacy uses SERVER_PEERS if provided (legacy path)
func pickByHashLegacy(clientID string) string {
	peers := os.Getenv("SERVER_PEERS")
	if peers == "" {
		return getSelf()
	}
	parts := strings.Split(peers, ",")
	filtered := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return getSelf()
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(clientID))
	idx := int(h.Sum32()) % len(filtered)
	return filtered[idx]
}

// computeIndex returns the replica index using either numeric or hash mode,
// and applies INDEX_BASE offset (1 for Compose, 0 for K8s StatefulSet).
func computeIndex(clientID string, replicas int) int {
	if replicas <= 0 {
		replicas = 1
	}
	indexMode := strings.ToLower(strings.TrimSpace(os.Getenv("INDEX_MODE"))) // "numeric" or "hash"
	base := 1
	if v := strings.TrimSpace(os.Getenv("INDEX_BASE")); v != "" {
		if b, err := strconv.Atoi(v); err == nil {
			base = b
		}
	}

	var remainder int
	if indexMode == "numeric" {
		if n, err := strconv.Atoi(clientID); err == nil {
			if n < 0 {
				n = -n
			}
			remainder = n % replicas
		} else {
			// fallback to hash if not numeric
			h := fnv.New32a()
			_, _ = h.Write([]byte(clientID))
			remainder = int(h.Sum32()) % replicas
		}
	} else {
		// default: hash mode
		h := fnv.New32a()
		_, _ = h.Write([]byte(clientID))
		remainder = int(h.Sum32()) % replicas
	}
	return remainder + base
}

// pickScaledTarget computes <SERVICE_PREFIX>-<idx><SERVICE_SUFFIX>:PORT
// Compatible with both Docker Compose (INDEX_BASE=1, no SERVICE_SUFFIX)
// and K8s StatefulSet (INDEX_BASE=0, SERVICE_SUFFIX like .server-headless.ns.svc.cluster.local).
func pickByHashScaled(clientID string) string {
	prefix := os.Getenv("SERVICE_PREFIX")
	if prefix == "" {
		return pickByHashLegacy(clientID)
	}
	replicasStr := os.Getenv("REPLICAS")
	replicas, err := strconv.Atoi(replicasStr)
	if err != nil || replicas <= 0 {
		replicas = 1
	}
	idx := computeIndex(clientID, replicas)
	suffix := os.Getenv("SERVICE_SUFFIX")
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	return fmt.Sprintf("%s-%d%s:%s", prefix, idx, suffix, port)
}

func handleJoin(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "missing client_id", http.StatusBadRequest)
		return
	}

	self := getSelf()

	log.Printf("/join client_id=%s registered to %s", clientID, self)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"client_id": clientID,
		"assigned":  self,
	})
}

func handleWhere(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "missing client_id", http.StatusBadRequest)
		return
	}

	hostPort := pickByHashScaled(clientID)
	log.Printf("/where client_id=%s assigned to %s", clientID, hostPort)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"client_id": clientID,
		"hostport":  hostPort,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	http.HandleFunc("/join", handleJoin)
	http.HandleFunc("/where", handleWhere)
	http.HandleFunc("/health", handleHealth)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	addr := ":" + port
	log.Printf("server starting on %s (hostname=%s)", addr, func() string { h, _ := os.Hostname(); return h }())
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("listen and serve: %v", err)
	}
}
