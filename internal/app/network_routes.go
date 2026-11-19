package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/xtulnx/webhook/internal/network"
)

func networkTransportHandler(next http.Handler, hub *network.Hub) http.Handler {
	if hub == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if hub.Handles(req.URL.Path) {
			hub.ServeHTTP(w, req)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func registerNetworkHookRoutes(r *mux.Router, hub *network.Hub) {
	if hub == nil {
		return
	}
	for _, route := range hub.Routes() {
		matcher := r.NewRoute()
		if route.Prefix != "" {
			matcher.PathPrefix(route.Prefix + "/")
		} else {
			matcher.Path(route.Path)
		}
		matcher.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			clientIP := extractIP(resolveRealIP(req))
			if clientIP == nil {
				http.Error(w, "invalid client IP", http.StatusForbidden)
				return
			}
			hub.ServeHook(w, req, network.RequestOrigin{ClientIP: clientIP.String(), Scheme: resolveOriginalProto(req)}, *maxBodySize)
		})
	}
}

func validateNetworkRoutes(cfg *network.HubConfig) error {
	type appRoute struct{ name, path string }
	hooksPath := "/" + strings.Trim(*hooksURLPrefix, "/")
	statusPath := "/" + strings.Trim(*statusURLPrefix, "/")
	appPaths := []appRoute{{"urlprefix", hooksPath}, {"status-path", statusPath}}
	if *adminEnabled {
		appPaths = append(appPaths, appRoute{"admin-path", "/" + strings.Trim(*adminURLPrefix, "/")})
	}
	for _, appPath := range appPaths {
		for _, reserved := range cfg.ReservedRoutes() {
			if network.PathsOverlap(appPath.path, reserved) {
				return fmt.Errorf("%s overlaps the reserved network route %q", appPath.name, reserved)
			}
		}
		for _, route := range cfg.Routes {
			if route.Prefix != "" && network.PathsOverlap(route.Prefix, appPath.path) ||
				route.Path == appPath.path && appPath.name == "status-path" ||
				appPath.name == "admin-path" && (route.Path == appPath.path || strings.HasPrefix(route.Path, appPath.path+"/")) {
				return fmt.Errorf("hook route %q overlaps %s", route.Mount(), appPath.name)
			}
		}
	}
	return nil
}

func prepareWebhookAgent(cfg *network.AgentConfig) error {
	if cfg.WebhookOnlyViaHub {
		if socket != "" {
			return errors.New("webhook_only_via_hub cannot use a local socket")
		}
		return nil
	}
	for _, service := range cfg.Services {
		if service.Name == "webhook" {
			if service.RelayTo != "" {
				return errors.New("with_webhook cannot relay the webhook service to a downstream node")
			}
			return nil
		}
	}
	cfg.Services = append(cfg.Services, network.Service{Name: "webhook", InProcess: true})
	return nil
}
