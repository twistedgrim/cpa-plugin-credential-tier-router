package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

//go:embed web/index.html web/app.css web/app.js
var webAssets embed.FS

type managementRequest struct {
	Method      string              `json:"Method"`
	MethodLower string              `json:"method"`
	Path        string              `json:"Path"`
	PathLower   string              `json:"path"`
	Headers     map[string][]string `json:"Headers"`
	Body        json.RawMessage     `json:"Body"`
	BodyLower   string              `json:"body"`
}

type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

func (r *runtime) handleManagement(ctx context.Context, raw []byte) (managementResponse, error) {
	var request managementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return managementResponse{}, fmt.Errorf("decode management request: %w", err)
	}
	method := firstText(request.Method, request.MethodLower)
	path := firstText(request.Path, request.PathLower)
	body, err := decodeManagementBody(request.Body, request.BodyLower)
	if err != nil {
		return managementResponse{}, err
	}
	path, source := normalizeManagementPath(path)
	if source == "resource" {
		if method != http.MethodGet || path != "/status" {
			return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "resource not found"}), nil
		}
		html, err := renderStatusHTML()
		if err != nil {
			return managementResponse{}, err
		}
		return managementResponse{StatusCode: http.StatusOK, Headers: map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: html}, nil
	}
	switch {
	case method == http.MethodGet && path == "/state":
		state, err := r.dashboard(ctx)
		if err != nil {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": safeError(err)}), nil
		}
		return jsonManagementResponse(http.StatusOK, state), nil
	case method == http.MethodPost && path == "/preview":
		result, err := r.run(ctx, false, "Manual preview")
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errProbeInProgress) {
				status = http.StatusConflict
			}
			return jsonManagementResponse(status, map[string]string{"error": safeError(err)}), nil
		}
		return jsonManagementResponse(http.StatusOK, result), nil
	case method == http.MethodPost && path == "/apply":
		result, err := r.run(ctx, true, "Manual apply")
		if err != nil {
			return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": safeError(err)}), nil
		}
		return jsonManagementResponse(http.StatusOK, result), nil
	case method == http.MethodPut && path == "/settings":
		var next settings
		if err := json.Unmarshal(body, &next); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "Invalid settings format"}), nil
		}
		if next.ManualTiers == nil {
			next.ManualTiers = map[string]tierName{}
		}
		if err := r.updateSettings(next); err != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": safeError(err)}), nil
		}
		return jsonManagementResponse(http.StatusOK, map[string]any{"saved": true, "settings": next}), nil
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]string{"error": "route not found"}), nil
	}
}

func decodeManagementBody(official json.RawMessage, legacy string) ([]byte, error) {
	if len(official) > 0 && string(official) != "null" {
		var encoded string
		if err := json.Unmarshal(official, &encoded); err != nil {
			return nil, errors.New("management Body must be base64")
		}
		if encoded == "" {
			return nil, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("management Body is not valid base64")
		}
		return decoded, nil
	}
	return []byte(legacy), nil
}

func normalizeManagementPath(path string) (string, string) {
	resourcePrefix := "/v0/resource/plugins/" + pluginID
	managementPrefix := "/v0/management/plugins/" + pluginID
	legacyPrefix := "/plugins/" + pluginID
	switch {
	case path == resourcePrefix:
		return "/", "resource"
	case strings.HasPrefix(path, resourcePrefix+"/"):
		return strings.TrimPrefix(path, resourcePrefix), "resource"
	case path == managementPrefix:
		return "/", "management"
	case strings.HasPrefix(path, managementPrefix+"/"):
		return strings.TrimPrefix(path, managementPrefix), "management"
	case path == legacyPrefix:
		return "/", "management"
	case strings.HasPrefix(path, legacyPrefix+"/"):
		return strings.TrimPrefix(path, legacyPrefix), "management"
	default:
		return path, "management"
	}
}

func renderStatusHTML() ([]byte, error) {
	html, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	css, err := webAssets.ReadFile("web/app.css")
	if err != nil {
		return nil, err
	}
	js, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		return nil, err
	}
	page := strings.Replace(string(html), "/*__INLINE_CSS__*/", string(css), 1)
	page = strings.Replace(page, "/*__INLINE_JS__*/", string(js), 1)
	return []byte(page), nil
}

func jsonManagementResponse(status int, payload any) managementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encode response failed"}`)
	}
	return managementResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: body}
}
