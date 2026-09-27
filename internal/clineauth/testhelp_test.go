package clineauth

import (
	"net/http/httptest"

	"ceraa/internal/oauthdevice"
	"ceraa/internal/secret"
)

func secretInit(key, file string) error { return secret.Init(key, file) }
func secretSet(name, value string) error {
	return secret.Set(name, value)
}

// stubEndpoints points the device flow at a test server.
func stubEndpoints(srv *httptest.Server) oauthdevice.Endpoints {
	return oauthdevice.Endpoints{
		DeviceCode: srv.URL + "/user_management/authorize/device",
		Token:      srv.URL + "/user_management/authenticate",
	}
}
