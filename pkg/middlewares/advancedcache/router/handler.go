package router

import "net/http"

type RouteNotEnabled struct {
}

func NewRouteNotEnabled() *RouteNotEnabled {
	return &RouteNotEnabled{}
}

func (f *RouteNotEnabled) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotAcceptable)
	_, _ = w.Write([]byte(`{
		"status": 403,
		"error": "Forbidden",
		"message": "Route is disabled"
	}`))
}

type RouteInternalError struct {
}

func NewRouteInternalError() *RouteInternalError {
	return &RouteInternalError{}
}

func (f *RouteInternalError) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{
		"status": 500,
		"error":"Internal server error",
		"message": "Please contact support though Reddy: Star team."
	}`))
}

type RouteNotFound struct {
}

func NewRouteNotFound() *RouteNotFound {
	return &RouteNotFound{}
}

func (f *RouteNotFound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"status": 404,"error":"Not Found","message":"Route not found, check the URL is correct."}`))
}

type UnavailableRoute struct{}

func NewUnavailableRoute() *UnavailableRoute {
	return &UnavailableRoute{}
}

func (c *UnavailableRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{
	  "status": 503,
	  "error": "Service unavailable",
	  "message": "Please try again later and contact support though Reddy: Star team."
	}`))
}
