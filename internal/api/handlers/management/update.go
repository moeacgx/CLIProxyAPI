package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/selfupdate"
)

func (h *Handler) ensureUpdateService() *selfupdate.Service {
	if h == nil {
		return nil
	}
	if h.updateService == nil {
		h.updateService = selfupdate.NewService(h.configFilePath)
	}
	return h.updateService
}

// GetUpdateStatus returns the current update status snapshot.
func (h *Handler) GetUpdateStatus(c *gin.Context) {
	service := h.ensureUpdateService()
	if service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update service not initialized"})
		return
	}
	c.JSON(http.StatusOK, service.Status(h.cfg))
}

// CheckUpdate checks the latest GitHub Release and returns status.
func (h *Handler) CheckUpdate(c *gin.Context) {
	service := h.ensureUpdateService()
	if service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update service not initialized"})
		return
	}
	status := service.Check(c.Request.Context(), h.cfg)
	if status.LastError != "" {
		c.JSON(http.StatusBadRequest, status)
		return
	}
	c.JSON(http.StatusOK, status)
}

// ConfirmUpdate downloads and applies the update package, then triggers restart.
func (h *Handler) ConfirmUpdate(c *gin.Context) {
	service := h.ensureUpdateService()
	if service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update service not initialized"})
		return
	}
	status := service.Confirm(c.Request.Context(), h.cfg)
	if status.LastError != "" {
		c.JSON(http.StatusBadRequest, status)
		return
	}
	if h.cfg != nil {
		staticDir := managementasset.StaticDir(h.configFilePath)
		managementasset.EnsureLatestManagementHTML(
			c.Request.Context(),
			staticDir,
			h.cfg.ProxyURL,
			h.cfg.RemoteManagement.PanelGitHubRepository,
		)
	}
	c.JSON(http.StatusOK, status)
}
