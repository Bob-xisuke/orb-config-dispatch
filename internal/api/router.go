package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-config-dispatch/internal/store"
)

// NewRouter wires the public HTTP surface. The service contract in README.md describes the
// error shape every entry must keep.
func NewRouter(st *store.Store) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	router.GET("/healthz", func(c *gin.Context) {
		if err := st.Ping(); err != nil {
			writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, messageStorageUnavailable)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "ok"})
	})

	router.POST("/v1/config-rollouts", registerRollout(st))
	router.GET("/v1/config-rollouts", listRollouts(st))

	router.NoRoute(func(c *gin.Context) {
		writeError(c, http.StatusNotFound, "route_not_found", "no route matches this path")
	})
	return router
}

// writeError emits the shared top-level error object.
func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
