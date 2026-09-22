package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"shpyrd/pkg/audit"
	"shpyrd/pkg/authz"
	"shpyrd/pkg/ext"
)

// audit records a mutation performed through the API. project is "" for
// cluster-level actions. Failures are logged, never returned: auditing must
// not break the action.
func (s *Server) audit(c *gin.Context, project, action, target, detail string) {
	if s.kube == nil || s.kube.Kube == nil {
		return
	}
	actor := "anonymous"
	if id, ok := ext.IdentityFrom(c); ok {
		actor = firstNonEmpty(id.Email, id.Name, id.Subject)
		if id.Provider == "token" {
			actor = "admin token"
		}
	}
	ref := audit.ClusterRef(s.deps().SystemNamespace)
	if project != "" {
		ref = audit.AppRef(project)
	}
	entry := audit.Entry{Actor: actor, Action: action, Target: target, Detail: detail, From: c.ClientIP(), Via: "api"}
	if err := audit.Record(c.Request.Context(), s.kube.Kube, ref, entry); err != nil {
		s.log.Warn("audit: cannot record", "action", action, "error", err)
	}
}

// appAudit lists the audit trail of a project.
func (s *Server) appAudit(c *gin.Context) {
	project := authz.ProjectFromNamespace(c.Param("ns"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	entries, err := audit.List(c.Request.Context(), s.kube.Kube, audit.AppRef(project), limit)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, entries)
}
