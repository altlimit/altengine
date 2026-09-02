// What a job is told about blob, and nothing more.
//
// Naming a `blobStore` on the container instance puts two variables in every job it launches,
// beside AE_JOB_ID:
//
//	AE_BLOB_URL    http://<this emulator>/v1/blob/<store>
//	AE_BLOB_TOKEN  the bearer to present with it
//
// The job then uses the blob API exactly as any other client does — POST /uploads for an upload
// link, GET /:id for a download one — so an image written against the hosted service works here
// unchanged, which is the whole point of the variables existing locally at all.
//
// WHAT IS NOT FAITHFUL. Hosted, the token is a platform-minted MAC over the job, the org and the
// two instances, expiring with the job and refused for anything but put/get/list. Here the
// emulator's data plane is dev-open — any bearer works — so the token is a readable string and
// bounds nothing. A job that deletes an object succeeds locally and is refused hosted; that is
// the same trade the rest of this emulator makes with API keys, and it is stated rather than
// papered over.
//
// THE HOST HAS TO BE REACHABLE FROM INSIDE THE CONTAINER, which `localhost` is not: it is the
// container's own loopback. Docker publishes the machine as `host.docker.internal` on Desktop
// and, since 20.10, on Linux with `--add-host` — the emulator is normally reached that way, so
// that is what a loopback listen address is rewritten to.

package container

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// dockerHostAlias is how a container addresses the machine the daemon runs on.
const dockerHostAlias = "host.docker.internal"

// BlobJobEnv is the platform environment for one job's blob access, or nil when the instance
// names no store — which is exactly the behaviour before this existed.
//
// The store is NOT checked for existence: instances here are created on first use, so a name
// that does not resolve yet is a store the job is about to create by writing to it.
func BlobJobEnv(cfg Config, r *http.Request) map[string]string {
	store := strings.TrimSpace(cfg.BlobStore)
	if store == "" {
		return nil
	}
	return map[string]string{
		"AE_BLOB_URL":   containerOrigin(r) + "/v1/blob/" + url.PathEscape(store),
		"AE_BLOB_TOKEN": "dev-job-token",
	}
}

// containerOrigin is this emulator's address AS SEEN FROM A CONTAINER.
func containerOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if h, port, err := net.SplitHostPort(host); err == nil && isLoopback(h) {
		host = net.JoinHostPort(dockerHostAlias, port)
	} else if isLoopback(host) {
		host = dockerHostAlias
	}
	return scheme + "://" + host
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
