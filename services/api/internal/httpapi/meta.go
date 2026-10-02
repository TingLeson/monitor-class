package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Build identity, injected at link time:
//
//	go build -ldflags "-X github.com/classwatch/classwatch/services/api/internal/httpapi.Version=1.2.3 \
//	                   -X github.com/classwatch/classwatch/services/api/internal/httpapi.Commit=$(git rev-parse HEAD)"
//
// The defaults are honest placeholders for `go run`, not a fake version: a
// "dev" build must be identifiable as such, otherwise a stale local binary is
// indistinguishable from a release during an incident.
var (
	// Version is the release version of this binary.
	Version = "dev"
	// Commit is the git revision this binary was built from.
	Commit = "unknown"
)

// Phase names the development phase this build implements.
//
// It is served so the frontend and the task book can be matched without guessing
// from behaviour: Phase 0 is a skeleton with no business features, and a client
// probing for /api/v1/classrooms should be able to see why it is missing.
const Phase = "phase-0"

// metaResponse describes the running service.
type metaResponse struct {
	Service string `json:"service"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Env     string `json:"env"`
	Phase   string `json:"phase"`
	// Time is RFC3339 in UTC. It is included so a client can detect clock skew
	// against its own clock, which matters for anything time-based later on.
	Time string `json:"time"`
}

// metaHandler reports what is running.
//
// It exposes no configuration beyond the environment name: version and commit are
// already public in the image tag, while DSNs, keys and secrets are not part of
// this struct and must never be added to it.
func metaHandler(env string) gin.HandlerFunc {
	return func(c *gin.Context) {
		RespondJSON(c, http.StatusOK, metaResponse{
			Service: "classwatch-api",
			Version: Version,
			Commit:  Commit,
			Env:     env,
			Phase:   Phase,
			Time:    time.Now().UTC().Format(time.RFC3339),
		})
	}
}
