package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"shpyrd/pkg/audit"
	"shpyrd/pkg/ext"
	projectpkg "shpyrd/pkg/project"
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
		switch id.Provider {
		case "token":
			actor = "admin token"
		case "api-token":
			// joao@acme.test (token ci): who, and which credential acted.
			actor = fmt.Sprintf("%s (token %s)", id.Email, id.Name)
		}
	}
	ref := audit.ClusterRef(s.deps().SystemNamespace)
	if project != "" {
		ref = audit.AppRefIn(projectpkg.NamespaceIn(s.workspace(c), project), project)
	}
	entry := audit.Entry{Actor: actor, Action: action, Target: target, Detail: detail, From: c.ClientIP(), Via: "api"}
	if err := audit.Record(c.Request.Context(), s.kube.Kube, ref, entry); err != nil {
		s.log.Warn("audit: cannot record", "action", action, "error", err)
	}
}

// auditAnonymous records a security event of an unauthenticated client.
func (s *Server) auditAnonymous(c *gin.Context, action, detail string) {
	s.auditFailure(c, action, c.ClientIP(), detail)
}

// auditFailure records a failed attempt against a named target (an account
// that was tried) by an unauthenticated client.
func (s *Server) auditFailure(c *gin.Context, action, target, detail string) {
	if s.kube == nil || s.kube.Kube == nil {
		return
	}
	entry := audit.Entry{Actor: "anonymous", Action: action, Target: target, Detail: detail, From: c.ClientIP(), Via: "api"}
	if err := audit.Record(c.Request.Context(), s.kube.Kube, audit.ClusterRef(s.deps().SystemNamespace), entry); err != nil {
		s.log.Warn("audit: cannot record", "action", action, "error", err)
	}
}

// appAudit lists the audit trail of a project.
func (s *Server) appAudit(c *gin.Context) {
	project := c.Param("slug")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	entries, err := audit.List(c.Request.Context(), s.kube.Kube, audit.AppRefIn(projectpkg.NamespaceIn(s.workspace(c), project), project), limit)
	if err != nil {
		abort(c, http.StatusBadGateway, err)
		return
	}
	c.JSON(http.StatusOK, entries)
}
