package httpx

import (
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

// wsCheckOrigin only accepts WS handshakes whose Origin header matches the
// dashboard's own host. Browsers send cookies on WS handshakes and CORS does
// not protect them, so without this a malicious cross-origin page could open a
// WebSocket using the user's session cookie.
func wsCheckOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// WSUpgrader is the shared websocket upgrader. It checks the Origin header so
// a malicious cross-origin page can't open a WebSocket using the user's
// session cookie.
var WSUpgrader = websocket.Upgrader{
	CheckOrigin: wsCheckOrigin,
}
